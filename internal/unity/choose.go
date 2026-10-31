package unity

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"vrclib/internal/core"
	"vrclib/internal/naming"
)

// What an import asks the player (stage "choose").
//
// Two cases, as the window words them:
//   - the asset has a version for each of several base bodies ("该素材包含多个素体版本"): the versions for
//     the project's base body are ticked; packages for no base body in particular go in with them.
//   - packages write the same files (left and right hand, with and without a feature; why "overlap"): the
//     biggest of each such group is ticked, the others are not, with what they overlap.
//
// An import that names its packages (an update carried into a project) asks nothing.

// PkgChoice: one package the player is to tick or not.
type PkgChoice struct {
	Name  string   `json:"name"`
	Path  string   `json:"path"`
	Bases []string `json:"bases,omitempty"`
	Size  int64    `json:"size"`
	Pick  bool     `json:"pick"`
	Why   string   `json:"why,omitempty"`  // alt: it writes what a ticked one writes
	Note  string   `json:"note,omitempty"` // in words
}

// PkgSkip: a package that was not imported, and why — dup, old, quest, pc, alt, base.
type PkgSkip struct {
	Name string `json:"name"`
	Why  string `json:"why"`
}

// listPackage: the paths a unitypackage writes, without reading what it writes.
func listPackage(p string) ([]string, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	var out []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, err
		}
		if path.Base(h.Name) != "pathname" {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, 4096))
		if err != nil {
			return out, err
		}
		if s := strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0]); s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

// projectBases: the base bodies a project is built on — the library's base bodies used in it, and the
// base bodies its Assets folders are named for.
func projectBases(st *core.Store, project string, defs []naming.BaseDef) []string {
	var out []string
	st.Mu.RLock()
	for _, a := range st.Assets {
		if a.Category != "素体" {
			continue
		}
		for _, u := range a.Usage {
			if u.Status == "used" && sameProject(u.Project, project) {
				out = append(out, a.Bases...)
			}
		}
	}
	st.Mu.RUnlock()
	if len(out) > 0 {
		return core.UniqStrings(out)
	}
	ents, _ := os.ReadDir(filepath.Join(project, "Assets"))
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, naming.DetectBases(e.Name(), defs)...)
		}
	}
	return core.UniqStrings(out)
}

// skipCode: the window's code for why pickPackages left a package out.
func skipCode(why string) string {
	switch {
	case strings.Contains(why, "重复"):
		return "dup"
	case strings.Contains(why, "旧版本"):
		return "old"
	case strings.HasPrefix(why, "Quest"):
		return "quest"
	case strings.HasPrefix(why, "PC"):
		return "pc"
	}
	return why
}

// choicesFor: what is to be asked about the packages an import would write, and why ("" or "overlap");
// nil when there is nothing to ask.
func choicesFor(st *core.Store, project string, in []pkgPick) ([]PkgChoice, string) {
	if len(in) < 2 {
		return nil, ""
	}
	st.Mu.RLock()
	defs := naming.ParseBases(st.Settings.Bases)
	st.Mu.RUnlock()
	cs := make([]PkgChoice, len(in))
	distinct := map[string]bool{}
	for i, p := range in {
		stem := strings.TrimSuffix(p.Name, filepath.Ext(p.Name))
		b := core.UniqStrings(append(naming.DetectBases(stem, defs), naming.ForBases(stem, defs)...))
		cs[i] = PkgChoice{Name: p.Name, Path: p.Path, Bases: b, Size: p.Size, Pick: true}
		if len(b) > 0 {
			distinct[strings.Join(b, ",")] = true
		}
	}
	// versions for several base bodies
	if len(distinct) >= 2 {
		mine := projectBases(st, project, defs)
		any := false
		for i := range cs {
			if len(cs[i].Bases) == 0 {
				continue // for every base body
			}
			cs[i].Pick = false
			for _, b := range cs[i].Bases {
				if core.ContainsStr(mine, b) {
					cs[i].Pick, any = true, true
				}
			}
		}
		if !any {
			for i := range cs {
				if len(cs[i].Bases) > 0 {
					cs[i].Note = "未能判断工程所用素体，请选择"
				}
			}
		}
		return cs, ""
	}
	// packages that write the same files
	lists := make([]map[string]bool, len(in))
	for i, p := range in {
		l, _ := listPackage(p.Path)
		lists[i] = map[string]bool{}
		for _, s := range l {
			if strings.Contains(path.Base(s), ".") { // files; folders are shared by any two packages of one shop
				lists[i][s] = true
			}
		}
	}
	order := make([]int, len(in))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return in[order[a]].Size > in[order[b]].Size })
	asked := false
	for x, i := range order {
		if !cs[i].Pick {
			continue
		}
		for _, j := range order[x+1:] {
			if !cs[j].Pick || len(lists[j]) == 0 {
				continue
			}
			same := 0
			for s := range lists[j] {
				if lists[i][s] {
					same++
				}
			}
			// most of the smaller one's files are the bigger one's: one or the other, not both
			if same*2 >= len(lists[j]) && same >= 3 {
				cs[j].Pick, cs[j].Why, cs[j].Note, asked = false, "alt", "与「"+in[i].Name+"」写入同一批文件", true
			}
		}
	}
	if !asked {
		return nil, ""
	}
	return cs, "overlap"
}

// topsOf: the folders under Assets a package wrote into ("Shop/Item").
func topsOf(entries []pkgEntry) []string {
	var out []string
	for _, e := range entries {
		parts := strings.Split(e.Path, "/")
		if len(parts) < 3 || parts[0] != "Assets" {
			continue
		}
		t := parts[1]
		if len(parts) >= 4 {
			t += "/" + parts[2]
		}
		if !core.ContainsStr(out, t) {
			out = append(out, t)
		}
	}
	return out
}
