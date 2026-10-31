package ai

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"vrclib/internal/core"
	"vrclib/internal/naming"
	"vrclib/internal/unity"
)

// Recipes from what the pipeline did, and recipes put on an avatar.
//
// The plugin has no command that reads a menu back out of the scene, so a recipe is made of what this
// program itself did in the session: each step that put something in the scene is written down as it is
// made (by a run of the pipeline, by the conversation, by a recipe), and 「撤销上一步」 takes the last one off
// again. What the player did by hand in Unity, and what UnitySkills did, is not in it.

// doneStep: one step of the session that a recipe keeps.
type doneStep struct {
	Cmd   string      // base (the avatar it was done on), avatar (placed), dress, place, skin, menu, skill
	Asset RecipeAsset // base, avatar, dress, place, skin
	Items []menuItem  // menu
}

func noteDone(project string, d doneStep) {
	sess.mu.Lock()
	ss := sessOf(project)
	ss.done = append(ss.done, d)
	sess.mu.Unlock()
}

// noteBase: the avatar the steps that follow are done on.
func noteBase(project, name string) {
	sess.mu.Lock()
	ss := sessOf(project)
	for i := len(ss.done) - 1; i >= 0; i-- {
		if ss.done[i].Cmd == "base" {
			if ss.done[i].Asset.Name == name {
				sess.mu.Unlock()
				return
			}
			break
		}
	}
	ss.done = append(ss.done, doneStep{Cmd: "base", Asset: RecipeAsset{Name: name}})
	sess.mu.Unlock()
}

// popDone: the last step that changed the scene was taken back.
func popDone(project string) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	ss := sessOf(project)
	for i := len(ss.done) - 1; i >= 0; i-- {
		switch ss.done[i].Cmd {
		case "base", "skin": // not a step of the undo history
			continue
		}
		ss.done = append(ss.done[:i], ss.done[i+1:]...)
		return
	}
}

func doneOf(project string) (done []doneStep, busy bool) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	ss := sessOf(project)
	return append([]doneStep{}, ss.done...), ss.busy
}

func savable(done []doneStep) bool {
	for _, d := range done {
		switch d.Cmd {
		case "dress", "place", "menu":
			return true
		}
	}
	return false
}

// RecipeReady: the session of a project has put something on the avatar or built a menu, and is not running:
// it can be kept as a recipe.
func RecipeReady(project string) bool {
	if project == "" || !core.IsUnityProject(project) {
		return false
	}
	done, busy := doneOf(project)
	return !busy && savable(done)
}

// SaveRecipe writes down what the pipeline did in a project.
func SaveRecipe(st *core.Store, project, name, note string) (*Recipe, error) {
	done, busy := doneOf(project)
	if busy {
		return nil, errors.New("流水线正在运行，请等它完成后再保存")
	}
	if !savable(done) {
		return nil, errors.New("本次还没有装配素材或生成菜单，没有可保存的内容")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("请填写方案名称")
	}
	r := &Recipe{Format: recipeFormat, Schema: recipeSchema, ID: newRecipeID(), Name: clipRunes(name, 80), Note: clipRunes(strings.TrimSpace(note), 2000),
		Made: time.Now().Unix(), Project: project}

	// the library cards the prefabs are of
	lib, _ := ProjectAssetsOf(st, project, false)
	cardOf := func(prefab string) (ProjAsset, bool) {
		var best ProjAsset
		for _, a := range lib {
			if a.Folder != "" && strings.HasPrefix(prefab, a.Folder+"/") && len(a.Folder) > len(best.Folder) {
				best = a
			}
		}
		return best, best.Folder != ""
	}
	booth := func(key string) string {
		if key == "" {
			return ""
		}
		st.Mu.RLock()
		defer st.Mu.RUnlock()
		for _, a := range st.Assets {
			if a.Key == key {
				return a.BoothID
			}
		}
		return ""
	}
	fill := func(a RecipeAsset) RecipeAsset {
		if c, ok := cardOf(a.Prefab); ok {
			if a.Kind == "" {
				a.Kind = c.Kind
			}
			if a.Key == "" {
				a.Key = c.Key
			}
			if a.Name == "" || a.Name == baseName(a.Prefab) {
				a.Name = firstOf(c.Name, a.Name)
			}
		}
		if a.Booth == "" {
			a.Booth = booth(a.Key)
		}
		return a
	}

	menu := map[string]int{} // a control, by where it is and what it is called: the last build of it counts
	var items []RecipeMenuItem
	skills := 0
	for _, d := range done {
		switch d.Cmd {
		case "base":
			if r.Base.Name == "" {
				r.Base.Name = d.Asset.Name
			}
		case "avatar":
			a := fill(d.Asset)
			r.Base = RecipeBase{Name: firstOf(d.Asset.Object, a.Name), Key: a.Key, Booth: a.Booth, Prefab: a.Prefab}
		case "dress", "place", "skin":
			a := fill(d.Asset)
			replaced := false
			for i := range r.Assets {
				if r.Assets[i].Prefab == a.Prefab && a.Prefab != "" {
					r.Assets[i], replaced = a, true
				}
			}
			if !replaced {
				r.Assets = append(r.Assets, a)
			}
		case "menu":
			for _, it := range d.Items {
				ri := RecipeMenuItem{Kind: it.Kind, Path: it.Path, Label: it.Label, Objects: it.Objects, Default: it.Default, Parameter: it.Parameter, MaterialsFrom: it.MaterialsFrom}
				k := strings.Join(it.Path, "\x00") + "\x01" + it.Label + "\x01" + it.Kind
				if i, ok := menu[k]; ok {
					items[i] = ri
					continue
				}
				menu[k] = len(items)
				items = append(items, ri)
			}
		case "skill":
			skills++
		}
	}
	// the base body's card, when the avatar is one the library knows by its name
	if r.Base.Key == "" && r.Base.Name != "" {
		for _, a := range lib {
			if a.Kind == "素体" && naming.NormKey(a.Name) == naming.NormKey(r.Base.Name) {
				r.Base.Key, r.Base.Booth = a.Key, booth(a.Key)
			}
		}
	}
	// pages, in the order they were first built
	page := map[string]int{}
	for _, it := range items {
		top := ""
		if len(it.Path) > 0 {
			top = it.Path[0]
		}
		i, ok := page[top]
		if !ok {
			i = len(r.Menus)
			page[top] = i
			r.Menus = append(r.Menus, RecipeMenu{Label: top})
		}
		r.Menus[i].Items = append(r.Menus[i].Items, it)
	}
	if skills > 0 {
		extra := fmt.Sprintf("（另有 %d 步通过 UnitySkills 完成的操作，方案中不包含）", skills)
		r.Note = strings.TrimSpace(r.Note + "\n" + extra)
	}
	if err := writeRecipe(r); err != nil {
		return nil, err
	}
	core.Logf("已保存装配方案「%s」：%d 项素材，%d 个菜单项", r.Name, len(r.Assets), len(items))
	return r, nil
}

func clipRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// StartRecipeRun applies a recipe to a project in the background, as a run of the pipeline: its log is the
// pipeline's, 「停止」 and 「撤销上一步」 are the pipeline's.
func StartRecipeRun(st *core.Store, project, id string) error {
	r, err := LoadRecipe(id)
	if err != nil {
		return err
	}
	if !core.IsUnityProject(project) {
		return errors.New("该文件夹不是 Unity 工程")
	}
	if len(r.Assets) == 0 && len(r.Menus) == 0 {
		return errors.New("该方案中没有素材和菜单")
	}
	if err := unity.CheckupReady(project); err != nil {
		return err
	}
	return startBusy(project, "方案「"+r.Name+"」", func(ctx context.Context) error { return runRecipe(ctx, st, project, r) })
}

// findPrefab: where a recipe's prefab is in this project — at its own path, or a prefab of the same file name
// elsewhere in Assets (the asset was imported into another folder).
func findPrefab(project, p string) (string, bool) {
	if p == "" || okAssetPath(p) != nil {
		return "", false
	}
	if core.FileExists(filepath.Join(project, filepath.FromSlash(p))) {
		return p, true
	}
	want := strings.ToLower(path.Base(p))
	found := ""
	root := filepath.Join(project, "Assets")
	n := 0
	_ = filepath.WalkDir(root, func(fp string, d os.DirEntry, err error) error {
		if err != nil || found != "" {
			return filepath.SkipDir
		}
		if n++; n > 200000 {
			return filepath.SkipAll
		}
		if !d.IsDir() && strings.ToLower(d.Name()) == want {
			if rel, err := filepath.Rel(project, fp); err == nil {
				found = filepath.ToSlash(rel)
			}
		}
		return nil
	})
	return found, found != ""
}

// renamed: a menu item's objects, as they are called in this scene.
func renamed(objs []string, names map[string]string) (out []string, all bool) {
	all = true
	for _, o := range objs {
		top, rest := o, ""
		if i := strings.Index(o, "/"); i >= 0 {
			top, rest = o[:i], o[i:]
		}
		n, ok := names[top]
		if !ok {
			all = false
			continue
		}
		out = append(out, n+rest)
	}
	return out, all
}

func runRecipe(ctx context.Context, st *core.Store, p string, r *Recipe) error {
	addStep(p, Step{Kind: "info", Text: fmt.Sprintf("开始应用方案「%s」：%d 项素材", r.Name, len(r.Assets))})
	st.Mu.RLock()
	defs := naming.ParseBases(st.Settings.Bases)
	st.Mu.RUnlock()

	o, err := inspect(ctx, p)
	if err != nil {
		return err
	}
	if len(o.Avatars) == 0 {
		pf, ok := findPrefab(p, r.Base.Prefab)
		if !ok {
			return errors.New("场景中没有模型：请先在 Unity 中放入模型「" + firstOf(r.Base.Name, "素体") + "」")
		}
		raw, err := tool(ctx, p, "放入素体 "+baseName(pf), "place_avatar", map[string]any{"prefab": pf, "name": baseName(pf)}, 2*time.Minute, nil)
		if err != nil {
			return err
		}
		markChanged(p)
		noteDone(p, doneStep{Cmd: "avatar", Asset: RecipeAsset{Name: r.Base.Name, Prefab: pf, Kind: "素体", Object: nameOf(raw, baseName(pf))}})
		if o, err = inspect(ctx, p); err != nil {
			return err
		}
		if len(o.Avatars) == 0 {
			return errors.New("放入素体后场景中仍没有模型")
		}
	}
	avatar := firstOf(o.Avatars[0].Path, o.Avatars[0].Name)
	avName := o.Avatars[0].Name
	noteBase(p, avName)
	other := r.Base.Name != "" && naming.NormKey(r.Base.Name) != naming.NormKey(avName)
	if other {
		addStep(p, Step{Kind: "info", Text: "方案是在「" + r.Base.Name + "」上做的，现在应用到「" + avName + "」：素材有对应该素体的版本时会改用那个版本"})
	}

	names := map[string]string{} // the recipe's object → this scene's
	prefabs := map[string]string{}
	skipped := 0
	for _, a := range r.Assets {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		label := firstOf(a.Name, baseName(a.Prefab))
		pf, ok := findPrefab(p, a.Prefab)
		if !ok {
			addStep(p, Step{Kind: "error", Text: "「" + label + "」不在该工程中，已跳过：请先在素材库中将它导入此工程"})
			skipped++
			continue
		}
		// on another base body: that body's version of it, when the asset has one
		if other && a.Kind != "皮肤" {
			if ps, err := prefabsIn(ctx, p, path.Dir(pf)); err == nil {
				if alt, note, ok := pickPrefab(ps, a.Kind, avName, defs); ok && alt.Path != pf && note != "" {
					addStep(p, Step{Kind: "info", Text: "「" + label + "」：" + note})
					pf = alt.Path
				}
			}
		}
		prefabs[a.Prefab] = pf
		switch a.Kind {
		case "皮肤":
			noteDone(p, doneStep{Cmd: "skin", Asset: RecipeAsset{Name: a.Name, Prefab: pf, Kind: a.Kind, Key: a.Key, Booth: a.Booth}})
			continue
		case "衣服", "头发", "配饰":
			raw, err := tool(ctx, p, "装配 "+label, "dress", map[string]any{"avatar": avatar, "prefab": pf}, 2*time.Minute, outNote)
			if err != nil {
				skipped++
				continue
			}
			markChanged(p)
			obj := nameOf(raw, baseName(pf))
			names[firstOf(a.Object, obj)] = obj
			noteDone(p, doneStep{Cmd: "dress", Asset: RecipeAsset{Name: a.Name, Prefab: pf, Kind: a.Kind, Key: a.Key, Booth: a.Booth, Object: obj, Parts: meshesOf(raw), Colors: a.Colors}})
		default:
			raw, err := tool(ctx, p, "放置 "+label, "place", map[string]any{"avatar": avatar, "prefab": pf}, 2*time.Minute, outNote)
			if err != nil {
				skipped++
				continue
			}
			markChanged(p)
			obj := objectOf(raw, baseName(pf))
			names[firstOf(a.Object, obj)] = obj
			noteDone(p, doneStep{Cmd: "place", Asset: RecipeAsset{Name: a.Name, Prefab: pf, Kind: a.Kind, Key: a.Key, Booth: a.Booth, Object: obj}})
		}
	}

	// the menu, with the objects as this scene calls them; what is not here is left out
	var items []menuItem
	dropped := 0
	for _, m := range r.Menus {
		for _, it := range m.Items {
			objs, all := renamed(it.Objects, names)
			if len(it.Objects) > 0 && !all {
				dropped++
				continue
			}
			mf := it.MaterialsFrom
			if mf != "" {
				if q, ok := prefabs[mf]; ok {
					mf = q
				} else if q, ok := findPrefab(p, mf); ok {
					mf = q
				} else {
					dropped++
					continue
				}
			}
			items = append(items, menuItem{Kind: it.Kind, Path: it.Path, Label: it.Label, Objects: objs, Default: it.Default, Parameter: it.Parameter, MaterialsFrom: mf})
		}
	}
	if dropped > 0 {
		addStep(p, Step{Kind: "info", Text: fmt.Sprintf("有 %d 个菜单项对应的素材没有装上，已从菜单中略去", dropped)})
	}
	if len(items) > 0 {
		if _, err := tool(ctx, p, fmt.Sprintf("生成菜单（%d 项）", len(items)), "build_menu", map[string]any{"avatar": avatar, "items": items, "icons": true}, 5*time.Minute, outNote); err != nil {
			return err
		}
		markChanged(p)
		noteDone(p, doneStep{Cmd: "menu", Items: items})
	}
	if raw, err := tool(ctx, p, "截图", "snapshot", map[string]any{"avatar": avatar, "views": []string{"front", "back"}}, 2*time.Minute, nil); err == nil {
		if imgs := keepShots(raw); len(imgs) > 0 {
			addStep(p, Step{Kind: "info", Text: "应用结果", Imgs: imgs})
		}
	}
	i := addStep(p, Step{Kind: "tool", Text: "上传前体检", Busy: true})
	if rec, err := unity.RunCheckup(ctx, p, avatar); err != nil {
		setStep(p, i, func(s *Step) { s.Busy, s.Out = false, core.TrimErr(err) })
	} else {
		b := rec.Brief()
		out := "PC " + firstOf(b.PC, "—") + "，Quest " + firstOf(b.Quest, "—")
		if b.Fail > 0 || b.Warn > 0 {
			out += fmt.Sprintf("，%d 项不通过、%d 项提醒，详见「验收」", b.Fail, b.Warn)
		}
		setStep(p, i, func(s *Step) { s.Busy, s.OK, s.Out = false, b.Fail == 0, out })
	}
	done := "方案已应用"
	if skipped > 0 {
		done += fmt.Sprintf("（%d 项素材未装上，见上方记录）", skipped)
	}
	addStep(p, Step{Kind: "say", Text: done + "。场景没有保存：确认无误后请在 Unity 中按 Ctrl+S 保存；每一步都可以按 Ctrl+Z 撤销。"})
	return nil
}
