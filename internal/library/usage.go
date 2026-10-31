package library

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"vrclib/internal/core"
)

// Which project uses which asset.
//
// Everything Unity imports has a GUID, written in the .meta beside it, and an asset brings the same GUIDs
// into every project it is imported into: a unitypackage carries them as the names of its entries, a folder
// of Unity files in its .meta files. So an asset is in a project when the project's Assets folder holds its
// GUIDs — "used" for most of them, "partial" for some (a part imported, or an older version).
//
// GUIDs that several assets have in common are left out of the count: they are the shaders and tools that
// outfits carry along (lilToon, Modular Avatar's prefabs), which would otherwise make every outfit seem to
// be in every project.
//
// The GUIDs of a package are read once and kept in guidcache.json, by the package's path, size and time.

const (
	projectDepth = 4   // project folders looked into this deep (a folder per model, versions inside)
	sharedGUID   = 3   // a GUID in this many assets or more is something they carry along, not theirs
	usedRatio    = 0.6 // of an asset's GUIDs in a project: "used"
	partialMin   = 0.1 // … or at least this much (and three files): "partial"
)

// ---------- the projects ----------

// findProjects: the Unity projects of the project folders, named by their path below the folder they were
// found in.
func findProjects(roots []string) []core.ProjectInfo {
	var out []core.ProjectInfo
	seen := map[string]bool{}
	add := func(p, name string) {
		k := core.PathKey(p)
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, core.ProjectInfo{Name: name, Path: p})
	}
	for _, root := range roots {
		root = filepath.Clean(root)
		if core.IsUnityProject(root) {
			add(root, filepath.Base(root))
			continue
		}
		var walk func(string, int)
		walk = func(dir string, depth int) {
			if depth > projectDepth {
				return
			}
			ents, err := os.ReadDir(dir)
			if err != nil {
				return
			}
			for _, e := range ents {
				if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || skipDirs[strings.ToLower(e.Name())] {
					continue
				}
				p := filepath.Join(dir, e.Name())
				if core.IsUnityProject(p) {
					rel, err := filepath.Rel(root, p)
					if err != nil {
						rel = e.Name()
					}
					add(p, filepath.ToSlash(rel))
					continue // a project inside a project is not one of its own
				}
				walk(p, depth+1)
			}
		}
		walk(root, 1)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// projectGUIDs: the GUIDs of what a project's Assets folder holds, each with the folder it lies in
// ("Assets/Shop/Item"), and how many there are.
func projectGUIDs(p string) map[string]string {
	out := map[string]string{}
	assets := filepath.Join(p, "Assets")
	_ = filepath.WalkDir(assets, func(fp string, d os.DirEntry, err error) error {
		if err != nil || core.Quitting.Load() {
			return nil
		}
		if d.IsDir() {
			if n := d.Name(); strings.HasPrefix(n, ".") || strings.HasSuffix(n, "~") {
				return filepath.SkipDir // what Unity itself does not import
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".meta") {
			return nil
		}
		if g := metaGUID(fp); g != "" {
			rel, _ := filepath.Rel(p, filepath.Dir(fp))
			out[g] = filepath.ToSlash(rel)
		}
		return nil
	})
	return out
}

// metaGUID: the guid line of a .meta file (it is among the first few).
func metaGUID(p string) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, 4096))
	for i := 0; i < 6 && sc.Scan(); i++ {
		if g, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "guid:"); ok {
			g = strings.TrimSpace(g)
			if len(g) == 32 {
				return strings.ToLower(g)
			}
		}
	}
	return ""
}

// ---------- the GUIDs of an asset ----------

type guidEntry struct {
	Size  int64    `json:"s"`
	MTime int64    `json:"t"`
	GUIDs []string `json:"g"`
}

var gcache struct {
	mu    sync.Mutex
	m     map[string]*guidEntry
	dir   string
	dirty bool
}

func guidFile() string { return filepath.Join(core.DataDir, "guidcache.json") }

func gcacheLoad() {
	if gcache.m != nil && gcache.dir == core.DataDir {
		return
	}
	gcache.m, gcache.dir = map[string]*guidEntry{}, core.DataDir
	if b, err := core.ReadDataFile(guidFile()); err == nil {
		_ = json.Unmarshal(b, &gcache.m)
	}
}

func gcacheSave() {
	gcache.mu.Lock()
	defer gcache.mu.Unlock()
	if !gcache.dirty {
		return
	}
	if b, err := json.Marshal(gcache.m); err == nil && os.WriteFile(guidFile(), b, 0644) == nil {
		gcache.dirty = false
	}
}

// packageGUIDs: the GUIDs a unitypackage brings, read once for a package as it is.
func packageGUIDs(p string) []string {
	fi, err := os.Stat(p)
	if err != nil {
		return nil
	}
	k := core.PathKey(p)
	gcache.mu.Lock()
	gcacheLoad()
	if e := gcache.m[k]; e != nil && e.Size == fi.Size() && e.MTime == fi.ModTime().Unix() {
		gcache.mu.Unlock()
		return e.GUIDs
	}
	gcache.mu.Unlock()
	guids := readPackageGUIDs(p)
	gcache.mu.Lock()
	gcache.m[k] = &guidEntry{Size: fi.Size(), MTime: fi.ModTime().Unix(), GUIDs: guids}
	gcache.dirty = true
	gcache.mu.Unlock()
	return guids
}

// readPackageGUIDs reads the entry names of a package: each asset is a folder named after its GUID. Only
// the ones with a file of their own count (a folder entry is not something that is in a project's files).
func readPackageGUIDs(p string) []string {
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil
	}
	defer zr.Close()
	has := map[string]bool{}
	tr := tar.NewReader(zr)
	for n := 0; n < 200000; n++ {
		h, err := tr.Next()
		if err != nil {
			break
		}
		guid, what := path.Split(path.Clean(strings.TrimPrefix(filepath.ToSlash(h.Name), "./")))
		guid = strings.Trim(guid, "/")
		if len(guid) == 32 && what == "asset" {
			has[strings.ToLower(guid)] = true
		}
	}
	out := make([]string, 0, len(has))
	for g := range has {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

// assetGUIDs: what an asset brings into a project — its packages' GUIDs, and those of the Unity files lying
// in its folders with their .meta.
func assetGUIDs(a *core.Asset) map[string]bool {
	out := map[string]bool{}
	for _, l := range a.Locations {
		if l.Kind == "unitypackage" {
			for _, g := range packageGUIDs(l.Path) {
				out[g] = true
			}
			continue
		}
		if l.Kind != "dir" {
			continue
		}
		for _, rel := range a.Packages {
			for _, g := range packageGUIDs(filepath.Join(l.Path, filepath.FromSlash(rel))) {
				out[g] = true
			}
		}
		_ = filepath.WalkDir(l.Path, func(fp string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(d.Name(), ".meta") {
				if g := metaGUID(fp); g != "" {
					out[g] = true
				}
			}
			return nil
		})
	}
	return out
}

// ---------- the count ----------

// RunUsageScan finds the projects and works out which of them uses which asset.
func RunUsageScan(st *core.Store, prog *core.Task) {
	st.Mu.RLock()
	roots := append([]string{}, st.Settings.ProjectRoots...)
	keep := append([]core.ProjectInfo{}, st.Projects...)
	assets := append([]*core.Asset{}, st.Assets...)
	st.Mu.RUnlock()

	prog.Set(0, 1, "正在查找 Unity 工程")
	projects := findProjects(roots)
	// a project added by hand that is no longer below any project folder: kept while it is there
	for _, p := range keep {
		if !core.IsUnityProject(p.Path) {
			continue
		}
		found := false
		for _, q := range projects {
			if core.PathKey(q.Path) == core.PathKey(p.Path) {
				found = true
			}
		}
		if !found {
			projects = append(projects, core.ProjectInfo{Name: p.Name, Path: p.Path})
		}
	}

	// what each asset brings, and which GUIDs several of them share
	byAsset := make([]map[string]bool, len(assets))
	owners := map[string]int{}
	for i, a := range assets {
		if core.Quitting.Load() {
			return
		}
		prog.Set(i, len(assets)+len(projects), "正在读取 "+a.Name)
		byAsset[i] = assetGUIDs(a)
		for g := range byAsset[i] {
			owners[g]++
		}
	}
	gcacheSave()
	own := make([]map[string]bool, len(assets))
	for i, gs := range byAsset {
		own[i] = map[string]bool{}
		for g := range gs {
			if owners[g] < sharedGUID {
				own[i][g] = true
			}
		}
	}

	usage := make([][]core.Usage, len(assets))
	for j := range projects {
		if core.Quitting.Load() {
			return
		}
		p := &projects[j]
		prog.Set(len(assets)+j, len(assets)+len(projects), "正在统计 "+p.Name)
		in := projectGUIDs(p.Path)
		p.Guids = len(in)
		for i := range assets {
			total := len(own[i])
			if total == 0 {
				continue
			}
			matched := 0
			folders := map[string]int{}
			for g := range own[i] {
				if dir, ok := in[g]; ok {
					matched++
					folders[topFolder(dir)]++
				}
			}
			if matched == 0 {
				continue
			}
			ratio := float64(matched) / float64(total)
			status := ""
			switch {
			case ratio >= usedRatio:
				status = "used"
			case ratio >= partialMin && matched >= 3:
				status = "partial"
			default:
				continue
			}
			best, bestN := "", 0
			for f, n := range folders {
				if n > bestN || (n == bestN && f < best) {
					best, bestN = f, n
				}
			}
			usage[i] = append(usage[i], core.Usage{Project: p.Name, Matched: matched, Total: total,
				Ratio: float64(int(ratio*1000)) / 1000, Status: status, Folder: best})
			p.Assets++
		}
	}

	st.Mu.Lock()
	for i, a := range assets {
		a.Usage = usage[i]
		a.GuidCount = len(own[i])
	}
	st.Projects = projects
	st.LastUsage = time.Now().Unix()
	st.Mu.Unlock()
	_ = st.Save()
	core.BumpRev()
	prog.Set(1, 1, "完成")
}

// topFolder: the folder an asset lies in, in a project — "Assets/Shop/Item", three levels at most.
func topFolder(dir string) string {
	parts := strings.Split(dir, "/")
	if len(parts) > 3 {
		parts = parts[:3]
	}
	return strings.Join(parts, "/")
}
