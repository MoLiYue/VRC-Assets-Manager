package pandl

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"vrclib/internal/core"
	"vrclib/internal/testkit"
	"vrclib/internal/webpane"
)

// mockWeb: test/mockweb.py, as the netdisk's address.
func mockWeb(t *testing.T) string {
	t.Helper()
	base := testkit.MockWeb(t, filepath.Join("..", ".."))
	t.Setenv("VRCLIB_PAN_BASE", base)
	return base
}

var loggedIn = []core.SavedCookie{{Name: "BDUSS", Value: "mockbduss"}, {Name: "STOKEN", Value: "mockstoken"}}

// The login is kept encrypted, read back as it was, and gone once dropped; Baidu says who it is.
func TestBaiduSession(t *testing.T) {
	mockWeb(t)
	st := testkit.NewStore(t)
	if LoadBaiduSession() != nil || CurrentBaiduAccount().LoggedIn {
		t.Fatal("a login before any was saved")
	}
	name, vip, err := NewBDClient(st, &BaiduSession{Cookies: loggedIn}).Whoami()
	if err != nil || name != "mio测试" || vip != 0 {
		t.Fatalf("whoami: %q %d %v", name, vip, err)
	}
	if err := SaveBaiduSession(&BaiduSession{Name: name, VIP: vip, Cookies: loggedIn}); err != nil {
		t.Fatal(err)
	}
	if a := CurrentBaiduAccount(); !a.LoggedIn || a.Name != "mio测试" {
		t.Errorf("account: %+v", a)
	}
	if s := LoadBaiduSession(); s == nil || len(s.Cookies) != 2 || s.Saved == 0 {
		t.Errorf("read back: %+v", s)
	}
	// a login Baidu no longer takes
	if _, _, err := NewBDClient(st, &BaiduSession{Cookies: []core.SavedCookie{{Name: "BDUSS", Value: "stale"}}}).Whoami(); !errors.Is(err, ErrBaiduLogin) {
		t.Errorf("a stale login: %v", err)
	}
	if _, _, err := NewBDClient(st, nil).Whoami(); !errors.Is(err, ErrBaiduLogin) {
		t.Errorf("no login: %v", err)
	}
	if err := LogoutBaidu(); err != nil {
		t.Errorf("logout: %v", err)
	}
	if LoadBaiduSession() != nil || core.StatOK(sessionFile()) {
		t.Error("the login is still there after the logout")
	}
	if SaveBaiduSession(&BaiduSession{}) == nil {
		t.Error("a login without BDUSS was saved")
	}
}

// While a netdisk page is open, the login is taken from the browser once the player is in — not before
// Baidu says it works — and what waited for it is told.
func TestWatchBaiduLogin(t *testing.T) {
	mockWeb(t)
	st := testkit.NewStore(t)
	tick, cookies, left, wake := watchTick, paneCookies, paneLeft, wakeQueue
	t.Cleanup(func() { watchTick, paneCookies, paneLeft, wakeQueue = tick, cookies, left, wake })
	watchTick = 10 * time.Millisecond
	asked := 0
	paneCookies = func([]string) ([]core.SavedCookie, error) {
		asked++
		switch {
		case asked < 3:
			return nil, nil // the player is still typing
		case asked < 5:
			return []core.SavedCookie{{Name: "BDUSS", Value: "half-done"}}, nil // set, but not yet a login
		}
		return loggedIn, nil
	}
	paneLeft = func(*webpane.PaneWatch) bool { return false }
	woken := make(chan bool, 1)
	wakeQueue = func() { woken <- true }
	WatchBaiduLogin(st)
	WatchBaiduLogin(st) // once is enough
	select {
	case <-woken:
	case <-time.After(5 * time.Second):
		t.Fatal("the login was not taken")
	}
	if a := CurrentBaiduAccount(); !a.LoggedIn || a.Name != "mio测试" || a.VIP != 0 {
		t.Errorf("account: %+v", a)
	}
	if asked < 5 {
		t.Errorf("taken before Baidu took it: asked %d times", asked)
	}
}

func TestVIPLabel(t *testing.T) {
	for vip, want := range map[int]string{0: "非会员", 1: "普通会员", 2: "超级会员"} {
		if got := VIPLabel(vip); got != want {
			t.Errorf("%d: %q", vip, got)
		}
	}
}
