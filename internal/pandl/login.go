package pandl

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"vrclib/internal/core"
	"vrclib/internal/webpane"
)

// The Baidu Netdisk login.
//
// The player logs in on Baidu's own page in the program's browser; once BDUSS is among its cookies, the
// cookies of baidu.com are copied out, checked against Baidu (who is it, which membership), and kept here
// encrypted under this computer's account (core.ProtectData). A password is never seen.
//
// What is asked of Baidu with the login is what its own web client asks:
//
//	GET /api/gettemplatevariable?fields=["bdstoken","username","loginstate"]   who is logged in
//	GET /api/gettemplatevariable?fields=["is_svip","is_vip"]                   the membership

func sessionFile() string { return filepath.Join(core.DataDir, "baidu-session.dat") }

var sess struct {
	mu  sync.Mutex
	at  time.Time
	dir string
	s   *BaiduSession
}

// LoadBaiduSession: the login kept on this computer, nil when there is none. (Read for every state the
// window asks for: kept until the file changes.)
func LoadBaiduSession() *BaiduSession {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	fi, err := os.Stat(sessionFile())
	if err != nil {
		sess.s, sess.dir = nil, ""
		return nil
	}
	if sess.dir == core.DataDir && sess.at.Equal(fi.ModTime()) {
		return copySession(sess.s)
	}
	sess.s, sess.at, sess.dir = nil, fi.ModTime(), core.DataDir
	b, err := core.ReadDataFile(sessionFile())
	if err != nil {
		return nil
	}
	dec, err := core.UnprotectData(b)
	if err != nil {
		core.Logf("baidu-session.dat 无法解密（可能来自另一台电脑或另一个 Windows 账户），已忽略")
		return nil
	}
	var s BaiduSession
	if json.Unmarshal(dec, &s) != nil || bduss(s.Cookies) == "" {
		return nil
	}
	sess.s = &s
	return copySession(&s)
}

func copySession(s *BaiduSession) *BaiduSession {
	if s == nil {
		return nil
	}
	c := *s
	c.Cookies = append([]core.SavedCookie{}, s.Cookies...)
	return &c
}

// SaveBaiduSession writes the login back (the name and level it answered with).
func SaveBaiduSession(s *BaiduSession) error {
	if s == nil || bduss(s.Cookies) == "" {
		return errors.New("没有百度网盘的登录信息")
	}
	if s.Saved == 0 {
		s.Saved = time.Now().Unix()
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	enc, err := core.ProtectData(b)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(core.DataDir, 0755); err != nil {
		return err
	}
	if err := os.WriteFile(sessionFile(), enc, 0600); err != nil {
		return err
	}
	sess.mu.Lock()
	sess.dir = "" // read again
	sess.mu.Unlock()
	return nil
}

// ForgetBaiduSession drops a login Baidu no longer accepts.
func ForgetBaiduSession() {
	_ = os.Remove(sessionFile())
	sess.mu.Lock()
	sess.s, sess.dir = nil, ""
	sess.mu.Unlock()
	core.Logf("百度网盘的登录已失效，已清除")
}

// LogoutBaidu drops the login because the player asked: the saved one, and the browser's cookies of it.
func LogoutBaidu() error {
	_ = os.Remove(sessionFile())
	sess.mu.Lock()
	sess.s, sess.dir = nil, ""
	sess.mu.Unlock()
	core.Logf("已退出百度网盘")
	// the browser's own copy too (and the one kept for it in web-session.dat): else the next netdisk page
	// would log the player in again. A browser that is not running has only the kept copy, which goes.
	if err := webpane.Pane.ForgetSites([]string{"baidu.com"}); err != nil && !errors.Is(err, webpane.ErrCDPClosed) {
		return err
	}
	return nil
}

// CurrentBaiduAccount: what the window shows about the login.
func CurrentBaiduAccount() BaiduAccount {
	s := LoadBaiduSession()
	if s == nil {
		return BaiduAccount{}
	}
	return BaiduAccount{LoggedIn: true, Name: s.Name, VIP: s.VIP}
}

// BaiduLoginURL: the page the player logs in on — the netdisk's own, which asks for a login when there is
// none. (VRCLIB_BAIDU_LOGIN: the stand-in's, in tests.)
func BaiduLoginURL() string {
	if v := os.Getenv("VRCLIB_BAIDU_LOGIN"); v != "" {
		return v
	}
	return core.PanBase() + "/"
}

func bduss(cs []core.SavedCookie) string {
	for _, c := range cs {
		if c.Name == "BDUSS" && c.Value != "" {
			return c.Value
		}
	}
	return ""
}

// cookieHeader: the Cookie header of a login, leaving out what has run out.
func cookieHeader(cs []core.SavedCookie) string {
	now := time.Now().Unix()
	seen := map[string]bool{}
	var out []string
	for _, c := range cs {
		if c.Name == "" || seen[c.Name] || (c.Expires > 0 && c.Expires < now) {
			continue
		}
		seen[c.Name] = true
		out = append(out, c.Name+"="+c.Value)
	}
	return strings.Join(out, "; ")
}

// ---------- asking Baidu with the login ----------

func (c *BDClient) http() *http.Client { return core.HTTPClient(c.st) }

// call asks one of the netdisk's interfaces (GET, or POST with a form) and reads its JSON into v.
func (c *BDClient) call(ctx context.Context, method, path string, q url.Values, form url.Values, v any) error {
	if c.s == nil || bduss(c.s.Cookies) == "" {
		return ErrBaiduLogin
	}
	if q == nil {
		q = url.Values{}
	}
	for k, d := range map[string]string{"channel": "chunlei", "web": "1", "app_id": "250528", "clienttype": "0"} {
		if q.Get(k) == "" {
			q.Set(k, d)
		}
	}
	u := core.PanBase() + path + "?" + q.Encode()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	}
	req.Header.Set("User-Agent", core.UA)
	req.Header.Set("Referer", core.PanBase()+"/disk/main")
	req.Header.Set("Cookie", cookieHeader(c.s.Cookies))
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(b))), v); err != nil {
		return errors.New("百度网盘的回应无法读取")
	}
	return nil
}

// Whoami: the account's name and membership level (0 none, 1 会员, 2 超级会员), or ErrBaiduLogin when the
// login no longer works.
func (c *BDClient) Whoami() (string, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var who struct {
		Errno  int `json:"errno"`
		Result struct {
			Name       string `json:"username"`
			LoginState int    `json:"loginstate"`
			BDSToken   string `json:"bdstoken"`
		} `json:"result"`
	}
	if err := c.call(ctx, "GET", "/api/gettemplatevariable", url.Values{"fields": {`["bdstoken","username","loginstate"]`}}, nil, &who); err != nil {
		return "", 0, err
	}
	if who.Errno == -6 || who.Result.LoginState != 1 {
		return "", 0, ErrBaiduLogin
	}
	c.token = who.Result.BDSToken
	var vip struct {
		Errno  int `json:"errno"`
		Result struct {
			VIP  int `json:"is_vip"`
			SVIP int `json:"is_svip"`
		} `json:"result"`
	}
	level := 0
	if err := c.call(ctx, "GET", "/api/gettemplatevariable", url.Values{"fields": {`["is_svip","is_vip"]`}}, nil, &vip); err == nil {
		switch {
		case vip.Result.SVIP == 1:
			level = 2
		case vip.Result.VIP == 1:
			level = 1
		}
	}
	return who.Result.Name, level, nil
}

// ---------- waiting for the login in the browser ----------

var watching struct {
	mu sync.Mutex
	on bool
}

// panCookieURLs: the sites whose cookies make up the login.
func panCookieURLs() []string {
	return []string{core.PanBase() + "/", "https://passport.baidu.com/", "https://www.baidu.com/"}
}

// paneCookies: the browser's cookies of those sites (a var for the tests).
var paneCookies = func(urls []string) ([]core.SavedCookie, error) { return webpane.Pane.Cookies(urls) }

// paneLeft: the player has gone from the page the login was waited for on (a var for the tests).
var paneLeft = func(w *webpane.PaneWatch) bool { return w.Left() }

// WatchBaiduLogin: a netdisk page is open; once the player logs in there, downloads can use it.
func WatchBaiduLogin(st *core.Store) {
	watching.mu.Lock()
	if watching.on {
		watching.mu.Unlock()
		return
	}
	watching.on = true
	watching.mu.Unlock()
	go func() {
		defer func() {
			watching.mu.Lock()
			watching.on = false
			watching.mu.Unlock()
		}()
		w := &webpane.PaneWatch{Kind: "pan", Start: time.Now()}
		for i := 0; i < 20*60; i++ {
			time.Sleep(watchTick)
			if core.Quitting.Load() || paneLeft(w) {
				return
			}
			cs, err := paneCookies(panCookieURLs())
			if err != nil || bduss(cs) == "" {
				continue
			}
			s := &BaiduSession{Cookies: cs}
			name, vip, err := NewBDClient(st, s).Whoami()
			if err != nil {
				continue // the cookies are there before the login is done: asked again in a moment
			}
			s.Name, s.VIP = name, vip
			if err := SaveBaiduSession(s); err != nil {
				core.Logf("百度网盘的登录信息未能保存：%v", err)
				return
			}
			core.Logf("百度网盘已登录：%s（%s）", name, VIPLabel(vip))
			core.BumpRev()
			wakeQueue() // what waited for the login carries on
			return
		}
	}()
}

var watchTick = time.Second

// wakeQueue: the downloads waiting for a login may go on (pandl's queue looks at it).
var wakeQueue = func() {}
