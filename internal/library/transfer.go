package library

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"vrclib/internal/core"
)

// Moving the library to another computer.
//
// An export is a zip: manifest.json says what it is, library.json is the library itself, and data/… carries
// the files kept beside it (the wish list, the shops followed, the recipes, the covers). What is this
// computer's own never goes in — no login, no API key, no AI address, no proxy, no download folder, and no
// check-up records.
//
// An import keeps those same things as they are here, so that a library from somewhere else cannot point
// this program at another service or another folder. Everything it writes is written down first, so that
// 「撤销」 puts the data folder back exactly as it was.

const exportKind = "library-export"
const exportFormat = 1

// dataSkip: what is never exported, and never taken from an import. The logins and the API key belong to
// the computer they were saved on; the AI address belongs with the key; the check-up records describe
// projects that are not here.
var dataSkip = []string{
	"library.json", "library.json.bak", "ai.json", "ai-keys.dat", "booth-session.dat", "gumroad-session.dat",
	"baidu-session.dat", "web-session.dat", "library.log", "pan-debug.txt", "checkups.json", "guidcache.json",
	"portable.txt", "thumbs", "shots", "booth-profile", "webview",
}

func skipData(rel string) bool {
	first := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
	for _, s := range dataSkip {
		if strings.EqualFold(rel, s) || strings.EqualFold(first, s) {
			return true
		}
	}
	return strings.HasSuffix(rel, ".part") || strings.HasPrefix(first, ".") ||
		strings.Contains(rel, ".unread-") || strings.Contains(rel, ".broken-")
}

var transfer struct {
	mu sync.Mutex
	st Transfer
}

func setTransfer(t Transfer) {
	transfer.mu.Lock()
	transfer.st = t
	transfer.mu.Unlock()
	core.BumpRev()
}

// TransferState: how the export or import under way is going.
func TransferState() Transfer {
	transfer.mu.Lock()
	defer transfer.mu.Unlock()
	return transfer.st
}

// StartExport writes the library into a zip in dir.
func StartExport(st *core.Store, dir, dlDir string) error {
	if strings.TrimSpace(dir) == "" || !filepath.IsAbs(dir) {
		return errors.New("请选择保存位置")
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return errors.New("保存位置不存在：" + dir)
	}
	transfer.mu.Lock()
	if transfer.st.Stage == "export" || transfer.st.Stage == "import" {
		transfer.mu.Unlock()
		return errors.New("正在导出或导入，请稍候")
	}
	transfer.st = Transfer{Stage: "export", Msg: "正在导出"}
	transfer.mu.Unlock()
	core.BumpRev()
	background(func() {
		file, err := writeExport(st, dir)
		if err != nil {
			setTransfer(Transfer{Stage: "failed", Err: core.TrimErr(err)})
			core.Logf("导出素材库失败：%v", err)
			return
		}
		setTransfer(Transfer{Stage: "done", File: file, Msg: "导出完成"})
		core.Logf("素材库已导出到 %s", file)
	})
	return nil
}

func writeExport(st *core.Store, dir string) (string, error) {
	name := strings.ReplaceAll(core.AppName, " ", "_") + "-素材库-" + time.Now().Format("20060102-1504") + ".zip"
	p := filepath.Join(dir, name)
	f, err := os.Create(p + ".part")
	if err != nil {
		return "", err
	}
	zw := zip.NewWriter(f)
	add := func(name string, b []byte) error {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = w.Write(b)
		return err
	}
	man, _ := json.Marshal(map[string]any{"app": core.AppName, "kind": exportKind, "format": exportFormat,
		"version": core.AppVersion, "made": time.Now().Unix()})
	err = add("manifest.json", man)
	if err == nil {
		st.Mu.RLock()
		lb, merr := json.Marshal(st)
		st.Mu.RUnlock()
		if merr != nil {
			err = merr
		} else {
			err = add("library.json", lb)
		}
	}
	// the files kept beside the library
	n := 0
	if err == nil {
		err = filepath.WalkDir(core.DataDir, func(fp string, d os.DirEntry, werr error) error {
			if werr != nil || d.IsDir() {
				return nil
			}
			rel, rerr := filepath.Rel(core.DataDir, fp)
			if rerr != nil || skipData(rel) {
				return nil
			}
			b, rerr := os.ReadFile(fp)
			if rerr != nil {
				return nil
			}
			n++
			setTransfer(Transfer{Stage: "export", Msg: "正在导出 " + rel, Done: n})
			return add("data/"+filepath.ToSlash(rel), b)
		})
	}
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(p + ".part")
		return "", err
	}
	if err := os.Rename(p+".part", p); err != nil {
		return "", err
	}
	return p, nil
}

// InspectExport reads what an export file holds.
func InspectExport(st *core.Store, p string) (*ExportInfo, error) {
	zr, err := zip.OpenReader(p)
	if err != nil {
		return nil, errors.New("无法打开该文件（不是 zip 或已损坏）")
	}
	defer zr.Close()
	man, lib, files, err := readExport(&zr.Reader)
	if err != nil {
		return nil, err
	}
	info := &ExportInfo{Path: p, Version: man.Version, Made: man.Made, Assets: len(lib.Assets),
		Cards: len(lib.Assets) + len(lib.User), Roots: lib.Settings.Roots}
	for _, f := range files {
		if strings.HasPrefix(f, "data/covers/") {
			info.Covers++
		}
	}
	st.Mu.RLock()
	here := append([]string{}, st.Settings.Roots...)
	st.Mu.RUnlock()
	for _, r := range lib.Settings.Roots {
		if !core.StatOK(r) && !core.ContainsStr(here, r) {
			info.Missing = append(info.Missing, r)
		}
	}
	return info, nil
}

type exportManifest struct {
	App     string `json:"app"`
	Kind    string `json:"kind"`
	Format  int    `json:"format"`
	Version string `json:"version"`
	Made    int64  `json:"made"`
}

// readExport reads the manifest and the library out of an export, and lists what else is in it.
func readExport(zr *zip.Reader) (exportManifest, *core.Store, []string, error) {
	var man exportManifest
	var lib core.Store
	var files []string
	gotMan, gotLib := false, false
	for _, f := range zr.File {
		name := filepath.ToSlash(f.Name)
		switch {
		case name == "manifest.json":
			b, err := readZipFile(f, 1<<20)
			if err != nil || json.Unmarshal(b, &man) != nil {
				return man, nil, nil, errors.New("导出文件的 manifest.json 无法读取")
			}
			gotMan = true
		case name == "library.json":
			b, err := readZipFile(f, 512<<20)
			if err != nil || json.Unmarshal(b, &lib) != nil {
				return man, nil, nil, errors.New("导出文件的 library.json 无法读取")
			}
			gotLib = true
		default:
			files = append(files, name)
		}
	}
	if !gotMan || man.Kind != exportKind {
		return man, nil, nil, errors.New("这不是素材库导出文件（MioVRCA 或 " + core.AppName + " 导出的）")
	}
	if man.Format > exportFormat {
		return man, nil, nil, errors.New("该文件由更新版本的软件导出，请先更新软件")
	}
	if !gotLib {
		return man, nil, nil, errors.New("导出文件中没有 library.json")
	}
	sort.Strings(files)
	return man, &lib, files, nil
}

func readZipFile(f *zip.File, max int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, max))
}

// ---------- the import ----------

func undoDir() string      { return filepath.Join(core.DataDir, ".import-undo") }
func undoListFile() string { return filepath.Join(undoDir(), "created.json") }

type undoList struct {
	Created   []string `json:"created"`   // files the import wrote that were not there before
	Overwrote []string `json:"overwrote"` // and the ones it wrote over, whose copies lie in the undo folder
	At        int64    `json:"at"`
}

// ImportLibrary brings an export file in.
func ImportLibrary(st *core.Store, opt ImportOptions) (*ImportResult, error) {
	if ImportBusy() {
		return nil, errors.New("素材库正在使用中，请在扫描或下载完成后重试")
	}
	zr, err := zip.OpenReader(opt.Path)
	if err != nil {
		return nil, errors.New("无法打开该文件（不是 zip 或已损坏）")
	}
	defer zr.Close()
	_, lib, _, err := readExport(&zr.Reader)
	if err != nil {
		return nil, err
	}
	setTransfer(Transfer{Stage: "import", Msg: "正在导入"})
	defer func() { setTransfer(Transfer{Stage: "done", Msg: "导入完成", CanUndo: true}) }()

	// what is this computer's own, which an import never changes
	st.Mu.RLock()
	mine := st.Settings
	hadAssets := len(st.Assets)
	st.Mu.RUnlock()

	// everything that follows can be taken back
	if err := os.RemoveAll(undoDir()); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(undoDir(), 0755); err != nil {
		return nil, err
	}
	var un undoList
	un.At = time.Now().Unix()
	if b, err := os.ReadFile(st.Path); err == nil {
		if err := os.WriteFile(filepath.Join(undoDir(), "library.json"), b, 0644); err != nil {
			return nil, err
		}
	}
	res := &ImportResult{CanUndo: true}
	// the files beside the library
	for _, f := range zr.File {
		name := filepath.ToSlash(f.Name)
		rel, ok := strings.CutPrefix(name, "data/")
		if !ok || f.FileInfo().IsDir() {
			continue
		}
		if !okDataPath(rel) || skipData(rel) {
			continue
		}
		b, err := readZipFile(f, 64<<20)
		if err != nil {
			continue
		}
		if strings.HasPrefix(rel, "recipes/") && !okRecipeFile(b) {
			core.Logf("导入：方案 %s 中有不安全的路径，已跳过", rel)
			continue
		}
		dst := filepath.Join(core.DataDir, filepath.FromSlash(rel))
		if old, err := os.ReadFile(dst); err == nil {
			_ = os.MkdirAll(filepath.Dir(filepath.Join(undoDir(), "data", filepath.FromSlash(rel))), 0755)
			if os.WriteFile(filepath.Join(undoDir(), "data", filepath.FromSlash(rel)), old, 0644) == nil {
				un.Overwrote = append(un.Overwrote, rel)
			}
		} else {
			un.Created = append(un.Created, rel)
		}
		if os.MkdirAll(filepath.Dir(dst), 0755) != nil {
			continue
		}
		if os.WriteFile(dst, b, 0644) != nil {
			continue
		}
		if strings.HasPrefix(rel, "covers/") {
			res.Covers++
		}
	}
	if b, err := json.Marshal(un); err == nil {
		_ = os.WriteFile(undoListFile(), b, 0644)
	}
	// the library itself
	lb, err := json.Marshal(lib)
	if err != nil {
		return nil, err
	}
	st.Mu.Lock()
	if strings.EqualFold(opt.Mode, "replace") {
		clearData(st) // json.Unmarshal merges into maps that are there: a replace empties them first
	}
	if err := json.Unmarshal(lb, st); err != nil {
		st.Mu.Unlock()
		return nil, errors.New("导入的 library.json 无法读取")
	}
	keepMine(st, mine)
	res.Cards = len(st.Assets) + len(st.User)
	// the folders of the other computer that are not here: the player maps or drops them
	for _, r := range st.Settings.Roots {
		if to := opt.Map[r]; to != "" {
			r = to
		}
		if !core.StatOK(r) {
			res.Pending = append(res.Pending, r)
		}
	}
	st.Settings.Roots = mapRoots(st.Settings.Roots, opt.Map)
	st.Mu.Unlock()
	if err := st.Save(); err != nil {
		return nil, err
	}
	core.Logf("已导入素材库：%d 张卡片、%d 张封面（此前 %d 个素材）", res.Cards, res.Covers, hadAssets)
	dataImported()
	StartPipeline(st, true, true, false, false, nil)
	return res, nil
}

// clearData empties what a library holds, leaving the store itself — its lock, its path and what it knows
// about the files on disk — as it is. The lock is held by the caller.
func clearData(st *core.Store) {
	st.Settings = core.Settings{}
	st.Assets, st.User, st.Booth, st.Overrides, st.Projects, st.FirstSeen = nil, nil, nil, nil, nil, nil
	st.Purchases, st.Pan, st.BoothMatch, st.Trans, st.Downloaded = nil, nil, nil, nil, nil
	st.Warnings, st.Update = nil, nil
	st.Version, st.LastScan, st.LastUsage = 0, 0, 0
	st.PurchaseSync, st.GumroadSync, st.ScanStart, st.UpdateChecked = 0, 0, 0, 0
	st.NotesSeen, st.AutoDLDir = "", ""
}

// keepMine puts this computer's own settings back over what an import brought.
func keepMine(st *core.Store, mine core.Settings) {
	s := &st.Settings
	s.Proxy, s.DownloadDir, s.NoUpdateCheck, s.SkipVersion = mine.Proxy, mine.DownloadDir, mine.NoUpdateCheck, mine.SkipVersion
	s.Lang, s.WindowMode, s.CloseQuit, s.NoSound = mine.Lang, mine.WindowMode, mine.CloseQuit, mine.NoSound
	s.XyExternal, s.XyNoticed, s.NoXyClip, s.TrayNoticed = mine.XyExternal, mine.XyNoticed, mine.NoXyClip, mine.TrayNoticed
	s.SetupDone = true
}

func mapRoots(roots []string, m map[string]string) []string {
	var out []string
	for _, r := range roots {
		if to := m[r]; to != "" {
			r = to
		}
		if core.StatOK(r) && !core.ContainsStr(out, r) {
			out = append(out, r)
		}
	}
	return out
}

// okDataPath: a name inside the export's data/ that may be written — a plain relative path, nothing that
// leads out of the data folder.
func okDataPath(rel string) bool {
	s := filepath.ToSlash(rel)
	return s != "" && s == path.Clean(s) && !strings.HasPrefix(s, "/") && !strings.Contains(s, "..") &&
		!filepath.IsAbs(filepath.FromSlash(s))
}

// okRecipeFile: a recipe from somebody else names only paths inside a project's Assets. (The format itself
// is internal/ai's; this is the one thing about it that decides whether the file may be written at all.)
func okRecipeFile(b []byte) bool {
	var r struct {
		Base   map[string]any `json:"base"`
		Assets []struct {
			Prefab string `json:"prefab"`
			URL    string `json:"url"`
		} `json:"assets"`
	}
	if json.Unmarshal(b, &r) != nil {
		return false
	}
	ok := func(p string) bool {
		if p == "" {
			return true
		}
		s := strings.ReplaceAll(p, `\`, "/")
		return s == path.Clean(s) && !strings.HasPrefix(s, "/") && !strings.Contains(s, "..") &&
			strings.HasPrefix(s, "Assets/")
	}
	if p, _ := r.Base["prefab"].(string); !ok(p) {
		return false
	}
	for _, a := range r.Assets {
		if !ok(a.Prefab) {
			return false
		}
		if u := strings.ToLower(strings.TrimSpace(a.URL)); u != "" &&
			!strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
			return false // nothing but a web address (a "javascript:" one is no link)
		}
	}
	return true
}

// UndoImport puts back the library that was there before the last import, and takes away what the import
// wrote.
func UndoImport(st *core.Store) error {
	if !core.StatOK(filepath.Join(undoDir(), "library.json")) {
		return errors.New("没有可撤销的导入")
	}
	var un undoList
	if b, err := os.ReadFile(undoListFile()); err == nil {
		_ = json.Unmarshal(b, &un)
	}
	// what the import made goes
	for _, rel := range un.Created {
		_ = os.Remove(filepath.Join(core.DataDir, filepath.FromSlash(rel)))
	}
	// what it wrote over comes back
	var lost []string
	for _, rel := range un.Overwrote {
		if b, err := os.ReadFile(filepath.Join(undoDir(), "data", filepath.FromSlash(rel))); err == nil {
			if err := os.WriteFile(filepath.Join(core.DataDir, filepath.FromSlash(rel)), b, 0644); err != nil {
				lost = append(lost, rel)
			}
		}
	}
	if len(lost) > 0 {
		// the copies stay in the undo folder: nothing is dropped, and undoing again can finish it
		return errors.New("有 " + strconv.Itoa(len(lost)) + " 个文件无法还原（可能正被占用），请关闭占用它们的程序后重试：" + strings.Join(lost, "、"))
	}
	dropEmptyDirs(un.Created)
	b, err := os.ReadFile(filepath.Join(undoDir(), "library.json"))
	if err != nil {
		return err
	}
	st.Mu.Lock()
	clearData(st)
	err = json.Unmarshal(b, st)
	st.Mu.Unlock()
	if err != nil {
		return errors.New("备份的 library.json 无法读取")
	}
	if err := st.Save(); err != nil {
		return err
	}
	_ = os.RemoveAll(undoDir())
	setTransfer(Transfer{})
	core.Logf("已撤销素材库导入")
	dataImported()
	StartPipeline(st, true, true, false, false, nil)
	return nil
}

// dropEmptyDirs takes away the folders the import made, once what it put in them is gone.
func dropEmptyDirs(created []string) {
	dirs := map[string]bool{}
	for _, rel := range created {
		for d := path.Dir(filepath.ToSlash(rel)); d != "." && d != "/"; d = path.Dir(d) {
			dirs[d] = true
		}
	}
	var all []string
	for d := range dirs {
		all = append(all, d)
	}
	sort.Slice(all, func(i, j int) bool { return len(all[i]) > len(all[j]) }) // deepest first
	for _, d := range all {
		_ = os.Remove(filepath.Join(core.DataDir, filepath.FromSlash(d)))
	}
}

// ---------- the folders an import could not place ----------

var pending struct {
	mu sync.Mutex
	m  map[string]bool
}

// MapPending: the folder of the other computer is this folder here; answers how many cards it moved.
func MapPending(st *core.Store, root, to string) (int, error) {
	if root == "" || to == "" {
		return 0, errors.New("请选择文件夹")
	}
	if fi, err := os.Stat(to); err != nil || !fi.IsDir() {
		return 0, errors.New("文件夹不存在：" + to)
	}
	n := 0
	st.Mu.Lock()
	for _, a := range st.Assets {
		for i := range a.Locations {
			if l := &a.Locations[i]; underOrSame(l.Path, root) {
				l.Path = swapPrefix(l.Path, root, to)
				l.Root = to
				n++
			}
		}
	}
	if !core.ContainsStr(st.Settings.Roots, to) {
		st.Settings.Roots = append(st.Settings.Roots, to)
	}
	var left []string
	for _, r := range st.Settings.Roots {
		if !underOrSame(r, root) {
			left = append(left, r)
		}
	}
	st.Settings.Roots = left
	st.Mu.Unlock()
	pending.mu.Lock()
	if pending.m != nil {
		delete(pending.m, root)
	}
	pending.mu.Unlock()
	_ = st.Save()
	StartPipeline(st, true, true, false, false, nil)
	return n, nil
}

// DropPending: leave the cards of that folder out.
func DropPending(root string) error {
	if root == "" {
		return errors.New("请选择文件夹")
	}
	pending.mu.Lock()
	if pending.m == nil {
		pending.m = map[string]bool{}
	}
	pending.m[root] = true
	pending.mu.Unlock()
	core.BumpRev()
	return nil
}

func underOrSame(p, dir string) bool {
	return core.PathKey(filepath.Clean(p)) == core.PathKey(filepath.Clean(dir)) ||
		under(filepath.Clean(p), filepath.Clean(dir))
}

func swapPrefix(p, from, to string) string {
	rel, err := filepath.Rel(filepath.Clean(from), filepath.Clean(p))
	if err != nil {
		return p
	}
	if rel == "." {
		return filepath.Clean(to)
	}
	return filepath.Join(to, rel)
}
