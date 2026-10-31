package library

import (
	"context"
	"sort"
	"strings"
	"time"

	"vrclib/internal/cloudshare"
	"vrclib/internal/core"
	"vrclib/internal/netdisk"
)

// Reading what a netdisk share holds.
//
// A card of a share shows its listing: what is in it, how large, whether it is a collection. The listing is
// read when the card is made or its link changes, and again now and then (SyncLoop) to spot what the seller
// changed — what is new and what is gone is written down with the listing (Added, Removed, Changed), which
// is what the card's 「有更新」 and the update into projects go by.
//
// One share at a time, with a pause between two: the services turn many reads in a row away.
//
// Google Drive and Dropbox are read through internal/cloudshare, Baidu Netdisk through netdisk's own
// reader — as a visitor, with the extraction code the card has.

var panGap = 1500 * time.Millisecond // between two shares; tests shorten it

// readShare reads one share's listing. It is a var so that tests can answer without the services.
var readShare = func(ctx context.Context, st *core.Store, key, link, pwd string) (*core.PanListing, error) {
	if cloudshare.IsCloudKey(key) {
		return cloudshare.FetchListingCtx(ctx, st, link)
	}
	surl := netdisk.ShareSurl(link)
	if surl == "" {
		surl = netdisk.ShareKey(key)
	}
	if pwd == "" {
		pwd = netdisk.SharePwdFromURL(link)
	}
	return netdisk.FetchBaiduShare(ctx, core.HTTPClient(st), surl, pwd)
}

// QueuePanFetch has a share read in the background (once: asked again while it waits, it is not queued
// twice).
func QueuePanFetch(st *core.Store, key string) {
	if !core.IsNetdiskKey(key) {
		return
	}
	share, _, _ := strings.Cut(key, "#") // a product inside a share: the share is what is read
	netdisk.PanMu.Lock()
	for _, k := range netdisk.PanQueue {
		if k == share {
			netdisk.PanMu.Unlock()
			return
		}
	}
	netdisk.PanQueue = append(netdisk.PanQueue, share)
	start := !netdisk.PanActive
	netdisk.PanActive = true
	netdisk.PanMu.Unlock()
	if start {
		background(func() { panWorker(st) })
	}
}

func panWorker(st *core.Store) {
	defer func() {
		netdisk.PanMu.Lock()
		netdisk.PanActive = false
		netdisk.PanMu.Unlock()
		core.BumpRev()
	}()
	core.RunTask(netdisk.TaskPan, func() {
		done := 0
		for {
			netdisk.PanMu.Lock()
			if len(netdisk.PanQueue) == 0 || core.Quitting.Load() {
				netdisk.PanQueue = nil
				netdisk.PanMu.Unlock()
				return
			}
			key := netdisk.PanQueue[0]
			netdisk.PanQueue = netdisk.PanQueue[1:]
			left := len(netdisk.PanQueue)
			netdisk.PanMu.Unlock()
			netdisk.TaskPan.Set(done, done+left+1, key)
			fetchShare(st, key)
			done++
			if left > 0 {
				time.Sleep(panGap)
			}
		}
	})
}

// fetchShare reads one share and writes its listing into the store, with what changed since the last one.
func fetchShare(st *core.Store, key string) {
	st.Mu.RLock()
	u := st.User[key]
	link, pwd := "", ""
	if u != nil {
		link, pwd = u.ShareURL, u.SharePwd
	}
	st.Mu.RUnlock()
	if link == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	l, err := readShare(ctx, st, key, link, pwd)
	cancel()
	now := time.Now().Unix()
	st.Mu.Lock()
	old := st.Pan[netdisk.ShareKey(key)]
	switch {
	case err != nil && old != nil:
		// what was read before stands; the card says the last read failed
		old.Err, old.Fetched = core.TrimErr(err), now
	case err != nil:
		st.Pan[netdisk.ShareKey(key)] = &core.PanListing{Surl: key, Err: core.TrimErr(err), Fetched: now}
	default:
		l.Surl, l.Fetched, l.Err = key, now, ""
		if old != nil && len(old.Files) > 0 {
			if added, removed := listingDiff(old.Files, l.Files); len(added)+len(removed) > 0 {
				l.Added, l.Removed, l.Changed = added, removed, now
			} else {
				l.Added, l.Removed, l.Changed = old.Added, old.Removed, old.Changed // nothing new: as it was
			}
		}
		st.Pan[netdisk.ShareKey(key)] = l
	}
	st.Mu.Unlock()
	if err != nil {
		core.Logf("读取网盘分享 %s 失败：%v", key, err)
	}
	_ = st.Save()
	core.BumpRev()
}

// listingDiff: the files of the new listing that the old one did not have (or had at another size), and
// the ones it no longer has.
func listingDiff(old, now []*core.PanFile) (added, removed []string) {
	a, b := flatFiles(old), flatFiles(now)
	for p, size := range b {
		if s, ok := a[p]; !ok || (s > 0 && size > 0 && s != size) {
			added = append(added, p)
		}
	}
	for p := range a {
		if _, ok := b[p]; !ok {
			removed = append(removed, p)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

// flatFiles: every file of a listing by its path ("/Folder/file.zip"), with its size.
func flatFiles(fs []*core.PanFile) map[string]int64 {
	out := map[string]int64{}
	var walk func([]*core.PanFile, string, int)
	walk = func(cs []*core.PanFile, at string, depth int) {
		if depth > 12 {
			return
		}
		for _, c := range cs {
			if c == nil || c.Name == "" {
				continue
			}
			p := at + "/" + c.Name
			if c.Dir {
				walk(c.Children, p, depth+1)
				continue
			}
			out[p] = c.Size
		}
	}
	walk(fs, "", 0)
	return out
}

// syncShares queues every share card whose listing is older than a day, for SyncLoop.
func syncShares(st *core.Store) int {
	st.Mu.RLock()
	var due []string
	day := time.Now().Add(-24 * time.Hour).Unix()
	for _, key := range netdisk.PanCardKeys(st) {
		if strings.Contains(key, "#") {
			continue
		}
		if l := st.Pan[netdisk.ShareKey(key)]; l == nil || l.Fetched < day {
			due = append(due, key)
		}
	}
	st.Mu.RUnlock()
	for _, k := range due {
		QueuePanFetch(st, k)
	}
	return len(due)
}
