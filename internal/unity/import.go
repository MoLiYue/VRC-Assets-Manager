package unity

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"vrclib/internal/archive"
	"vrclib/internal/core"
	"vrclib/internal/library"
)

// TaskImport: the one-click import, as the window's task list shows it.
var TaskImport = &core.Task{Name: "import", Label: "导入素材"}

// ImportReq: what an import is asked for. It is read straight out of the request body, so the names are the
// window's (web/app.js).
type ImportReq struct {
	Key     string   `json:"key"`               // the asset to import
	Project string   `json:"project"`           // the Unity project it goes into
	Paths   []string `json:"paths,omitempty"`   // only these packages (empty: the asset's own)
	Pwd     string   `json:"pwd,omitempty"`     // the password of an archive that needs one
	Recycle bool     `json:"recycle,omitempty"` // put the archives in the Recycle Bin once unpacked
	Split   bool     `json:"split,omitempty"`   // unpack a collection and make a card for each product
	Quiet   bool     `json:"quiet,omitempty"`   // no sound and no notice at the end (an update carries it)
}

// ImportJob: how an import is going, as the window reads it.
type ImportJob struct {
	Key      string      `json:"key"`
	Name     string      `json:"name"`
	Project  string      `json:"project"`
	Stage    string      `json:"stage"` // unpack, choose, copy, done, failed, cancelled
	Msg      string      `json:"msg,omitempty"`
	Done     int64       `json:"done,omitempty"`
	Total    int64       `json:"total,omitempty"`
	Choices  []PkgChoice `json:"choices,omitempty"`  // packages the player is to pick from (stage "choose")
	Why      string      `json:"why,omitempty"`      // of the choice: "" versions for several base bodies, "overlap"
	Imported []string    `json:"imported,omitempty"` // the packages that went in
	Skipped  []PkgSkip   `json:"skipped,omitempty"`  // and the ones left out, with the reason
	Tops     []string    `json:"tops,omitempty"`     // the folders under Assets they went into
	Failed   []string    `json:"failed,omitempty"`   // archives that could not be unpacked
	Removed  int         `json:"removed,omitempty"`  // archives put in the Recycle Bin once unpacked
	Packages []string    `json:"packages,omitempty"`
	Files    int         `json:"files,omitempty"` // files written into the project
	Kept     int         `json:"kept,omitempty"`  // files of packages already installed, left alone
	KeptPkgs []string    `json:"keptPkgs,omitempty"`
	Split    bool        `json:"split,omitempty"`  // it is unpacking a collection, not importing
	Bundle   int         `json:"bundle,omitempty"` // and this many products came out of it
	Err      string      `json:"err,omitempty"`
	Started  int64       `json:"started,omitempty"`
	Ended    int64       `json:"ended,omitempty"`
}

var (
	impMu  sync.Mutex
	impJob *ImportJob
)

// ImportSnapshot: the import under way, or the last one until it is dismissed; nil when there is none.
func ImportSnapshot() *ImportJob {
	impMu.Lock()
	defer impMu.Unlock()
	if impJob == nil {
		return nil
	}
	j := *impJob
	return &j
}

// DismissImport: the window has shown the result; forget it. An import that waits for a choice is called
// off.
func DismissImport() {
	impMu.Lock()
	if impJob != nil && impJob.Stage == "choose" {
		select {
		case impPick <- nil:
		default:
		}
	}
	impJob = nil
	impMu.Unlock()
	core.BumpRev()
}

// ImportAsk: what the player is to confirm before this import runs ("" = nothing). A base body going into a
// project built on another one is the case it is for.
func ImportAsk(st *core.Store, key, project string) string {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	a := assetOf(st, key)
	if a == nil || a.Category != "素体" || len(a.Bases) == 0 {
		return ""
	}
	for _, other := range st.Assets {
		if other.Key == a.Key || other.Category != "素体" || len(other.Bases) == 0 {
			continue
		}
		for _, u := range other.Usage {
			if u.Status != "used" || !sameProject(u.Project, project) {
				continue
			}
			if !core.ContainsStr(a.Bases, other.Bases[0]) {
				return "「" + a.Name + "」是 " + a.Bases[0] + " 的素体，目标工程「" + filepath.Base(project) +
					"」用的是 " + other.Bases[0] + " 素体。仍要导入到这个工程吗？"
			}
		}
	}
	return ""
}

// sameProject: the usage names the project ("Kaguya" for a project at …/Kaguya, or the whole path).
func sameProject(named, project string) bool {
	return named != "" && (core.PathKey(named) == core.PathKey(project) ||
		strings.EqualFold(named, filepath.Base(project)))
}

// assetOf: the card of a key. The store is held.
func assetOf(st *core.Store, key string) *core.Asset {
	for _, a := range st.Assets {
		if a.Key == key || (a.AltKey != "" && a.AltKey == key) {
			return a
		}
	}
	return nil
}

// StartImport begins an import in the background. Only one runs at a time.
func StartImport(st *core.Store, req ImportReq) error {
	if strings.TrimSpace(req.Key) == "" {
		return errors.New("未指定要导入的素材")
	}
	st.Mu.RLock()
	a := assetOf(st, req.Key)
	var name string
	var dirs, pkgs, arcs []string
	bundle := 0
	if a != nil {
		name, bundle = a.Name, a.Bundle
		for _, l := range a.Locations {
			dirs = append(dirs, l.Path)
			if l.Kind != "dir" {
				if strings.EqualFold(filepath.Ext(l.Path), ".unitypackage") {
					pkgs = append(pkgs, l.Path)
				} else {
					arcs = append(arcs, l.Path)
				}
			}
		}
		for _, l := range a.Locations {
			if l.Kind != "dir" {
				continue
			}
			for _, rel := range a.Packages {
				if p := filepath.Join(l.Path, filepath.FromSlash(rel)); core.StatOK(p) {
					pkgs = append(pkgs, p)
				}
			}
			for _, rel := range a.Archives {
				if p := filepath.Join(l.Path, filepath.FromSlash(rel)); core.StatOK(p) {
					arcs = append(arcs, p)
				}
			}
		}
	}
	st.Mu.RUnlock()
	if a == nil {
		return errors.New("未找到该素材")
	}
	if !req.Split && bundle >= 2 {
		return errors.New("合集包包含 " + itoa(bundle) + " 个素材，请先拆分，再分别导入")
	}
	if !req.Split && !core.IsUnityProject(req.Project) {
		return errors.New("该文件夹不是 Unity 工程（未找到 Assets 和 ProjectSettings 文件夹）")
	}
	impMu.Lock()
	if impJob != nil && impJob.Stage != "done" && impJob.Stage != "failed" && impJob.Stage != "cancelled" {
		impMu.Unlock()
		return ErrImportBusy
	}
	impJob = &ImportJob{Key: req.Key, Name: name, Project: req.Project, Stage: "unpack",
		Split: req.Split, Started: time.Now().Unix()}
	if req.Split {
		impJob.Project = ""
	}
	impMu.Unlock()
	core.BumpRev()
	go func() {
		core.RunTask(TaskImport, func() { runImport(st, req, dirs, pkgs, arcs) })
	}()
	return nil
}

// setImp changes the job the window reads.
func setImp(f func(*ImportJob)) {
	impMu.Lock()
	if impJob != nil {
		f(impJob)
	}
	impMu.Unlock()
	core.BumpRev()
}

func endImp(stage, msg, err string) {
	setImp(func(j *ImportJob) {
		j.Stage, j.Msg, j.Err, j.Ended = stage, msg, err, time.Now().Unix()
	})
}

// runImport does the work: unpack what is still packed, pick the packages, and write them into the project.
func runImport(st *core.Store, req ImportReq, dirs, pkgs, arcs []string) {
	// what is still inside an archive is unpacked first, in the asset's own folder
	if len(arcs) > 0 {
		setImp(func(j *ImportJob) { j.Msg = "正在解压" })
		st.Mu.RLock()
		keep := st.Settings.KeepZip
		st.Mu.RUnlock()
		remove := core.RecycleOnly
		if keep || !req.Recycle {
			remove = func([]string) error { return nil }
		}
		res := archive.UnpackAll(arcs, req.Pwd, remove, func(name string, i, n int) {
			setImp(func(j *ImportJob) { j.Msg, j.Done, j.Total = "正在解压 "+name, int64(i), int64(n) })
		})
		var failed []string
		for f, why := range res.Failed {
			core.Logf("导入解压失败 %s：%s", f, why)
			failed = append(failed, filepath.Base(f)+"："+why)
		}
		sort.Strings(failed)
		setImp(func(j *ImportJob) { j.Failed, j.Removed = failed, res.Removed })
		if len(res.Failed) > 0 && len(res.Done) == 0 {
			endImp("failed", "", "解压失败，请检查压缩包或密码")
			return
		}
		// what came out may hold the packages
		for _, d := range res.Done {
			dirs = append(dirs, d)
		}
		for _, d := range res.Done {
			pkgs = append(pkgs, packagesIn(d)...)
		}
	}
	if req.Split {
		finishSplit(st, dirs)
		return
	}
	if len(req.Paths) > 0 {
		pkgs = req.Paths
	}
	if len(pkgs) == 0 {
		endImp("failed", "", "该素材中没有可导入的 unitypackage")
		return
	}
	in, left := pickPackages(uniq(pkgs), ProjectTarget(req.Project))
	if len(in) == 0 {
		endImp("failed", "", "没有要导入的 unitypackage")
		return
	}
	var skipped []PkgSkip
	for _, p := range left {
		skipped = append(skipped, PkgSkip{Name: p.Name, Why: skipCode(p.Why)})
	}
	// what the player is to choose, unless the packages were named
	if len(req.Paths) == 0 {
		if cs, why := choicesFor(st, req.Project, in); cs != nil {
			select { // an answer left over from before is not this one's
			case <-impPick:
			default:
			}
			setImp(func(j *ImportJob) { j.Stage, j.Msg, j.Choices, j.Why = "choose", "", cs, why })
			var picked []string
		wait:
			for {
				select {
				case picked = <-impPick:
					break wait
				case <-time.After(500 * time.Millisecond):
					if core.Quitting.Load() {
						endImp("cancelled", "已停止", "")
						return
					}
				}
			}
			if picked == nil {
				endImp("cancelled", "已取消", "")
				return
			}
			var chosen []pkgPick
			for _, c := range cs {
				for _, p := range in {
					if p.Path == c.Path {
						if core.ContainsStr(picked, c.Path) {
							chosen = append(chosen, p)
						} else {
							code := "base"
							if why == "overlap" {
								code = "alt"
							}
							skipped = append(skipped, PkgSkip{Name: c.Name, Why: code})
						}
					}
				}
			}
			if len(chosen) == 0 {
				endImp("cancelled", "未选择要导入的包", "")
				return
			}
			in = chosen
		}
	}
	setImp(func(j *ImportJob) {
		j.Stage, j.Msg, j.Total, j.Done, j.Choices, j.Why = "copy", "", int64(len(in)), 0, nil, ""
		j.Skipped = skipped
	})
	var tops []string
	files, kept := 0, 0
	var done []string
	var keptPkgs []string
	for i, p := range in {
		if core.Quitting.Load() {
			endImp("cancelled", "已停止", "")
			return
		}
		setImp(func(j *ImportJob) { j.Msg, j.Done = "正在导入 "+p.Name, int64(i) })
		entries, err := readPackage(p.Path)
		if err != nil {
			endImp("failed", "", p.Name+"："+core.TrimErr(err))
			return
		}
		w, k, err := writePackage(req.Project, entries)
		if err != nil {
			endImp("failed", "", "无法写入 "+filepath.Base(req.Project)+"："+core.TrimErr(err))
			return
		}
		files, kept = files+w, kept+k
		done = append(done, p.Name)
		for _, t := range topsOf(entries) {
			if !core.ContainsStr(tops, t) {
				tops = append(tops, t)
			}
		}
		if k > 0 {
			keptPkgs = append(keptPkgs, p.Name)
		}
	}
	setImp(func(j *ImportJob) {
		j.Stage, j.Msg, j.Done = "done", "", int64(len(in))
		j.Imported, j.Files, j.Kept, j.KeptPkgs, j.Tops = done, files, kept, keptPkgs, tops
		j.Packages = done
	})
	core.Logf("已导入 %d 个包、%d 个文件到 %s", len(done), files, req.Project)
	if !req.Quiet {
		core.Notify(core.Notice{Kind: "import", What: req.Key})
	}
	// Unity picks the new files up when it is open
	if ProjectRunning(req.Project) {
		go refreshProject(req.Project)
	}
}

// finishSplit: the collection is unpacked; its levels are marked so that the scan makes a card for each
// product.
func finishSplit(st *core.Store, dirs []string) {
	n := 0
	for _, d := range uniq(dirs) {
		if info := library.MarkBundles(st, d, nil); info.Products > n {
			n = info.Products
		}
	}
	_ = st.Save()
	library.StartPipeline(st, true, true, false, false, nil)
	if n < 2 {
		endImp("failed", "", "未发现多个素材，未拆分")
		return
	}
	setImp(func(j *ImportJob) { j.Bundle = n })
	endImp("done", "已拆分为 "+itoa(n)+" 个素材", "")
	core.Logf("合集已拆分为 %d 个素材", n)
}

func refreshProject(p string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = BridgeCall(ctx, p, "refresh", map[string]any{}, 25*time.Second)
}

// packagesIn: the unitypackages lying in a folder.
func packagesIn(dir string) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.EqualFold(filepath.Ext(p), ".unitypackage") {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func uniq(in []string) []string {
	var out []string
	for _, s := range in {
		if !core.ContainsStr(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// ChooseImport: the packages the player picked while the job waits at stage "choose"; false when no job
// waits, or a path is not one it offered.
func ChooseImport(paths []string) bool {
	impMu.Lock()
	defer impMu.Unlock()
	if impJob == nil || impJob.Stage != "choose" || len(paths) == 0 {
		return false
	}
	for _, p := range paths {
		ok := false
		for _, c := range impJob.Choices {
			ok = ok || c.Path == p
		}
		if !ok {
			return false
		}
	}
	select {
	case impPick <- paths:
		return true
	default:
		return false
	}
}

var impPick = make(chan []string, 1)

// ImportAndWait runs an import and waits for it, telling on about every change on the way. It is what
// carrying an asset's update into a project uses (internal/follow).
func ImportAndWait(ctx context.Context, st *core.Store, req ImportReq, on func(ImportJob)) (ImportJob, error) {
	if err := StartImport(st, req); err != nil {
		return ImportJob{}, err
	}
	last := ""
	for {
		j := ImportSnapshot()
		if j == nil {
			return ImportJob{}, errors.New("导入已被取消")
		}
		if s := j.Stage + "|" + j.Msg; s != last {
			last = s
			if on != nil {
				on(*j)
			}
		}
		switch j.Stage {
		case "done", "failed", "cancelled":
			return *j, nil
		}
		select {
		case <-ctx.Done():
			return *j, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// ErrImportBusy: another import is already running.
var ErrImportBusy = errors.New("另一个素材正在导入，请稍后再试")
