package library

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"vrclib/internal/booth"
	"vrclib/internal/cloudshare"
	"vrclib/internal/core"
	"vrclib/internal/naming"
	"vrclib/internal/netdisk"
	"vrclib/internal/translate"
)

// The pipeline: what the library does after something changed — walk the folders, work out which project
// uses what, read the Booth pages of the products that need it, and translate the names. One run at a time;
// a request that comes while one runs is turned away, and the window says so.

var (
	pipeMu      sync.Mutex
	pipeRunning bool
	// a run asked for while one is going: the folders have changed again (a split was marked, a setting
	// written), so another run follows instead of the request being dropped
	pipeAgain *pipeReq
	bgJobs    atomic.Int32 // the work that follows a run (covers, shares, translating)
)

// pipeReq: what a run is to do.
type pipeReq struct {
	scan, usage, booth, force bool
	keys                      []string
}

// merge: what two runs together have to do.
func (r *pipeReq) merge(o pipeReq) {
	r.scan, r.usage, r.booth, r.force = r.scan || o.scan, r.usage || o.usage, r.booth || o.booth, r.force || o.force
	if len(o.keys) == 0 {
		r.keys = nil // one of them wants every card
	} else if r.keys != nil {
		r.keys = append(r.keys, o.keys...)
	}
}

// StartPipeline runs the library's work in the background and answers false when it is already running.
func StartPipeline(st *core.Store, scan, usage, boothInfo, force bool, keys []string) bool {
	req := pipeReq{scan, usage, boothInfo, force, keys}
	pipeMu.Lock()
	if pipeRunning {
		// not dropped: what it was to do is added to the run that follows this one
		if pipeAgain == nil {
			c := req
			pipeAgain = &c
		} else {
			pipeAgain.merge(req)
		}
		pipeMu.Unlock()
		return false
	}
	pipeRunning = true
	pipeMu.Unlock()
	go func() {
		for r := req; ; {
			runPipeline(st, r.scan, r.usage, r.booth, r.force, r.keys)
			pipeMu.Lock()
			next := pipeAgain
			pipeAgain = nil
			if next == nil {
				pipeRunning = false
				pipeMu.Unlock()
				core.BumpRev()
				return
			}
			pipeMu.Unlock()
			r = *next
		}
	}()
	return true
}

// RunPipeline is StartPipeline in the caller's goroutine (what the -scan command line does).
func RunPipeline(st *core.Store, scan, usage, boothInfo, force bool, keys []string) {
	pipeMu.Lock()
	if pipeRunning {
		pipeMu.Unlock()
		return
	}
	pipeRunning = true
	pipeMu.Unlock()
	defer func() {
		pipeMu.Lock()
		pipeRunning = false
		pipeMu.Unlock()
		core.BumpRev()
	}()
	runPipeline(st, scan, usage, boothInfo, force, keys)
}

func runPipeline(st *core.Store, scan, usage, boothInfo, force bool, keys []string) {
	if scan {
		core.RunTask(core.TaskScan, func() { RunFolderScan(st, core.TaskScan) })
	}
	if usage {
		core.RunTask(core.TaskUsage, func() { RunUsageScan(st, core.TaskUsage) })
	}
	if boothInfo {
		core.RunTask(core.TaskBooth, func() { booth.RunBoothFetch(st, core.TaskBooth, force, keys) })
	}
	if scan {
		autoPkgBatch(st) // the cards that show no picture: the previews inside their packages
	}
	KickTranslate(st)
}

// PipelineBusy: the pipeline is running.
func PipelineBusy() bool {
	pipeMu.Lock()
	defer pipeMu.Unlock()
	return pipeRunning
}

// BackgroundBusy: the pipeline, or any of the work that follows it, is running.
func BackgroundBusy() bool { return PipelineBusy() || bgJobs.Load() > 0 }

// background runs one piece of after-work, counted so that the tests and the window can wait for it.
func background(f func()) {
	bgJobs.Add(1)
	go func() {
		defer bgJobs.Add(-1)
		f()
	}()
}

// The Chinese names worked out for the cards, kept in the data folder: a name is translated once, and the
// window shows it under the original (settings: 「隐藏中文译名」).
var zh struct {
	mu  sync.Mutex
	m   map[string]string
	dir string // the data folder it was read from
}

func zhFile() string { return filepath.Join(core.DataDir, "zhnames.json") }

func zhLoad() {
	if zh.dir == core.DataDir && zh.m != nil {
		return
	}
	zh.dir, zh.m = core.DataDir, map[string]string{}
	if b, err := core.ReadDataFile(zhFile()); err == nil {
		_ = json.Unmarshal(b, &zh.m)
	}
}

// zhOf: the Chinese name already worked out for this source text ("" when there is none yet).
func zhOf(src string) string {
	if src == "" {
		return ""
	}
	zh.mu.Lock()
	defer zh.mu.Unlock()
	zhLoad()
	return zh.m[src]
}

func zhPut(src, out string) {
	zh.mu.Lock()
	defer zh.mu.Unlock()
	zhLoad()
	zh.m[src] = out
	if b, err := json.Marshal(zh.m); err == nil {
		_ = os.WriteFile(zhFile(), b, 0644)
	}
}

// KickTranslate has the Chinese names of the cards worked out in the background. Names that are already
// Chinese or plain ASCII are left as they are; the rest are asked for one at a time, and only once ever.
func KickTranslate(st *core.Store) {
	background(func() {
		st.Mu.RLock()
		var want []string
		seen := map[string]bool{}
		for _, a := range st.Assets {
			src := translate.ZHSource(a.Name)
			if src == "" || seen[src] || !needsZh(src) || zhOf(src) != "" {
				continue
			}
			seen[src] = true
			want = append(want, src)
		}
		st.Mu.RUnlock()
		if len(want) == 0 {
			return
		}
		core.RunTask(translate.TaskTrans, func() {
			for i, src := range want {
				if core.Quitting.Load() {
					return
				}
				translate.TaskTrans.Set(i, len(want), src)
				out, err := translate.TranslateText(st, src)
				if err != nil {
					core.Logf("翻译失败 %q：%v", src, err)
					return // the service is not answering: the rest waits for the next run
				}
				zhPut(src, translate.ZHGlossary.Replace(out))
				core.BumpRev()
			}
			translate.TaskTrans.Set(len(want), len(want), "完成")
		})
	})
}

// needsZh: the name is worth translating — it carries something that is not plain ASCII and not already
// Chinese (a Japanese product name).
func needsZh(s string) bool {
	ascii, han, kana := true, false, false
	for _, r := range s {
		switch {
		case r > 127:
			ascii = false
			switch {
			case r >= 0x3040 && r <= 0x30FF:
				kana = true
			case r >= 0x4E00 && r <= 0x9FFF:
				han = true
			}
		}
	}
	if ascii {
		return false
	}
	return kana || !han
}

// WatchRoots watches the asset folders and rescans by itself when they change.
func WatchRoots(st *core.Store) {
	seen := map[string]int64{}
	for {
		time.Sleep(watchTick)
		if core.Quitting.Load() {
			return
		}
		st.Mu.RLock()
		roots := append([]string{}, st.Settings.Roots...)
		off, done := st.Settings.NoWatch, st.LastScan
		st.Mu.RUnlock()
		if off || done == 0 || PipelineBusy() {
			continue
		}
		changed := false
		for _, r := range roots {
			fi, err := os.Stat(r)
			if err != nil {
				continue
			}
			if at := fi.ModTime().Unix(); seen[r] != 0 && at != seen[r] {
				changed = true
			}
			seen[r] = fi.ModTime().Unix()
		}
		if changed {
			core.Logf("素材文件夹有变化，开始重新扫描")
			StartPipeline(st, true, true, false, false, nil)
		}
	}
}

// SyncLoop re-reads the shares and the Booth pages now and then, to spot what changed. It runs for as long
// as the program does.
func SyncLoop(st *core.Store) {
	time.Sleep(syncFirst)
	for {
		if core.Quitting.Load() {
			return
		}
		st.Mu.RLock()
		off := st.Settings.NoSync
		st.Mu.RUnlock()
		if !off && !BackgroundBusy() {
			if booth.ResumeDue() { // a batch Booth had turned away carries on
				StartPipeline(st, false, false, true, false, nil)
			}
			syncShares(st) // the shares read more than a day ago: what the sellers changed
		}
		time.Sleep(syncTick)
	}
}

// How often the folders and the shares are looked at. Tests shorten them.
var (
	watchTick = 20 * time.Second
	syncFirst = 3 * time.Minute
	syncTick  = 15 * time.Minute
)

// ---------- the cards the window shows ----------

// AllViews: every card of the library — what was scanned, the netdisk shares, and the purchases that are
// not on disk yet. The store is held by the caller.
func AllViews(st *core.Store) []AssetView {
	out := make([]AssetView, 0, len(st.Assets)+len(st.User))
	owned := map[string]bool{}
	for _, a := range st.Assets {
		v := viewOfAsset(st, a)
		out = append(out, v)
		if v.BoothID != "" {
			owned[v.BoothID] = true
		}
	}
	// a share a seller sent: a card of its own until it has been downloaded
	for _, key := range netdisk.PanCardKeys(st) {
		u := st.User[key]
		if u == nil || u.Downloaded != "" {
			continue
		}
		v := viewOfShare(st, key, u)
		out = append(out, v)
		if v.BoothID != "" {
			owned[v.BoothID] = true
		}
	}
	// something bought but not downloaded yet
	for _, id := range core.SortedKeys(st.Purchases) {
		if owned[id] {
			continue
		}
		out = append(out, viewOfPurchase(st, id))
	}
	return out
}

func viewOfAsset(st *core.Store, a *core.Asset) AssetView {
	v := AssetView{Key: a.Key, Name: a.Name, RawName: a.RawName, Category: a.Category, Bases: a.Bases,
		Size: a.Size, Files: a.Files, MTime: a.MTime, Added: a.FirstSeen, Locations: a.Locations,
		Usage: a.Usage, Packages: a.Packages, Archives: a.Archives}
	v.Bundle, v.BundlePacked = bundleView(st, a)
	id, how := booth.AssetBooth(st, a.Key, a)
	v.BoothID, v.BoothFrom = id, how
	if id != "" {
		v.Booth = st.Booth[id]
	}
	if p := st.Purchases[id]; p != nil {
		v.Purchase = p
	}
	apply(st, &v, a.Key, a.AltKey)
	if v.Cover == "" {
		v.Cover = GeneratedCover(a.Key)
	}
	if v.Cover == "" {
		v.Cover = autoCover(st, &v, a)
	}
	if v.Cover == "" { // last: Unity's own preview inside a package, small as it is
		if c := pkgCoverOf(a.Key); c != "" {
			v.Cover, v.CoverPkg = c, true
		}
	}
	return v
}

func viewOfShare(st *core.Store, key string, u *core.UserData) AssetView {
	v := AssetView{Key: key, Share: u.ShareURL, Name: u.Name}
	l, part := netdisk.PanSub(st, key)
	v.Pan = l
	if l != nil {
		info := netdisk.AnalyzePan(l, naming.ParseBases(st.Settings.Bases))
		if parts := netdisk.SplitPanCached(l); len(parts) > 1 {
			v.Bundle, v.BundlePacked = len(parts), packedParts(parts)
		}
		if v.Name == "" {
			v.Name = firstOf(partName(part), info.Parts[0].Name, l.Title)
		}
		v.Category, v.Bases = info.Category, info.Bases
		v.Files, v.Size = info.Files, info.Size
	}
	if v.Name == "" {
		v.Name = key
	}
	id, how := booth.AssetBooth(st, key, nil)
	v.BoothID, v.BoothFrom = id, how
	if id != "" {
		v.Booth = st.Booth[id]
	}
	v.Added = st.FirstSeen[key]
	apply(st, &v, key, "")
	if v.Cover == "" && v.Booth != nil {
		v.Cover = v.Booth.Cover
	}
	return v
}

func viewOfPurchase(st *core.Store, id string) AssetView {
	p := st.Purchases[id]
	key := "purchase:" + id
	v := AssetView{Key: key, Name: p.Name, BoothID: id, BoothFrom: "library", Purchase: p,
		Booth: st.Booth[id], Added: p.First, Cover: p.Cover}
	if cloudshare.IsCloudKey(id) || core.IsGumID(id) {
		v.BoothID = ""
	}
	apply(st, &v, key, "")
	if v.Cover == "" && v.Booth != nil {
		v.Cover = v.Booth.Cover
	}
	return v
}

// apply lays what the player wrote down over a card.
func apply(st *core.Store, v *AssetView, key, alt string) {
	u := st.User[key]
	if u == nil && alt != "" {
		u = st.User[alt]
	}
	if u == nil {
		v.NameZh = zhOf(translate.ZHSource(v.Name))
		return
	}
	if u.Name != "" {
		v.Name = u.Name
	}
	if u.Category != "" {
		v.Category = u.Category
	}
	if len(u.Bases) > 0 {
		v.Bases = u.Bases
	}
	v.NameZh, v.Tags, v.Notes, v.Hidden, v.Fav = u.NameZh, u.Tags, u.Notes, u.Hidden, u.Fav
	if v.NameZh == "" {
		v.NameZh = zhOf(translate.ZHSource(v.Name))
	}
	if u.Cover != "" {
		v.Cover = u.Cover
	}
	if len(u.Styles) > 0 {
		v.Styles = u.Styles
	} else if v.Booth != nil {
		v.Styles = stylesOf(st.Settings.Styles, v.Booth)
	}
}

// autoCover: the picture to show when the player picked none — the Booth product's, or one lying in the
// asset's own folder.
func autoCover(st *core.Store, v *AssetView, a *core.Asset) string {
	if v.Booth != nil && v.Booth.Cover != "" {
		return v.Booth.Cover
	}
	best, bestSize := "", int64(-1)
	for _, c := range a.Covers {
		fi, err := os.Stat(c)
		if err != nil {
			continue
		}
		// a picture named like a preview comes first; otherwise the largest one
		if n := strings.ToLower(filepath.Base(c)); strings.Contains(n, "thumb") || strings.Contains(n, "preview") || strings.Contains(n, "サムネ") {
			return c
		}
		if fi.Size() > bestSize {
			best, bestSize = c, fi.Size()
		}
	}
	return best
}

// StyleNames: the names of the style tags the settings define ("甜美=ロリ|姫" → "甜美").
func StyleNames(styles []string) []string {
	out := []string{}
	for _, s := range styles {
		if name := strings.TrimSpace(strings.SplitN(s, "=", 2)[0]); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// stylesOf: the style tags a product's Booth tags earn it.
func stylesOf(styles []string, b *core.BoothInfo) []string {
	if b == nil {
		return nil
	}
	text := strings.ToLower(b.Name + " " + strings.Join(b.Tags, " "))
	var out []string
	for _, s := range styles {
		name, words, ok := strings.Cut(s, "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			continue
		}
		for _, w := range strings.Split(words, "|") {
			if w = strings.ToLower(strings.TrimSpace(w)); w != "" && strings.Contains(text, w) {
				out = append(out, name)
				break
			}
		}
	}
	if b.Adult && !core.ContainsStr(out, naming.AdultStyle) {
		out = append(out, naming.AdultStyle)
	}
	return out
}

// OwnedBoothIDs: the Booth products the library already holds, by item number.
func OwnedBoothIDs(st *core.Store) map[string]string {
	out := map[string]string{}
	for _, a := range st.Assets {
		if id, _ := booth.AssetBooth(st, a.Key, a); id != "" {
			out[id] = a.Key
		}
	}
	for _, key := range netdisk.PanCardKeys(st) {
		if u := st.User[key]; u != nil && u.Downloaded == "" {
			if id, _ := booth.AssetBooth(st, key, nil); id != "" {
				out[id] = key
			}
		}
	}
	return out
}

// ---------- what was brought in from another computer ----------

var (
	importedMu sync.Mutex
	importedFn []func()
)

// OnDataImported registers what to do once the library's files were replaced from outside (a transfer, or
// its undo): everything that keeps its own copy reads it again.
func OnDataImported(f func()) {
	importedMu.Lock()
	importedFn = append(importedFn, f)
	importedMu.Unlock()
}

// dataImported tells them all.
func dataImported() {
	importedMu.Lock()
	fns := append([]func(){}, importedFn...)
	importedMu.Unlock()
	for _, f := range fns {
		f()
	}
}

// ---------- small helpers ----------

func firstOf(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func partName(p *netdisk.PanPart) string {
	if p == nil {
		return ""
	}
	return p.Name
}

// packedParts: the products of a collection are still inside archives, so it cannot be split before they
// are unpacked.
func packedParts(parts []netdisk.PanPart) bool {
	for _, p := range parts {
		if strings.Contains(strings.ToLower(p.Path), ".zip") || strings.Contains(strings.ToLower(p.Path), ".rar") ||
			strings.Contains(strings.ToLower(p.Path), ".7z") {
			return true
		}
	}
	return false
}
