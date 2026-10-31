package library

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"vrclib/internal/core"
	"vrclib/internal/netdisk"
	"vrclib/internal/testkit"
)

func waitPan(t *testing.T) {
	t.Helper()
	for i := 0; i < 500; i++ {
		netdisk.PanMu.Lock()
		busy := netdisk.PanActive || len(netdisk.PanQueue) > 0
		netdisk.PanMu.Unlock()
		if !busy {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the share reads did not end")
}

// A share's listing is read and kept; read again, what the seller added, changed or took away is written
// down with it, and the card says it has something new. A read that fails keeps what was read before.
func TestQueuePanFetch(t *testing.T) {
	gap := panGap
	panGap = time.Millisecond
	t.Cleanup(func() { panGap = gap })
	var mu sync.Mutex
	asked := map[string]int{}
	files := []*core.PanFile{{Name: "Dress.zip", Size: 10}, {Name: "Tex", Dir: true, Children: []*core.PanFile{{Name: "a.png", Size: 1}}}}
	fail := false
	old := readShare
	readShare = func(_ context.Context, _ *core.Store, key, link, _ string) (*core.PanListing, error) {
		mu.Lock()
		defer mu.Unlock()
		asked[key]++
		if fail {
			return nil, errors.New("Dropbox 返回错误（HTTP 500）")
		}
		return &core.PanListing{Title: "Dress", Files: files}, nil
	}
	t.Cleanup(func() { readShare = old })

	st := testkit.NewStore(t)
	const key = "db:abc123xyz456789"
	st.User[key] = &core.UserData{ShareURL: "https://www.dropbox.com/scl/fi/abc123xyz456789/Dress.zip"}
	st.User["pan:1Baidu"] = &core.UserData{ShareURL: "https://pan.baidu.com/s/1Baidu"}
	QueuePanFetch(st, key)
	QueuePanFetch(st, key)          // asked again while it waits: once
	QueuePanFetch(st, "pan:1Baidu") // a Baidu share is read too
	waitPan(t)
	mu.Lock()
	if asked[key] != 1 || asked["pan:1Baidu"] != 1 {
		t.Errorf("asked: %v", asked)
	}
	mu.Unlock()
	st.Mu.RLock()
	l := st.Pan[key]
	st.Mu.RUnlock()
	if l == nil || l.Surl != key || len(l.Files) != 2 || l.Fetched == 0 || l.Changed != 0 {
		t.Fatalf("the listing: %+v", l)
	}
	// the seller replaces a file and takes the textures away
	mu.Lock()
	files = []*core.PanFile{{Name: "Dress.zip", Size: 12}, {Name: "Dress_PSD.zip", Size: 5}}
	mu.Unlock()
	QueuePanFetch(st, key)
	waitPan(t)
	st.Mu.RLock()
	l = st.Pan[key]
	st.Mu.RUnlock()
	if strings.Join(l.Added, ",") != "/Dress.zip,/Dress_PSD.zip" || strings.Join(l.Removed, ",") != "/Tex/a.png" || l.Changed == 0 {
		t.Errorf("what changed: added %v, removed %v, at %d", l.Added, l.Removed, l.Changed)
	}
	// read again with nothing new: the change stays written down until the player has looked
	changed := l.Changed
	QueuePanFetch(st, key)
	waitPan(t)
	st.Mu.RLock()
	l = st.Pan[key]
	st.Mu.RUnlock()
	if l.Changed != changed || len(l.Added) != 2 {
		t.Errorf("an unchanged read lost the change: %+v", l)
	}
	// the service fails: what was read stands, with the error
	mu.Lock()
	fail = true
	mu.Unlock()
	QueuePanFetch(st, key)
	waitPan(t)
	st.Mu.RLock()
	l = st.Pan[key]
	st.Mu.RUnlock()
	if len(l.Files) != 2 || !strings.Contains(l.Err, "HTTP 500") {
		t.Errorf("after a failed read: %+v", l)
	}
	// the card shows what is in the share
	if v := viewOf(st, "Dress"); v.Key != key || v.Files != 2 {
		t.Errorf("the card: %+v", v)
	}
}

// SyncLoop's part: the cloud shares read more than a day ago are read again.
func TestSyncShares(t *testing.T) {
	gap := panGap
	panGap = time.Millisecond
	t.Cleanup(func() { panGap = gap })
	old := readShare
	readShare = func(context.Context, *core.Store, string, string, string) (*core.PanListing, error) {
		return &core.PanListing{Files: []*core.PanFile{{Name: "x.zip", Size: 1}}}, nil
	}
	t.Cleanup(func() { readShare = old })
	st := testkit.NewStore(t)
	for _, k := range []string{"gd:fresh0000000000", "gd:stale0000000000", "db:never0000000000"} {
		st.User[k] = &core.UserData{ShareURL: "https://example.com/" + k}
	}
	st.Pan["gd:fresh0000000000"] = &core.PanListing{Fetched: time.Now().Unix()}
	st.Pan["gd:stale0000000000"] = &core.PanListing{Fetched: time.Now().Add(-48 * time.Hour).Unix()}
	st.User["pan:1Old"] = &core.UserData{ShareURL: "https://pan.baidu.com/s/1Old"}
	if n := syncShares(st); n != 3 {
		t.Errorf("due: %d", n)
	}
	waitPan(t)
}
