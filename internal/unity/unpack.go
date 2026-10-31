package unity

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"vrclib/internal/core"
)

// Taking a unitypackage apart.
//
// A unitypackage is a gzipped tar in which every asset has a folder named after its GUID holding
// "pathname" (where it goes in the project), "asset" (the file itself), "asset.meta" (what Unity keeps
// beside it) and sometimes "preview.png". A folder without "asset" is a folder of the project, which is
// made when something goes into it.
//
// A file that is there already is only replaced where this program put it: the GUID in the .meta beside it
// is the one the package carries. Anything else is left alone and the new file gets another name, which is
// what Unity itself does when a package is dragged in.

// pkgEntry: one asset of a package.
type pkgEntry struct {
	GUID    string
	Path    string // "Assets/Shop/Item/file.fbx"
	Asset   []byte
	Meta    []byte
	HasData bool
}

// readPackage reads the entries of a unitypackage.
func readPackage(p string) ([]pkgEntry, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, errors.New("不是有效的 unitypackage（无法解压）")
	}
	defer zr.Close()
	at := map[string]*pkgEntry{}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		name := path.Clean(strings.TrimPrefix(filepath.ToSlash(h.Name), "./"))
		guid, what := path.Split(name)
		guid = strings.Trim(guid, "/")
		if guid == "" || strings.Contains(guid, "/") {
			continue // not <guid>/<what>
		}
		e := at[guid]
		if e == nil {
			e = &pkgEntry{GUID: guid}
			at[guid] = e
		}
		switch what {
		case "pathname":
			b, err := io.ReadAll(io.LimitReader(tr, 4096))
			if err != nil {
				return nil, err
			}
			// the first line is the path; some packages write a number on a second one
			e.Path = strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
		case "asset":
			b, err := io.ReadAll(io.LimitReader(tr, 2<<30))
			if err != nil {
				return nil, err
			}
			e.Asset, e.HasData = b, true
		case "asset.meta":
			b, err := io.ReadAll(io.LimitReader(tr, 1<<20))
			if err != nil {
				return nil, err
			}
			e.Meta = b
		}
	}
	var out []pkgEntry
	for _, e := range at {
		if e.Path == "" || strings.Contains(e.Path, "..") || filepath.IsAbs(e.Path) {
			continue
		}
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// writePackage writes the entries of a package into a project. It answers how many files it wrote and how
// many it left alone because they were not this program's to replace.
func writePackage(project string, entries []pkgEntry) (wrote int, kept int, err error) {
	for _, e := range entries {
		if !e.HasData {
			continue // a folder: made when something goes into it
		}
		dst := filepath.Join(project, filepath.FromSlash(e.Path))
		if !under(dst, project) {
			continue // nothing outside the project, whatever the package says
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return wrote, kept, err
		}
		if core.StatOK(dst) && !oursToReplace(dst, e.GUID) {
			// Unity's own way: the file that is there stays, and the new one gets a name of its own
			dst = uniqueAsset(dst)
			kept++
		}
		if err := os.WriteFile(dst, e.Asset, 0644); err != nil {
			return wrote, kept, err
		}
		if len(e.Meta) > 0 {
			// without it Unity gives the file a GUID of its own, and what refers to it is broken
			if err := os.WriteFile(dst+".meta", e.Meta, 0644); err != nil {
				return wrote, kept, err
			}
		}
		wrote++
	}
	return wrote, kept, nil
}

// oursToReplace: the file that lies there is the same asset — its .meta carries the GUID the package has.
// A file without a .meta (copied in while Unity was closed) is not ours to write over.
func oursToReplace(dst, guid string) bool {
	b, err := os.ReadFile(dst + ".meta")
	if err != nil {
		return false
	}
	return strings.Contains(string(b), guid)
}

// uniqueAsset: "file.fbx" → "file 1.fbx", the way Unity names what it brings in beside something.
func uniqueAsset(p string) string {
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 1; ; i++ {
		q := base + " " + itoa(i) + ext
		if !core.StatOK(q) {
			return q
		}
	}
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

// under: p lies inside dir.
func under(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	if err != nil {
		return false
	}
	return rel != "." && !strings.HasPrefix(rel, "..")
}

// ---------- which packages of an asset go in ----------

// pkgPick: one unitypackage that could be imported.
type pkgPick struct {
	Path string // where it lies
	Name string // what it is called
	Size int64
	Why  string // why it is left out ("" = it goes in)
}

// pickPackages works out which of an asset's unitypackages are imported, and why the others are not.
//
// The same package in two folders is one; several versions of one package are the newest only; a Quest
// version goes in only when the project builds for Quest. What is left over after that — variants that
// write over each other's files (left and right hand, with and without a feature) — is for the player to
// pick from.
func pickPackages(paths []string, target string) (in []pkgPick, out []pkgPick) {
	var items []pkgItem
	seen := map[string]bool{} // name+size: the same file in two places
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		name := filepath.Base(p)
		key := strings.ToLower(name) + "|" + itoa(int(fi.Size()))
		if seen[key] {
			out = append(out, pkgPick{Path: p, Name: name, Size: fi.Size(), Why: "与另一处的同名同大小文件重复"})
			continue
		}
		seen[key] = true
		stem, ver := splitVersion(strings.TrimSuffix(name, filepath.Ext(name)))
		items = append(items, pkgItem{pkgPick{Path: p, Name: name, Size: fi.Size()}, strings.ToLower(stem), ver})
	}
	// of several versions of one package, the newest
	best := map[string]int{}
	for i, it := range items {
		if j, ok := best[it.stem]; !ok || newerVersion(it.ver, items[j].ver) {
			best[it.stem] = i
		}
	}
	quest := target == "quest"
	for i, it := range items {
		switch {
		case best[it.stem] != i:
			out = append(out, with(it.pick, "同一个包的较旧版本（"+it.ver+"）"))
		case isQuestPkg(it.pick.Name) && !quest:
			out = append(out, with(it.pick, "Quest 版，当前工程为 PC 平台"))
		case !isQuestPkg(it.pick.Name) && quest && hasQuestTwin(items2names(items), it.stem):
			out = append(out, with(it.pick, "PC 版，当前工程为 Quest 平台"))
		default:
			in = append(in, it.pick)
		}
	}
	sort.Slice(in, func(i, j int) bool { return in[i].Name < in[j].Name })
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return in, out
}

func with(p pkgPick, why string) pkgPick { p.Why = why; return p }

// pkgItem: a package with its name split into what it is and which version of it.
type pkgItem struct {
	pick pkgPick
	stem string
	ver  string
}

func items2names(items []pkgItem) []string {
	var out []string
	for _, it := range items {
		out = append(out, strings.ToLower(it.pick.Name))
	}
	return out
}

func isQuestPkg(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "quest") || strings.Contains(n, "android")
}

func hasQuestTwin(names []string, stem string) bool {
	for _, n := range names {
		if isQuestPkg(n) && strings.HasPrefix(n, stem) {
			return true
		}
	}
	return false
}

// splitVersion: "Kipfel_1.0.3" → "Kipfel", "1.0.3".
func splitVersion(s string) (stem, ver string) {
	toks := strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == '_' || r == '-' })
	for len(toks) > 0 {
		last := strings.ToLower(toks[len(toks)-1])
		if !isVersionTok(last) {
			break
		}
		ver, toks = last, toks[:len(toks)-1]
	}
	if len(toks) == 0 {
		return s, ver
	}
	return strings.Join(toks, "_"), ver
}

func isVersionTok(s string) bool {
	s = strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(s, "version"), "ver"), "v")
	if s == "" {
		return false
	}
	digits := false
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digits = true
		case r == '.' || r == '_':
		case r >= 'a' && r <= 'z':
			if len(s) > 8 {
				return false
			}
		default:
			return false
		}
	}
	return digits
}

// newerVersion: a is a later version than b ("" is older than any number).
func newerVersion(a, b string) bool {
	if a == b {
		return false
	}
	if a == "" || b == "" {
		return b == ""
	}
	as, bs := numsOf(a), numsOf(b)
	for i := 0; i < len(as) || i < len(bs); i++ {
		x, y := 0, 0
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		if x != y {
			return x > y
		}
	}
	return a > b
}

func numsOf(s string) []int {
	var out []int
	n, any := 0, false
	for _, r := range s {
		if r >= '0' && r <= '9' {
			n, any = n*10+int(r-'0'), true
			continue
		}
		if any {
			out, n, any = append(out, n), 0, false
		}
	}
	if any {
		out = append(out, n)
	}
	return out
}
