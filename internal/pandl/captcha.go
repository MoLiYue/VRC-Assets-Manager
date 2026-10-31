package pandl

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"vrclib/internal/core"
	"vrclib/internal/netdisk"
)

// The captcha Baidu may ask for before it takes a share's extraction code. The download stops at stage
// "captcha" with the picture (as a data: address, so that the window shows it without asking Baidu itself),
// the window shows it, and what the player reads comes back through AnswerCaptcha. A wrong one gets another
// picture; "看不清" asks for one too.

const captchaWait = 15 * time.Minute

var caps struct {
	mu sync.Mutex
	m  map[string]chan string // the download's key → where the answer goes
}

// asker: what a download asks the player with, for the share of this key.
func asker(id int64, key string) netdisk.CaptchaAsker {
	return func(ctx context.Context, img []byte, bad bool) (string, error) {
		ch := make(chan string, 1)
		caps.mu.Lock()
		if caps.m == nil {
			caps.m = map[string]chan string{}
		}
		caps.m[key] = ch
		caps.mu.Unlock()
		defer func() {
			caps.mu.Lock()
			if caps.m[key] == ch {
				delete(caps.m, key)
			}
			caps.mu.Unlock()
		}()
		pic := "data:" + http.DetectContentType(img) + ";base64," + base64.StdEncoding.EncodeToString(img)
		setJob(id, func(j *PanJob) { j.Stage, j.Captcha, j.CaptchaN, j.CaptchaBad = "captcha", pic, j.CaptchaN+1, bad })
		core.Notify(core.Notice{Kind: "captcha", What: key})
		select {
		case code := <-ch:
			setJob(id, func(j *PanJob) { j.Stage, j.Captcha, j.Msg = "fetch", "", "正在验证" })
			return code, nil
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(captchaWait):
			return "", errors.New("等待输入验证码超时")
		}
	}
}

// AnswerCaptcha: what the player read in the captcha Baidu asked for (refresh: another picture instead).
func AnswerCaptcha(key, code string, refresh bool) error {
	code = strings.TrimSpace(code)
	if !refresh && code == "" {
		return errors.New("请输入验证码")
	}
	if refresh {
		code = ""
	}
	caps.mu.Lock()
	ch := caps.m[key]
	caps.mu.Unlock()
	if ch == nil {
		return errors.New("该下载当前不需要验证码")
	}
	select {
	case ch <- code:
		return nil
	default:
		return errors.New("验证码已提交，请稍候")
	}
}

// ---------- the player's own netdisk ----------

// ListDisk: what is in a folder of the player's own netdisk — folders first — and the folder it ended up
// reading ("/" when the one asked for is not there any more).
func ListDisk(st *core.Store, dir string) ([]DiskEntry, string, error) {
	dir = path.Clean("/" + strings.TrimSpace(dir))
	s := LoadBaiduSession()
	if s == nil {
		return nil, dir, ErrBaiduLogin
	}
	c := NewBDClient(st, s)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ents, err := c.list(ctx, dir)
	if err != nil && dir != "/" && !errors.Is(err, ErrBaiduLogin) && strings.Contains(err.Error(), "没有该文件夹") {
		dir = "/"
		ents, err = c.list(ctx, dir)
	}
	if errors.Is(err, ErrBaiduLogin) {
		ForgetBaiduSession()
	}
	if err != nil {
		return nil, dir, err
	}
	out := make([]DiskEntry, 0, len(ents))
	for _, e := range ents {
		out = append(out, DiskEntry{Name: e.Name, Path: e.Path, Dir: e.IsDir == 1, Size: e.Size, MTime: e.MTime})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, dir, nil
}
