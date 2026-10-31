package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"vrclib/internal/core"
	"vrclib/internal/naming"
	"vrclib/internal/unity"
)

// The pipeline without an AI: what can be decided by rule is done by rule, through the plugin in the
// editor (unityhelper/com.miovrc.pipeline) —
//
//	place_avatar  the base body, when the scene has no avatar yet
//	dress         an outfit, hair or an accessory: Modular Avatar's Setup Outfit
//	place         a prop or a plugin, under the avatar as it is
//	build_menu    the menu: outfits share one choice, hair another, accessories and props are switches,
//	              skins pick the body's look, and 「一键脱光」 closes the outfits
//	snapshot      pictures of the result
//	checkup       the upload check
//
// Every step is shown in the log as it goes. The scene is not saved: each of the plugin's steps is one
// Ctrl+Z, and 「撤销上一步」 takes back the last of them.

// Step: one line of the log the window shows.
type Step struct {
	Kind string   `json:"kind"` // tool, say, info, error, ask
	Text string   `json:"text"`
	Out  string   `json:"out,omitempty"`
	OK   bool     `json:"ok,omitempty"`
	Busy bool     `json:"busy,omitempty"`
	At   int64    `json:"at,omitempty"`
	Imgs []string `json:"imgs,omitempty"`
	Opts string   `json:"opts,omitempty"` // an ask: "once" (no 「本次对话始终允许」), "budget"
}

type session struct {
	steps   []Step
	busy    bool
	cancel  context.CancelFunc
	changed bool       // the last run changed the scene: 「撤销上一步」 has something to take back
	done    []doneStep // what the session put in the scene, for a recipe (recipe_run.go)
}

var sess struct {
	mu sync.Mutex
	m  map[string]*session
}

func sessOf(project string) *session {
	k := core.PathKey(project)
	if sess.m == nil {
		sess.m = map[string]*session{}
	}
	s := sess.m[k]
	if s == nil {
		s = &session{}
		sess.m[k] = s
	}
	return s
}

// addStep appends a line and answers its place, to change it later.
func addStep(project string, s Step) int {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	ss := sessOf(project)
	if s.At == 0 {
		s.At = time.Now().Unix()
	}
	ss.steps = append(ss.steps, s)
	if len(ss.steps) > 400 {
		ss.steps = ss.steps[len(ss.steps)-400:]
	}
	core.BumpRev()
	return len(ss.steps) - 1
}

func setStep(project string, i int, f func(*Step)) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	ss := sessOf(project)
	if i >= 0 && i < len(ss.steps) {
		f(&ss.steps[i])
	}
	core.BumpRev()
}

// sessionView: what /api/ai/session answers.
func sessionView(project string) map[string]any {
	sess.mu.Lock()
	ss := sessOf(project)
	steps := append([]Step{}, ss.steps...)
	busy, changed := ss.busy, ss.changed
	sess.mu.Unlock()
	changes := sessionChanges(project)
	if changed && changes == 0 {
		changes = 1
	}
	return map[string]any{"ok": true, "busy": busy, "steps": steps, "changes": changes, "kit": unity.AIKitStatus(project), "usage": sessionUsage(project)}
}

// ---------- the assets of a project ----------

// ProjAsset: one asset of a Unity project, as the pipeline's list shows it.
type ProjAsset struct {
	Folder  string   `json:"folder"` // Assets/<shop>/<item>
	Name    string   `json:"name"`
	Kind    string   `json:"kind"`             // 素体, 衣服, 头发, 配饰, 道具, 皮肤, 插件, 面捕, 表情, 光影, 互动, 其他
	Key     string   `json:"key,omitempty"`    // the library card it is, when it is one
	Source  string   `json:"source,omitempty"` // the same, as the window reads it: "" = not from the library
	Bases   []string `json:"bases,omitempty"`
	Prefabs int      `json:"prefabs"`
	Worn    bool     `json:"worn,omitempty"` // it is on the avatar already
	Cover   string   `json:"cover,omitempty"`
}

// kindOf: the pipeline's kind for a library category.
func kindOf(cat string) string {
	switch cat {
	case "素体", "衣服", "头发", "配饰", "道具", "面捕", "插件":
		return cat
	case "材质":
		return "皮肤"
	case "动作":
		return "表情"
	}
	return "其他"
}

// ProjectAssetsOf: the assets of a project — the library's cards it uses, and the folders of prefabs it holds
// that are no card (fresh: ask the editor again). ok is false when the folder is not a Unity project.
func ProjectAssetsOf(st *core.Store, project string, fresh bool) (list []ProjAsset, ok bool) {
	if !core.IsUnityProject(project) {
		return nil, false
	}
	st.Mu.RLock()
	name := st.UsageName(project)
	seen := map[string]bool{}
	for _, a := range st.Assets {
		for _, u := range a.Usage {
			if u.Project != name || u.Folder == "" || seen[strings.ToLower(u.Folder)] {
				continue
			}
			seen[strings.ToLower(u.Folder)] = true
			cat := a.Category
			if us := st.User[a.Key]; us != nil && us.Category != "" {
				cat = us.Category
			}
			list = append(list, ProjAsset{Folder: u.Folder, Name: a.Name, Kind: kindOf(cat), Key: a.Key, Source: a.Key, Bases: a.Bases})
		}
	}
	st.Mu.RUnlock()
	// folders of prefabs the library knows nothing of (an asset brought in by hand)
	for _, f := range prefabFolders(project) {
		covered := false
		for _, a := range list {
			if l := strings.ToLower(a.Folder); strings.HasPrefix(strings.ToLower(f)+"/", l+"/") || strings.HasPrefix(l+"/", strings.ToLower(f)+"/") {
				covered = true
			}
		}
		if !covered {
			n := path.Base(f)
			list = append(list, ProjAsset{Folder: f, Name: n, Kind: kindOf(naming.Classify(n))})
		}
	}
	for i := range list {
		list[i].Prefabs = countPrefabs(filepath.Join(project, filepath.FromSlash(list[i].Folder)))
	}
	// what is on the avatar: asked of the editor when it is open
	if unity.AIKitStatus(project).Alive {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		worn := wornNames(ctx, project)
		cancel()
		for i := range list {
			for _, w := range worn {
				if strings.EqualFold(w, path.Base(list[i].Folder)) || strings.EqualFold(w, list[i].Name) {
					list[i].Worn = true
				}
			}
		}
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].Folder < list[j].Folder })
	return list, true
}

// prefabFolders: the folders of the project's Assets (two or three levels down: Assets/<shop>/<item>) that
// hold prefabs.
func prefabFolders(project string) []string {
	var out []string
	assets := filepath.Join(project, "Assets")
	_ = filepath.WalkDir(assets, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(project, p)
		rel = filepath.ToSlash(rel)
		depth := strings.Count(rel, "/")
		if d.IsDir() {
			if n := d.Name(); strings.HasPrefix(n, ".") || strings.HasSuffix(n, "~") || depth > 3 {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.EqualFold(filepath.Ext(p), ".prefab") && depth >= 2 {
			parts := strings.Split(rel, "/")
			f := strings.Join(parts[:min(3, len(parts)-1)], "/")
			if !core.ContainsStr(out, f) {
				out = append(out, f)
			}
		}
		return nil
	})
	return out
}

func countPrefabs(dir string) int {
	n := 0
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.EqualFold(filepath.Ext(p), ".prefab") {
			n++
		}
		return nil
	})
	return n
}

// ---------- asking the editor ----------

type overview struct {
	Avatars []struct {
		Name     string `json:"name"`
		Path     string `json:"path"`
		Children []struct {
			Object string `json:"object"`
			Path   string `json:"path"`
		} `json:"children"`
	} `json:"avatars"`
}

func inspect(ctx context.Context, project string) (*overview, error) {
	raw, err := unity.BridgeCall(ctx, project, "inspect", map[string]any{}, 60*time.Second)
	if err != nil {
		return nil, err
	}
	var o overview
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, errors.New("无法读取场景")
	}
	return &o, nil
}

func wornNames(ctx context.Context, project string) []string {
	o, err := inspect(ctx, project)
	if err != nil || len(o.Avatars) == 0 {
		return nil
	}
	var out []string
	for _, c := range o.Avatars[0].Children {
		out = append(out, c.Object)
	}
	return out
}

type prefabInfo struct {
	Path      string   `json:"path"`
	Name      string   `json:"name"`
	Renderers int      `json:"renderers"`
	Whole     bool     `json:"wholeAvatar"`
	Meshes    []string `json:"meshNames"`
}

func prefabsIn(ctx context.Context, project, folder string) ([]prefabInfo, error) {
	raw, err := unity.BridgeCall(ctx, project, "prefabs", map[string]any{"folders": []string{folder}, "limit": 60}, 60*time.Second)
	if err != nil {
		return nil, err
	}
	var r struct {
		Prefabs []prefabInfo `json:"prefabs"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, errors.New("无法读取 prefab 列表")
	}
	return r.Prefabs, nil
}

// pickPrefab: of an asset's prefabs, the one to put on this avatar — the base body's whole model for a 素体,
// else not a whole model; the version for this avatar's base body when the names say which is which; the one
// with the most renderers (the whole outfit, not a part of it).
func pickPrefab(ps []prefabInfo, kind, avatar string, defs []naming.BaseDef) (prefabInfo, string, bool) {
	var cand []prefabInfo
	for _, p := range ps {
		if (kind == "素体") == p.Whole {
			cand = append(cand, p)
		}
	}
	if len(cand) == 0 {
		return prefabInfo{}, "", false
	}
	note := ""
	if kind != "素体" && avatar != "" {
		want := naming.DetectBases(avatar, defs)
		if len(want) == 0 {
			want = []string{avatar}
		}
		var mine, any []prefabInfo
		for _, p := range cand {
			bs := naming.DetectBases(p.Name+" "+p.Path, defs)
			if len(bs) > 0 {
				any = append(any, p)
			}
			for _, w := range want {
				if core.ContainsStr(bs, w) || strings.Contains(strings.ToLower(p.Name+" "+p.Path), strings.ToLower(w)) {
					mine = append(mine, p)
					break
				}
			}
		}
		switch {
		case len(mine) > 0:
			cand = mine
		case len(any) > 1:
			note = "未找到适配「" + avatar + "」的版本，已选用其中一个；如素体不在识别规则中，可在设置中添加"
		}
	}
	sort.SliceStable(cand, func(i, j int) bool {
		if cand[i].Renderers != cand[j].Renderers {
			return cand[i].Renderers > cand[j].Renderers
		}
		return cand[i].Path < cand[j].Path
	})
	return cand[0], note, true
}

// ---------- a run ----------

// RunAsset: one asset the window asks to be put on the avatar.
type RunAsset struct {
	Folder string `json:"folder"`
	Kind   string `json:"kind"`
	Name   string `json:"name"`
}

// RunReq: what /api/ai/run is asked.
type RunReq struct {
	Project   string     `json:"project"`
	Mode      string     `json:"mode"` // dress, chat
	Assets    []RunAsset `json:"assets"`
	Hierarchy string     `json:"hierarchy"`
	NoAI      bool       `json:"noAI"`
	Text      string     `json:"text"`
}

// StartRun begins a run of the pipeline in the background.
func StartRun(st *core.Store, req RunReq) error {
	p := req.Project
	if !core.IsUnityProject(p) {
		return errors.New("该文件夹不是 Unity 工程")
	}
	switch req.Mode {
	case "dress":
		if len(req.Assets) == 0 {
			return errors.New("请先勾选要装配的素材")
		}
	case "chat":
		if strings.TrimSpace(req.Text) == "" {
			return errors.New("请输入要做的事")
		}
	default:
		return errors.New("无法识别的操作")
	}
	if err := unity.CheckupReady(p); err != nil {
		return err
	}
	what := map[bool]string{true: "不使用 AI", false: "AI"}[req.NoAI]
	return startBusy(p, what, func(ctx context.Context) error {
		if req.Mode == "chat" {
			return runChat(ctx, st, p, strings.TrimSpace(req.Text))
		}
		return runDress(ctx, st, req)
	})
}

// startBusy runs one thing of a project's pipeline in the background: one at a time, stopped by 「停止」, its
// end in the log.
func startBusy(p, what string, run func(ctx context.Context) error) error {
	sess.mu.Lock()
	ss := sessOf(p)
	if ss.busy {
		sess.mu.Unlock()
		return errors.New("流水线正在运行")
	}
	ctx, cancel := context.WithCancel(context.Background())
	ss.busy, ss.cancel = true, cancel
	sess.mu.Unlock()
	core.BumpRev()
	go func() {
		defer func() {
			sess.mu.Lock()
			ss.busy, ss.cancel = false, nil
			sess.mu.Unlock()
			cancel()
			core.BumpRev()
		}()
		t0 := time.Now()
		err := run(ctx)
		switch {
		case ctx.Err() != nil:
			addStep(p, Step{Kind: "info", Text: "已停止"})
		case err != nil:
			addStep(p, Step{Kind: "error", Text: core.TrimErr(err)})
		}
		core.Logf("流水线（%s）：%s，用时 %.0f 秒", what, map[bool]string{true: "完成", false: "未完成"}[err == nil], time.Since(t0).Seconds())
		if time.Since(t0) > 15*time.Second && ctx.Err() == nil {
			core.Notify(core.Notice{Kind: "pipeline", What: p})
		}
	}()
	return nil
}

// tool runs one command of the plugin as a line of the log.
func tool(ctx context.Context, project, text, cmd string, args map[string]any, wait time.Duration, out func(json.RawMessage) string) (json.RawMessage, error) {
	i := addStep(project, Step{Kind: "tool", Text: text, Busy: true})
	raw, err := unity.BridgeCall(ctx, project, cmd, args, wait)
	if err != nil {
		setStep(project, i, func(s *Step) { s.Busy, s.OK, s.Out = false, false, core.TrimErr(err) })
		return nil, err
	}
	o := ""
	if out != nil {
		o = out(raw)
	}
	setStep(project, i, func(s *Step) { s.Busy, s.OK, s.Out = false, true, o })
	return raw, nil
}

// menuItem: one entry of the plan build_menu takes.
type menuItem struct {
	Kind          string   `json:"kind"`
	Path          []string `json:"path"`
	Label         string   `json:"label"`
	Objects       []string `json:"objects,omitempty"`
	Default       bool     `json:"default,omitempty"`
	Parameter     string   `json:"parameter,omitempty"`
	MaterialsFrom string   `json:"materialsFrom,omitempty"`
}

func runDress(ctx context.Context, st *core.Store, req RunReq) error {
	p := req.Project
	var llm *Client
	if !req.NoAI {
		c, err := clientFor(st)
		if err != nil {
			return err
		}
		llm = c
		usageProject.Store(c, p)
		defer usageProject.Delete(c)
		before := convTokens(p)
		defer func() { noteRun(convTokens(p) - before) }()
		addStep(p, Step{Kind: "info", Text: fmt.Sprintf("开始装配 %d 项素材（AI：%s）", len(req.Assets), c.Model)})
	} else {
		addStep(p, Step{Kind: "info", Text: fmt.Sprintf("开始装配 %d 项素材（不使用 AI）", len(req.Assets))})
		if strings.TrimSpace(req.Hierarchy) != "" {
			addStep(p, Step{Kind: "info", Text: "自定义菜单层级需要 AI 整理，本次未采用"})
		}
	}
	var dressed []outfitInfo
	st.Mu.RLock()
	defs := naming.ParseBases(st.Settings.Bases)
	st.Mu.RUnlock()

	// the avatar: the one in the scene, or the base body that was picked
	o, err := inspect(ctx, p)
	if err != nil {
		return err
	}
	var rest []RunAsset
	var base *RunAsset
	for i, a := range req.Assets {
		if a.Kind == "素体" && base == nil {
			base = &req.Assets[i]
			continue
		}
		rest = append(rest, a)
	}
	if len(o.Avatars) == 0 {
		if base == nil {
			return errors.New("场景中没有模型：请在「素材」中勾选一个素体，或先在 Unity 中放入模型")
		}
		ps, err := prefabsIn(ctx, p, base.Folder)
		if err != nil {
			return err
		}
		pf, _, ok := pickPrefab(ps, "素体", "", defs)
		if !ok {
			return errors.New("「" + base.Name + "」中没有完整的模型 prefab（带 VRC Avatar Descriptor）")
		}
		raw, err := tool(ctx, p, "放入素体 "+pf.Name, "place_avatar", map[string]any{"prefab": pf.Path, "name": pf.Name}, 2*time.Minute, nil)
		if err != nil {
			return err
		}
		markChanged(p)
		noteDone(p, doneStep{Cmd: "avatar", Asset: RecipeAsset{Name: base.Name, Prefab: pf.Path, Kind: "素体", Object: nameOf(raw, pf.Name)}})
		if o, err = inspect(ctx, p); err != nil {
			return err
		}
		if len(o.Avatars) == 0 {
			return errors.New("放入素体后场景中仍没有模型")
		}
	} else if base != nil {
		addStep(p, Step{Kind: "info", Text: "场景中已有模型「" + o.Avatars[0].Name + "」，未再放入素体"})
	}
	avatar := o.Avatars[0].Path
	if avatar == "" {
		avatar = o.Avatars[0].Name
	}
	avName := o.Avatars[0].Name
	noteBase(p, avName)

	// what goes on it
	var items []menuItem
	outfits, hairs := 0, 0
	type skin struct{ label, prefab string }
	var skins []skin
	for _, a := range rest {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		ps, err := prefabsIn(ctx, p, a.Folder)
		if err != nil {
			return err
		}
		pf, note, ok := pickPrefab(ps, a.Kind, avName, defs)
		if !ok {
			addStep(p, Step{Kind: "error", Text: "「" + a.Name + "」中没有可用的 prefab，已跳过"})
			continue
		}
		if note != "" {
			addStep(p, Step{Kind: "info", Text: "「" + a.Name + "」：" + note})
		}
		label := strings.TrimSpace(a.Name)
		if label == "" {
			label = pf.Name
		}
		switch a.Kind {
		case "皮肤":
			skins = append(skins, skin{label, pf.Path})
			noteDone(p, doneStep{Cmd: "skin", Asset: RecipeAsset{Name: label, Prefab: pf.Path, Kind: a.Kind}})
			continue
		case "衣服", "头发", "配饰":
			raw, err := tool(ctx, p, "装配 "+label, "dress", map[string]any{"avatar": avatar, "prefab": pf.Path}, 2*time.Minute, outNote)
			if err != nil {
				continue // the next one may still go on; the log says why this one did not
			}
			markChanged(p)
			obj := nameOf(raw, pf.Name)
			cols := colours(ps, pf, defs)
			noteDone(p, doneStep{Cmd: "dress", Asset: RecipeAsset{Name: label, Prefab: pf.Path, Kind: a.Kind, Object: obj, Parts: meshesOf(raw), Colors: cols}})
			if a.Kind == "衣服" || a.Kind == "头发" {
				o := outfitInfo{Kind: a.Kind, Category: []string{a.Kind}, Name: label, Object: obj, Meshes: meshesOf(raw), Colours: cols}
				outfitDraft(&o, (a.Kind == "衣服" && outfits0(dressed, "衣服") == 0) || (a.Kind == "头发" && outfits0(dressed, "头发") == 0))
				dressed = append(dressed, o)
			}
			switch a.Kind {
			case "衣服":
				items = append(items, menuItem{Kind: "outfit", Path: []string{"衣服"}, Label: label, Objects: []string{obj}, Default: outfits == 0})
				outfits++
			case "头发":
				items = append(items, menuItem{Kind: "outfit", Path: []string{"头发"}, Label: label, Objects: []string{obj}, Default: hairs == 0, Parameter: "Hair_Choose"})
				hairs++
			default:
				items = append(items, menuItem{Kind: "toggle", Path: []string{"配饰"}, Label: label, Objects: []string{obj}, Default: true})
			}
		default: // props, plugins, face tracking, expressions, lighting, interaction: placed as they are
			raw, err := tool(ctx, p, "放置 "+label, "place", map[string]any{"avatar": avatar, "prefab": pf.Path}, 2*time.Minute, outNote)
			if err != nil {
				continue
			}
			markChanged(p)
			noteDone(p, doneStep{Cmd: "place", Asset: RecipeAsset{Name: label, Prefab: pf.Path, Kind: a.Kind, Object: objectOf(raw, pf.Name)}})
			if a.Kind == "道具" {
				items = append(items, menuItem{Kind: "toggle", Path: []string{"道具"}, Label: label, Objects: []string{objectOf(raw, pf.Name)}})
			}
		}
	}
	if outfits > 0 {
		items = append(items, menuItem{Kind: "strip", Path: []string{"衣服"}, Label: "一键脱光"})
	}
	if len(skins) > 0 {
		items = append(items, menuItem{Kind: "skin", Path: []string{"皮肤"}, Label: "默认", Default: true})
		for _, s := range skins {
			items = append(items, menuItem{Kind: "skin", Path: []string{"皮肤"}, Label: s.label, MaterialsFrom: s.prefab})
		}
	}

	// the AI names and groups the outfits' menus; the rest stays as the rules made it
	if llm != nil && len(dressed) > 0 {
		var keep []menuItem
		for _, it := range items {
			if !(it.Kind == "outfit" && len(it.Path) == 1 && (it.Path[0] == "衣服" || it.Path[0] == "头发")) {
				keep = append(keep, it)
			}
		}
		arranged, note := arrangeMenu(ctx, p, llm, dressed, req.Hierarchy)
		if note != "" {
			addStep(p, Step{Kind: "info", Text: note})
		}
		items = append(arranged, keep...)
	}
	// the menu
	if len(items) > 0 {
		if _, err := tool(ctx, p, fmt.Sprintf("生成菜单（%d 项）", len(items)), "build_menu", map[string]any{"avatar": avatar, "items": items, "icons": true}, 5*time.Minute, outNote); err != nil {
			return err
		}
		markChanged(p)
		noteDone(p, doneStep{Cmd: "menu", Items: items})
	}
	// pictures of it
	if raw, err := tool(ctx, p, "截图", "snapshot", map[string]any{"avatar": avatar, "views": []string{"front", "back"}}, 2*time.Minute, nil); err == nil {
		if imgs := keepShots(raw); len(imgs) > 0 {
			addStep(p, Step{Kind: "info", Text: "装配结果", Imgs: imgs})
		}
	}
	// and the upload check
	i := addStep(p, Step{Kind: "tool", Text: "上传前体检", Busy: true})
	rec, err := unity.RunCheckup(ctx, p, avatar)
	if err != nil {
		setStep(p, i, func(s *Step) { s.Busy, s.Out = false, core.TrimErr(err) })
	} else {
		b := rec.Brief()
		out := "PC " + firstOf(b.PC, "—") + "，Quest " + firstOf(b.Quest, "—")
		if b.Fail > 0 || b.Warn > 0 {
			out += fmt.Sprintf("，%d 项不通过、%d 项提醒，详见「验收」", b.Fail, b.Warn)
		}
		setStep(p, i, func(s *Step) { s.Busy, s.OK, s.Out = false, b.Fail == 0, out })
	}
	if llm != nil {
		sum := "体检：未完成"
		if err == nil {
			b := rec.Brief()
			sum = fmt.Sprintf("体检：PC %s，Quest %s，%d 项不通过、%d 项提醒。", firstOf(b.PC, "—"), firstOf(b.Quest, "—"), b.Fail, b.Warn)
		}
		review(ctx, p, llm, "装配了 "+fmt.Sprint(len(req.Assets))+" 项素材，菜单 "+fmt.Sprint(len(items))+" 项。\n"+sum+"\n（截图在记录里，这次没有附给你，没有看到画面。）")
	}
	addStep(p, Step{Kind: "say", Text: "装配完成。场景没有保存：确认无误后请在 Unity 中按 Ctrl+S 保存；每一步都可以按 Ctrl+Z 撤销。"})
	return nil
}

func outfits0(os []outfitInfo, kind string) int {
	n := 0
	for _, o := range os {
		if o.Kind == kind {
			n++
		}
	}
	return n
}

// meshesOf: the meshes of what dress put on, by their path under the avatar.
func meshesOf(raw json.RawMessage) []string {
	var r struct {
		Outfit struct {
			Meshes []struct {
				Path string `json:"path"`
			} `json:"meshes"`
		} `json:"outfit"`
	}
	_ = json.Unmarshal(raw, &r)
	var out []string
	for _, m := range r.Outfit.Meshes {
		if m.Path != "" {
			out = append(out, m.Path)
		}
	}
	return out
}

// colours: the other prefabs of the folder that are the same outfit in another colour — as many renderers,
// and not another base body's version.
func colours(ps []prefabInfo, picked prefabInfo, defs []naming.BaseDef) []string {
	mine := naming.DetectBases(picked.Name+" "+picked.Path, defs)
	var out []string
	for _, p := range ps {
		if p.Path == picked.Path || p.Whole || p.Renderers != picked.Renderers || path.Dir(p.Path) != path.Dir(picked.Path) {
			continue
		}
		if bs := naming.DetectBases(p.Name+" "+p.Path, defs); len(bs) > 0 && !sameBases(bs, mine) {
			continue
		}
		out = append(out, p.Path)
	}
	return out
}

func sameBases(a, b []string) bool {
	for _, x := range a {
		if core.ContainsStr(b, x) {
			return true
		}
	}
	return false
}

func markChanged(project string) {
	sess.mu.Lock()
	sessOf(project).changed = true
	sess.mu.Unlock()
}

// outNote: what a step of the plugin says that the player is to know (its warnings).
func outNote(raw json.RawMessage) string {
	var r struct {
		Warnings []any `json:"warnings"`
	}
	if json.Unmarshal(raw, &r) != nil || len(r.Warnings) == 0 {
		return ""
	}
	var out []string
	for _, w := range r.Warnings {
		out = append(out, fmt.Sprint(w))
	}
	return strings.Join(out, "；")
}

func nameOf(raw json.RawMessage, fallback string) string {
	var r struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &r) == nil && r.Name != "" {
		return r.Name
	}
	return fallback
}

func objectOf(raw json.RawMessage, fallback string) string {
	var r struct {
		Object string `json:"object"`
		Name   string `json:"name"`
	}
	if json.Unmarshal(raw, &r) == nil {
		if r.Object != "" {
			return r.Object
		}
		if r.Name != "" {
			return r.Name
		}
	}
	return fallback
}

// keepShots copies the pictures the plugin made into the data folder, where /shot serves them, and answers
// their names.
func keepShots(raw json.RawMessage) []string {
	var r struct {
		Shots []struct {
			File string `json:"file"`
		} `json:"shots"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return nil
	}
	_ = os.MkdirAll(ShotsDir(), 0755)
	var out []string
	for i, s := range r.Shots {
		src, err := os.Open(filepath.FromSlash(s.File))
		if err != nil {
			continue
		}
		name := fmt.Sprintf("run-%d-%d%s", time.Now().UnixNano(), i, strings.ToLower(filepath.Ext(s.File)))
		dst, err := os.Create(filepath.Join(ShotsDir(), name))
		if err == nil {
			_, err = io.Copy(dst, src)
			dst.Close()
		}
		src.Close()
		if err == nil {
			out = append(out, name)
		}
	}
	return out
}

// CancelRun stops a run.
func CancelRun(project string) {
	sess.mu.Lock()
	ss := sessOf(project)
	if ss.cancel != nil {
		ss.cancel()
	}
	sess.mu.Unlock()
}

// UndoLast takes back the last step the plugin made in the scene.
func UndoLast(ctx context.Context, project string) (string, error) {
	raw, err := unity.BridgeCall(ctx, project, "undo", map[string]any{}, 30*time.Second)
	if err != nil {
		return "", err
	}
	var r struct {
		Undone string `json:"undone"`
	}
	_ = json.Unmarshal(raw, &r)
	sess.mu.Lock()
	sessOf(project).changed = false
	sess.mu.Unlock()
	if strings.HasPrefix(r.Undone, "MioVRCA") {
		popDone(project)
	}
	steps.mu.Lock()
	delete(steps.m, core.PathKey(project))
	steps.mu.Unlock()
	addStep(project, Step{Kind: "info", Text: "已撤销：" + strings.TrimPrefix(r.Undone, "MioVRCA ")})
	return r.Undone, nil
}

// ResetSession clears the log (a new conversation).
func ResetSession(project string) error {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	ss := sessOf(project)
	if ss.busy {
		return errors.New("流水线正在运行")
	}
	ss.steps, ss.changed, ss.done = nil, false, nil
	dropConv(project)
	core.BumpRev()
	return nil
}

func firstOf(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
