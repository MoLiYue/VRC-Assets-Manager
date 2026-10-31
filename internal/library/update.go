package library

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"vrclib/internal/booth"
	"vrclib/internal/core"
	"vrclib/internal/naming"
	"vrclib/internal/netdisk"
	"vrclib/internal/translate"
)

// Spotting that an asset has a newer version.
//
// What a purchase offers is its file list, as the store's own library page gives it; what the asset holds is
// what lies in its folders. A file of the purchase that is nowhere on disk is new — compared by the name
// without its archive extension, because a zip that was unpacked leaves a folder of its name behind.
//
// A file whose name is the same product at another version ("Kaguya_v1.06" → "Kaguya_v1.07") says where the
// version on disk is: that folder is what the player may send to the Recycle Bin once the new one is in.
//
// The player can also say "已是最新" of a card, which writes the names down as seen (updates-seen.json) so
// that they are not offered again.

var seen struct {
	mu  sync.Mutex
	m   map[string][]string // card key → the file names already dealt with
	dir string              // the data folder it was read from ("" = not read yet)
}

func seenFile() string { return filepath.Join(core.DataDir, "updates-seen.json") }

// seenLoad reads the file, and reads it again when the data folder has changed (a library brought in from
// another computer, or a test with a folder of its own).
func seenLoad() {
	if seen.dir == core.DataDir && seen.m != nil {
		return
	}
	seen.dir, seen.m = core.DataDir, map[string][]string{}
	if b, err := core.ReadDataFile(seenFile()); err == nil {
		_ = json.Unmarshal(b, &seen.m)
	}
}

func seenOf(key string) []string {
	seen.mu.Lock()
	defer seen.mu.Unlock()
	seenLoad()
	return seen.m[key]
}

func seenPut(key string, files []string) {
	seen.mu.Lock()
	defer seen.mu.Unlock()
	seenLoad()
	for _, f := range files {
		if !core.ContainsStr(seen.m[key], f) {
			seen.m[key] = append(seen.m[key], f)
		}
	}
	if b, err := json.Marshal(seen.m); err == nil {
		_ = os.WriteFile(seenFile(), b, 0644)
	}
}

// UpdateNotes: what is new for each card that has something new, by key. The store is not held by the
// caller (the folders are read).
func UpdateNotes(st *core.Store) map[string]*UpdateNote {
	out := map[string]*UpdateNote{}
	st.Mu.RLock()
	type want struct {
		key, name string
		id        string
		dirs      []string
	}
	var list []want
	for _, a := range st.Assets {
		id, _ := booth.AssetBooth(st, a.Key, a)
		if id == "" || st.Purchases[id] == nil {
			continue
		}
		w := want{key: a.Key, name: a.Name, id: id}
		for _, l := range a.Locations {
			w.dirs = append(w.dirs, l.Path)
		}
		list = append(list, w)
	}
	purchases := map[string]*core.Purchase{}
	for id, p := range st.Purchases {
		purchases[id] = p
	}
	st.Mu.RUnlock()

	for _, w := range list {
		p := purchases[w.id]
		if len(p.Files) == 0 {
			continue
		}
		have := onDisk(w.dirs)
		var added, replaced []string
		for _, f := range p.Files {
			stem := strings.ToLower(core.StripArchiveExt(filepath.Base(f)))
			if stem == "" || have[stem] {
				continue
			}
			if core.ContainsStr(seenOf(w.key), f) {
				continue // 「已是最新」
			}
			if older(stem, have) {
				replaced = append(replaced, f)
			} else {
				added = append(added, f)
			}
		}
		if len(added) == 0 && len(replaced) == 0 {
			continue
		}
		n := &UpdateNote{Key: w.key, Name: w.name, Added: added, Replaced: replaced,
			Item: w.id, DLs: p.Downloads, CanDL: len(p.Downloads) > 0, Found: time.Now().Unix(),
			Source: "booth", Gumroad: core.IsGumID(w.id)}
		if n.Gumroad {
			n.Source = "gumroad"
		}
		n.Version = versionOf(append(added, replaced...))
		n.Old = oldCopies(w.dirs, append(added, replaced...))
		out[w.key] = n
	}
	// a share that was downloaded into an asset's folder: what it offers now against what lies there. A
	// share not downloaded yet is a card of its own and shows its listing, so there is nothing to tell.
	for _, key := range netdisk.PanCardKeys(st) {
		st.Mu.RLock()
		u, l := st.User[key], st.Pan[netdisk.ShareKey(key)]
		var into string
		if u != nil {
			into = u.Downloaded
		}
		st.Mu.RUnlock()
		if u == nil || l == nil || into == "" || len(l.Files) == 0 {
			continue
		}
		added, changed := diffShare(l.Files, into)
		paths := append(added, changed...)
		if len(paths) == 0 {
			continue
		}
		noteKey, name := cardAt(st, into, key, u.Name)
		var left []string
		for _, p := range paths {
			if !core.ContainsStr(seenOf(noteKey), p) {
				left = append(left, p)
			}
		}
		if len(left) == 0 {
			continue
		}
		found := l.Changed
		if found == 0 {
			found = l.Fetched
		}
		out[noteKey] = &UpdateNote{Key: noteKey, Name: name, Added: added, Replaced: changed,
			Source: shareSource(key), PanKey: key, Paths: left, CanDL: u.ShareURL != "", Found: found}
	}
	// a share the player linked to an asset by hand: there is no card of the share to download, so the new
	// files are the player's own to fetch. It is still worth saying that there are some.
	for _, w := range list2(st) {
		if out[w.key] != nil || w.share == "" {
			continue
		}
		st.Mu.RLock()
		l := st.Pan[netdisk.ShareID(w.share)]
		st.Mu.RUnlock()
		if l == nil || len(l.Files) == 0 {
			continue
		}
		added, changed := diffShareDirs(l.Files, w.dirs)
		if len(added)+len(changed) == 0 {
			continue
		}
		found := l.Changed
		if found == 0 {
			found = l.Fetched
		}
		out[w.key] = &UpdateNote{Key: w.key, Name: w.name, Added: added, Replaced: changed,
			Source: "pan", Paths: append(added, changed...), CanDL: false, Found: found}
	}
	return out
}

// want2: an asset and the share the player linked to it by hand.
type want2 struct {
	key, name, share string
	dirs             []string
}

func list2(st *core.Store) []want2 {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var out []want2
	for _, a := range st.Assets {
		u := st.User[a.Key]
		if u == nil || u.ShareURL == "" {
			continue
		}
		w := want2{key: a.Key, name: a.Name, share: u.ShareURL}
		for _, l := range a.Locations {
			w.dirs = append(w.dirs, l.Path)
		}
		out = append(out, w)
	}
	return out
}

// diffShareDirs is diffShare over several folders of one asset.
func diffShareDirs(files []*core.PanFile, dirs []string) (added, changed []string) {
	have := map[string]int64{}
	for _, d := range dirs {
		for n, s := range sizesIn(d) {
			if s > have[n] {
				have[n] = s
			}
		}
	}
	return diffAgainst(files, have)
}

// diffShare: of what a share offers, the files that are not in the folder it was downloaded into (added)
// and the ones there in another size (changed). The paths are the share's own, as a download asks for them.
func diffShare(files []*core.PanFile, into string) (added, changed []string) {
	return diffAgainst(files, sizesIn(into))
}

// diffAgainst compares what a share offers with the names and sizes already there.
func diffAgainst(files []*core.PanFile, have map[string]int64) (added, changed []string) {
	var walk func([]*core.PanFile, string, int)
	walk = func(cs []*core.PanFile, at string, depth int) {
		if depth > scanMaxDepth {
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
			size, ok := have[strings.ToLower(c.Name)]
			switch {
			case !ok:
				added = append(added, p)
			case c.Size > 0 && size != c.Size:
				changed = append(changed, p)
			}
		}
	}
	walk(files, "", 0)
	return added, changed
}

// sizesIn: the files of a folder by lower-case name, with their sizes. A name that is there more than once
// keeps the largest (a download beside what was unpacked out of it).
func sizesIn(dir string) map[string]int64 {
	out := map[string]int64{}
	var walk func(string, int)
	walk = func(d string, depth int) {
		if depth > scanMaxDepth {
			return
		}
		ents, err := os.ReadDir(d)
		if err != nil {
			return
		}
		for _, e := range ents {
			if e.IsDir() {
				walk(filepath.Join(d, e.Name()), depth+1)
				continue
			}
			if fi, err := e.Info(); err == nil {
				n := strings.ToLower(e.Name())
				if fi.Size() > out[n] {
					out[n] = fi.Size()
				}
			}
		}
	}
	walk(dir, 0)
	return out
}

// cardAt: the card a downloaded share belongs to — the asset whose folder it went into, or the share's own
// card when the library does not hold it (yet).
func cardAt(st *core.Store, dir, shareKey, shareName string) (key, name string) {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	for _, a := range st.Assets {
		for _, l := range a.Locations {
			if core.PathKey(l.Path) == core.PathKey(dir) || under(filepath.Clean(dir), filepath.Clean(l.Path)) {
				return a.Key, a.Name
			}
		}
	}
	if shareName == "" {
		shareName = filepath.Base(dir)
	}
	return shareKey, shareName
}

// onDisk: the names that lie in an asset's folders, without their archive extensions.
func onDisk(dirs []string) map[string]bool {
	out := map[string]bool{}
	for _, d := range dirs {
		var walk func(string, int)
		walk = func(p string, depth int) {
			if depth > 4 {
				return
			}
			ents, err := os.ReadDir(p)
			if err != nil {
				// a single file is a name of its own
				out[strings.ToLower(core.StripArchiveExt(filepath.Base(p)))] = true
				return
			}
			for _, e := range ents {
				out[strings.ToLower(core.StripArchiveExt(e.Name()))] = true
				if e.IsDir() {
					walk(filepath.Join(p, e.Name()), depth+1)
				}
			}
		}
		out[strings.ToLower(core.StripArchiveExt(filepath.Base(d)))] = true
		walk(d, 0)
	}
	return out
}

// older: something on disk is the same thing at another version ("kaguya_v1.06" beside "kaguya_v1.07").
func older(stem string, have map[string]bool) bool {
	base := trimVersion(stem)
	if base == stem || base == "" {
		return false
	}
	for h := range have {
		if h != stem && trimVersion(h) == base {
			return true
		}
	}
	return false
}

// trimVersion: a name without what makes it a version ("Kaguya_v1.06" → "kaguya", "Dress 1.2" → "dress").
func trimVersion(s string) string {
	toks := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return r == ' ' || r == '_' || r == '-' || r == '.'
	})
	for len(toks) > 0 && translate.ReVersionTok.MatchString(toks[len(toks)-1]) {
		toks = toks[:len(toks)-1]
	}
	return strings.Join(toks, " ")
}

// versionOf: what the new files call themselves, when they agree on it.
func versionOf(files []string) string {
	for _, f := range files {
		toks := strings.FieldsFunc(core.StripArchiveExt(filepath.Base(f)), func(r rune) bool {
			return r == ' ' || r == '_' || r == '-'
		})
		for i := len(toks) - 1; i >= 0; i-- {
			if translate.ReVersionTok.MatchString(strings.ToLower(toks[i])) {
				// "v1.07" and "ver1.07" are the same version as "1.07": the number is what it is called by
				t := toks[i]
				for _, p := range []string{"version", "ver", "v", "r"} {
					if len(t) > len(p) && strings.EqualFold(t[:len(p)], p) && t[len(p)] >= '0' && t[len(p)] <= '9' {
						t = t[len(p):]
						break
					}
				}
				return t
			}
		}
	}
	return ""
}

// oldCopies: the folders of the asset that hold the version the new files replace.
func oldCopies(dirs []string, files []string) []OldCopy {
	var bases []string
	for _, f := range files {
		if b := trimVersion(strings.ToLower(core.StripArchiveExt(filepath.Base(f)))); b != "" {
			bases = append(bases, b)
		}
	}
	if len(bases) == 0 {
		return nil
	}
	var out []OldCopy
	add := func(p string) {
		fi, err := os.Stat(p)
		if err != nil {
			return
		}
		o := OldCopy{Path: p, Name: filepath.Base(p)}
		if fi.IsDir() {
			files, size, _ := walkFiles(p)
			o.Files, o.Size = len(files), size
		} else {
			o.Files, o.Size = 1, fi.Size()
		}
		out = append(out, o)
	}
	for _, d := range dirs {
		if core.ContainsStr(bases, trimVersion(strings.ToLower(filepath.Base(d)))) {
			add(d)
			continue
		}
		ents, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if core.ContainsStr(bases, trimVersion(strings.ToLower(core.StripArchiveExt(e.Name())))) {
				add(filepath.Join(d, e.Name()))
			}
		}
	}
	return out
}

func shareSource(key string) string {
	switch {
	case strings.HasPrefix(key, "gd:"):
		return "gd"
	case strings.HasPrefix(key, "db:"):
		return "db"
	}
	return "pan"
}

// AckUpdate: the player has the new files, or does not want them (files: only those, nil = all of them).
func AckUpdate(st *core.Store, key string, files []string) {
	if len(files) == 0 {
		if n := UpdateNotes(st)[key]; n != nil {
			files = append(append([]string{}, n.Added...), n.Replaced...)
		}
	}
	if len(files) > 0 {
		seenPut(key, files)
	}
	st.Mu.Lock()
	if core.IsNetdiskKey(key) {
		if u := st.User[key]; u != nil {
			if l := st.Pan[netdisk.ShareKey(key)]; l != nil && l.Changed > u.PanSeen {
				u.PanSeen = l.Changed
			}
		}
	}
	st.Mu.Unlock()
	_ = st.Save()
	core.BumpRev()
}

// PanFetched: these paths of a share were downloaded, so what the share holds now is accounted for.
func PanFetched(st *core.Store, key string, paths []string) {
	st.Mu.Lock()
	if u := st.User[key]; u != nil {
		if l := st.Pan[netdisk.ShareKey(key)]; l != nil {
			u.PanSeen = l.Fetched
		}
		for _, p := range paths {
			if !core.ContainsStr(u.PanGot, p) {
				u.PanGot = append(u.PanGot, p)
			}
		}
		u.Updated = time.Now().Unix()
	}
	st.Mu.Unlock()
	_ = st.Save()
	core.BumpRev()
}

// CheckRemovable: whether a folder may be moved to the Recycle Bin — it is inside one of allowed, is not one
// of them itself, holds none of keep, and no Unity project lives in it.
func CheckRemovable(path string, allowed, projects, keep []string) error {
	p := filepath.Clean(path)
	if !core.StatOK(p) {
		return errors.New("文件夹已不存在")
	}
	inside := false
	for _, a := range allowed {
		a = filepath.Clean(a)
		if core.PathKey(p) == core.PathKey(a) {
			return errors.New("这是素材文件夹本身")
		}
		if under(p, a) {
			inside = true
		}
	}
	if !inside {
		return errors.New("不在素材文件夹内")
	}
	for _, k := range keep {
		if k == "" {
			continue
		}
		if under(filepath.Clean(k), p) || core.PathKey(filepath.Clean(k)) == core.PathKey(p) {
			return errors.New("新版本的文件在其中")
		}
	}
	for _, pr := range projects {
		if pr == "" {
			continue
		}
		pr = filepath.Clean(pr)
		if under(pr, p) || core.PathKey(pr) == core.PathKey(p) || under(p, pr) {
			return errors.New("其中有 Unity 工程")
		}
	}
	if hasProject(p, 0) {
		return errors.New("其中有 Unity 工程")
	}
	return nil
}

// under: p lies inside dir.
func under(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	if err != nil {
		return false
	}
	return rel != "." && !strings.HasPrefix(rel, "..")
}

func hasProject(dir string, depth int) bool {
	if depth > 3 {
		return false
	}
	if core.IsUnityProject(dir) {
		return true
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range ents {
		if e.IsDir() && !naming.IsStructuralName(e.Name(), "") && hasProject(filepath.Join(dir, e.Name()), depth+1) {
			return true
		}
	}
	return false
}
