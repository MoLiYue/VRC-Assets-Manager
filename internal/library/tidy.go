package library

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"vrclib/internal/archive"
	"vrclib/internal/core"
)

// Tidying the library.
//
// Two pages so far: the same file lying in several places (dup), and the archives that can go now that what
// was in them is unpacked beside them (arc). Both only ever list what could go — nothing is moved until the
// player has seen the list and says which of it to move, and then only to the Recycle Bin.
//
// The list the player confirms has to be the one shown: every scan gets a number (Seq), the window sends it
// back, and a confirmation that names another number, a file outside the list, or every copy of a group is
// refused. That way a scan that ran again in between cannot take a file the player never saw.

const (
	tidyMaxFiles  = 400000 // files walked at most
	tidyMaxGroups = 2000   // groups listed at most
)

type tidyState struct {
	mu   sync.Mutex
	run  map[string]TidyRun
	res  map[string]*TidyScan
	stop map[string]*atomic.Bool
}

var tidy = tidyState{run: map[string]TidyRun{}, res: map[string]*TidyScan{}, stop: map[string]*atomic.Bool{}}

var tidySeq atomic.Int64

// TidyStatus: the tidy pages as they stand (dlDir: where downloads go, which is tidied too).
func TidyStatus(st *core.Store, dlDir string) TidyView {
	tidy.mu.Lock()
	defer tidy.mu.Unlock()
	var v TidyView
	v.Dup.Run, v.Dup.Dup = tidy.run["dup"], tidy.res["dup"]
	v.Arc.Run, v.Arc.Arc = tidy.run["arc"], tidy.res["arc"]
	v.Space.Run, v.Fit.Run = tidy.run["space"], tidy.run["fit"]
	return v
}

// TidyStart begins one of the scans ("dup", "arc").
func TidyStart(st *core.Store, what, dlDir string, minSize int64) error {
	switch what {
	case "dup", "arc":
	default:
		return errors.New("尚不支持的整理项目：" + what)
	}
	tidy.mu.Lock()
	if tidy.run[what].Running {
		tidy.mu.Unlock()
		return errors.New("正在扫描，请稍候")
	}
	stop := &atomic.Bool{}
	tidy.stop[what] = stop
	tidy.run[what] = TidyRun{Running: true, Msg: "正在扫描"}
	tidy.mu.Unlock()
	core.BumpRev()

	dirs := tidyDirs(st, dlDir)
	background(func() {
		var scan *TidyScan
		var err error
		switch what {
		case "dup":
			scan, err = scanDups(dirs, minSize, stop)
		case "arc":
			scan, err = scanArchives(dirs, stop)
		}
		tidy.mu.Lock()
		if err != nil {
			tidy.run[what] = TidyRun{Err: core.TrimErr(err)}
		} else {
			tidy.run[what] = TidyRun{}
			tidy.res[what] = scan
		}
		tidy.mu.Unlock()
		core.BumpRev()
	})
	return nil
}

// TidyCancel stops one of them.
func TidyCancel(what string) {
	tidy.mu.Lock()
	if s := tidy.stop[what]; s != nil {
		s.Store(true)
	}
	tidy.mu.Unlock()
}

// tidyDirs: the folders a tidy scan walks — the asset folders and, when it lies elsewhere, the download
// folder.
func tidyDirs(st *core.Store, dlDir string) []string {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	dirs := append([]string{}, st.Settings.Roots...)
	if dlDir != "" && !underAny(dlDir, dirs) {
		dirs = append(dirs, dlDir)
	}
	return dirs
}

func underAny(p string, dirs []string) bool {
	for _, d := range dirs {
		if core.PathKey(filepath.Clean(p)) == core.PathKey(filepath.Clean(d)) || under(filepath.Clean(p), filepath.Clean(d)) {
			return true
		}
	}
	return false
}

// scanDups: the files that lie in more than one place, by what is in them. Only files of the same size are
// read, so that most of the library is never opened.
func scanDups(dirs []string, minSize int64, stop *atomic.Bool) (*TidyScan, error) {
	if minSize < 1 {
		minSize = 1
	}
	bySize := map[int64][]string{}
	n := 0
	for _, d := range dirs {
		err := filepath.WalkDir(d, func(p string, e os.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return nil
			}
			if stop.Load() || core.Quitting.Load() {
				return io.EOF
			}
			fi, err := e.Info()
			if err != nil || fi.Size() < minSize {
				return nil
			}
			n++
			if n > tidyMaxFiles {
				return io.EOF
			}
			bySize[fi.Size()] = append(bySize[fi.Size()], p)
			return nil
		})
		if err != nil && err != io.EOF {
			return nil, err
		}
	}
	out := &TidyScan{Seq: tidySeq.Add(1), Ran: time.Now().Unix()}
	for size, paths := range bySize {
		if len(paths) < 2 || stop.Load() {
			continue
		}
		byHash := map[string][]string{}
		for _, p := range paths {
			h, err := fileHash(p)
			if err != nil {
				continue
			}
			byHash[h] = append(byHash[h], p)
		}
		for _, same := range byHash {
			if len(same) < 2 {
				continue
			}
			// which copy is kept: the one that lies highest up (a copy in a subfolder is the spare one),
			// then the shorter path, then by name
			sort.Slice(same, func(i, j int) bool {
				di, dj := depthOf(same[i]), depthOf(same[j])
				if di != dj {
					return di < dj
				}
				if len(same[i]) != len(same[j]) {
					return len(same[i]) < len(same[j])
				}
				return same[i] < same[j]
			})
			g := TidyGroup{Size: size * int64(len(same)-1)}
			for i, p := range same {
				f := TidyFile{Path: p, Size: size, Keep: i == 0}
				if fi, err := os.Stat(p); err == nil {
					f.MTime = fi.ModTime().Unix()
				}
				if f.Keep {
					f.Why = "保留这一份"
				}
				g.Files = append(g.Files, f)
			}
			out.Groups, out.Files, out.Size = append(out.Groups, g), out.Files+len(same)-1, out.Size+g.Size
			if len(out.Groups) >= tidyMaxGroups {
				out.Cut = true
				break
			}
		}
	}
	sort.Slice(out.Groups, func(i, j int) bool { return out.Groups[i].Size > out.Groups[j].Size })
	return out, nil
}

// scanArchives: the archives whose contents lie unpacked beside them, so that the archive can go.
func scanArchives(dirs []string, stop *atomic.Bool) (*TidyScan, error) {
	out := &TidyScan{Seq: tidySeq.Add(1), Ran: time.Now().Unix()}
	for _, d := range dirs {
		err := filepath.WalkDir(d, func(p string, e os.DirEntry, err error) error {
			if err != nil || e.IsDir() || !archive.IsArchiveFile(p) {
				return nil
			}
			if stop.Load() || core.Quitting.Load() {
				return io.EOF
			}
			stem, _, _ := archive.VolumeName(filepath.Base(p))
			if stem == "" {
				return nil
			}
			// unpacked beside it: a folder of the archive's name, holding something
			into := filepath.Join(filepath.Dir(p), stem)
			if fi, err := os.Stat(into); err != nil || !fi.IsDir() {
				return nil
			}
			if files, _, _ := walkFiles(into); len(files) == 0 {
				return nil
			}
			fi, err := e.Info()
			if err != nil {
				return nil
			}
			g := TidyGroup{Size: fi.Size(), Files: []TidyFile{
				{Path: p, Size: fi.Size(), Why: "已解压到 " + filepath.Base(into)},
				{Path: into, Keep: true, Why: "解压出的文件夹"},
			}}
			out.Groups, out.Files, out.Size = append(out.Groups, g), out.Files+1, out.Size+fi.Size()
			if len(out.Groups) >= tidyMaxGroups {
				out.Cut = true
				return io.EOF
			}
			return nil
		})
		if err != nil && err != io.EOF {
			return nil, err
		}
	}
	return out, nil
}

// depthOf: how many folders deep a path lies.
func depthOf(p string) int {
	return strings.Count(filepath.ToSlash(filepath.Clean(p)), "/")
}

func fileHash(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha1.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// TidyRecycle moves the chosen files to the Recycle Bin, checking once more that they are the ones the
// player was shown (seq: which scan the list came from).
func TidyRecycle(st *core.Store, what string, paths []string, dlDir string, seq int64) (TidyResult, error) {
	var res TidyResult
	if len(paths) == 0 {
		return res, errors.New("未选择要清理的文件")
	}
	tidy.mu.Lock()
	scan := tidy.res[what]
	tidy.mu.Unlock()
	if scan == nil {
		return res, errors.New("请先扫描，再清理")
	}
	if seq == 0 || seq != scan.Seq {
		return res, errors.New("列表已更新，请重新扫描后再清理")
	}
	// every path has to be one the list offered, and no group may lose every copy
	offered := map[string]*TidyGroup{}
	for i := range scan.Groups {
		for _, f := range scan.Groups[i].Files {
			if !f.Keep {
				offered[core.PathKey(f.Path)] = &scan.Groups[i]
			}
		}
	}
	taken := map[*TidyGroup]int{}
	for _, p := range paths {
		g := offered[core.PathKey(p)]
		if g == nil {
			return res, errors.New("有文件不在扫描结果中，未清理：" + filepath.Base(p))
		}
		taken[g]++
	}
	for g, n := range taken {
		left := 0
		for _, f := range g.Files {
			if f.Keep {
				left++
			}
		}
		if left == 0 || n >= len(g.Files) {
			return res, errors.New("每组至少要保留一份，未清理")
		}
	}
	// and the file has to be what it was: the same size as when it was shown
	dirs := tidyDirs(st, dlDir)
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			return res, errors.New("文件已不存在：" + filepath.Base(p))
		}
		if !underAny(p, dirs) {
			return res, errors.New("文件不在素材文件夹内，未清理：" + filepath.Base(p))
		}
		want := int64(-1)
		if g := offered[core.PathKey(p)]; g != nil {
			for _, f := range g.Files {
				if core.PathKey(f.Path) == core.PathKey(p) {
					want = f.Size
				}
			}
		}
		if want >= 0 && !fi.IsDir() && fi.Size() != want {
			return res, errors.New("文件已改变，请重新扫描：" + filepath.Base(p))
		}
	}
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if err := core.RecycleOnly([]string{p}); err != nil {
			res.Failed = append(res.Failed, filepath.Base(p)+"："+core.TrimErr(err))
			continue
		}
		res.Removed++
		res.Freed += fi.Size()
		core.Logf("整理：已移至回收站 %s", p)
	}
	// the list is stale now: what was moved is no longer there
	tidy.mu.Lock()
	delete(tidy.res, what)
	tidy.mu.Unlock()
	if res.Removed > 0 {
		StartPipeline(st, true, true, false, false, nil)
	}
	core.BumpRev()
	if len(res.Failed) > 0 && res.Removed == 0 {
		return res, errors.New("未能移至回收站")
	}
	return res, nil
}
