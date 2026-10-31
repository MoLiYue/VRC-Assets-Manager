package ai

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vrclib/internal/testkit"
	"vrclib/internal/unity"
	"vrclib/internal/unity/unitytest"
)

func sceneProject(t *testing.T, answer func(*fakeScene, string, map[string]any) (any, string)) (string, *fakeScene) {
	t.Helper()
	proj := t.TempDir()
	for _, d := range []string{"Assets", "ProjectSettings"} {
		_ = os.MkdirAll(filepath.Join(proj, d), 0755)
	}
	f := &fakeScene{dir: proj}
	unitytest.FakeUnity(t, proj, func(cmd string, args map[string]any) (any, string) { return answer(f, cmd, args) })
	_ = os.WriteFile(filepath.Join(proj, "Packages", unity.PipePkg, "package.json"), []byte(`{}`), 0644)
	t.Cleanup(func() { _ = ResetSession(proj) })
	return proj, f
}

// A run kept as a recipe: the base body it put in, what it put on, the menu as it was built. 「撤销上一步」
// takes the menu out of it again. Applied to another project — the outfit imported into another folder, the
// hair not imported at all, the scene naming things its own way — it puts on what is there and builds the
// menu with this scene's names, leaving out what is not there.
func TestRecipeSaveAndApply(t *testing.T) {
	st := testkit.NewStore(t)
	a, _ := sceneProject(t, func(f *fakeScene, cmd string, args map[string]any) (any, string) {
		if cmd == "undo" {
			return map[string]any{"undone": "MioVRCA 生成菜单"}, ""
		}
		return f.answer(cmd, args)
	})
	if RecipeReady(a) {
		t.Error("ready before anything was done")
	}
	if err := StartRun(st, RunReq{Project: a, Mode: "dress", NoAI: true, Assets: []RunAsset{
		{Folder: "Assets/Manuka", Kind: "素体", Name: "Manuka"},
		{Folder: "Assets/Shop/Moon Dress", Kind: "衣服", Name: "Moon Dress"},
		{Folder: "Assets/Shop/Twin Hair", Kind: "头发", Name: "Twin Hair"},
		{Folder: "Assets/Shop/Gun", Kind: "道具", Name: "Gun"}}}); err != nil {
		t.Fatal(err)
	}
	waitRun(t, a)
	if !RecipeReady(a) {
		t.Fatal("a run that dressed the avatar cannot be saved")
	}
	if _, err := SaveRecipe(st, a, "  ", ""); err == nil {
		t.Error("saved with no name")
	}
	r, err := SaveRecipe(st, a, "月光套装", "备注")
	if err != nil {
		t.Fatal(err)
	}
	var assets []string
	for _, x := range r.Assets {
		assets = append(assets, x.Kind+":"+x.Prefab+"→"+x.Object)
	}
	n := 0
	for _, m := range r.Menus {
		n += len(m.Items)
	}
	if r.Base.Name != "Manuka" || r.Base.Prefab != "Assets/Manuka/Manuka.prefab" || n != 4 || strings.Join(assets, ",") !=
		"衣服:Assets/Shop/Moon Dress/Moon Dress_Manuka.prefab→Moon Dress_Manuka,头发:Assets/Shop/Twin Hair/Twin Hair.prefab→Twin Hair,道具:Assets/Shop/Gun/Gun.prefab→Gun" {
		t.Errorf("the recipe: %+v", r)
	}
	if r.Menus[1].Items[0].Parameter != "Hair_Choose" || !r.Menus[0].Items[0].Default {
		t.Errorf("the menu lost what build_menu was given: %+v", r.Menus)
	}
	if got, err := LoadRecipe(r.ID); err != nil || got.Name != "月光套装" || got.Project != a {
		t.Errorf("not kept: %+v %v", got, err)
	}
	if _, err := UndoLast(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if r2, err := SaveRecipe(st, a, "无菜单", ""); err != nil || len(r2.Menus) != 0 || len(r2.Assets) != 3 {
		t.Errorf("the menu that was undone is still in it: %+v %v", r2, err)
	}

	// another project
	b, f := sceneProject(t, func(f *fakeScene, cmd string, args map[string]any) (any, string) {
		if cmd == "dress" || cmd == "place" {
			f.mu.Lock()
			f.cmds = append(f.cmds, cmd+" "+str(args, "prefab"))
			f.mu.Unlock()
			n := strings.TrimSuffix(filepath.Base(str(args, "prefab")), ".prefab") + " (1)"
			return map[string]any{"name": n, "object": n}, ""
		}
		return f.answer(cmd, args)
	})
	for _, p := range []string{"Assets/Manuka/Manuka.prefab", "Assets/Imported/Moon Dress_Manuka.prefab", "Assets/Shop/Gun/Gun.prefab"} {
		testkit.WriteFile(t, filepath.Join(b, filepath.FromSlash(p)), 1)
	}
	if err := StartRecipeRun(st, b, r.ID); err != nil {
		t.Fatal(err)
	}
	log := logOf(waitRun(t, b))
	f.mu.Lock()
	cmds, menu := strings.Join(f.cmds, " | "), f.menu
	f.mu.Unlock()
	for _, want := range []string{"place_avatar Assets/Manuka/Manuka.prefab", "dress Assets/Imported/Moon Dress_Manuka.prefab", "place Assets/Shop/Gun/Gun.prefab", "build_menu", "checkup"} {
		if !strings.Contains(cmds, want) {
			t.Errorf("not asked: %s\n%s", want, cmds)
		}
	}
	var got []string
	for _, m := range menu {
		got = append(got, m["label"].(string)+"="+strings.Join(strsOf(m["objects"]), "+"))
	}
	if strings.Join(got, ",") != "Moon Dress=Moon Dress_Manuka (1),一键脱光=,Gun=Gun (1)" {
		t.Errorf("the menu: %v", got)
	}
	for _, want := range []string{"「Twin Hair」不在该工程中", "1 个菜单项", "方案已应用（1 项素材未装上"} {
		if !strings.Contains(log, want) {
			t.Errorf("the log lacks %q:\n%s", want, log)
		}
	}
	// and what it did can be kept in turn
	if !RecipeReady(b) {
		t.Error("an applied recipe cannot be saved again")
	}
	if err := StartRecipeRun(st, b, "../x"); err == nil {
		t.Error("an id out of the folder")
	}
}

func TestImportRecipeChecksEveryPath(t *testing.T) {
	testkit.NewStore(t)
	for _, bad := range []string{
		`{"format":"miovrca-recipe","schema":1,"name":"x","base":{"prefab":"../x.prefab"}}`,
		`{"format":"miovrca-recipe","schema":1,"name":"x","base":{},"menus":[{"items":[{"kind":"skin","materialsFrom":"C:/x.prefab"}]}]}`,
	} {
		if _, err := ImportRecipe([]byte(bad), false); err == nil {
			t.Errorf("accepted: %s", bad)
		}
	}
}
