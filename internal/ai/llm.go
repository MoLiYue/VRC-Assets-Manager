package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"vrclib/internal/core"
)

// Talking to the AI service the player picked.
//
// Two wires: the OpenAI one (POST <base>/chat/completions, "Authorization: Bearer"), which DeepSeek, Qwen,
// Kimi and most others speak, and Anthropic's (POST <base>/v1/messages, "x-api-key"). Only text is sent
// here: the requests of the pipeline are one question and one answer each.
//
// A service that is busy (429) is asked again after the wait it names; one that failed (5xx) twice more. A
// conversation past the model's context, a wrong key and a model that is not there are said in words.

// Provider: one service the window offers.
type Provider struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Base  string `json:"base"`
	Model string `json:"model,omitempty"`
	Wire  string `json:"wire"` // openai, claude
	Note  string `json:"note,omitempty"`
}

// Providers: the services offered, in the order the window lists them.
var Providers = []Provider{
	{ID: "deepseek", Label: "DeepSeek", Base: "https://api.deepseek.com", Model: "deepseek-chat", Wire: "openai", Note: "DeepSeek 官方接口：工作日北京时间高峰时段价格为平时的 2 倍"},
	{ID: "qwen", Label: "通义千问", Base: "https://dashscope.aliyuncs.com/compatible-mode/v1", Model: "qwen-plus", Wire: "openai", Note: "阿里云百炼的 OpenAI 兼容接口"},
	{ID: "kimi", Label: "Kimi", Base: "https://api.moonshot.cn/v1", Model: "kimi-k2-0905-preview", Wire: "openai", Note: "月之暗面的 OpenAI 兼容接口"},
	{ID: "openai", Label: "ChatGPT（API）", Base: "https://api.openai.com/v1", Model: "gpt-4.1", Wire: "openai", Note: "OpenAI 的 API，按用量计费（不是 ChatGPT 套餐）"},
	{ID: "claude", Label: "Claude", Base: "https://api.anthropic.com", Model: "claude-sonnet-4-5", Wire: "claude", Note: "Anthropic 的 API"},
	{ID: "custom", Label: "其他（OpenAI 兼容）", Base: "", Wire: "openai", Note: "任何 OpenAI 兼容的接口，例如本地运行的模型"},
}

func providerOf(id string) (Provider, bool) {
	for _, p := range Providers {
		if p.ID == id {
			return p, true
		}
	}
	return Provider{}, false
}

// Msg: one message of a request.
type Msg struct {
	Role    string `json:"role"` // user, assistant
	Content string `json:"content"`
}

// Usage: what one request cost, in tokens.
type Usage struct {
	In, Out, Cached int
}

// Answer: the reply, and what it cost.
type Answer struct {
	Text  string
	Usage Usage
	Cut   bool // the reply ended at the output limit
}

// Client: one service, with its address, model and key.
type Client struct {
	Wire, Base, Model, Key string
	HTTP                   *http.Client
}

// clientFor: the service the player saved, ready to ask; an error says what is missing.
func clientFor(st *core.Store) (*Client, error) {
	cfg := LoadAIConfig()
	p, ok := providerOf(cfg.Provider)
	if !ok {
		return nil, errors.New("请先在「AI 服务」中选择服务商")
	}
	prof := cfg.Profiles[p.ID]
	if prof == nil || prof.Model == "" {
		return nil, errors.New("请先在「AI 服务」中选择模型")
	}
	base := prof.BaseURL
	if base == "" {
		base = p.Base
	}
	key := keyFor(p.ID, base)
	if key == "" {
		return nil, errors.New("请先在「AI 服务」中填写 API Key")
	}
	return &Client{Wire: p.Wire, Base: base, Model: prof.Model, Key: key, HTTP: core.HTTPClient(st)}, nil
}

// retryWait: how long to wait for a busy service (tests shorten it).
var retryWait = func(d time.Duration) time.Duration { return d }

// Chat asks one question with a system prompt and answers the reply.
func (c *Client) Chat(ctx context.Context, system string, msgs []Msg, maxOut int) (*Answer, error) {
	var last error
	for try := 0; try < 4; try++ {
		a, wait, err := c.once(ctx, system, msgs, maxOut)
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

// once: one request; wait > 0 when it is worth asking again after that long.
func (c *Client) once(ctx context.Context, system string, msgs []Msg, maxOut int) (*Answer, time.Duration, error) {
	if maxOut <= 0 {
		maxOut = 4096
	}
	var u string
	var body any
	if c.Wire == "claude" {
		u = strings.TrimRight(c.Base, "/")
		if !strings.HasSuffix(u, "/v1") {
			u += "/v1"
		}
		u += "/messages"
		body = map[string]any{"model": c.Model, "max_tokens": maxOut, "messages": msgs,
			// marked for the service's cache: the same system prompt is sent with every request of a run
			"system": []map[string]any{{"type": "text", "text": system, "cache_control": map[string]string{"type": "ephemeral"}}}}
	} else {
		u = strings.TrimRight(c.Base, "/")
		if !strings.Contains(u, "/v1") && !strings.Contains(u, "compatible-mode") {
			u += "/v1"
		}
		u += "/chat/completions"
		all := append([]Msg{{Role: "system", Content: system}}, msgs...)
		body = map[string]any{"model": c.Model, "messages": all, "max_tokens": maxOut, "stream": false}
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
	a := &Answer{}
	if c.Wire == "claude" {
		var r struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
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
			if bl.Type == "text" {
				a.Text += bl.Text
			}
		}
		a.Cut, a.Usage = r.Stop == "max_tokens", Usage{r.Usage.In, r.Usage.Out, r.Usage.Cached}
	} else {
		var r struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
				Finish string `json:"finish_reason"`
			} `json:"choices"`
			Usage struct {
				In      int `json:"prompt_tokens"`
				Out     int `json:"completion_tokens"`
				Details struct {
					Cached int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
				Hit int `json:"prompt_cache_hit_tokens"` // DeepSeek's own name for it
			} `json:"usage"`
		}
		if json.Unmarshal(raw, &r) != nil || len(r.Choices) == 0 {
			return nil, 0, errors.New("AI 服务的回应无法读取")
		}
		a.Text, a.Cut = r.Choices[0].Message.Content, r.Choices[0].Finish == "length"
		a.Usage = Usage{r.Usage.In, r.Usage.Out, max(r.Usage.Details.Cached, r.Usage.Hit)}
	}
	noteUsage(c, a.Usage)
	return a, 0, nil
}

func retryAfter(resp *http.Response) time.Duration {
	switch {
	case resp.StatusCode == 429:
		if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && n > 0 && n < 120 {
			return time.Duration(n) * time.Second
		}
		return 5 * time.Second
	case resp.StatusCode >= 500:
		return 3 * time.Second
	}
	return 0
}

// serviceErr: what an error answer of the service means for the player.
func serviceErr(status int, raw []byte) error {
	var r struct {
		Error struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &r)
	msg := strings.TrimSpace(r.Error.Message)
	low := strings.ToLower(msg + " " + fmt.Sprint(r.Error.Code))
	switch {
	case status == 401 || status == 403:
		return errors.New("API Key 无效或没有权限，请在「AI 服务」中检查（服务商：" + msg + "）")
	case strings.Contains(low, "context") && (strings.Contains(low, "length") || strings.Contains(low, "maximum")):
		return errors.New("请求超出了模型的上下文长度，请换用上下文更长的模型，或减少一次装配的素材")
	case status == 404 || strings.Contains(low, "does not exist") || strings.Contains(low, "model_not_found"):
		return errors.New("服务商没有这个模型，请在「AI 服务」中重新选择模型")
	case status == 429:
		return errors.New("AI 服务暂时繁忙（请求过多或额度不足），请稍后重试")
	case status >= 500:
		return errors.New("AI 服务出错（HTTP " + strconv.Itoa(status) + "），请稍后重试")
	}
	if msg == "" {
		msg = "HTTP " + strconv.Itoa(status)
	}
	return errors.New("AI 服务返回错误：" + msg)
}
