package unity

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"vrclib/internal/core"
)

// TaskNP: making a new VRChat project, as the window's task list shows it.
var TaskNP = &core.Task{Name: "newproject", Label: "新建工程"}

// NewProjectReq: the form the window sends for 「新建基础工程」.
type NewProjectReq struct {
	Name    string   `json:"name"`
	Parent  string   `json:"parent"`            // the folder the project is made in
	Base    string   `json:"base,omitempty"`    // the base body to put in, by asset key ("" = none)
	Plugins []string `json:"plugins,omitempty"` // the packages to install (Modular Avatar …)
	AIKit   bool     `json:"aiKit,omitempty"`   // install UnitySkills as well
}

// NewProjectJob: how making a project is going. The stages are the ones the window lists (prepare, repos,
// packages, settings, base, kit), then done or failed.
type NewProjectJob struct {
	Name     string   `json:"name"`
	Path     string   `json:"path"`
	Stage    string   `json:"stage"`
	Msg      string   `json:"msg,omitempty"`
	Done     int      `json:"done,omitempty"`
	Total    int      `json:"total,omitempty"`
	Packages []string `json:"packages,omitempty"` // what was installed
	Notes    []string `json:"notes,omitempty"`    // what the player is to know (a plugin left out, UnitySkills …)
	Err      string   `json:"err,omitempty"`
	Started  int64    `json:"started,omitempty"`
	Ended    int64    `json:"ended,omitempty"`
}

// BaseChoice: one of the base bodies the new-project form offers (the library's 素体 cards).
type BaseChoice struct {
	Key   string   `json:"key"`
	Name  string   `json:"name"`
	Bases []string `json:"bases,omitempty"`
}

// Toolchain: what this computer has for making and opening projects, as the new-project form reads it.
type Toolchain struct {
	Alcom      string       `json:"alcom,omitempty"`     // the path of ALCOM, "" when it is not installed
	AlcomSeen  bool         `json:"alcomSeen,omitempty"` // no ALCOM, but its or VCC's settings are there
	VCC        string       `json:"vcc,omitempty"`
	Hub        string       `json:"hub,omitempty"`
	Unity      string       `json:"unity,omitempty"`     // the editor of the version VRChat wants
	UnityWant  string       `json:"unityWant,omitempty"` // that version
	HubLink    string       `json:"hubInstallLink,omitempty"`
	Editors    []string     `json:"editors,omitempty"` // every Unity version installed
	Plugins    []plugin     `json:"plugins,omitempty"`
	Bases      []BaseChoice `json:"bases,omitempty"`
	DefaultDir string       `json:"defaultDir,omitempty"` // where projects are made by default
	SkillsVer  string       `json:"skillsVersion,omitempty"`
	Note       string       `json:"note,omitempty"`
}

var (
	npMu  sync.Mutex
	npJob *NewProjectJob
)

// NewProjectSnapshot: the project being made, or the last one until it is dismissed.
func NewProjectSnapshot() *NewProjectJob {
	npMu.Lock()
	defer npMu.Unlock()
	if npJob == nil {
		return nil
	}
	j := *npJob
	return &j
}

// DismissNewProject: the window has shown the result; forget it.
func DismissNewProject() {
	npMu.Lock()
	npJob = nil
	npMu.Unlock()
	core.BumpRev()
}

// FindToolchain: what is on this computer, and what the new-project form offers.
func FindToolchain(st *core.Store) Toolchain {
	t := Toolchain{Alcom: FindAlcom(), VCC: FindVCC(), Hub: FindUnityHub(), UnityWant: UnityWant,
		HubLink: "unityhub://" + UnityWant + "/" + unityWantHash, SkillsVer: SkillsVersion}
	t.AlcomSeen = t.Alcom == "" && (alcomConfigured() || core.StatOK(filepath.Join(vccDir(), "settings.json")))
	for ver, exe := range unityEditors() {
		t.Editors = append(t.Editors, ver)
		if ver == UnityWant {
			t.Unity = exe
		}
	}
	sort.Strings(t.Editors)
	t.Plugins = append(append([]plugin{}, basePlugins...), gitPlugins...)
	st.Mu.RLock()
	for _, a := range st.Assets {
		if a.Category == "素体" {
			t.Bases = append(t.Bases, BaseChoice{Key: a.Key, Name: a.Name, Bases: a.Bases})
		}
	}
	if len(st.Settings.ProjectRoots) > 0 {
		t.DefaultDir = st.Settings.ProjectRoots[0]
	}
	st.Mu.RUnlock()
	sort.Slice(t.Bases, func(i, j int) bool { return t.Bases[i].Name < t.Bases[j].Name })
	return t
}

// npMarker: the file a project being made carries until it is done. A folder that holds it is what an
// attempt that failed left behind, which a new attempt may take over.
const npMarker = ".miovrca-creating"

// NewProjectTaken: a project of that name is there already (what a failed attempt left behind does not count).
func NewProjectTaken(p string) bool {
	ents, err := os.ReadDir(p)
	if err != nil {
		return false // not there, or not a folder we can look into: nothing to lose
	}
	if len(ents) == 0 || core.StatOK(filepath.Join(p, npMarker)) {
		return false
	}
	return true
}

var npCancel context.CancelFunc

// StartNewProject begins making a project in the background.
func StartNewProject(st *core.Store, req NewProjectReq) error {
	name := strings.TrimSpace(req.Name)
	if name == "" || core.SafeName(name, 80) != name || strings.ContainsAny(name, `/\`) {
		return errors.New("工程名称无效（不能包含 \\ / : * ? \" < > | 等字符）")
	}
	parent := filepath.Clean(strings.TrimSpace(req.Parent))
	if fi, err := os.Stat(parent); err != nil || !fi.IsDir() {
		return errors.New("保存位置不存在：" + parent)
	}
	dir := filepath.Join(parent, name)
	if NewProjectTaken(dir) {
		return errors.New("该位置已有同名文件夹：" + dir)
	}
	npMu.Lock()
	if npJob != nil && npJob.Stage != "done" && npJob.Stage != "failed" {
		npMu.Unlock()
		return errors.New("正在创建另一个工程，请稍候")
	}
	ctx, cancel := context.WithCancel(context.Background())
	npCancel = cancel
	npJob = &NewProjectJob{Name: name, Path: dir, Stage: "prepare", Msg: "正在准备", Started: time.Now().Unix()}
	npMu.Unlock()
	core.BumpRev()
	go core.RunTask(TaskNP, func() { runNewProject(ctx, st, req, dir) })
	return nil
}

// CancelNewProject stops the one being made.
func CancelNewProject() {
	npMu.Lock()
	if npCancel != nil {
		npCancel()
	}
	npMu.Unlock()
}

func setNP(f func(*NewProjectJob)) {
	npMu.Lock()
	if npJob != nil {
		f(npJob)
	}
	npMu.Unlock()
	core.BumpRev()
}

func npStage(stage, msg string) { setNP(func(j *NewProjectJob) { j.Stage, j.Msg = stage, msg }) }
func npNote(n string) {
	if n != "" {
		setNP(func(j *NewProjectJob) { j.Notes = append(j.Notes, n) })
	}
}

func npFail(ctx context.Context, err error) {
	msg := core.TrimErr(err)
	if ctx.Err() != nil {
		msg = "已取消"
	}
	core.Logf("新建工程失败：%s", msg)
	setNP(func(j *NewProjectJob) { j.Stage, j.Err, j.Ended = "failed", msg, time.Now().Unix() })
}

// runNewProject makes the project, stage after stage, as the window lists them.
func runNewProject(ctx context.Context, st *core.Store, req NewProjectReq, dir string) {
	// ---- prepare: the folders, the Unity version, the manifest
	for _, d := range []string{"Assets", "Packages", "ProjectSettings"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0755); err != nil {
			npFail(ctx, err)
			return
		}
	}
	_ = os.WriteFile(filepath.Join(dir, npMarker), []byte("MioVRCA 正在创建该工程\n"), 0644)
	ver := "m_EditorVersion: " + UnityWant + "\nm_EditorVersionWithRevision: " + UnityWant + " (" + unityWantHash + ")\n"
	if err := os.WriteFile(filepath.Join(dir, "ProjectSettings", "ProjectVersion.txt"), []byte(ver), 0644); err != nil {
		npFail(ctx, err)
		return
	}
	if err := writeUnityManifest(dir); err != nil {
		npFail(ctx, err)
		return
	}
	// ---- repos: VRChat's own two, and the ones of the plugins picked
	npStage("repos", "正在读取插件仓库")
	want := []string{"com.vrchat.avatars"}
	repos := []string{"com.vrchat.repos.official", "com.vrchat.repos.curated"}
	var gits []string
	for _, pk := range req.Plugins {
		pl, ok := pluginOf(pk)
		if !ok {
			continue
		}
		if pl.Git {
			gits = append(gits, pk)
			continue
		}
		if !core.ContainsStr(want, pk) {
			want = append(want, pk)
		}
		if !core.ContainsStr(repos, pl.Repo) {
			repos = append(repos, pl.Repo)
		}
	}
	for _, pl := range basePlugins { // what the pipeline cannot do without goes in whatever the form said
		if pl.Fixed && !core.ContainsStr(want, pl.Pkg) {
			want = append(want, pl.Pkg)
			if !core.ContainsStr(repos, pl.Repo) {
				repos = append(repos, pl.Repo)
			}
		}
	}
	c := core.HTTPClient(st)
	var listings []*vpmListing
	for _, id := range repos {
		rctx, cancel := context.WithTimeout(ctx, fetchTimeout)
		l, err := fetchListing(rctx, c, id)
		cancel()
		if err != nil {
			if ctx.Err() != nil || strings.HasPrefix(id, "com.vrchat.") {
				npFail(ctx, err) // without the SDK there is no VRChat project
				return
			}
			npNote(err.Error() + "，相关插件未安装")
			continue
		}
		listings = append(listings, l)
	}
	// the resolver VCC adds to every project, when the official repository has it
	for _, l := range listings {
		if _, ok := l.Packages["com.vrchat.core.vpm-resolver"]; ok && !core.ContainsStr(want, "com.vrchat.core.vpm-resolver") {
			want = append(want, "com.vrchat.core.vpm-resolver")
		}
	}
	// ---- packages
	npStage("packages", "正在下载插件")
	picked, err := resolve(listings, want, UnityWant)
	if err != nil {
		// a plugin that cannot be resolved is left out; the SDK is not optional
		var ok []string
		for _, w := range want {
			if _, e := resolve(listings, []string{w}, UnityWant); e == nil {
				ok = append(ok, w)
			} else if strings.HasPrefix(w, "com.vrchat.") {
				npFail(ctx, e)
				return
			} else {
				npNote(e.Error() + "，已跳过")
			}
		}
		want = ok
		if picked, err = resolve(listings, want, UnityWant); err != nil {
			npFail(ctx, err)
			return
		}
	}
	names := make([]string, 0, len(picked))
	for n := range picked {
		names = append(names, n)
	}
	sort.Strings(names)
	setNP(func(j *NewProjectJob) { j.Total, j.Done = len(names), 0 })
	for i, n := range names {
		if ctx.Err() != nil {
			npFail(ctx, ctx.Err())
			return
		}
		setNP(func(j *NewProjectJob) { j.Msg, j.Done = "正在下载 "+n, i })
		if err := installVPM(ctx, c, dir, picked[n]); err != nil {
			npFail(ctx, err)
			return
		}
		setNP(func(j *NewProjectJob) { j.Packages = append(j.Packages, n+" "+picked[n].Version) })
	}
	if err := writeVPMManifest(dir, want, picked); err != nil {
		npFail(ctx, err)
		return
	}
	for _, g := range gits {
		if g == SkillsPkg {
			npNote(installSkills(ctx, dir))
		}
	}
	// ---- settings: the template's files where a stand-in offers them; Unity writes the rest
	npStage("settings", "正在写入工程设置")
	if b := vpmBase(); b != "" {
		for _, f := range []string{"ProjectSettings.asset", "TagManager.asset", "AudioManager.asset"} {
			if data, err := httpGet(ctx, c, b+"/template/"+f, 4<<20); err == nil {
				if f == "ProjectSettings.asset" {
					data = []byte(strings.Replace(string(data), "productName: World", "productName: "+filepath.Base(dir), 1))
				}
				_ = os.WriteFile(filepath.Join(dir, "ProjectSettings", f), data, 0644)
			}
		}
	}
	addToVCC(dir)
	st.Mu.Lock()
	known := false
	for _, p := range st.Projects {
		if core.PathKey(p.Path) == core.PathKey(dir) {
			known = true
		}
	}
	if !known {
		st.Projects = append(st.Projects, core.ProjectInfo{Name: filepath.Base(dir), Path: dir})
	}
	st.Mu.Unlock()
	_ = st.Save()
	_ = os.Remove(filepath.Join(dir, npMarker))
	// ---- base: the base body, imported as any asset
	if req.Base != "" {
		npStage("base", "正在导入素体")
		res, err := ImportAndWait(ctx, st, ImportReq{Key: req.Base, Project: dir, Recycle: true, Quiet: true}, nil)
		switch {
		case err != nil:
			npNote("素体未导入：" + core.TrimErr(err))
		case res.Stage != "done":
			npNote("素体未导入：" + firstNonEmpty(res.Err, res.Msg, "导入失败"))
		}
		DismissImport()
	}
	// ---- kit: the pipeline's plugin, and the editor
	if req.AIKit {
		npStage("kit", "正在安装 Unity 插件")
		note, err := InstallAIKit(dir)
		if err != nil {
			npNote(core.TrimErr(err))
		} else {
			npNote(note)
		}
	}
	setNP(func(j *NewProjectJob) { j.Stage, j.Msg, j.Ended = "done", "", time.Now().Unix() })
	core.Logf("已新建工程 %s（%d 个包）", dir, len(names))
	core.Notify(core.Notice{Kind: "newproject", What: dir})
}

// addToVCC adds the project to the list VCC and ALCOM show, when this computer has their settings.
func addToVCC(dir string) {
	f := filepath.Join(vccDir(), "settings.json")
	b, err := os.ReadFile(f)
	if err != nil {
		return
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return
	}
	list, _ := m["userProjects"].([]any)
	for _, p := range list {
		if s, _ := p.(string); core.PathKey(s) == core.PathKey(dir) {
			return
		}
	}
	m["userProjects"] = append(list, dir)
	if out, err := json.MarshalIndent(m, "", "  "); err == nil {
		_ = os.WriteFile(f, out, 0644)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
