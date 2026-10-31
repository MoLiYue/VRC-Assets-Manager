package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
)

// The AI's part of a run (CHANGELOG 1.7.12): one request for the menus of what was put on, checked by the
// program and sent back once with what is wrong; when it is still wrong, the draft the rules made is used.
// Then one request for a word on the result — the pictures and the check, in the player's terms.
//
// What the AI is given for each outfit: its kind, the place in the menu it goes, the object in the scene,
// its meshes, the other colours of it (prefabs of the same folder), and the draft. What it answers is the
// same items with names a player would give them: parts named, colours as choices, sub menus named.

const menuSystem = `你是 MioVRCA new 流水线的菜单整理助手。程序已经把素材穿到了 VRChat 模型上，并按规则拟好了菜单草案。
请只整理菜单：给子菜单、部件开关、配色起玩家看得懂的名字，合并同一件衣服的配色，去掉没有意义的部件开关。
规则：
- 只能使用给出的物体路径（objects）和 prefab 路径（materialsFrom），不要编造。
- kind 只能是 outfit、part、toggle、strip、skin、install。
- 同一页（同一个 path）最多 8 项，同一页里不能重名。
- 不要改变每件衣服所在的分类（path 的第一段）。
只回答一个 JSON 代码块：{"items": [...]}，不要别的文字。`

const reviewSystem = `程序刚在玩家的 Unity 工程里完成了一次装配：放入素材、生成菜单、截图并做了上传前体检。
请用两三条简短的要点告诉玩家结果：看到的问题（穿模、错位、粉色材质）和体检中需要处理的项目；没有问题就直说。
不要重复程序已经列出的步骤，也不要让玩家去做程序已经做完的事。`

// outfitInfo: one outfit as the AI is told of it.
type outfitInfo struct {
	Kind     string     `json:"kind"`
	Category []string   `json:"category"`
	Name     string     `json:"name"`
	Object   string     `json:"object"`
	Meshes   []string   `json:"meshes,omitempty"`
	Colours  []string   `json:"colours,omitempty"` // other prefabs of the same outfit (materialsFrom)
	Draft    []menuItem `json:"draft"`
}

// outfitDraft: what the rules make of one outfit for the AI — its choice and its parts, in a sub menu of
// its own; the colours as choices of the same outfit.
func outfitDraft(o *outfitInfo, first bool) {
	sub := append(append([]string{}, o.Category...), o.Name)
	param := ""
	if o.Kind == "头发" {
		param = "Hair_Choose"
	}
	o.Draft = append(o.Draft, menuItem{Kind: "outfit", Path: sub, Label: o.Name, Objects: []string{o.Object}, Default: first, Parameter: param})
	for _, c := range o.Colours {
		o.Draft = append(o.Draft, menuItem{Kind: "outfit", Path: sub, Label: strings.TrimSuffix(path.Base(c), ".prefab"),
			Objects: []string{o.Object}, MaterialsFrom: c, Parameter: param})
	}
	for _, m := range o.Meshes {
		if m == o.Object || len(o.Meshes) < 2 {
			continue
		}
		o.Draft = append(o.Draft, menuItem{Kind: "part", Path: sub, Label: path.Base(m), Objects: []string{m}})
	}
}

var reJSONBlock = regexp.MustCompile("(?s)```(?:json)?\\s*(\\{.*?\\})\\s*```")

// parseItems reads the items out of an answer: a JSON block, or the answer itself when it is JSON.
func parseItems(text string) ([]menuItem, error) {
	s := strings.TrimSpace(text)
	if m := reJSONBlock.FindStringSubmatch(s); m != nil {
		s = m[1]
	} else if i, j := strings.Index(s, "{"), strings.LastIndex(s, "}"); i >= 0 && j > i {
		s = s[i : j+1]
	}
	var r struct {
		Items []menuItem `json:"items"`
	}
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return nil, errors.New("回答不是有效的 JSON")
	}
	if len(r.Items) == 0 {
		return nil, errors.New("回答中没有菜单项")
	}
	return r.Items, nil
}

// checkItems: what is wrong with a plan — objects and prefabs that are not there, kinds that are not, more
// than 8 on a page, two of one name on a page, an outfit moved to another category.
func checkItems(items []menuItem, outfits []outfitInfo) []string {
	objects, prefabs, cats := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, o := range outfits {
		objects[o.Object] = true
		for _, m := range o.Meshes {
			objects[m] = true
		}
		for _, c := range o.Colours {
			prefabs[c] = true
		}
		if len(o.Category) > 0 {
			cats[o.Category[0]] = true
		}
		for _, d := range o.Draft {
			for _, x := range d.Objects {
				objects[x] = true
			}
		}
	}
	kinds := map[string]bool{"outfit": true, "part": true, "toggle": true, "strip": true, "skin": true, "install": true}
	var bad []string
	perPage, names := map[string]int{}, map[string]bool{}
	for i, it := range items {
		where := fmt.Sprintf("第 %d 项「%s」", i+1, it.Label)
		if !kinds[it.Kind] {
			bad = append(bad, where+"：kind「"+it.Kind+"」不存在")
		}
		if strings.TrimSpace(it.Label) == "" {
			bad = append(bad, where+"：缺少名字")
		}
		if len(it.Path) == 0 {
			bad = append(bad, where+"：缺少 path")
		} else if !cats[it.Path[0]] {
			bad = append(bad, where+"：分类「"+it.Path[0]+"」不是给出的分类之一")
		}
		for _, x := range it.Objects {
			if !objects[x] {
				bad = append(bad, where+"：物体「"+x+"」不存在")
			}
		}
		if it.MaterialsFrom != "" && !prefabs[it.MaterialsFrom] {
			bad = append(bad, where+"：materialsFrom「"+it.MaterialsFrom+"」不是给出的 prefab")
		}
		page := strings.Join(it.Path, "/")
		perPage[page]++
		key := page + "|" + it.Label
		if names[key] {
			bad = append(bad, where+"：与同一页的另一项重名")
		}
		names[key] = true
	}
	for page, n := range perPage {
		if n > 8 {
			bad = append(bad, fmt.Sprintf("「%s」一页有 %d 项，超过 8 项", page, n))
		}
	}
	return bad
}

// arrangeMenu asks the AI for the menus of these outfits and answers the plan to build — the AI's, checked,
// or the draft — and how it went, for the log.
func arrangeMenu(ctx context.Context, project string, c *Client, outfits []outfitInfo, wish ...string) ([]menuItem, string) {
	var draft []menuItem
	for _, o := range outfits {
		draft = append(draft, o.Draft...)
	}
	data, _ := json.Marshal(map[string]any{"outfits": outfits})
	question := "请整理下面这些衣服和头发的菜单。\n"
	if len(wish) > 0 && strings.TrimSpace(wish[0]) != "" {
		question += "玩家希望的菜单层级：" + strings.TrimSpace(wish[0]) + "\n"
	}
	question += string(data)
	msgs := []Msg{{Role: "user", Content: question}}
	for round := 0; round < 2; round++ {
		if over, used := overBudget(project); over {
			return draft, fmt.Sprintf("已达到本次运行的用量上限（%d tokens），按草案生成", used)
		}
		i := addStep(project, Step{Kind: "tool", Text: map[int]string{0: "AI 整理菜单", 1: "AI 修改菜单方案"}[round], Busy: true})
		ctx2, cancel := context.WithTimeout(ctx, 10*time.Minute)
		a, err := c.Chat(ctx2, menuSystem, msgs, 8192)
		cancel()
		if err != nil {
			setStep(project, i, func(s *Step) { s.Busy, s.Out = false, core0(err) })
			return draft, "AI 请求失败，按草案生成"
		}
		if a.Cut {
			setStep(project, i, func(s *Step) { s.Busy, s.Out = false, "回答超出了输出长度上限" })
			return draft, "AI 的回答被截断，按草案生成"
		}
		items, perr := parseItems(a.Text)
		var bad []string
		if perr != nil {
			bad = []string{perr.Error()}
		} else {
			bad = checkItems(items, outfits)
		}
		if len(bad) == 0 {
			setStep(project, i, func(s *Step) {
				s.Busy, s.OK, s.Out = false, true, fmt.Sprintf("方案通过校验（%d 项）", len(items))
			})
			return items, ""
		}
		setStep(project, i, func(s *Step) { s.Busy, s.Out = false, "方案有问题："+strings.Join(first(bad, 3), "；") })
		msgs = append(msgs, Msg{Role: "assistant", Content: a.Text},
			Msg{Role: "user", Content: "方案有以下问题，请改正后重新给出完整的 JSON：\n- " + strings.Join(first(bad, 20), "\n- ")})
	}
	return draft, "AI 的方案两次都未通过校验，按草案生成"
}

// review asks the AI for a word on the result.
func review(ctx context.Context, project string, c *Client, summary string) {
	if over, _ := overBudget(project); over {
		return
	}
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	a, err := c.Chat(ctx2, reviewSystem, []Msg{{Role: "user", Content: summary}}, 1024)
	if err != nil {
		addStep(project, Step{Kind: "error", Text: "AI 点评失败：" + core0(err)})
		return
	}
	if t := strings.TrimSpace(a.Text); t != "" {
		addStep(project, Step{Kind: "say", Text: t})
	}
}

// overBudget: this project's conversation has used the tokens one run may.
func overBudget(project string) (bool, int) {
	b := budget()
	used := convTokens(project)
	return b > 0 && int64(used) >= b, used
}

func first(l []string, n int) []string {
	if len(l) > n {
		return l[:n]
	}
	return l
}

func core0(err error) string {
	s := err.Error()
	if r := []rune(s); len(r) > 300 {
		s = string(r[:300]) + "…"
	}
	return s
}
