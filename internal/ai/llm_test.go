package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vrclib/internal/core"
	"vrclib/internal/testkit"
	"vrclib/internal/unity"
	"vrclib/internal/unity/unitytest"
)

const mockKey = "sk-good-key-1234"

func mockLLM(t *testing.T) string {
	t.Helper()
	old := retryWait
	retryWait = func(time.Duration) time.Duration { return 10 * time.Millisecond }
	t.Cleanup(func() { retryWait = old })
	return testkit.MockLLM(t, filepath.Join("..", ".."))
}

func faults(t *testing.T, base, set string) {
	t.Helper()
	resp, err := http.Get(base + "/__faults?set=" + set)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

// Both wires: a plain answer; a busy service and one in trouble asked again; a reply cut at the output
// limit said so; a conversation past the context, a wrong key and a model that is not there, in words.
func TestChatWires(t *testing.T) {
	base := mockLLM(t)
	testkit.NewStore(t)
	ctx := context.Background()
	for _, wire := range []string{"openai", "claude"} {
		c := &Client{Wire: wire, Base: base, Model: "mio-large", Key: mockKey, HTTP: &http.Client{Timeout: 20 * time.Second}}
		a, err := c.Chat(ctx, "you are a helper", []Msg{{Role: "user", Content: "你好"}}, 100)
		if err != nil || !strings.Contains(a.Text, "已经记下") {
			t.Errorf("%s: %+v %v", wire, a, err)
		}
		faults(t, base, "429,500")
		if a, err := c.Chat(ctx, "x", []Msg{{Role: "user", Content: "再来"}}, 100); err != nil || a.Text == "" {
			t.Errorf("%s after 429 and 500: %v", wire, err)
		}
		faults(t, base, "cut")
		if a, err := c.Chat(ctx, "x", []Msg{{Role: "user", Content: "装配到模型上"}}, 100); err != nil || !a.Cut {
			t.Errorf("%s cut: %+v %v", wire, a, err)
		}
		faults(t, base, "context")
		if _, err := c.Chat(ctx, "x", []Msg{{Role: "user", Content: "x"}}, 100); err == nil || !strings.Contains(err.Error(), "上下文长度") {
			t.Errorf("%s context: %v", wire, err)
		}
		bad := *c
		bad.Key = "sk-wrong"
		if _, err := bad.Chat(ctx, "x", []Msg{{Role: "user", Content: "x"}}, 100); err == nil || !strings.Contains(err.Error(), "API Key 无效") {
			t.Errorf("%s wrong key: %v", wire, err)
		}
		no := *c
		no.Model = "mio-none"
		if _, err := no.Chat(ctx, "x", []Msg{{Role: "user", Content: "x"}}, 100); err == nil || !strings.Contains(err.Error(), "没有这个模型") {
			t.Errorf("%s no model: %v", wire, err)
		}
	}
}

// What the program checks of a plan.
func TestCheckItems(t *testing.T) {
	o := []outfitInfo{{Kind: "衣服", Category: []string{"衣服"}, Object: "Dress", Meshes: []string{"Dress/Coat"}, Colours: []string{"Assets/D/Red.prefab"}}}
	ok := []menuItem{{Kind: "outfit", Path: []string{"衣服", "裙子"}, Label: "白", Objects: []string{"Dress"}},
		{Kind: "outfit", Path: []string{"衣服", "裙子"}, Label: "红", Objects: []string{"Dress"}, MaterialsFrom: "Assets/D/Red.prefab"},
		{Kind: "part", Path: []string{"衣服", "裙子"}, Label: "外套", Objects: []string{"Dress/Coat"}}}
	if bad := checkItems(ok, o); len(bad) != 0 {
		t.Errorf("a good plan: %v", bad)
	}
	worse := append(append([]menuItem{}, ok...),
		menuItem{Kind: "part", Path: []string{"衣服", "裙子"}, Label: "外套", Objects: []string{"Dress/NOPE"}},
		menuItem{Kind: "dance", Path: []string{"头发"}, Label: "x", MaterialsFrom: "Assets/elsewhere.prefab"})
	for i := 0; i < 6; i++ {
		worse = append(worse, menuItem{Kind: "part", Path: []string{"衣服", "裙子"}, Label: "p" + string(rune('a'+i)), Objects: []string{"Dress"}})
	}
	bad := strings.Join(checkItems(worse, o), "\n")
	for _, want := range []string{"Dress/NOPE」不存在", "重名", "kind「dance」", "分类「头发」", "materialsFrom", "超过 8 项"} {
		if !strings.Contains(bad, want) {
			t.Errorf("not caught: %s\n%s", want, bad)
		}
	}
	if items, err := parseItems("好的：\n```json\n{\"items\":[{\"kind\":\"outfit\",\"path\":[\"衣服\"],\"label\":\"a\"}]}\n```"); err != nil || len(items) != 1 {
		t.Errorf("a JSON block: %v %v", items, err)
	}
	if _, err := parseItems("我觉得可以"); err == nil {
		t.Error("an answer without JSON")
	}
}

// A run with the AI: the outfit goes on as without it, the AI names its menu (a sub menu of its own, its
// colour a choice) and the plan passes the check; the AI says a word on the result; the usage is counted.
func TestDressWithAI(t *testing.T) {
	base := mockLLM(t)
	st := testkit.NewStore(t)
	proj := t.TempDir()
	for _, d := range []string{"Assets", "ProjectSettings"} {
		_ = os.MkdirAll(filepath.Join(proj, d), 0755)
	}
	const P = "Assets/KDress/Kaguya/Kaguya prefab/"
	f := &fakeScene{dir: proj, placed: true}
	unitytest.FakeUnity(t, proj, func(cmd string, args map[string]any) (any, string) {
		switch cmd {
		case "inspect":
			return map[string]any{"avatars": []any{map[string]any{"name": "Kaguya", "path": "Kaguya"}}}, ""
		case "prefabs":
			return map[string]any{"prefabs": []any{
				map[string]any{"path": P + "Envy cat.prefab", "name": "Envy cat", "renderers": 3},
				map[string]any{"path": P + "Melty Devil .prefab", "name": "Melty Devil ", "renderers": 3}}}, ""
		case "dress":
			return map[string]any{"name": "Envy cat", "changed": true, "outfit": map[string]any{"object": "Envy cat", "meshes": []any{
				map[string]any{"path": "Envy cat/UV4_Cardigan"}, map[string]any{"path": "Envy cat/UV2_Sandal"}}}}, ""
		}
		return f.answer(cmd, args)
	})
	_ = os.WriteFile(filepath.Join(proj, "Packages", unity.PipePkg, "package.json"), []byte(`{}`), 0644)
	cfgMu.Lock()
	cfg := loadCfgLocked()
	cfg.Provider = "custom"
	cfg.Profiles["custom"] = &AIProfile{BaseURL: base, Model: "mio-large"}
	_ = saveCfgLocked(cfg)
	_ = saveKeysLocked(map[string]string{"custom": mockKey})
	cfgMu.Unlock()
	if v := configView(); v["ready"] != true {
		t.Fatalf("not ready: %v", v)
	}
	if err := StartRun(st, RunReq{Project: proj, Mode: "dress", Assets: []RunAsset{{Folder: "Assets/KDress", Kind: "衣服", Name: "恶魔水手服"}}}); err != nil {
		t.Fatal(err)
	}
	steps := waitRun(t, proj)
	var log []string
	for _, s := range steps {
		log = append(log, s.Kind+":"+s.Text+" "+s.Out)
	}
	all := strings.Join(log, "\n")
	if !strings.Contains(all, "方案通过校验") || !strings.Contains(all, "体检没有需要处理的问题") {
		t.Errorf("the log:\n%s", all)
	}
	f.mu.Lock()
	menu := f.menu
	f.mu.Unlock()
	var got []string
	for _, m := range menu {
		p, _ := m["path"].([]any)
		got = append(got, m["kind"].(string)+":"+m["label"].(string)+"@"+strings.Join(strsOf(p), "/"))
	}
	joined := strings.Join(got, ",")
	for _, want := range []string{"outfit:嫉妒猫@衣服/恶魔水手服", "outfit:融化恶魔@衣服/恶魔水手服", "part:UV4_Cardigan@衣服/恶魔水手服", "strip:一键脱光@衣服"} {
		if !strings.Contains(joined, want) {
			t.Errorf("menu lacks %s: %s", want, joined)
		}
	}
	var rep struct {
		Models []struct {
			Model string `json:"model"`
			Total Tally  `json:"total"`
		} `json:"models"`
	}
	b, _ := json.Marshal(usageReport())
	_ = json.Unmarshal(b, &rep)
	if len(rep.Models) != 1 || rep.Models[0].Model != "mio-large" || rep.Models[0].Total.Requests != 2 {
		t.Errorf("usage: %s", b)
	}
	_ = core.DataDir
}

// A plan that names an object that is not there: sent back once with what is wrong; wrong again, the draft
// is built — and the second request carried the problems.
func TestArrangeFallsBackToDraft(t *testing.T) {
	testkit.NewStore(t)
	var asked []string
	srv := httptestLLM(t, func(body string) string {
		asked = append(asked, body)
		return "```json\n{\"items\":[{\"kind\":\"outfit\",\"path\":[\"衣服\",\"裙\"],\"label\":\"a\",\"objects\":[\"Ghost\"]}]}\n```"
	})
	c := &Client{Wire: "openai", Base: srv, Model: "m", Key: "k", HTTP: &http.Client{Timeout: 10 * time.Second}}
	o := outfitInfo{Kind: "衣服", Category: []string{"衣服"}, Name: "Dress", Object: "Dress"}
	outfitDraft(&o, true)
	items, note := arrangeMenu(context.Background(), t.TempDir(), c, []outfitInfo{o})
	if !strings.Contains(note, "两次都未通过") || len(items) != 1 || items[0].Objects[0] != "Dress" {
		t.Errorf("%q %+v", note, items)
	}
	if len(asked) != 2 || !strings.Contains(asked[1], "Ghost") || !strings.Contains(asked[1], "不存在") {
		t.Errorf("the second request did not carry the problems: %d requests", len(asked))
	}
}
