package ai

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"vrclib/internal/core"
	"vrclib/internal/testkit"
	"vrclib/internal/unity"
	"vrclib/internal/unity/unitytest"
)

// fakeScene: what the plugin answers for a scene that has no avatar until one is placed.
type fakeScene struct {
	mu     sync.Mutex
	cmds   []string
	placed bool
	menu   []map[string]any
	dir    string
}

func (f *fakeScene) answer(cmd string, args map[string]any) (any, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds = append(f.cmds, cmd+" "+strings.TrimSpace(str(args, "prefab")+" "+strings.Join(strsOf(args["folders"]), ",")))
	switch cmd {
	case "inspect":
		if !f.placed {
			return map[string]any{"avatars": []any{}}, ""
		}
		return map[string]any{"avatars": []any{map[string]any{"name": "Manuka", "path": "Manuka", "children": []any{map[string]any{"object": "Body"}}}}}, ""
	case "prefabs":
		switch strsOf(args["folders"])[0] {
		case "Assets/Manuka":
			return map[string]any{"prefabs": []any{map[string]any{"path": "Assets/Manuka/Manuka.prefab", "name": "Manuka", "renderers": 5, "wholeAvatar": true}}}, ""
		case "Assets/Shop/Moon Dress":
			return map[string]any{"prefabs": []any{
				map[string]any{"path": "Assets/Shop/Moon Dress/Moon Dress_Kaguya.prefab", "name": "Moon Dress_Kaguya", "renderers": 4},
				map[string]any{"path": "Assets/Shop/Moon Dress/Moon Dress_Manuka.prefab", "name": "Moon Dress_Manuka", "renderers": 4},
				map[string]any{"path": "Assets/Shop/Moon Dress/Parts/Ribbon.prefab", "name": "Ribbon", "renderers": 1}}}, ""
		case "Assets/Shop/Twin Hair":
			return map[string]any{"prefabs": []any{map[string]any{"path": "Assets/Shop/Twin Hair/Twin Hair.prefab", "name": "Twin Hair", "renderers": 2}}}, ""
		case "Assets/Shop/Gun":
			return map[string]any{"prefabs": []any{map[string]any{"path": "Assets/Shop/Gun/Gun.prefab", "name": "Gun", "renderers": 1}}}, ""
		}
		return map[string]any{"prefabs": []any{}}, ""
	case "place_avatar":
		f.placed = true
		return map[string]any{"name": "Manuka"}, ""
	case "dress":
		n := strings.TrimSuffix(filepath.Base(str(args, "prefab")), ".prefab")
		return map[string]any{"name": n, "changed": true, "warnings": []any{}}, ""
	case "place":
		return map[string]any{"name": "Gun", "object": "Gun", "changed": true}, ""
	case "build_menu":
		b, _ := json.Marshal(args["items"])
		_ = json.Unmarshal(b, &f.menu)
		return map[string]any{"changed": true}, ""
	case "snapshot":
		shot := filepath.Join(f.dir, "Temp", "front.png")
		_ = os.MkdirAll(filepath.Dir(shot), 0755)
		_ = os.WriteFile(shot, testkit.ZipBytes(nil, map[string][]byte{}), 0644)
		return map[string]any{"shots": []any{map[string]any{"view": "front", "file": shot}}}, ""
	case "checkup":
		return map[string]any{"avatar": "Manuka", "ranks": map[string]any{"pc": "Good", "quest": "Poor"}, "items": []any{
			map[string]any{"id": "params", "label": "同步参数", "level": "ok"}}}, ""
	}
	return nil, "不认识的操作：" + cmd
}

func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }
func strsOf(v any) []string {
	l, _ := v.([]any)
	var out []string
	for _, x := range l {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func waitRun(t *testing.T, p string) []Step {
	t.Helper()
	time.Sleep(50 * time.Millisecond)
	for i := 0; i < 400; i++ {
		v := sessionView(p)
		if !v["busy"].(bool) {
			return v["steps"].([]Step)
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the run did not end")
	return nil
}

// A scene with no avatar: the base body goes in, the outfit in the version for it, the hair, the prop; the
// menu is outfits as one choice with 「一键脱光」, hair as another, the prop a switch; then pictures and the check.
func TestDressWithoutAI(t *testing.T) {
	st := testkit.NewStore(t)
	proj := t.TempDir()
	for _, d := range []string{"Assets", "ProjectSettings"} {
		_ = os.MkdirAll(filepath.Join(proj, d), 0755)
	}
	f := &fakeScene{dir: proj}
	unitytest.FakeUnity(t, proj, f.answer)
	_ = os.WriteFile(filepath.Join(proj, "Packages", unity.PipePkg, "package.json"), []byte(`{}`), 0644)
	err := StartRun(st, RunReq{Project: proj, Mode: "dress", NoAI: true, Assets: []RunAsset{
		{Folder: "Assets/Manuka", Kind: "素体", Name: "Manuka"},
		{Folder: "Assets/Shop/Moon Dress", Kind: "衣服", Name: "Moon Dress"},
		{Folder: "Assets/Shop/Twin Hair", Kind: "头发", Name: "Twin Hair"},
		{Folder: "Assets/Shop/Gun", Kind: "道具", Name: "Gun"},
		{Folder: "Assets/Shop/Empty", Kind: "衣服", Name: "Empty"}}})
	if err != nil {
		t.Fatal(err)
	}
	steps := waitRun(t, proj)
	f.mu.Lock()
	cmds := strings.Join(f.cmds, " | ")
	menu := f.menu
	f.mu.Unlock()
	for _, want := range []string{"place_avatar Assets/Manuka/Manuka.prefab", "dress Assets/Shop/Moon Dress/Moon Dress_Manuka.prefab",
		"dress Assets/Shop/Twin Hair/Twin Hair.prefab", "place Assets/Shop/Gun/Gun.prefab", "build_menu", "snapshot", "checkup"} {
		if !strings.Contains(cmds, want) {
			t.Errorf("not asked: %s\n%s", want, cmds)
		}
	}
	if strings.Contains(cmds, "Moon Dress_Kaguya") || strings.Contains(cmds, "Ribbon") {
		t.Errorf("another base body's version, or a part, was put on: %s", cmds)
	}
	var got []string
	for _, m := range menu {
		got = append(got, m["kind"].(string)+":"+m["label"].(string))
	}
	if strings.Join(got, ",") != "outfit:Moon Dress,outfit:Twin Hair,toggle:Gun,strip:一键脱光" {
		t.Errorf("menu: %v", got)
	}
	if menu[1]["parameter"] != "Hair_Choose" || menu[0]["default"] != true {
		t.Errorf("hair's own choice, the first outfit worn: %v", menu)
	}
	var text []string
	shots := 0
	for _, s := range steps {
		text = append(text, s.Kind+":"+s.Text)
		shots += len(s.Imgs)
		if s.Busy {
			t.Errorf("a step left busy: %+v", s)
		}
	}
	all := strings.Join(text, "\n")
	if !strings.Contains(all, "Empty」中没有可用的 prefab") || !strings.Contains(all, "装配完成") || shots != 1 {
		t.Errorf("the log:\n%s", all)
	}
	if v := sessionView(proj); v["changes"].(int) == 0 {
		t.Error("nothing to take back after a run that changed the scene")
	}
	// started again while it runs is refused; a log cleared
	if err := ResetSession(proj); err != nil || len(sessionView(proj)["steps"].([]Step)) != 0 {
		t.Errorf("reset: %v", err)
	}
	if err := StartRun(st, RunReq{Project: proj, Mode: "chat", Text: "  "}); err == nil {
		t.Error("a chat with nothing written was started")
	}
}

// The assets of a project: the library's cards it uses, and a folder of prefabs that is no card.
func TestProjectAssetsOf(t *testing.T) {
	st := testkit.NewStore(t)
	proj := filepath.Join(t.TempDir(), "Kaguya")
	for _, f := range []string{"Assets/Shop/Moon/Moon.prefab", "Assets/Shop/Moon/Tex/a.png", "Assets/Other/Gun/Gun.prefab", "ProjectSettings/x"} {
		testkit.WriteFile(t, filepath.Join(proj, filepath.FromSlash(f)), 1)
	}
	st.Projects = []core.ProjectInfo{{Name: "Kaguya", Path: proj}}
	st.Assets = []*core.Asset{{Key: "name:moon", Name: "Moon Dress", Category: "衣服", Usage: []core.Usage{{Project: "Kaguya", Status: "used", Folder: "Assets/Shop/Moon"}}}}
	list, ok := ProjectAssetsOf(st, proj, false)
	if !ok || len(list) != 2 {
		t.Fatalf("%v %+v", ok, list)
	}
	if a := list[1]; a.Folder != "Assets/Shop/Moon" || a.Source != "name:moon" || a.Kind != "衣服" || a.Prefabs != 1 {
		t.Errorf("the card: %+v", a)
	}
	if a := list[0]; a.Folder != "Assets/Other/Gun" || a.Source != "" || a.Prefabs != 1 {
		t.Errorf("the folder of its own: %+v", a)
	}
	if _, ok := ProjectAssetsOf(st, t.TempDir(), false); ok {
		t.Error("a folder that is no project")
	}
}
