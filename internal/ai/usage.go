package ai

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"vrclib/internal/core"
)

// What the AI service was asked for, in tokens, and about what it cost.
//
// Kept in ai-usage.json by day and by model. The amounts are estimates at the prices of the price list —
// DeepSeek's own, the others what the player enters — and the services' own bills are what counts.

// Price: per million tokens.
type Price struct {
	Currency string  `json:"currency"`       // CNY, USD
	In       float64 `json:"in"`             // input the service did not have cached
	Hit      float64 `json:"hit"`            // input read from its cache
	Out      float64 `json:"out"`            // output
	Peak     float64 `json:"peak,omitempty"` // DeepSeek: how much more in its busy hours
}

// builtinPrices: what the services publish (DeepSeek's, as of 2026-09).
var builtinPrices = map[string]Price{
	"deepseek/deepseek-chat":     {Currency: "CNY", In: 2, Hit: 0.5, Out: 8, Peak: 2},
	"deepseek/deepseek-reasoner": {Currency: "CNY", In: 4, Hit: 1, Out: 16, Peak: 2},
}

// Tally: tokens of some requests.
type Tally struct {
	In       int                `json:"in"`
	Hit      int                `json:"hit"`
	Out      int                `json:"out"`
	Tokens   int                `json:"tokens"`
	Requests int                `json:"requests,omitempty"`
	Cost     map[string]float64 `json:"cost,omitempty"`
	Unpriced bool               `json:"unpriced,omitempty"`
}

func (t *Tally) add(u Usage, p *Price) {
	in := u.In - u.Cached
	if in < 0 {
		in = 0
	}
	t.In, t.Hit, t.Out = t.In+in, t.Hit+u.Cached, t.Out+u.Out
	t.Tokens += u.In + u.Out
	t.Requests++
	if p == nil {
		t.Unpriced = true
		return
	}
	if t.Cost == nil {
		t.Cost = map[string]float64{}
	}
	t.Cost[p.Currency] += (float64(in)*p.In + float64(u.Cached)*p.Hit + float64(u.Out)*p.Out) / 1e6
}

func (t *Tally) merge(o Tally) {
	t.In, t.Hit, t.Out, t.Tokens, t.Requests = t.In+o.In, t.Hit+o.Hit, t.Out+o.Out, t.Tokens+o.Tokens, t.Requests+o.Requests
	t.Unpriced = t.Unpriced || o.Unpriced
	for k, v := range o.Cost {
		if t.Cost == nil {
			t.Cost = map[string]float64{}
		}
		t.Cost[k] += v
	}
}

type usageFile struct {
	Days   map[string]map[string]*Tally `json:"days"`   // "2026-10-30" → "provider/model" → tally
	Prices map[string]Price             `json:"prices"` // the player's own, by "provider/model"
	Budget int64                        `json:"budget"` // tokens one run may use (0 = no limit)
	Runs   []int                        `json:"runs"`   // tokens of the last runs, for the estimate
	Set    bool                         `json:"set"`    // the budget was set by the player
}

var use struct {
	mu   sync.Mutex
	f    *usageFile
	dir  string
	conv map[string]*Tally // this program's run: by project
}

func usagePath() string { return filepath.Join(core.DataDir, "ai-usage.json") }

func useLoad() *usageFile {
	if use.f != nil && use.dir == core.DataDir {
		return use.f
	}
	f := &usageFile{}
	if b, err := core.ReadDataFile(usagePath()); err == nil {
		_ = json.Unmarshal(b, f)
	}
	if f.Days == nil {
		f.Days = map[string]map[string]*Tally{}
	}
	if f.Prices == nil {
		f.Prices = map[string]Price{}
	}
	if !f.Set {
		f.Budget = 1000000 // a run uses at most a million tokens by default (CHANGELOG 1.7.12)
	}
	use.f, use.dir = f, core.DataDir
	return f
}

func useSave() {
	if b, err := json.Marshal(use.f); err == nil {
		_ = os.WriteFile(usagePath(), b, 0644)
	}
}

func priceOf(f *usageFile, key string) *Price {
	if p, ok := f.Prices[key]; ok {
		return &p
	}
	if p, ok := builtinPrices[key]; ok {
		if p.Peak > 1 && deepseekPeak(time.Now()) {
			p.In, p.Hit, p.Out = p.In*p.Peak, p.Hit*p.Peak, p.Out*p.Peak
		}
		return &p
	}
	return nil
}

// deepseekPeak: DeepSeek's busy hours, 09:00–24:00 Beijing time on weekdays, cost twice as much.
func deepseekPeak(t time.Time) bool {
	bj := t.In(time.FixedZone("CST", 8*3600))
	return bj.Weekday() != time.Saturday && bj.Weekday() != time.Sunday && bj.Hour() >= 9
}

// current: the provider and model a request was made with.
var currentModel = func(c *Client) string {
	cfg := LoadAIConfig()
	return cfg.Provider + "/" + c.Model
}

// usageProject: the project a request is for (set by the run that makes it).
var usageProject sync.Map // *Client → project

func noteUsage(c *Client, u Usage) {
	key := currentModel(c)
	day := time.Now().Format("2006-01-02")
	use.mu.Lock()
	f := useLoad()
	p := priceOf(f, key)
	if f.Days[day] == nil {
		f.Days[day] = map[string]*Tally{}
	}
	t := f.Days[day][key]
	if t == nil {
		t = &Tally{}
		f.Days[day][key] = t
	}
	t.add(u, p)
	if proj, ok := usageProject.Load(c); ok {
		if use.conv == nil {
			use.conv = map[string]*Tally{}
		}
		k := core.PathKey(proj.(string))
		if use.conv[k] == nil {
			use.conv[k] = &Tally{}
		}
		use.conv[k].add(u, p)
	}
	useSave()
	use.mu.Unlock()
	core.BumpRev()
}

// convTokens: what this project's conversation has used so far.
func convTokens(project string) int {
	use.mu.Lock()
	defer use.mu.Unlock()
	if t := use.conv[core.PathKey(project)]; t != nil {
		return t.Tokens
	}
	return 0
}

// budget: tokens one run may use (0: no limit).
func budget() int64 {
	use.mu.Lock()
	defer use.mu.Unlock()
	return useLoad().Budget
}

// noteRun keeps what a run used, for the estimate shown before the next one.
func noteRun(tokens int) {
	if tokens <= 0 {
		return
	}
	use.mu.Lock()
	f := useLoad()
	f.Runs = append(f.Runs, tokens)
	if len(f.Runs) > 10 {
		f.Runs = f.Runs[len(f.Runs)-10:]
	}
	useSave()
	use.mu.Unlock()
}

// sessionUsage: the pipeline's status line — this conversation, today, the estimate, DeepSeek's busy hours.
func sessionUsage(project string) map[string]any {
	use.mu.Lock()
	defer use.mu.Unlock()
	f := useLoad()
	var today Tally
	for _, t := range f.Days[time.Now().Format("2006-01-02")] {
		today.merge(*t)
	}
	conv := Tally{}
	if t := use.conv[core.PathKey(project)]; t != nil {
		conv = *t
	}
	out := map[string]any{"conv": conv, "today": today}
	if n := len(f.Runs); n > 0 {
		sum := 0
		for _, r := range f.Runs {
			sum += r
		}
		out["estimate"] = map[string]any{"tokens": sum / n, "runs": n}
	}
	if cfg := LoadAIConfig(); cfg.Provider == "deepseek" && deepseekPeak(time.Now()) {
		out["peak"] = map[string]any{"until": "24:00"}
	}
	return out
}

// usageReport: what the usage window shows — every model used, today, this month, since counting began.
func usageReport() map[string]any {
	use.mu.Lock()
	defer use.mu.Unlock()
	f := useLoad()
	now := time.Now()
	today, month := now.Format("2006-01-02"), now.Format("2006-01")
	type row struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Label    string `json:"label"`
		Today    Tally  `json:"today"`
		Month    Tally  `json:"month"`
		Total    Tally  `json:"total"`
		Price    *Price `json:"price,omitempty"`
	}
	by := map[string]*row{}
	var tToday, tMonth, tTotal Tally
	for day, models := range f.Days {
		for key, t := range models {
			r := by[key]
			if r == nil {
				prov, model := splitKey(key)
				label := prov
				if p, ok := providerOf(prov); ok {
					label = p.Label
				}
				r = &row{Provider: prov, Model: model, Label: label, Price: priceOf(f, key)}
				by[key] = r
			}
			r.Total.merge(*t)
			tTotal.merge(*t)
			if day == today {
				r.Today.merge(*t)
				tToday.merge(*t)
			}
			if len(day) >= 7 && day[:7] == month {
				r.Month.merge(*t)
				tMonth.merge(*t)
			}
		}
	}
	rows := make([]*row, 0, len(by))
	for _, r := range by {
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Total.Tokens > rows[j].Total.Tokens })
	cfg := LoadAIConfig()
	return map[string]any{"ok": true, "models": rows, "today": tToday, "month": tMonth, "total": tTotal,
		"budget": f.Budget, "provider": cfg.Provider, "balance": cfg.Provider == "deepseek" || cfg.Provider == "kimi"}
}

func splitKey(k string) (string, string) {
	for i := 0; i < len(k); i++ {
		if k[i] == '/' {
			return k[:i], k[i+1:]
		}
	}
	return k, ""
}

func setPrice(provider, model string, p *Price) {
	use.mu.Lock()
	f := useLoad()
	if p == nil {
		delete(f.Prices, provider+"/"+model)
	} else {
		f.Prices[provider+"/"+model] = *p
	}
	useSave()
	use.mu.Unlock()
}

func setBudget(tokens int64) {
	use.mu.Lock()
	f := useLoad()
	if tokens < 0 {
		tokens = 0
	}
	f.Budget, f.Set = tokens, true
	useSave()
	use.mu.Unlock()
}

func clearUsage() {
	use.mu.Lock()
	f := useLoad()
	f.Days, f.Runs = map[string]map[string]*Tally{}, nil
	use.conv = nil
	useSave()
	use.mu.Unlock()
}
