package library

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"vrclib/internal/core"
)

// Covers out of unitypackages.
//
// A unitypackage carries, for many of its assets, the preview Unity made of it (preview.png beside the
// asset in the package): the rendering of a prefab, the thumbnail of a texture. They are 128 pixels at most,
// so such a cover comes last — after the player's own, the one made in Unity, the Booth product's, and a
// picture in the asset's folder — and the card says it is only a preview.
//
// Which preview is taken: a prefab's before anything else (it shows the whole thing), the largest of them,
// and of the packages of an asset, the largest package first (the main one, not an extra).
//
// What was found is kept in pkgcovers.json, with the package it came from and that package's time: an asset
// is looked into again only when its packages changed, or when the player asks.

const (
	pkgMaxPreview = 512 << 10 // a preview larger than this is no preview
	pkgMaxEntries = 20000     // entries of a package read at most
)

// PkgCover: a cover read out of a unitypackage.
type PkgCover struct {
	Path  string `json:"path"`  // the picture, in the data folder
	Pkg   string `json:"pkg"`   // the package it came from
	Entry string `json:"entry"` // the asset of the package whose preview it is ("Assets/…/Dress.prefab")
	MTime int64  `json:"mtime"` // the package's time when it was read
}

// pkgRecord: what is known about one card.
type pkgRecord struct {
	Cover  *PkgCover `json:"cover,omitempty"`
	Failed string    `json:"failed,omitempty"` // why none was found, the last time it was looked for
	Tried  int64     `json:"tried,omitempty"`
	Sig    string    `json:"sig,omitempty"` // the packages and their times, when it was looked into
	Off    bool      `json:"off,omitempty"` // the player removed it: not taken again by itself
}

// PkgCoverStatus: what the details panel shows.
type PkgCoverStatus struct {
	Cover  *PkgCover `json:"cover,omitempty"`
	Busy   bool      `json:"busy,omitempty"`
	Failed string    `json:"failed,omitempty"`
}

// PkgBatch: reading the previews of every card that shows no picture.
type PkgBatch struct {
	Running bool   `json:"running"`
	Done    int    `json:"done"`
	Total   int    `json:"total"`
	Now     string `json:"now,omitempty"`
	Made    int    `json:"made"`
	Failed  int    `json:"failed"`
	Err     string `json:"err,omitempty"`
	Last    string `json:"last,omitempty"`
	Ended   int64  `json:"ended,omitempty"`
}

var pkg struct {
	mu   sync.Mutex
	m    map[string]*pkgRecord
	dir  string // the data folder it was read from
	busy map[string]bool
	b    PkgBatch
	stop atomic.Bool
}

func pkgFile() string     { return filepath.Join(core.DataDir, "pkgcovers.json") }
func pkgCoverDir() string { return filepath.Join(core.CoversDir(), "pkg") }

// pkgLoad reads pkgcovers.json (again when the data folder changed). pkg.mu is held.
func pkgLoad() {
	if pkg.m != nil && pkg.dir == core.DataDir {
		return
	}
	pkg.m, pkg.dir = map[string]*pkgRecord{}, core.DataDir
	if pkg.busy == nil {
		pkg.busy = map[string]bool{}
	}
	if b, err := core.ReadDataFile(pkgFile()); err == nil {
		_ = json.Unmarshal(b, &pkg.m)
	}
}

func pkgSave() {
	if b, err := json.Marshal(pkg.m); err == nil {
		_ = os.WriteFile(pkgFile(), b, 0644)
	}
}

// pkgCoverOf: the cover a card has from a unitypackage, "" when it has none.
func pkgCoverOf(key string) string {
	pkg.mu.Lock()
	defer pkg.mu.Unlock()
	pkgLoad()
	if r := pkg.m[key]; r != nil && r.Cover != nil && !r.Off && core.StatOK(r.Cover.Path) {
		return r.Cover.Path
	}
	return ""
}

// PkgCoverStatusOf: what a card's unitypackages offer. key "": only the batch is asked about.
func PkgCoverStatusOf(st *core.Store, key string) PkgCoverStatus {
	pkg.mu.Lock()
	defer pkg.mu.Unlock()
	pkgLoad()
	s := PkgCoverStatus{Busy: pkg.busy[key]}
	if r := pkg.m[key]; r != nil {
		if r.Cover != nil && !r.Off {
			s.Cover = r.Cover
		}
		s.Failed = r.Failed
	}
	return s
}

// PkgBatchSnapshot: how the batch is going.
func PkgBatchSnapshot() PkgBatch {
	pkg.mu.Lock()
	defer pkg.mu.Unlock()
	return pkg.b
}

// CancelPkgBatch stops it.
func CancelPkgBatch() { pkg.stop.Store(true) }

// pkgsOf: a card's unitypackages, the largest first, with a signature of them all (paths, sizes, times).
func pkgsOf(st *core.Store, key string) ([]string, string) {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	type one struct {
		p    string
		size int64
	}
	var list []one
	for _, a := range st.Assets {
		if a.Key != key && a.AltKey != key {
			continue
		}
		for _, l := range a.Locations {
			if l.Kind == "unitypackage" {
				list = append(list, one{l.Path, l.Size})
				continue
			}
			for _, rel := range a.Packages {
				p := filepath.Join(l.Path, filepath.FromSlash(rel))
				if fi, err := os.Stat(p); err == nil {
					list = append(list, one{p, fi.Size()})
				}
			}
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].size > list[j].size })
	var out []string
	var sig strings.Builder
	for _, o := range list {
		out = append(out, o.p)
		if fi, err := os.Stat(o.p); err == nil {
			sig.WriteString(o.p + "|" + itoa(int(fi.Size())) + "|" + itoa(int(fi.ModTime().Unix())) + ";")
		}
	}
	return out, sig.String()
}

// ExtractPkgCover reads the preview out of a card's unitypackage and makes it the cover.
func ExtractPkgCover(st *core.Store, key string) (string, error) {
	pkg.mu.Lock()
	pkgLoad()
	if pkg.busy[key] {
		pkg.mu.Unlock()
		return "", errors.New("正在读取该素材的 unitypackage，请稍候")
	}
	pkg.busy[key] = true
	pkg.mu.Unlock()
	core.BumpRev()
	defer func() {
		pkg.mu.Lock()
		delete(pkg.busy, key)
		pkg.mu.Unlock()
		core.BumpRev()
	}()
	pkgs, sig := pkgsOf(st, key)
	if len(pkgs) == 0 {
		return "", errors.New("该素材中没有 unitypackage")
	}
	c, err := extractFrom(pkgs)
	pkg.mu.Lock()
	r := &pkgRecord{Tried: time.Now().Unix(), Sig: sig}
	if err != nil {
		r.Failed = err.Error()
	} else {
		r.Cover = c
	}
	pkg.m[key] = r
	pkgSave()
	pkg.mu.Unlock()
	if err != nil {
		return "", err
	}
	return c.Path, nil
}

// RemovePkgCover drops a cover that came from a unitypackage. It is not taken again by itself.
func RemovePkgCover(st *core.Store, key string) {
	pkg.mu.Lock()
	pkgLoad()
	if r := pkg.m[key]; r != nil {
		if r.Cover != nil {
			_ = os.Remove(r.Cover.Path)
		}
		r.Cover, r.Off, r.Failed = nil, true, ""
	} else {
		pkg.m[key] = &pkgRecord{Off: true}
	}
	pkgSave()
	pkg.mu.Unlock()
	core.BumpRev()
}

// StartPkgBatch begins reading the previews of every card that shows no picture — the ones never looked
// into, and the ones whose packages changed since. (Asked for by the player, the ones that found none before
// are tried again too.)
func StartPkgBatch(st *core.Store) error { return startPkg(st, true) }

// autoPkgBatch is the same after a scan: only what is new or changed, and quietly.
func autoPkgBatch(st *core.Store) { _ = startPkg(st, false) }

func startPkg(st *core.Store, byHand bool) error {
	// what the cards are, and what their packages are now — read before pkg.mu is taken: the cards are
	// built holding the store and then ask pkg.mu, so the two are never taken the other way round
	st.Mu.RLock()
	views := AllViews(st)
	st.Mu.RUnlock()
	sigs := map[string]string{}
	for _, v := range views {
		if len(v.Packages) > 0 && (v.Cover == "" || v.CoverPkg) {
			_, sigs[v.Key] = pkgsOf(st, v.Key)
		}
	}
	var keys []string
	pkg.mu.Lock()
	pkgLoad()
	if pkg.b.Running {
		pkg.mu.Unlock()
		return errors.New("正在提取封面，请稍候")
	}
	for _, v := range views {
		sig, ok := sigs[v.Key]
		if !ok {
			continue
		}
		r := pkg.m[v.Key]
		switch {
		case r != nil && r.Off:
			continue // the player removed it
		case r != nil && !byHand && r.Sig == sig:
			continue // looked into already, and nothing changed
		}
		keys = append(keys, v.Key)
	}
	if len(keys) == 0 {
		pkg.mu.Unlock()
		if byHand {
			return errors.New("没有需要提取封面的素材")
		}
		return nil
	}
	pkg.b = PkgBatch{Running: true, Total: len(keys)}
	pkg.stop.Store(false)
	pkg.mu.Unlock()
	core.BumpRev()
	background(func() {
		core.RunTask(TaskPkgCover, func() {
			made, failed, last := 0, 0, ""
			for i, k := range keys {
				if pkg.stop.Load() || core.Quitting.Load() {
					break
				}
				pkg.mu.Lock()
				pkg.b.Done, pkg.b.Now = i, k
				pkg.mu.Unlock()
				TaskPkgCover.Set(i, len(keys), k)
				core.BumpRev()
				if _, err := ExtractPkgCover(st, k); err != nil {
					failed, last = failed+1, k+"："+err.Error()
					continue
				}
				made++
			}
			pkg.mu.Lock()
			pkg.b = PkgBatch{Done: len(keys), Total: len(keys), Made: made, Failed: failed, Last: last, Ended: time.Now().Unix()}
			pkg.mu.Unlock()
			core.BumpRev()
		})
	})
	return nil
}

// ---------- reading a package ----------

// preview: one asset's preview in a package.
type preview struct {
	entry string
	data  []byte
}

// extractFrom looks into the packages, the largest first, and keeps the best preview of the first that has
// one.
func extractFrom(pkgs []string) (*PkgCover, error) {
	var lastErr error
	for _, p := range pkgs {
		best, err := bestPreview(p)
		if err != nil {
			lastErr = err
			continue
		}
		if best == nil {
			continue
		}
		fi, _ := os.Stat(p)
		sum := sha1.Sum([]byte(p))
		dst := filepath.Join(pkgCoverDir(), hex.EncodeToString(sum[:])[:20]+".png")
		if err := os.MkdirAll(pkgCoverDir(), 0755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(dst, best.data, 0644); err != nil {
			return nil, err
		}
		c := &PkgCover{Path: dst, Pkg: p, Entry: best.entry}
		if fi != nil {
			c.MTime = fi.ModTime().Unix()
		}
		return c, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("unitypackage 中没有可用的预览图")
}

// bestPreview reads the previews of one package and picks the one to show: a prefab's first, the largest.
func bestPreview(p string) (*preview, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, errors.New("不是有效的 unitypackage")
	}
	defer zr.Close()
	paths, prevs := map[string]string{}, map[string][]byte{}
	tr := tar.NewReader(zr)
	for n := 0; n < pkgMaxEntries; n++ {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if len(prevs) > 0 {
				break // a package cut short: what was read is used
			}
			return nil, errors.New("unitypackage 已损坏")
		}
		name := path.Clean(strings.TrimPrefix(filepath.ToSlash(h.Name), "./"))
		guid, what := path.Split(name)
		guid = strings.Trim(guid, "/")
		if guid == "" || strings.Contains(guid, "/") {
			continue
		}
		switch what {
		case "pathname":
			b, _ := io.ReadAll(io.LimitReader(tr, 4096))
			paths[guid] = strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
		case "preview.png":
			if h.Size <= 0 || h.Size > pkgMaxPreview {
				continue
			}
			b, err := io.ReadAll(io.LimitReader(tr, pkgMaxPreview))
			if err == nil && pictureExt(b) == ".png" {
				prevs[guid] = b
			}
		}
	}
	var best *preview
	rank := func(entry string) int {
		switch strings.ToLower(path.Ext(entry)) {
		case ".prefab":
			return 3
		case ".fbx", ".blend":
			return 2
		case ".png", ".jpg", ".jpeg", ".tga", ".psd":
			return 1
		}
		return 0
	}
	for guid, b := range prevs {
		c := &preview{entry: paths[guid], data: b}
		if best == nil || rank(c.entry) > rank(best.entry) ||
			(rank(c.entry) == rank(best.entry) && len(c.data) > len(best.data)) ||
			(rank(c.entry) == rank(best.entry) && len(c.data) == len(best.data) && c.entry < best.entry) {
			best = c
		}
	}
	return best, nil
}
