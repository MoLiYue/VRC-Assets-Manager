package library

import (
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"vrclib/internal/archive"
	"vrclib/internal/core"
	"vrclib/internal/naming"
)

// Collections (合集包).
//
// A folder that holds several products — a download of a whole shop's output, or one a seller packed by
// category — is one card to begin with, marked "合集 · N". It is not imported as it is; the player splits it,
// and then each product is a card of its own.
//
// Splitting writes a mark on the folder levels that hold the products ("bundle" in Overrides), which the
// scan then walks through. The marks are also what 恢复 undoes: taking the mark off a level takes the ones
// below it too, so one 恢复 on the outermost makes the folder one card again.
//
// How sure the scan has to be before it calls a folder a collection by itself: four products, or two with
// the seller's own category folders around them. Three products and a readme is not enough to go by — that
// is what a single product with its extras looks like.
const (
	bundleSure = 4 // products that make a collection on their own
)

// surveyBundle: the products a container folder holds, how deep they lie, and whether some of them are
// still inside archives. The levels that hold products come back too, deepest last.
func surveyBundle(dir string, overrides map[string]string) (products int, packed bool, cats bool, levels []string) {
	type level struct {
		path     string
		products int
	}
	byLevel := map[string]int{}
	var walk func(string, int)
	walk = func(d string, depth int) {
		if depth > scanMaxDepth {
			return
		}
		if overrides[core.PathKey(d)] == "ignore" {
			return
		}
		dirs, files := readKids(d)
		for _, f := range files {
			if archive.IsArchiveFile(f.Name()) {
				if _, rest, _ := archive.VolumeName(f.Name()); rest == "" {
					packed = true
				}
			}
		}
		for _, sub := range dirs {
			p := filepath.Join(d, sub.Name())
			if core.IsUnityProject(p) {
				continue
			}
			kids, kidFiles := readKids(p)
			if isProductDir(p, kids, kidFiles) {
				products++
				byLevel[d]++
				if naming.IsCategoryWordName(filepath.Base(d)) {
					cats = true
				}
				continue
			}
			walk(p, depth+1)
		}
	}
	walk(dir, 0)
	// the level a product lies in counts what lies below it too, so that an outer level is marked as well
	var all []level
	for p, n := range byLevel {
		all = append(all, level{p, n})
	}
	sort.Slice(all, func(i, j int) bool { return len(all[i].path) < len(all[j].path) })
	for _, l := range all {
		n := 0
		for _, o := range all {
			if o.path == l.path || strings.HasPrefix(o.path, l.path+string(filepath.Separator)) {
				n += o.products
			}
		}
		_ = n
	}
	return products, packed, cats, levels
}

// packedBundle: a folder whose products are still inside an archive. What the archive holds is read from
// its listing — nothing is unpacked to find out — and an archive inside an archive is a product of its own
// (that is how a seller packs a collection: one zip of one zip per product).
func packedBundle(dir string) (int, bool) {
	dirs, files := readKids(dir)
	_ = dirs
	var arcs []string
	for _, f := range files {
		if archive.IsArchiveFile(f.Name()) {
			arcs = append(arcs, filepath.Join(dir, f.Name()))
		}
	}
	if len(arcs) == 0 {
		return 0, false
	}
	n := 0
	for _, set := range archive.Sets(arcs) {
		ents, err := archive.ListEntries(set)
		if err != nil {
			continue // rar and 7z without a tool: what is in them shows after the import unpacks it
		}
		for _, e := range ents {
			name := path.Base(filepath.ToSlash(e.Name))
			if strings.HasPrefix(name, ".") || strings.HasPrefix(filepath.ToSlash(e.Name), "__MACOSX") {
				continue
			}
			if archive.IsArchiveFile(name) {
				if _, rest, _ := archive.VolumeName(name); rest != "" {
					n++
				}
			}
		}
	}
	if n >= 2 {
		return n, true
	}
	return 0, false
}

// bundleOf: what the scan makes of a container folder — the number of products when it is sure enough that
// this is a collection, and 0 when it is to be one card as it is.
func bundleOf(dir string, overrides map[string]string) (int, bool) {
	n, packed, cats, _ := surveyBundle(dir, overrides)
	if n >= bundleSure || (n >= 2 && cats) {
		return n, packed
	}
	return 0, false
}

// MarkBundles marks the folder levels of a collection, so that the next scan makes a card for each product.
// The folder itself is marked too, unless the player has already said what it is.
func MarkBundles(st *core.Store, dir string, only []string) BundleInfo {
	st.Mu.Lock()
	overrides := map[string]string{}
	for k, v := range st.Overrides {
		overrides[k] = v
	}
	st.Mu.Unlock()
	n, _, _, _ := surveyBundle(dir, overrides)
	info := BundleInfo{Products: n}
	if n == 0 {
		return info
	}
	st.Mu.Lock()
	defer st.Mu.Unlock()
	mark := func(p string) {
		k := core.PathKey(p)
		if len(only) > 0 && !core.ContainsStr(only, p) {
			return
		}
		if st.Overrides[k] == "" { // what the player said of it stands ("split", "asset")
			st.Overrides[k] = "bundle"
		}
		if !core.ContainsStr(info.Containers, p) {
			info.Containers = append(info.Containers, p)
		}
	}
	// the levels marked are the way down to the products: this folder, and under it the one child that
	// holds most of them, until a level holds them itself. A sibling that holds a few is left alone — the
	// scan walks through every level below a marked one anyway.
	for cur, i := dir, 0; i < scanMaxDepth; i++ {
		mark(cur)
		kids, _ := readKids(cur)
		best, bestN := "", 0
		for _, k := range kids {
			p := filepath.Join(cur, k.Name())
			ds, fs := readKids(p)
			if isProductDir(p, ds, fs) || core.IsUnityProject(p) {
				continue
			}
			if m, _, _, _ := surveyBundle(p, overrides); m > bestN {
				best, bestN = p, m
			}
		}
		if best == "" || bestN < 2 {
			break
		}
		cur = best
	}
	core.Logf("合集拆分：%s 共 %d 个素材，标记 %d 层", dir, n, len(info.Containers))
	return info
}

// ForgetBundles drops the marks of a folder and of everything below it, so that it is one card again. The
// store is held by the caller.
func ForgetBundles(st *core.Store, path string) {
	k := core.PathKey(path)
	for key, mode := range st.Overrides {
		if mode != "bundle" {
			continue
		}
		if key == k || strings.HasPrefix(key, k+string(filepath.Separator)) || strings.HasPrefix(key, k+"/") {
			delete(st.Overrides, key)
		}
	}
}

// OpenWrappers marks away the folder levels that only wrap what is inside them (the folder a download's
// archive unpacked into, and the one inside that), down to the first that holds more than one thing.
func OpenWrappers(st *core.Store, path string) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	p := path
	for i := 0; i < scanMaxDepth; i++ {
		dirs, files := readKids(p)
		if len(files) > 0 || len(dirs) != 1 {
			return
		}
		p = filepath.Join(p, dirs[0].Name())
		kids, kidFiles := readKids(p)
		if isProductDir(p, kids, kidFiles) {
			return // what is inside is a product: the wrapper above it is the card
		}
		if st.Overrides[core.PathKey(p)] == "" {
			st.Overrides[core.PathKey(p)] = "bundle"
		}
	}
}

// bundleView: what a card says about being a collection — what the scan found, unless the player has said
// the folder is one asset (which takes effect before the next scan).
func bundleView(st *core.Store, a *core.Asset) (int, bool) {
	for _, l := range a.Locations {
		if st.Overrides[core.PathKey(l.Path)] == "asset" {
			return 0, false
		}
	}
	return a.Bundle, a.BundlePacked
}

// splitPacked: a collection whose products are still inside archives has to be unpacked before it can be
// split, which is a job of its own.
func splitPacked(dir string) bool {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range ents {
		if !e.IsDir() && archive.IsArchiveFile(e.Name()) {
			return true
		}
	}
	return false
}
