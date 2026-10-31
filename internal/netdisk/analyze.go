package netdisk

import (
	"path"
	"strconv"
	"strings"
	"sync"

	"vrclib/internal/archive"
	"vrclib/internal/core"
	"vrclib/internal/naming"
)

// How a share's listing is read.
//
// A share holds either one product or several (a collection). What tells them apart is the folders: a folder
// that directly holds something to import or unpack — a unitypackage, an archive, an fbx — is a product's
// folder; one that holds only folders of that kind is a level above (a category folder, or the wrapper a
// seller packs everything into). The products found at the same level are then put back together where they
// are the same thing: the versions of one outfit for several base bodies, its colours, and its PSD pack are
// one product, not three (what naming.IsPartName and naming.CommonChildPrefix read out of their names).
//
// Everything here works on the listing alone, which is what the share's page gives: names, sizes and the
// shape of the folders. Nothing is downloaded to find out.

const (
	maxDepth = 6   // folder levels walked into (a share of a seller is nowhere near this deep)
	maxParts = 200 // products a share is split into at most
)

// analyzed: what the last analysis of a share found, by share id and the moment it was read. The matching
// of cards to Booth products asks for it for every card on every refresh, so it is not worked out again.
var analyzed sync.Map // string → []PanPart

func cacheKey(l *core.PanListing) string {
	return l.Surl + "@" + strconv.FormatInt(l.Fetched, 10)
}

// AnalyzePan: what a listing says about the product in it — its category, the base bodies it names, and the
// products inside when it holds several.
func AnalyzePan(l *core.PanListing, bases []naming.BaseDef) PanInfo {
	if l == nil {
		return PanInfo{}
	}
	parts := splitParts(l, bases)
	analyzed.Store(cacheKey(l), parts)
	info := PanInfo{Parts: parts}
	info.Files, info.Size = countFiles(l.Files)
	if info.Files == 0 {
		info.Files, info.Size = l.Count, l.Size
	}
	// the words the share itself offers, for the category and the base bodies: its title, and the names
	// inside it (a share of one product names it in its files, a collection in its folders)
	text := l.Title
	if len(parts) == 1 {
		text += " | " + parts[0].Name
		text += " | " + strings.Join(namesIn(l.Files, 2), " | ")
	}
	if cat := naming.BracketCategory(text); cat != "" {
		info.Category = cat
	} else if len(parts) == 1 {
		info.Category = naming.Classify(text)
	}
	info.Bases = basesIn(text, bases)
	return info
}

// SplitPanCached: the products a share holds, as the last analysis found them (one entry for a share that is
// a single product).
func SplitPanCached(l *core.PanListing) []PanPart {
	if l == nil {
		return nil
	}
	if v, ok := analyzed.Load(cacheKey(l)); ok {
		return v.([]PanPart)
	}
	parts := splitParts(l, nil)
	analyzed.Store(cacheKey(l), parts)
	return parts
}

// PanBoothID: the Booth item number a share's name or its files carry ("8099091/AONAMI_….zip"), "" when
// there is none to be sure of — no number at all, or more than one and no telling which is the share's.
//
// info.Parts nil means the caller knows the share to be a collection: a number inside it belongs to one of
// its products, so only the share's own title is read.
func PanBoothID(l *core.PanListing, info PanInfo) string {
	if l == nil {
		return ""
	}
	ids := map[string]bool{}
	add := func(s string) {
		if id := naming.BoothIDFromName(s); id != "" {
			ids[id] = true
		}
	}
	add(l.Title)
	if info.Parts != nil {
		for _, p := range info.Parts {
			add(p.Name)
			add(path.Base(p.Path))
			if p.BoothID != "" {
				ids[p.BoothID] = true
			}
		}
		for _, n := range namesIn(l.Files, maxDepth) {
			add(n)
		}
	}
	if len(ids) != 1 {
		return "" // none, or no telling which one is the share's
	}
	for id := range ids {
		return id
	}
	return ""
}

// ---------- splitting ----------

// splitParts: the products a listing holds. One entry, with an empty path, for a share that is a single
// product — that is what a card of the whole share stands for.
func splitParts(l *core.PanListing, defs []naming.BaseDef) []PanPart {
	var found []PanPart
	collect(l.Files, "", defs, 0, &found)
	found = regroup(found, defs)
	if len(found) <= 1 {
		whole := PanPart{Name: shareName(l, found)}
		if len(found) == 1 {
			whole.Category, whole.Bases, whole.BoothID = found[0].Category, found[0].Bases, found[0].BoothID
		}
		whole.Files, whole.Size = countFiles(l.Files)
		return []PanPart{whole}
	}
	if len(found) > maxParts {
		found = found[:maxParts]
	}
	return found
}

// collect walks a level of the listing: what is a product is taken, what is a level above is walked into.
func collect(children []*core.PanFile, base string, defs []naming.BaseDef, depth int, out *[]PanPart) {
	if depth > maxDepth || len(*out) > maxParts {
		return
	}
	for _, c := range children {
		if c == nil || c.Name == "" {
			continue
		}
		p := path.Join(base, c.Name)
		switch {
		case !c.Dir:
			if naming.LooseAssetExt[core.LowerExt(c.Name)] || archive.IsArchiveFile(c.Name) {
				*out = append(*out, partOf(c, p, defs))
			}
		case isProductDir(c):
			*out = append(*out, partOf(c, p, defs))
		default:
			collect(c.Children, p, defs, depth+1, out)
		}
	}
}

// isProductDir: this folder is a product's own — it holds, right in it, something that is imported or
// unpacked, or nothing but the parts of one product (its textures, its colours, its prefabs).
func isProductDir(d *core.PanFile) bool {
	if len(d.Children) == 0 {
		return false
	}
	dirs, parts := 0, 0
	for _, c := range d.Children {
		if c == nil {
			continue
		}
		if !c.Dir {
			ext := core.LowerExt(c.Name)
			if naming.StrongFileExt[ext] || naming.LooseAssetExt[ext] || archive.IsArchiveFile(c.Name) {
				return true
			}
			continue
		}
		dirs++
		if naming.IsPartName(c.Name) || naming.IsStructuralName(c.Name, naming.NormKey(d.Name)) {
			parts++
		}
	}
	return dirs > 0 && dirs == parts
}

// partOf: one product, as its folder or its file says.
func partOf(f *core.PanFile, p string, defs []naming.BaseDef) PanPart {
	name := naming.CleanName(core.StripArchiveExt(f.Name))
	if f.Dir && (naming.IsGenericName(name) || naming.IsMostlyDigits(name)) {
		// a folder named by the item number, or "更新归总": what is inside says more
		if inner := naming.CommonChildPrefix(childNames(f)); inner != "" {
			name = inner
		}
	}
	part := PanPart{Path: p, Name: name, BoothID: naming.BoothIDFromName(f.Name)}
	text := f.Name + " | " + strings.Join(namesIn(f.Children, 2), " | ")
	if cat := naming.BracketCategory(text); cat != "" {
		part.Category = cat
	} else {
		part.Category = naming.Classify(text)
	}
	part.Bases = basesIn(text, defs)
	if f.Dir {
		part.Files, part.Size = countFiles(f.Children)
	} else {
		part.Files, part.Size = 1, f.Size
	}
	return part
}

// regroup puts back together what is one product: the same outfit for several base bodies, its colours, its
// PSD pack. They lie side by side under names that share the product's own ("AONAMI_manuka.zip",
// "AONAMI_shinano.zip"), or are named only by what they are ("Eye_Blue", "テクスチャ", "PSD").
func regroup(parts []PanPart, defs []naming.BaseDef) []PanPart {
	if len(parts) < 2 {
		return parts
	}
	// the ones that only say "a part of a product" belong to the product beside them, not to a card
	var named []PanPart
	side := 0
	for _, p := range parts {
		switch {
		case naming.IsPartName(p.Name), naming.RePSDName.MatchString(strings.ToLower(p.Name)):
			side++
		default:
			named = append(named, p)
		}
	}
	if len(named) == 0 {
		return parts[:1] // nothing but parts: one product, under the folder they lie in
	}
	if len(named) == 1 && side > 0 {
		return named
	}
	parts = named
	if len(parts) < 2 {
		return parts
	}
	// one product in several versions: every name carries the product's own
	var names []string
	for _, p := range parts {
		names = append(names, p.Name)
	}
	if pre := naming.CommonChildPrefix(names); pre != "" && sameProduct(parts, pre, defs) {
		one := parts[0]
		one.Name, one.Path = pre, commonDir(parts)
		one.Bases = mergeBases(parts)
		one.Files, one.Size = 0, 0
		for _, p := range parts {
			one.Files, one.Size = one.Files+p.Files, one.Size+p.Size
		}
		return []PanPart{one}
	}
	return parts
}

// sameProduct: every one of these is the product pre in another version — its name is pre and nothing more
// than a base body, a colour or a number after it.
func sameProduct(parts []PanPart, pre string, defs []naming.BaseDef) bool {
	key := naming.NormKey(pre)
	if key == "" {
		return false
	}
	for _, p := range parts {
		k := naming.NormKey(p.Name)
		if !strings.HasPrefix(k, key) {
			return false
		}
		rest := strings.TrimPrefix(p.Name, pre)
		rest = strings.Trim(rest, " _-.·")
		if rest == "" {
			continue
		}
		// what may follow the product's name: a base body, a part word, a version number
		if len(naming.ForBases(rest, defs)) > 0 || naming.IsPartName(rest) || naming.IsGenericName(rest) {
			continue
		}
		return false
	}
	return true
}

// basesIn: the base bodies a text names — the ones the table knows, and the ones written as "For_Milfy"
// or "Milfy対応" that it does not.
func basesIn(text string, defs []naming.BaseDef) []string {
	out := naming.DetectBases(text, defs)
	for _, b := range naming.ForBases(text, defs) {
		if !core.ContainsStr(out, b) {
			out = append(out, b)
		}
	}
	return out
}

func mergeBases(parts []PanPart) []string {
	var out []string
	for _, p := range parts {
		for _, b := range p.Bases {
			if !core.ContainsStr(out, b) {
				out = append(out, b)
			}
		}
	}
	return out
}

// commonDir: the folder all of these lie in.
func commonDir(parts []PanPart) string {
	dir := path.Dir(parts[0].Path)
	for _, p := range parts[1:] {
		if path.Dir(p.Path) != dir {
			return ""
		}
	}
	if dir == "." || dir == "/" {
		return ""
	}
	return dir
}

// shareName: what to call a share that is one product — what the product's own files agree on, else the
// share's title.
func shareName(l *core.PanListing, found []PanPart) string {
	if len(found) == 1 && found[0].Name != "" && !naming.IsGenericName(found[0].Name) {
		return found[0].Name
	}
	if pre := naming.CommonChildPrefix(namesIn(l.Files, 2)); pre != "" {
		return pre
	}
	if t := naming.CleanName(l.Title); t != "" {
		return t
	}
	return l.Title
}

// ---------- walking helpers ----------

func childNames(f *core.PanFile) []string {
	var out []string
	for _, c := range f.Children {
		if c != nil {
			out = append(out, c.Name)
		}
	}
	return out
}

// namesIn: the names in a listing, down to depth levels.
func namesIn(children []*core.PanFile, depth int) []string {
	var out []string
	var walk func([]*core.PanFile, int)
	walk = func(cs []*core.PanFile, d int) {
		if d > depth || len(out) > 400 {
			return
		}
		for _, c := range cs {
			if c == nil {
				continue
			}
			out = append(out, c.Name)
			if c.Dir {
				walk(c.Children, d+1)
			}
		}
	}
	walk(children, 1)
	return out
}

func countFiles(children []*core.PanFile) (files int, size int64) {
	var walk func([]*core.PanFile, int)
	walk = func(cs []*core.PanFile, d int) {
		if d > maxDepth {
			return
		}
		for _, c := range cs {
			if c == nil {
				continue
			}
			if c.Dir {
				walk(c.Children, d+1)
				continue
			}
			files++
			size += c.Size
		}
	}
	walk(children, 0)
	return files, size
}
