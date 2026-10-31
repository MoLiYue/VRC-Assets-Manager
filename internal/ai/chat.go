package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"vrclib/internal/core"
	"vrclib/internal/unity"
)

// The conversation (CHANGELOG 1.7.11, 1.7.12, 1.7.14).
//
// The player writes what is wanted; the AI does it with tools — the pipeline plugin's (look at the avatar,
// list prefabs, dress, place, build the menu, take a picture, undo) and, through UnitySkills, the editor's
// own. What it says and does is the log; what the player is to decide is asked there:
//
//   - a UnitySkills operation that changes something is asked about; 「本次对话始终允许」 lets that one
//     operation go without asking for the rest of the conversation. Deleting, saving the scene, writing a
//     script, running a menu item or code is asked about every time.
//   - a run that has used its tokens (the budget in 「用量」) asks whether to go on.
//
// A run stops by itself when it goes round in circles: the same failing operation a third time, eight
// failures in a row, or the very same operation four times in a row.

const chatSystem = `你是 MioVRCA new 的 VRChat 改模助手，在玩家打开的 Unity 工程里操作。
- 先用 inspect_avatar 看清场景，再动手；不确定物体路径时用 inspect_object 查。
- 穿衣服用 dress，放道具或插件用 place_prefab，菜单用 build_menu，拍照确认用 look。
- 这些工具不够用时，先用 unity_find_skills 找到合适的 UnitySkills 操作，再用 unity_skill 执行。
- 每一步都可以 Ctrl+Z 撤销，场景不会自动保存；不要替玩家保存场景。
- 做完后用简短的要点说明做了什么、还需要玩家确认什么。`

// tools the AI is offered.
var chatTools = []struct {
	Name, Desc string
	Params     map[string]any
}{
	{"inspect_avatar", "查看场景中的模型：它的子物体、菜单和参数", sObj(nil, nil)},
	{"inspect_object", "查看模型下一个物体的详细信息（网格、材质、组件）", sObj(map[string]any{"path": sStr("物体路径，相对于模型")}, []string{"path"})},
	{"list_prefabs", "列出工程中这些文件夹里的 prefab", sObj(map[string]any{"folders": sArr(sStr("Assets/ 下的文件夹"))}, []string{"folders"})},
	{"dress", "把一件衣服（或头发、配饰）的 prefab 穿到模型上（Modular Avatar Setup Outfit）", sObj(map[string]any{"prefab": sStr("prefab 路径"), "name": sStr("可选：物体名称")}, []string{"prefab"})},
	{"place_prefab", "把道具或插件的 prefab 原样放到模型下", sObj(map[string]any{"prefab": sStr("prefab 路径")}, []string{"prefab"})},
	{"build_menu", "按方案生成或更新模型的菜单", sObj(map[string]any{"items": sArr(map[string]any{"type": "object"})}, []string{"items"})},
	{"look", "从几个角度给模型拍照，确认效果", sObj(map[string]any{"views": sArr(sStr("front、back、left、right 等")), "question": sStr("想确认什么")}, nil)},
	{"undo", "撤销本插件做的上一步", sObj(nil, nil)},
	{"unity_find_skills", "按用途查找可用的 UnitySkills 操作", sObj(map[string]any{"intent": sStr("想做什么，英文")}, []string{"intent"})},
	{"unity_skill", "执行一个 UnitySkills 操作", sObj(map[string]any{"name": sStr("操作名"), "args": map[string]any{"type": "object"}}, []string{"name"})},
}

func sObj(props map[string]any, req []string) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	m := map[string]any{"type": "object", "properties": props}
	if len(req) > 0 {
		m["required"] = req
	}
	return m
}
func sStr(desc string) map[string]any         { return map[string]any{"type": "string", "description": desc} }
func sArr(item map[string]any) map[string]any { return map[string]any{"type": "array", "items": item} }

// ---------- the conversation, in neither wire's terms ----------

type call struct {
	ID   string
	Name string
	Args json.RawMessage
}

type turn struct {
	Role    string // user, assistant, results
	Text    string
	Calls   []call
	Results []result
}

type result struct {
	ID, Text string
}

type convState struct {
	turns   []turn
	allowed map[string]bool // UnitySkills operations let go for this conversation
	avatar  string
}

var convs struct {
	mu sync.Mutex
	m  map[string]*convState
}

func convOf(project string) *convState {
	convs.mu.Lock()
	defer convs.mu.Unlock()
	if convs.m == nil {
		convs.m = map[string]*convState{}
	}
	k := core.PathKey(project)
	if convs.m[k] == nil {
		convs.m[k] = &convState{allowed: map[string]bool{}}
	}
	return convs.m[k]
}

func dropConv(project string) {
	convs.mu.Lock()
	delete(convs.m, core.PathKey(project))
	convs.mu.Unlock()
}

// ---------- the wires, with tools ----------

type toolAnswer struct {
	Text  string
	Calls []call
	Cut   bool
}

// ChatTools asks with the conversation so far and the tools.
func (c *Client) ChatTools(ctx context.Context, system string, turns []turn) (*toolAnswer, error) {
	var last error
	for try := 0; try < 4; try++ {
		a, wait, err := c.toolsOnce(ctx, system, turns)
		if err == nil {
			return a, nil
		}
		last = err
		if wait <= 0 || ctx.Err() != nil {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(retryWait(wait)):
		}
	}
	return nil, last
}

func (c *Client) toolsOnce(ctx context.Context, system string, turns []turn) (*toolAnswer, time.Duration, error) {
	var u string
	var body map[string]any
	if c.Wire == "claude" {
		u = strings.TrimRight(c.Base, "/")
		if !strings.HasSuffix(u, "/v1") {
			u += "/v1"
		}
		u += "/messages"
		var tools []any
		for _, t := range chatTools {
			tools = append(tools, map[string]any{"name": t.Name, "description": t.Desc, "input_schema": t.Params})
		}
		var msgs []any
		for _, t := range turns {
			switch t.Role {
			case "user":
				msgs = append(msgs, map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": t.Text}}})
			case "assistant":
				var bl []any
				if t.Text != "" {
					bl = append(bl, map[string]any{"type": "text", "text": t.Text})
				}
				for _, cl := range t.Calls {
					in := json.RawMessage(cl.Args)
					if len(in) == 0 || !json.Valid(in) {
						in = json.RawMessage("{}")
					}
					bl = append(bl, map[string]any{"type": "tool_use", "id": cl.ID, "name": cl.Name, "input": in})
				}
				msgs = append(msgs, map[string]any{"role": "assistant", "content": bl})
			case "results":
				var bl []any
				for _, r := range t.Results {
					bl = append(bl, map[string]any{"type": "tool_result", "tool_use_id": r.ID, "content": r.Text})
				}
				msgs = append(msgs, map[string]any{"role": "user", "content": bl})
			}
		}
		body = map[string]any{"model": c.Model, "max_tokens": 8192, "tools": tools, "messages": msgs,
			"system": []map[string]any{{"type": "text", "text": system, "cache_control": map[string]string{"type": "ephemeral"}}}}
	} else {
		u = strings.TrimRight(c.Base, "/")
		if !strings.Contains(u, "/v1") && !strings.Contains(u, "compatible-mode") {
			u += "/v1"
		}
		u += "/chat/completions"
		var tools []any
		for _, t := range chatTools {
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": t.Name, "description": t.Desc, "parameters": t.Params}})
		}
		msgs := []any{map[string]any{"role": "system", "content": system}}
		for _, t := range turns {
			switch t.Role {
			case "user":
				msgs = append(msgs, map[string]any{"role": "user", "content": t.Text})
			case "assistant":
				m := map[string]any{"role": "assistant", "content": t.Text}
				var tc []any
				for _, cl := range t.Calls {
					tc = append(tc, map[string]any{"id": cl.ID, "type": "function", "function": map[string]any{"name": cl.Name, "arguments": string(cl.Args)}})
				}
				if len(tc) > 0 {
					m["tool_calls"] = tc
				}
				msgs = append(msgs, m)
			case "results":
				for _, r := range t.Results {
					msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": r.ID, "content": r.Text})
				}
			}
		}
		body = map[string]any{"model": c.Model, "messages": msgs, "tools": tools, "max_tokens": 8192, "stream": false}
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(b))
	if err != nil {
		return nil, 0, errors.New("服务地址无效")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", core.UA)
	if c.Wire == "claude" {
		req.Header.Set("x-api-key", c.Key)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+c.Key)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		return nil, 2 * time.Second, errors.New("无法连接 AI 服务：" + core.TrimErr(err))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != 200 {
		return nil, retryAfter(resp), serviceErr(resp.StatusCode, raw)
	}
	a := &toolAnswer{}
	var use Usage
	if c.Wire == "claude" {
		var r struct {
			Content []struct {
				Type  string          `json:"type"`
				Text  string          `json:"text"`
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
			Stop  string `json:"stop_reason"`
			Usage struct {
				In     int `json:"input_tokens"`
				Out    int `json:"output_tokens"`
				Cached int `json:"cache_read_input_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(raw, &r) != nil {
			return nil, 0, errors.New("AI 服务的回应无法读取")
		}
		for _, bl := range r.Content {
			switch bl.Type {
			case "text":
				a.Text += bl.Text
			case "tool_use":
				a.Calls = append(a.Calls, call{ID: bl.ID, Name: bl.Name, Args: bl.Input})
			}
		}
		a.Cut = r.Stop == "max_tokens"
		use = Usage{r.Usage.In, r.Usage.Out, r.Usage.Cached}
	} else {
		var r struct {
			Choices []struct {
				Message struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"message"`
				Finish string `json:"finish_reason"`
			} `json:"choices"`
			Usage struct {
				In  int `json:"prompt_tokens"`
				Out int `json:"completion_tokens"`
				Hit int `json:"prompt_cache_hit_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(raw, &r) != nil || len(r.Choices) == 0 {
			return nil, 0, errors.New("AI 服务的回应无法读取")
		}
		m := r.Choices[0].Message
		a.Text, a.Cut = m.Content, r.Choices[0].Finish == "length"
		for _, tc := range m.ToolCalls {
			a.Calls = append(a.Calls, call{ID: tc.ID, Name: tc.Function.Name, Args: json.RawMessage(tc.Function.Arguments)})
		}
		use = Usage{r.Usage.In, r.Usage.Out, r.Usage.Hit}
	}
	noteUsage(c, use)
	return a, 0, nil
}

// ---------- asking the player ----------

var asks struct {
	mu sync.Mutex
	m  map[string]chan [2]bool // project → (ok, all)
}

// ask shows a question in the log and waits for the player's answer.
func ask(ctx context.Context, project, text, opts string) (ok, all bool, err error) {
	ch := make(chan [2]bool, 1)
	asks.mu.Lock()
	if asks.m == nil {
		asks.m = map[string]chan [2]bool{}
	}
	asks.m[core.PathKey(project)] = ch
	asks.mu.Unlock()
	defer func() {
		asks.mu.Lock()
		delete(asks.m, core.PathKey(project))
		asks.mu.Unlock()
	}()
	i := addStep(project, Step{Kind: "ask", Text: text, Busy: true, Opts: opts})
	core.Notify(core.Notice{Kind: "ask", What: project})
	select {
	case a := <-ch:
		out := "已拒绝"
		switch {
		case a[0] && a[1]:
			out = "本次对话始终允许"
		case a[0]:
			out = "已允许"
		}
		setStep(project, i, func(s *Step) { s.Busy, s.Out = false, out })
		return a[0], a[1], nil
	case <-ctx.Done():
		setStep(project, i, func(s *Step) { s.Busy, s.Out = false, "已停止" })
		return false, false, ctx.Err()
	}
}

// AnswerAsk: the player's answer to what the log asks.
func AnswerAsk(project string, ok, all bool) error {
	asks.mu.Lock()
	ch := asks.m[core.PathKey(project)]
	asks.mu.Unlock()
	if ch == nil {
		return errors.New("当前没有需要回答的问题")
	}
	select {
	case ch <- [2]bool{ok, all}:
		return nil
	default:
		return errors.New("已经回答过了")
	}
}

// skillRisk: how a UnitySkills operation is asked about — "" not at all (it only reads), "ask" once for the
// conversation, "once" every time (it deletes, saves, writes a script, runs a menu item or code). The name
// is compared without its case: an AI writes "Asset_Delete" as well as "asset_delete" (CHANGELOG 1.7.14).
func skillRisk(name string) string {
	n := strings.ToLower(name)
	for _, w := range []string{"delete", "remove", "destroy", "save", "script", "execute", "menu_item", "menuitem", "eval", "code", "run_"} {
		if strings.Contains(n, w) {
			return "once"
		}
	}
	for _, w := range []string{"get", "find", "list", "inspect", "query", "info", "search", "describe", "health"} {
		if strings.HasPrefix(n, w) || strings.Contains(n, "_"+w) {
			return ""
		}
	}
	return "ask"
}

// ---------- a turn of the conversation ----------

// loopGuard: how a run goes round in circles.
type loopGuard struct {
	failed     map[string]int
	failsInRow int
	last       string
	same       int
}

func (g *loopGuard) note(sig string, ok bool) string {
	if g.failed == nil {
		g.failed = map[string]int{}
	}
	if sig == g.last {
		g.same++
	} else {
		g.last, g.same = sig, 1
	}
	if ok {
		g.failsInRow = 0
	} else {
		g.failsInRow++
		g.failed[sig]++
	}
	switch {
	case !ok && g.failed[sig] >= 3:
		return "同一个操作第三次失败"
	case g.failsInRow >= 8:
		return "连续 8 步都失败"
	case g.same >= 4:
		return "连续 4 次执行完全相同的操作"
	}
	return ""
}

// runChat runs the conversation from what the player wrote until the AI has nothing more to do.
func runChat(ctx context.Context, st *core.Store, project, text string) error {
	c, err := clientFor(st)
	if err != nil {
		return err
	}
	usageProject.Store(c, project)
	defer usageProject.Delete(c)
	before := convTokens(project)
	defer func() { noteRun(convTokens(project) - before) }()
	cv := convOf(project)
	addStep(project, Step{Kind: "user", Text: text})
	cv.turns = append(cv.turns, turn{Role: "user", Text: text})
	var guard loopGuard
	limit := budget() // what this run may use; going on past it allows as much again, for this run only
	for round := 0; round < 200; round++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if used := int64(convTokens(project) - before); limit > 0 && used >= limit {
			ok, _, err := ask(ctx, project, fmt.Sprintf("本次运行已用 %d tokens，达到用量上限。要继续吗？", used), "budget")
			if err != nil || !ok {
				addStep(project, Step{Kind: "info", Text: "已在用量上限处停止"})
				return err
			}
			limit += budget()
		}
		a, err := c.ChatTools(ctx, chatSystem, cv.turns)
		if err != nil {
			return err
		}
		if a.Cut && len(a.Calls) > 0 {
			// the arguments stopped half way: told, and asked again for less at a time
			cv.turns = append(cv.turns, turn{Role: "assistant", Text: a.Text, Calls: a.Calls})
			var rs []result
			for _, cl := range a.Calls {
				rs = append(rs, result{ID: cl.ID, Text: "错误：这次调用的参数超出了输出长度上限而被截断。请分几次完成，每次少放一些内容。"})
			}
			cv.turns = append(cv.turns, turn{Role: "results", Results: rs})
			addStep(project, Step{Kind: "error", Text: "AI 的回答超出了输出长度上限，已让它分几次完成"})
			if why := guard.note("cut", false); why != "" {
				addStep(project, Step{Kind: "info", Text: "已暂停：" + why})
				return nil
			}
			continue
		}
		if t := strings.TrimSpace(a.Text); t != "" {
			addStep(project, Step{Kind: "say", Text: t})
		}
		cv.turns = append(cv.turns, turn{Role: "assistant", Text: a.Text, Calls: a.Calls})
		if len(a.Calls) == 0 {
			return nil
		}
		var rs []result
		for _, cl := range a.Calls {
			out, ok := doTool(ctx, st, project, cv, cl)
			rs = append(rs, result{ID: cl.ID, Text: out})
			if why := guard.note(cl.Name+string(cl.Args), ok); why != "" {
				cv.turns = append(cv.turns, turn{Role: "results", Results: rs})
				addStep(project, Step{Kind: "info", Text: "已暂停：" + why + "。可以补充说明后继续"})
				return nil
			}
		}
		cv.turns = append(cv.turns, turn{Role: "results", Results: rs})
	}
	return errors.New("对话步数过多，已停止")
}

// doTool runs one tool the AI asked for, as a line of the log, and answers what the AI is told.
func doTool(ctx context.Context, st *core.Store, project string, cv *convState, cl call) (string, bool) {
	var args map[string]any
	if len(cl.Args) > 0 && json.Unmarshal(cl.Args, &args) != nil {
		addStep(project, Step{Kind: "tool", Text: cl.Name, Out: "参数无法读取"})
		return "错误：参数不是有效的 JSON", false
	}
	if args == nil {
		args = map[string]any{}
	}
	sval := func(k string) string { s, _ := args[k].(string); return s }
	avatar := func() (string, error) {
		if cv.avatar != "" {
			return cv.avatar, nil
		}
		o, err := inspect(ctx, project)
		if err != nil {
			return "", err
		}
		if len(o.Avatars) == 0 {
			return "", errors.New("场景中没有模型")
		}
		cv.avatar = firstOf(o.Avatars[0].Path, o.Avatars[0].Name)
		return cv.avatar, nil
	}
	bridge := func(text, cmd string, a map[string]any, needAvatar bool) (string, bool) {
		if needAvatar {
			av, err := avatar()
			if err != nil {
				addStep(project, Step{Kind: "tool", Text: text, Out: err.Error()})
				return "错误：" + err.Error(), false
			}
			a["avatar"] = av
		}
		raw, err := tool(ctx, project, text, cmd, a, 3*time.Minute, outNote)
		if err != nil {
			return "错误：" + core.TrimErr(err), false
		}
		switch cmd {
		case "dress", "place", "build_menu":
			markChanged(project)
			noteBase(project, path.Base(cv.avatar))
		}
		pf, _ := a["prefab"].(string)
		switch cmd {
		case "dress":
			noteDone(project, doneStep{Cmd: "dress", Asset: RecipeAsset{Name: baseName(pf), Prefab: pf, Object: nameOf(raw, baseName(pf)), Parts: meshesOf(raw)}})
		case "place":
			noteDone(project, doneStep{Cmd: "place", Asset: RecipeAsset{Name: baseName(pf), Prefab: pf, Object: objectOf(raw, baseName(pf))}})
		case "build_menu":
			var items []menuItem
			if b, err := json.Marshal(a["items"]); err == nil {
				_ = json.Unmarshal(b, &items)
			}
			noteDone(project, doneStep{Cmd: "menu", Items: items})
		}
		return clip(string(raw), 8000), true
	}
	switch cl.Name {
	case "inspect_avatar":
		out, ok := bridge("查看模型", "inspect", map[string]any{}, false)
		if ok {
			var o overview
			if json.Unmarshal([]byte(out), &o) == nil && len(o.Avatars) > 0 {
				cv.avatar = firstOf(o.Avatars[0].Path, o.Avatars[0].Name)
			}
		}
		return out, ok
	case "inspect_object":
		return bridge("查看 "+sval("path"), "inspect_object", map[string]any{"path": sval("path")}, true)
	case "list_prefabs":
		return bridge("列出 prefab", "prefabs", map[string]any{"folders": args["folders"], "limit": 60}, false)
	case "dress":
		a := map[string]any{"prefab": sval("prefab")}
		if n := sval("name"); n != "" {
			a["name"] = n
		}
		return bridge("装配 "+baseName(sval("prefab")), "dress", a, true)
	case "place_prefab":
		return bridge("放置 "+baseName(sval("prefab")), "place", map[string]any{"prefab": sval("prefab")}, true)
	case "build_menu":
		a := map[string]any{"items": args["items"], "icons": true}
		return bridge("生成菜单", "build_menu", a, true)
	case "look":
		av, err := avatar()
		if err != nil {
			return "错误：" + err.Error(), false
		}
		views := args["views"]
		if views == nil {
			views = []string{"front"}
		}
		raw, err := tool(ctx, project, "截图", "snapshot", map[string]any{"avatar": av, "views": views}, 2*time.Minute, nil)
		if err != nil {
			return "错误：" + core.TrimErr(err), false
		}
		if imgs := keepShots(raw); len(imgs) > 0 {
			addStep(project, Step{Kind: "info", Text: "截图", Imgs: imgs})
		}
		return "截图已拍好，显示在玩家的记录里；这次没有看到画面（截图没有附给你）。请让玩家自己看一眼，不要描述画面。", true
	case "undo":
		ctx2, cancel := context.WithTimeout(ctx, 40*time.Second)
		defer cancel()
		u, err := UndoLast(ctx2, project)
		if err != nil {
			return "错误：" + core.TrimErr(err), false
		}
		return "已撤销：" + u, true
	case "unity_find_skills":
		i := addStep(project, Step{Kind: "tool", Text: "查找 UnitySkills 操作", Busy: true})
		out, err := unity.SkillsFind(ctx, project, sval("intent"), 6)
		if err != nil {
			setStep(project, i, func(s *Step) { s.Busy, s.Out = false, core.TrimErr(err) })
			return "错误：" + core.TrimErr(err), false
		}
		setStep(project, i, func(s *Step) { s.Busy, s.OK = false, true })
		return clip(out, 8000), true
	case "unity_skill":
		name := sval("name")
		sargs, _ := args["args"].(map[string]any)
		key := strings.ToLower(name)
		switch risk := skillRisk(name); {
		case risk == "once" || (risk == "ask" && !cv.allowed[key]):
			opts := ""
			if risk == "once" {
				opts = "once"
			}
			b, _ := json.Marshal(sargs)
			ok, all, err := ask(ctx, project, "AI 想执行 UnitySkills 操作「"+name+"」："+clip(string(b), 300), opts)
			if err != nil {
				return "错误：已停止", false
			}
			if !ok {
				return "玩家拒绝了这个操作。不要再尝试它，换一种方式或询问玩家。", false
			}
			if all && risk == "ask" {
				cv.allowed[key] = true
			}
		}
		i := addStep(project, Step{Kind: "tool", Text: "UnitySkills：" + name, Busy: true})
		unity.WantSkills(project, unity.SkillsLease)
		out, ok, err := unity.SkillsCall(ctx, project, name, sargs)
		if err != nil {
			setStep(project, i, func(s *Step) { s.Busy, s.Out = false, core.TrimErr(err) })
			return "错误：" + core.TrimErr(err), false
		}
		setStep(project, i, func(s *Step) { s.Busy, s.OK = false, ok })
		if skillRisk(name) != "" {
			markChanged(project)
			noteDone(project, doneStep{Cmd: "skill"})
		}
		return clip(out, 8000), ok
	}
	addStep(project, Step{Kind: "tool", Text: cl.Name, Out: "没有这个工具"})
	return "错误：没有这个工具：" + cl.Name, false
}

func baseName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		p = p[i+1:]
	}
	return strings.TrimSuffix(p, ".prefab")
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…（已截断）"
	}
	return s
}
