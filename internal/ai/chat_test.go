package ai

import (
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

// chatProject: a project with the plugin answering as the scene of test/mockllm.py's story — Kaguya, the
// devil sailor outfit in three colours — and the AI service saved as the stand-in.
func chatProject(t *testing.T, wire string) (*core.Store, string, *[]string) {
	t.Helper()
	base := mockLLM(t)
	st := testkit.NewStore(t)
	proj := t.TempDir()
	for _, d := range []string{"Assets", "ProjectSettings"} {
		_ = os.MkdirAll(filepath.Join(proj, d), 0755)
	}
	var mu sync.Mutex
	var cmds []string
	const P = "Assets/KDress/Kaguya/Kaguya prefab/"
	unitytest.FakeUnity(t, proj, func(cmd string, args map[string]any) (any, string) {
		mu.Lock()
		cmds = append(cmds, cmd)
		mu.Unlock()
		switch cmd {
		case "inspect":
			return map[string]any{"avatars": []any{map[string]any{"name": "Kaguya", "path": "Kaguya"}}}, ""
		case "prefabs":
			return map[string]any{"prefabs": []any{map[string]any{"path": P + "Envy cat.prefab", "name": "Envy cat", "renderers": 3}}}, ""
		case "dress":
			return map[string]any{"name": "Envy cat", "changed": true}, ""
		case "inspect_object":
			return map[string]any{"object": "Envy cat", "meshes": []any{map[string]any{"path": "Envy cat/UV2_Sandal"}}}, ""
		case "build_menu":
			if strings.Contains(fmtJSON(args), "NOPE") {
				return nil, "未找到物体：Envy cat/NOPE"
			}
			return map[string]any{"changed": true}, ""
		case "snapshot":
			return map[string]any{"shots": []any{}}, ""
		}
		return nil, "不认识的操作：" + cmd
	})
	_ = os.WriteFile(filepath.Join(proj, "Packages", unity.PipePkg, "package.json"), []byte(`{}`), 0644)
	prov := map[string]string{"openai": "custom", "claude": "claude"}[wire]
	cfgMu.Lock()
	cfg := loadCfgLocked()
	cfg.Provider = prov
	cfg.Profiles[prov] = &AIProfile{BaseURL: base, Model: "mio-large"}
	_ = saveCfgLocked(cfg)
	_ = saveKeysLocked(map[string]string{prov: mockKey})
	cfgMu.Unlock()
	t.Cleanup(func() { _ = ResetSession(proj) })
	return st, proj, &cmds
}

func logOf(steps []Step) string {
	var out []string
	for _, s := range steps {
		out = append(out, s.Kind+":"+s.Text+" "+s.Out)
	}
	return strings.Join(out, "\n")
}

// The story of test/mockllm.py, on both wires: look, list, dress, look closer, a menu with a path that is not
// there — told, and done again right —, a picture, and a word at the end.
func TestChatDresses(t *testing.T) {
	for _, wire := range []string{"openai", "claude"} {
		t.Run(wire, func(t *testing.T) {
			st, proj, cmds := chatProject(t, wire)
			if err := StartRun(st, RunReq{Project: proj, Mode: "chat", Text: "把恶魔水手服穿到头像上"}); err != nil {
				t.Fatal(err)
			}
			steps := waitRun(t, proj)
			log := logOf(steps)
			got := strings.Join(*cmds, ",")
			if !strings.Contains(got, "inspect,prefabs,dress,inspect_object,build_menu,build_menu,snapshot") {
				t.Errorf("%s: commands %s\n%s", wire, got, log)
			}
			wants := []string{"user:把恶魔水手服穿到头像上", "未找到物体：Envy cat/NOPE", "say:做好了"}
			if wire == "openai" {
				// what the AI was told of the picture shows in its last words (mockllm.py reads the last tool
				// result on this wire only)
				wants = append(wants, "这次我看不了图")
			}
			for _, want := range wants {
				if !strings.Contains(log, want) {
					t.Errorf("%s: the log lacks %q\n%s", wire, want, log)
				}
			}
		})
	}
}

// Deleting is asked about first, every time; refused, it is not done, and the AI is told so.
func TestChatAsksBeforeDeleting(t *testing.T) {
	st, proj, _ := chatProject(t, "openai")
	if err := StartRun(st, RunReq{Project: proj, Mode: "chat", Text: "删掉 Assets/Old"}); err != nil {
		t.Fatal(err)
	}
	var asked *Step
	for i := 0; i < 400 && asked == nil; i++ {
		for _, s := range sessionView(proj)["steps"].([]Step) {
			if s.Kind == "ask" && s.Busy {
				s := s
				asked = &s
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if asked == nil {
		t.Fatalf("not asked:\n%s", logOf(sessionView(proj)["steps"].([]Step)))
	}
	if !strings.Contains(asked.Text, "asset_delete") || asked.Opts != "once" {
		t.Errorf("the question: %+v", asked)
	}
	if err := AnswerAsk(proj, false, false); err != nil {
		t.Fatal(err)
	}
	log := logOf(waitRun(t, proj))
	if !strings.Contains(log, "已拒绝") || strings.Contains(log, "UnitySkills：asset_delete") || !strings.Contains(log, "处理完了") {
		t.Errorf("the log:\n%s", log)
	}
	if err := AnswerAsk(proj, true, false); err == nil {
		t.Error("an answer when nothing is asked")
	}
}

func TestSkillRiskAndLoopGuard(t *testing.T) {
	for name, want := range map[string]string{"asset_delete": "once", "Asset_Delete": "once", "scene_save": "once", "script_create": "once",
		"execute_menu_item": "once", "gameobject_set_active": "ask", "get_hierarchy": "", "scene_find_objects": ""} {
		if got := skillRisk(name); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
	var g loopGuard
	if g.note("a", false) != "" || g.note("b", false) != "" || g.note("a", false) != "" {
		t.Error("stopped too soon")
	}
	if why := g.note("a", false); !strings.Contains(why, "第三次") {
		t.Errorf("the same failing operation a third time: %q", why)
	}
	g = loopGuard{}
	for i := 0; i < 3; i++ {
		g.note("x", true)
	}
	if why := g.note("x", true); !strings.Contains(why, "4 次") {
		t.Errorf("the same operation four times: %q", why)
	}
	g = loopGuard{}
	why := ""
	for i := 0; i < 8; i++ {
		why = g.note(string(rune('a'+i)), false)
	}
	if !strings.Contains(why, "8 步") {
		t.Errorf("eight failures in a row: %q", why)
	}
}
