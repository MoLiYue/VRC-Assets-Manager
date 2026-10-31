package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"vrclib/internal/core"
	"vrclib/internal/unity"
)

// The AI service the player picked, and what a conversation with it has done.
//
// The address and the model are kept in ai.json, which a library export carries; the API key never is. It
// is kept in ai-keys.dat, encrypted under this computer's account, and is only ever sent to the address it
// was saved with — a library brought in from another computer cannot point the key somewhere else.

// AIProfile: one service.
type AIProfile struct {
	BaseURL string  `json:"baseUrl,omitempty"`
	Model   string  `json:"model,omitempty"`
	In      float64 `json:"in,omitempty"`  // price per million tokens in, for the usage page
	Out     float64 `json:"out,omitempty"` // and out
	HasKey  bool    `json:"hasKey,omitempty"`
}

// AIConfig: which service is in use, and what is known of each.
type AIConfig struct {
	Provider string                `json:"provider,omitempty"`
	Profiles map[string]*AIProfile `json:"profiles,omitempty"`
	Budget   int64                 `json:"budget,omitempty"` // tokens one run may use (0 = no limit)
}

var cfgMu sync.Mutex

func aiFile() string   { return filepath.Join(core.DataDir, "ai.json") }
func keysFile() string { return filepath.Join(core.DataDir, "ai-keys.dat") }

// LoadAIConfig reads ai.json, with the keys kept beside it marked (but not given out).
func LoadAIConfig() AIConfig {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return loadCfgLocked()
}

func loadCfgLocked() AIConfig {
	cfg := AIConfig{Profiles: map[string]*AIProfile{}}
	if b, err := core.ReadDataFile(aiFile()); err == nil {
		_ = json.Unmarshal(b, &cfg)
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]*AIProfile{}
	}
	keys := loadKeysLocked()
	for name, p := range cfg.Profiles {
		p.HasKey = keys[name] != ""
	}
	return cfg
}

func saveCfgLocked(cfg AIConfig) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(core.DataDir, 0755); err != nil {
		return err
	}
	return os.WriteFile(aiFile(), append(b, '\n'), 0644)
}

func loadKeysLocked() map[string]string {
	out := map[string]string{}
	b, err := core.ReadDataFile(keysFile())
	if err != nil {
		return out
	}
	dec, err := core.UnprotectData(b)
	if err != nil {
		return out
	}
	_ = json.Unmarshal(dec, &out)
	return out
}

func saveKeysLocked(keys map[string]string) error {
	b, err := json.Marshal(keys)
	if err != nil {
		return err
	}
	enc, err := core.ProtectData(b)
	if err != nil {
		return err
	}
	return os.WriteFile(keysFile(), enc, 0600)
}

// keyFor: the key saved for a service, and the address it was saved with. A key is only ever sent to that
// address.
func keyFor(provider, baseURL string) string {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	cfg := loadCfgLocked()
	p := cfg.Profiles[provider]
	if p == nil || !sameHost(p.BaseURL, baseURL) {
		return ""
	}
	return loadKeysLocked()[provider]
}

// sameHost: two addresses are the same service.
func sameHost(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ua, erra := url.Parse(a)
	ub, errb := url.Parse(b)
	if erra != nil || errb != nil {
		return false
	}
	return strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host)
}

// ---------- what a conversation has done ----------

// aiStep: one change made to a project's scene, as 「撤销上一步」 offers it.
type aiStep struct {
	What string `json:"what"`
	At   int64  `json:"at"`
	Note string `json:"note,omitempty"`
}

var steps struct {
	mu sync.Mutex
	m  map[string][]aiStep // project → the steps that can be taken back
}

// NoteFix writes down a change made to the scene outside a conversation (a checkup fix), so that
// 「撤销上一步」 can take it back like one of the AI's own.
func NoteFix(project string, fix any) {
	if project == "" {
		return
	}
	note := ""
	if b, err := json.Marshal(fix); err == nil {
		note = string(b)
		if len(note) > 2000 {
			note = note[:2000]
		}
	}
	steps.mu.Lock()
	if steps.m == nil {
		steps.m = map[string][]aiStep{}
	}
	k := core.PathKey(project)
	// 「撤销上一步」 is one step deep: what a fix leaves to take back is that fix, not a pile of them
	steps.m[k] = []aiStep{{What: "fix", At: time.Now().Unix(), Note: note}}
	steps.mu.Unlock()
	core.BumpRev()
}

// sessionChanges: how many steps of a project can be taken back.
func sessionChanges(project string) int {
	steps.mu.Lock()
	defer steps.mu.Unlock()
	return len(steps.m[core.PathKey(project)])
}

// ---------- the routes ----------

// RegisterAI adds the AI's own routes to the server: the service the player picks, its usage, and the
// pipeline's runs, conversation and session.
func RegisterAI(st *core.Store, post func(string, func(http.ResponseWriter, map[string]json.RawMessage))) {
	str := func(b map[string]json.RawMessage, k string) string {
		var s string
		_ = json.Unmarshal(b[k], &s)
		return s
	}
	fail := func(w http.ResponseWriter, msg string) {
		core.WriteJSON(w, map[string]any{"ok": false, "err": msg})
	}

	post("/api/ai/config", func(w http.ResponseWriter, b map[string]json.RawMessage) {
		core.WriteJSON(w, map[string]any{"ok": true, "ai": configView()})
	})
	// a request of the service with what the form says, before it is saved
	post("/api/ai/test", func(w http.ResponseWriter, b map[string]json.RawMessage) {
		provider, base, model := strings.TrimSpace(str(b, "provider")), strings.TrimSpace(str(b, "baseUrl")), strings.TrimSpace(str(b, "model"))
		pv, ok := providerOf(provider)
		if !ok {
			fail(w, "请选择 AI 服务")
			return
		}
		if base == "" {
			base = pv.Base
		}
		key := strings.TrimSpace(str(b, "key"))
		if key == "" {
			key = keyFor(provider, base)
		}
		if key == "" || model == "" {
			fail(w, "请填写 API Key 并选择模型")
			return
		}
		c := &Client{Wire: pv.Wire, Base: base, Model: model, Key: key, HTTP: core.HTTPClient(st)}
		ctx, cancel := timeout(60 * time.Second)
		defer cancel()
		t0 := time.Now()
		a, err := c.Chat(ctx, "回答尽量简短。", []Msg{{Role: "user", Content: "请只回答：可以"}}, 64)
		if err != nil {
			fail(w, core.TrimErr(err))
			return
		}
		core.WriteJSON(w, map[string]any{"ok": true, "note": fmt.Sprintf("连接成功（%.1f 秒）：%s", time.Since(t0).Seconds(), strings.TrimSpace(a.Text))})
	})
	post("/api/ai/usage", func(w http.ResponseWriter, b map[string]json.RawMessage) {
		core.WriteJSON(w, usageReport())
	})
	post("/api/ai/usage/budget", func(w http.ResponseWriter, b map[string]json.RawMessage) {
		var n int64
		_ = json.Unmarshal(b["tokens"], &n)
		setBudget(n)
		core.WriteJSON(w, usageReport())
	})
	post("/api/ai/usage/price", func(w http.ResponseWriter, b map[string]json.RawMessage) {
		var pr *Price
		if raw := b["price"]; len(raw) > 0 && string(raw) != "null" {
			pr = &Price{}
			if json.Unmarshal(raw, pr) != nil || pr.In < 0 || pr.Out < 0 || pr.Hit < 0 {
				fail(w, "价格无效")
				return
			}
			if pr.Currency != "USD" {
				pr.Currency = "CNY"
			}
		}
		setPrice(str(b, "provider"), str(b, "model"), pr)
		core.WriteJSON(w, usageReport())
	})
	post("/api/ai/usage/clear", func(w http.ResponseWriter, b map[string]json.RawMessage) {
		clearUsage()
		core.WriteJSON(w, usageReport())
	})
	// what is left on a DeepSeek or Kimi account, asked of the service
	post("/api/ai/balance", func(w http.ResponseWriter, b map[string]json.RawMessage) {
		bal, err := balance(st)
		if err != nil {
			fail(w, core.TrimErr(err))
			return
		}
		core.WriteJSON(w, map[string]any{"ok": true, "balance": bal})
	})
	// the service the player picked. The key is kept apart and never answered with.
	post("/api/ai/save", func(w http.ResponseWriter, b map[string]json.RawMessage) {
		provider := strings.TrimSpace(str(b, "provider"))
		if provider == "" || strings.ContainsAny(provider, `/\.`) {
			fail(w, "请选择 AI 服务")
			return
		}
		base := strings.TrimSpace(str(b, "baseUrl"))
		if base != "" {
			if u, err := url.Parse(base); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
				fail(w, "服务地址无效")
				return
			}
		}
		cfgMu.Lock()
		cfg := loadCfgLocked()
		p := cfg.Profiles[provider]
		if p == nil {
			p = &AIProfile{}
			cfg.Profiles[provider] = p
		}
		p.BaseURL, p.Model = base, strings.TrimSpace(str(b, "model"))
		if v := str(b, "in"); v != "" {
			p.In = atof(v)
		}
		if v := str(b, "out"); v != "" {
			p.Out = atof(v)
		}
		cfg.Provider = provider
		keys := loadKeysLocked()
		if raw, sent := b["key"]; sent {
			if k := strings.TrimSpace(str(b, "key")); k != "" {
				keys[provider] = k
			} else if string(raw) == `""` {
				delete(keys, provider) // 「清除」
			}
		}
		p.HasKey = keys[provider] != ""
		err := saveCfgLocked(cfg)
		if err == nil {
			err = saveKeysLocked(keys)
		}
		cfgMu.Unlock()
		if err != nil {
			fail(w, "无法保存 AI 设置："+core.TrimErr(err))
			return
		}
		core.BumpRev()
		core.WriteJSON(w, map[string]any{"ok": true, "ai": configView()})
	})
	// the models a service offers, asked for with the key saved for that very address
	post("/api/ai/models", func(w http.ResponseWriter, b map[string]json.RawMessage) {
		provider, base := strings.TrimSpace(str(b, "provider")), strings.TrimSpace(str(b, "baseUrl"))
		models, err := listModels(st, provider, base)
		if err != nil {
			fail(w, core.TrimErr(err))
			return
		}
		core.WriteJSON(w, map[string]any{"ok": true, "models": models})
	})
	// the log of this project's runs, whether one is going, and what 「撤销上一步」 has to take back
	post("/api/ai/session", func(w http.ResponseWriter, b map[string]json.RawMessage) {
		core.WriteJSON(w, sessionView(strings.TrimSpace(str(b, "project"))))
	})
	known := func(w http.ResponseWriter, b map[string]json.RawMessage) (string, bool) {
		p, ok := unity.KnownProject(st, strings.TrimSpace(str(b, "project")))
		if !ok {
			fail(w, "该工程不在工程列表中")
		}
		return p, ok
	}
	// the pipeline's plugin (and UnitySkills) put into the project, or taken out
	post("/api/ai/setup", func(w http.ResponseWriter, b map[string]json.RawMessage) {
		p, ok := known(w, b)
		if !ok {
			return
		}
		var on bool
		_ = json.Unmarshal(b["on"], &on)
		if !on {
			if err := unity.RemoveAIKit(p); err != nil {
				fail(w, core.TrimErr(err))
				return
			}
			core.WriteJSON(w, map[string]any{"ok": true, "kit": unity.AIKitStatus(p)})
			return
		}
		note, err := unity.InstallAIKit(p)
		if err != nil {
			fail(w, core.TrimErr(err))
			return
		}
		core.WriteJSON(w, map[string]any{"ok": true, "note": note, "kit": unity.AIKitStatus(p)})
	})
	post("/api/ai/run", func(w http.ResponseWriter, b map[string]json.RawMessage) {
		p, ok := known(w, b)
		if !ok {
			return
		}
		var req RunReq
		raw, _ := json.Marshal(b)
		if err := json.Unmarshal(raw, &req); err != nil {
			fail(w, "请求无法读取")
			return
		}
		req.Project = p
		if err := StartRun(st, req); err != nil {
			fail(w, core.TrimErr(err))
			return
		}
		core.WriteJSON(w, map[string]any{"ok": true})
	})
	// the player's answer to what the log asks (an operation, or going on past the budget)
	post("/api/ai/answer", func(w http.ResponseWriter, b map[string]json.RawMessage) {
		p, ok := known(w, b)
		if !ok {
			return
		}
		var yes, all bool
		_ = json.Unmarshal(b["ok"], &yes)
		_ = json.Unmarshal(b["all"], &all)
		if err := AnswerAsk(p, yes, all); err != nil {
			fail(w, err.Error())
			return
		}
		core.WriteJSON(w, map[string]any{"ok": true})
	})
	post("/api/ai/cancel", func(w http.ResponseWriter, b map[string]json.RawMessage) {
		if p, ok := known(w, b); ok {
			CancelRun(p)
			core.WriteJSON(w, map[string]any{"ok": true})
		}
	})
	post("/api/ai/undo", func(w http.ResponseWriter, b map[string]json.RawMessage) {
		p, ok := known(w, b)
		if !ok {
			return
		}
		ctx, cancel := timeout(40 * time.Second)
		defer cancel()
		undone, err := UndoLast(ctx, p)
		if err != nil {
			fail(w, core.TrimErr(err))
			return
		}
		core.WriteJSON(w, map[string]any{"ok": true, "undone": undone})
	})
	post("/api/ai/reset", func(w http.ResponseWriter, b map[string]json.RawMessage) {
		p, ok := known(w, b)
		if !ok {
			return
		}
		if err := ResetSession(p); err != nil {
			fail(w, core.TrimErr(err))
			return
		}
		core.WriteJSON(w, map[string]any{"ok": true})
	})
}

// timeout: a context that gives up after d.
func timeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

func atof(s string) float64 {
	var f float64
	_ = json.Unmarshal([]byte(s), &f)
	return f
}

// listModels asks a service what it offers (the OpenAI shape, which the services this program talks to all
// answer in).
func listModels(st *core.Store, provider, base string) ([]string, error) {
	if base == "" {
		return nil, errors.New("请先填写服务地址")
	}
	u := strings.TrimRight(base, "/")
	if !strings.Contains(u, "/v1") {
		u += "/v1"
	}
	req, err := http.NewRequest("GET", u+"/models", nil)
	if err != nil {
		return nil, errors.New("服务地址无效")
	}
	req.Header.Set("User-Agent", core.UA)
	if k := keyFor(provider, base); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	ctx, cancel := timeout(20 * time.Second)
	defer cancel()
	resp, err := core.HTTPClient(st).Do(req.WithContext(ctx))
	if err != nil {
		return nil, errors.New("无法连接 AI 服务：" + core.TrimErr(err))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, errors.New("AI 服务返回错误（HTTP " + itoa(resp.StatusCode) + "）")
	}
	var r struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &r) != nil {
		return nil, errors.New("无法读取模型列表")
	}
	var out []string
	for _, m := range r.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	return out, nil
}

// configView: what the window's AI settings read — the services offered, the one picked, what is saved of
// each (a key only as its last characters), and whether a run can go.
func configView() map[string]any {
	cfg := LoadAIConfig()
	cfgMu.Lock()
	keys := loadKeysLocked()
	cfgMu.Unlock()
	tails := map[string]string{}
	for id, k := range keys {
		if len(k) > 4 {
			tails[id] = "…" + k[len(k)-4:]
		} else if k != "" {
			tails[id] = "…"
		}
	}
	ready := false
	if p := cfg.Profiles[cfg.Provider]; p != nil && p.Model != "" && keys[cfg.Provider] != "" {
		ready = true
	}
	return map[string]any{"provider": cfg.Provider, "profiles": cfg.Profiles, "providers": Providers, "keys": tails,
		"ready": ready, "defaultHierarchy": "", "vision": map[string]any{}, "budget": budget()}
}

// balance: what is left on the account, for the services that tell it (DeepSeek, Kimi).
func balance(st *core.Store) (any, error) {
	c, err := clientFor(st)
	if err != nil {
		return nil, err
	}
	cfg := LoadAIConfig()
	var u string
	switch cfg.Provider {
	case "deepseek":
		u = strings.TrimRight(c.Base, "/") + "/user/balance"
	case "kimi":
		u = strings.TrimRight(c.Base, "/") + "/users/me/balance"
	default:
		return nil, errors.New("该服务商不提供余额查询")
	}
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, errors.New("无法连接 AI 服务：" + core.TrimErr(err))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, serviceErr(resp.StatusCode, raw)
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil, errors.New("余额无法读取")
	}
	return v, nil
}
