package purchases

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"vrclib/internal/core"
	"vrclib/internal/webpane"
)

// The logins of the stores, kept on this computer encrypted (core.ProtectData: DPAPI under the player's
// account on Windows). They are the cookies the player's own browsing left in the pane; the program only
// copies them out of it, and never sees a password.
func boothSessionFile() string { return filepath.Join(core.DataDir, "booth-session.dat") }
func gumSessionFile() string   { return filepath.Join(core.DataDir, "gumroad-session.dat") }

// A session file is read for every /api/state the window asks for, so what was read is kept until the file
// changes.
var sess struct {
	mu   sync.Mutex
	at   map[string]time.Time
	have map[string][]core.SavedCookie
}

func loadSession(file string) []core.SavedCookie {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	fi, err := os.Stat(file)
	if err != nil {
		delete(sess.have, file)
		delete(sess.at, file)
		return nil
	}
	if sess.at != nil && sess.at[file].Equal(fi.ModTime()) {
		return sess.have[file]
	}
	b, err := core.ReadDataFile(file)
	if err != nil {
		return nil
	}
	dec, err := core.UnprotectData(b)
	if err != nil {
		core.Logf("%s 无法解密（可能来自另一台电脑或另一个 Windows 账户），已忽略", filepath.Base(file))
		return nil
	}
	var cs []core.SavedCookie
	if json.Unmarshal(dec, &cs) != nil {
		return nil
	}
	if sess.at == nil {
		sess.at, sess.have = map[string]time.Time{}, map[string][]core.SavedCookie{}
	}
	sess.at[file], sess.have[file] = fi.ModTime(), cs
	return cs
}

func saveSession(file string, cs []core.SavedCookie) error {
	if len(cs) == 0 {
		return dropSession(file)
	}
	b, err := json.Marshal(cs)
	if err != nil {
		return err
	}
	enc, err := core.ProtectData(b)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(file, enc, 0600); err != nil {
		return err
	}
	sess.mu.Lock()
	sess.at, sess.have = nil, nil // read again from disk
	sess.mu.Unlock()
	return nil
}

func dropSession(file string) error {
	sess.mu.Lock()
	sess.at, sess.have = nil, nil
	sess.mu.Unlock()
	if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// LoadBoothSession: the cookies of the Booth login kept on this computer (empty when there is none).
func LoadBoothSession() []core.SavedCookie { return loadSession(boothSessionFile()) }

// LoadGumSession: the cookies of the Gumroad login, nil when there is none.
func LoadGumSession() []core.SavedCookie { return loadSession(gumSessionFile()) }

// ForgetBoothLogin drops the saved Booth login.
func ForgetBoothLogin() error {
	core.Logf("已退出 Booth 登录")
	return dropSession(boothSessionFile())
}

// LogoutGumroad drops the Gumroad login.
func LogoutGumroad() error {
	gumAccount.Store(&GumroadAccount{})
	core.Logf("已退出 Gumroad 登录")
	return dropSession(gumSessionFile())
}

// cookieHeader: the Cookie header a saved login makes, leaving out what has run out.
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

// ErrStoreLogin: the store wants the player to log in again (the saved login is gone or was turned away).
var ErrStoreLogin = errors.New("需要重新登录")

// maxPage: how much of one page is read. A library page of a hundred products is a few hundred kilobytes.
const maxPage = 8 << 20

// fetchPage reads one page of a store with a saved login. It is a var so that the tests can count the
// requests and answer them without a server.
var fetchPage = func(c *http.Client, url, cookie string) (body []byte, status int, err error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", core.UA)
	req.Header.Set("Accept-Language", "ja,zh-CN;q=0.8")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxPage))
	// the library and the orders are the player's own pages: being sent to the sign-in page means the login
	// is no longer good
	if resp.StatusCode == 401 || resp.StatusCode == 403 || strings.Contains(resp.Request.URL.Path, "/users/sign_in") {
		return b, resp.StatusCode, ErrStoreLogin
	}
	return b, resp.StatusCode, nil
}

// ---------- waiting for a login in the pane ----------

// watchLogin waits for the cookies of a store to turn up in the pane while the player is on its pages, and
// calls then. It gives up when the player leaves the page or closes it.
func watchLogin(kind string, urls []string, file string, then func()) {
	w := &webpane.PaneWatch{Kind: kind, Start: time.Now()}
	for i := 0; i < 20*60; i++ { // twenty minutes at most
		time.Sleep(time.Second)
		if core.Quitting.Load() {
			return
		}
		if w.Left() {
			return
		}
		cs, err := webpane.Pane.Cookies(urls)
		if err != nil || !loggedIn(kind, cs) {
			continue
		}
		if err := saveSession(file, cs); err != nil {
			core.Logf("%s 的登录信息未能保存：%v", kind, err)
			return
		}
		core.Logf("%s 已登录，开始读取已购", kind)
		core.BumpRev()
		then()
		return
	}
}

// loggedIn: these cookies are a login, not the ones a visitor gets. Which cookie says so is the store's own
// (Booth: the session of accounts.booth.pm; Gumroad: its session cookie).
func loggedIn(kind string, cs []core.SavedCookie) bool {
	want := map[string][]string{
		"booth":   {"_plaza_session_nktz7u", "_plaza_session", "session"},
		"gumroad": {"_gumroad_app_session", "_gumroad_guid", "session"},
	}[kind]
	for _, c := range cs {
		for _, w := range want {
			if c.Name == w && c.Value != "" {
				return true
			}
		}
	}
	return false
}

// WatchGumroadLogin: the Gumroad login page is open; once the player is in, read the library.
func WatchGumroadLogin(st *core.Store) {
	go watchLogin("gumroad", []string{core.GumroadBase() + "/"}, gumSessionFile(), func() { StartGumroadSync(st) })
}
