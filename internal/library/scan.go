package library

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"vrclib/internal/archive"
	"vrclib/internal/core"
	"vrclib/internal/naming"
)

// Scanning the asset folders.
//
// What makes a card is a product folder: one that directly holds something to import or unpack — a
// unitypackage, an archive, an fbx — or holds nothing but the parts of one product (its textures, its
// colours, its prefabs). A folder above those is walked into instead: that is how a folder of categories
// ("衣服/", "头发/") or the folder a seller packed everything into does not become a card of its own.
//
// The same product found in several folders (an older version kept beside the new one, a copy on another
// disk) is one card with several places, unless the player asked for them to stay apart (NoGroup).
//
// What the player decided by hand comes first: Overrides says of a folder that it is to be left out
// ("ignore"), that it is one product however it looks ("single"), or that what is in it are products of
// their own ("split").

const (
	scanMaxDepth = 8
	scanMaxCards = 20000
)

// skipDirs: folders that are never walked into — ours, and the system's.
var skipDirs = map[string]bool{
	"covers": true, "booth-profile": true, "webview": true, "$recycle.bin": true, "system volume information": true,
	"node_modules": true, ".git": true, ".svn": true,
}

// managerAssets: when dir is another asset manager's own folder, the folder inside it that holds the
// assets — the rest (its database, its backups, its pictures) is not walked into. "" for any other folder.
//
//	Avatar Explorer V1: <program>/Datas/ItemsData.json, its assets in Datas/Items
//	Avatar Explorer V2: <data>/database/items.json, its assets in Items
//	KonoAsset: <data>/metadata/avatars.json, its assets in data
func managerAssets(dir string) (string, bool) {
	switch {
	case core.StatOK(filepath.Join(dir, "Datas", "ItemsData.json")):
		return filepath.Join(dir, "Datas", "Items"), true
	case core.StatOK(filepath.Join(dir, "database", "items.json")):
		return filepath.Join(dir, "Items"), true
	case core.StatOK(filepath.Join(dir, "metadata", "avatars.json")):
		return filepath.Join(dir, "data"), true
	}
	return "", false
}

// RunFolderScan walks the asset folders and writes what it finds into the store.
func RunFolderScan(st *core.Store, prog *core.Task) {
	st.Mu.RLock()
	roots := append([]string{}, st.Settings.Roots...)
	overrides := map[string]string{}
	for k, v := range st.Overrides {
		overrides[k] = v
	}
	noGroup := map[string]bool{}
	for k, u := range st.User {
		if u.NoGroup {
			noGroup[k] = true
		}
	}
	st.Mu.RUnlock()

	var found []*core.Asset
	var warns []string
	for i, root := range roots {
		if core.Quitting.Load() {
			return
		}
		prog.Set(i, len(roots), "正在扫描 "+root)
		if !core.StatOK(root) {
			warns = append(warns, "素材文件夹不存在："+root)
			continue
		}
		found = append(found, scanRoot(root, overrides, prog)...)
		if len(found) > scanMaxCards {
			warns = append(warns, "素材数量过多，已停止扫描（上限 "+itoa(scanMaxCards)+"）")
			break
		}
	}
	assets := mergeSame(found, noGroup)

	now := time.Now().Unix()
	st.Mu.Lock()
	for _, a := range assets {
		if st.FirstSeen[a.Key] == 0 {
			st.FirstSeen[a.Key] = now
		}
		a.FirstSeen = st.FirstSeen[a.Key]
	}
	st.Assets = assets
	st.LastScan = now
	st.Warnings = warns
	st.Mu.Unlock()
	_ = st.Save()
	core.BumpRev()
	prog.Set(len(roots), len(roots), "完成")
}

// scanRoot walks one asset folder.
func scanRoot(root string, overrides map[string]string, prog *core.Task) []*core.Asset {
	var out []*core.Asset
	var walk func(dir string, depth int, splitting bool)
	walk = func(dir string, depth int, splitting bool) {
		if depth > scanMaxDepth || core.Quitting.Load() || len(out) > scanMaxCards {
			return
		}
		ents, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		mode := overrides[core.PathKey(dir)]
		if mode == "ignore" {
			return
		}
		// what lies right here
		var dirs []os.DirEntry
		var files []os.DirEntry
		for _, e := range ents {
			name := e.Name()
			if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "~") {
				continue
			}
			if e.IsDir() {
				if skipDirs[strings.ToLower(name)] {
					continue
				}
				dirs = append(dirs, e)
				continue
			}
			files = append(files, e)
		}
		if items, ok := managerAssets(dir); ok {
			// another manager's folder: only its assets, as any folder of assets (its records are taken over
			// separately, by 「导入素材信息」)
			if core.StatOK(items) && core.PathKey(items) != core.PathKey(dir) {
				walk(items, depth+1, false)
			}
			return
		}
		// a loose file that is an asset on its own ("Dress.unitypackage" lying in the asset folder). A set of
		// volumes is one card: the part that opens it stands for the set.
		if splitting || dir == root || !isProductDir(dir, dirs, files) {
			var loose []string
			for _, f := range files {
				if naming.LooseAssetExt[core.LowerExt(f.Name())] || archive.IsArchiveFile(f.Name()) {
					loose = append(loose, filepath.Join(dir, f.Name()))
				}
			}
			for _, p := range firstOfSets(loose) {
				out = append(out, fileAsset(root, p))
			}
		}
		for _, d := range dirs {
			p := filepath.Join(dir, d.Name())
			sub := overrides[core.PathKey(p)]
			if sub == "ignore" {
				continue
			}
			if core.IsUnityProject(p) {
				continue // a Unity project is not an asset
			}
			if items, ok := managerAssets(p); ok {
				// another manager's folder: only its folder of assets is walked, as any folder of assets
				if core.StatOK(items) {
					walk(items, depth+1, false)
				}
				continue
			}
			kids, kidFiles := readKids(p)
			switch {
			case sub == "bundle", sub == "split", sub == "single":
				// the player (or a split) said what is in it are products of their own
				walk(p, depth+1, true)
			case isProductDir(p, kids, kidFiles):
				prog.Set(len(out), len(out)+1, d.Name())
				a := dirAsset(root, p)
				if sub != "asset" && !splitting {
					// its products may still be inside an archive, which the listing tells
					a.Bundle, a.BundlePacked = packedBundle(p)
				}
				out = append(out, a)
			case splitting:
				walk(p, depth+1, true) // a level of a collection being split: walked through
			default:
				// a folder above the products: one card, marked as a collection when the shape of it
				// says so clearly enough
				prog.Set(len(out), len(out)+1, d.Name())
				a := dirAsset(root, p)
				if sub != "asset" {
					a.Bundle, a.BundlePacked = bundleOf(p, overrides)
				}
				out = append(out, a)
			}
		}
	}
	walk(root, 0, false)
	return out
}

// firstOfSets: of the files given, the ones that stand for themselves — one per set of volumes.
func firstOfSets(paths []string) []string {
	if len(paths) < 2 {
		return paths
	}
	keep := map[string]bool{}
	for _, s := range archive.Sets(paths) {
		keep[s.Main] = true
	}
	var out []string
	for _, p := range paths {
		if !archive.IsArchiveFile(p) || keep[p] {
			out = append(out, p)
		}
	}
	return out
}

func readKids(dir string) (dirs, files []os.DirEntry) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if e.IsDir() {
			if !skipDirs[strings.ToLower(e.Name())] {
				dirs = append(dirs, e)
			}
			continue
		}
		files = append(files, e)
	}
	return dirs, files
}

// isProductDir: this folder is a product's own — it holds, right in it, something that is imported or
// unpacked, or nothing but the parts of one product.
func isProductDir(dir string, dirs, files []os.DirEntry) bool {
	for _, f := range files {
		ext := core.LowerExt(f.Name())
		if naming.StrongFileExt[ext] || naming.LooseAssetExt[ext] || archive.IsArchiveFile(f.Name()) {
			return true
		}
	}
	if len(dirs) == 0 {
		return false
	}
	key := naming.NormKey(filepath.Base(dir))
	for _, d := range dirs {
		if !naming.IsPartName(d.Name()) && !naming.IsStructuralName(d.Name(), key) {
			return false
		}
	}
	return true
}

// dirAsset: the card a product folder makes.
func dirAsset(root, dir string) *core.Asset {
	a := &core.Asset{RawName: filepath.Base(dir), HasDir: true}
	a.Name = naming.CleanName(a.RawName)
	files, size, mtime := walkFiles(dir)
	if naming.IsGenericName(a.Name) || naming.IsMostlyDigits(a.Name) {
		if inner := naming.CommonChildPrefix(baseNames(files)); inner != "" {
			a.Name = inner
		}
	}
	a.Locations = []core.Location{{Path: dir, Kind: "dir", Size: size, Root: root}}
	a.Size, a.Files, a.MTime = size, len(files), mtime
	fill(a, dir, files)
	a.Key, a.AltKey = cardKeyOf(a.Name, dir)
	return a
}

// fileAsset: the card a single file makes.
func fileAsset(root, p string) *core.Asset {
	a := &core.Asset{RawName: filepath.Base(p)}
	a.Name = naming.CleanName(core.StripArchiveExt(a.RawName))
	kind := strings.TrimPrefix(core.LowerExt(p), ".")
	if kind == "" {
		kind = "file"
	}
	size, mtime := int64(0), int64(0)
	if fi, err := os.Stat(p); err == nil {
		size, mtime = fi.Size(), fi.ModTime().Unix()
	}
	a.Locations = []core.Location{{Path: p, Kind: kind, Size: size, Root: root}}
	a.Size, a.Files, a.MTime = size, 1, mtime
	fill(a, filepath.Dir(p), []string{p})
	a.Key, a.AltKey = cardKeyOf(a.Name, p)
	return a
}

// fill works out what a card is of: its category, the base bodies it fits, and what kind of files it holds.
func fill(a *core.Asset, dir string, files []string) {
	text := a.RawName + " | " + strings.Join(baseNames(files), " | ")
	if cat := naming.BracketCategory(a.RawName); cat != "" {
		a.Category = cat
	} else {
		a.Category = naming.Classify(text)
	}
	defs := naming.ParseBases(nil)
	a.Bases = naming.DetectBases(text, defs)
	for _, b := range naming.ForBases(text, defs) {
		if !core.ContainsStr(a.Bases, b) {
			a.Bases = append(a.Bases, b)
		}
	}
	for _, f := range files {
		rel := relOf(dir, f)
		switch ext := core.LowerExt(f); {
		case ext == ".unitypackage":
			a.Packages = append(a.Packages, rel)
		case archive.IsArchiveFile(f):
			a.Archives = append(a.Archives, rel)
		case ImageExt[ext]:
			a.Covers = append(a.Covers, f)
		case naming.PSDExt[ext]:
			a.PSDs = append(a.PSDs, core.PSDFile{Path: rel})
			a.PSDCount++
		case ext == ".fbx" || ext == ".blend":
			a.ModelFiles++
		}
	}
	sort.Strings(a.Packages)
	sort.Strings(a.Archives)
	sort.Strings(a.Covers)
}

// cardKeyOf: a card answers to its product name, and to the place it was found at — which is the key an
// earlier version of the program wrote down, and what keeps the player's notes with the folder when two
// products of the same name are told apart.
func cardKeyOf(name, path string) (key, alt string) {
	k := naming.NormKey(name)
	if k == "" {
		k = naming.NormKey(filepath.Base(path))
	}
	return "name:" + k, "path:" + core.PathKey(path)
}

// mergeSame puts the same product found in several folders into one card.
func mergeSame(found []*core.Asset, noGroup map[string]bool) []*core.Asset {
	var out []*core.Asset
	at := map[string]*core.Asset{}
	for _, a := range found {
		prev, ok := at[a.Key]
		if !ok || noGroup[a.Key] || noGroup[a.AltKey] || prev.Category != a.Category {
			if ok { // the same name, kept apart: the place makes the key
				a.Key = a.AltKey
			}
			at[a.Key] = a
			out = append(out, a)
			continue
		}
		prev.Locations = append(prev.Locations, a.Locations...)
		prev.Size += a.Size
		prev.Files += a.Files
		prev.Packages = append(prev.Packages, a.Packages...)
		prev.Archives = append(prev.Archives, a.Archives...)
		prev.Covers = append(prev.Covers, a.Covers...)
		prev.PSDs = append(prev.PSDs, a.PSDs...)
		prev.PSDCount += a.PSDCount
		prev.ModelFiles += a.ModelFiles
		if a.MTime > prev.MTime {
			prev.MTime = a.MTime
		}
		for _, b := range a.Bases {
			if !core.ContainsStr(prev.Bases, b) {
				prev.Bases = append(prev.Bases, b)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ---------- walking ----------

func walkFiles(dir string) (files []string, size int64, mtime int64) {
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
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			p := filepath.Join(d, e.Name())
			if e.IsDir() {
				if !skipDirs[strings.ToLower(e.Name())] {
					walk(p, depth+1)
				}
				continue
			}
			files = append(files, p)
			if fi, err := e.Info(); err == nil {
				size += fi.Size()
				if t := fi.ModTime().Unix(); t > mtime {
					mtime = t
				}
			}
		}
	}
	walk(dir, 0)
	sort.Strings(files)
	return files, size, mtime
}

func baseNames(files []string) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, filepath.Base(f))
	}
	return out
}

func relOf(dir, p string) string {
	if r, err := filepath.Rel(dir, p); err == nil {
		return filepath.ToSlash(r)
	}
	return filepath.Base(p)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
