package core

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// The data of MioVRCA, the program this one is a modified version of.
//
// The first time this program starts with a data folder of its own that holds no library yet, what MioVRCA
// kept for this Windows user is copied in: the library, the covers, the settings, the AI's settings and
// keys, the wish list, the recipes, the logins. It is copied, not moved or used in place — MioVRCA goes on
// with its own, and neither program writes the other's files. It happens once (a mark is left), so a
// library emptied here on purpose is not filled again.
//
// Not copied: the built-in browsers' profiles (big, and locked while MioVRCA runs: a site may ask to log in
// again), the log, the update downloads, half-written files, and the default download folder (the library
// refers to what is in it where it is).

// UpstreamFolders: where MioVRCA keeps its data under base (%LOCALAPPDATA%), newest name first.
var UpstreamFolders = []string{"MioVRC_AssetManager", "VRC素材库"}

const upstreamMark = ".from-miovrca"

// upstreamSkip: what of MioVRCA's data folder is not copied (top-level names, lower case).
var upstreamSkip = map[string]bool{
	"webview": true, "webview2": true, "booth-profile": true, "web-login": true, "web-xianyu": true,
	"library.log": true, "update": true, "exe.stamp": true, "downloads": true, ".import-undo": true,
	"portable.txt": true,
}

// ImportUpstreamData copies MioVRCA's data into dst, when dst is the per-user data folder under base and
// has no library of its own. It answers the folder copied from ("" when nothing was).
func ImportUpstreamData(base, dst string) (string, error) {
	if base == "" || PathKey(dst) != PathKey(AppDataFolder(base)) {
		return "", nil // a portable copy, or a folder given on the command line: its own data only
	}
	if FileExists(filepath.Join(dst, "library.json")) || FileExists(filepath.Join(dst, upstreamMark)) {
		return "", nil
	}
	src := ""
	for _, n := range UpstreamFolders {
		if d := filepath.Join(base, n); PathKey(d) != PathKey(dst) && FileExists(filepath.Join(d, "library.json")) {
			src = d
			break
		}
	}
	if src == "" {
		return "", nil
	}
	if err := os.MkdirAll(dst, 0755); err != nil {
		return "", err
	}
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // what cannot be read is left behind; the rest still comes
		}
		rel, _ := filepath.Rel(src, p)
		if rel == "." {
			return nil
		}
		top := strings.ToLower(strings.SplitN(filepath.ToSlash(rel), "/", 2)[0])
		name := strings.ToLower(d.Name())
		if rel == "library.json" || upstreamSkip[top] || strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, ".part") || strings.HasPrefix(name, ".write-test-") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		to := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(to, 0755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return copyNew(p, to)
	})
	if err != nil {
		return "", err
	}
	// the library last: it is what says "there is data here", so a copy cut short is taken up again next time
	if err := copyNew(filepath.Join(src, "library.json"), filepath.Join(dst, "library.json")); err != nil {
		return "", err
	}
	if !FileExists(filepath.Join(dst, "library.json")) {
		return "", errors.New("未能复制 MioVRCA 的 library.json")
	}
	_ = os.WriteFile(filepath.Join(dst, upstreamMark), []byte(src+"\n"), 0644)
	return src, nil
}

// copyNew copies a file; one that is already there is left as it is.
func copyNew(from, to string) error {
	if FileExists(to) {
		return nil
	}
	in, err := os.Open(from)
	if err != nil {
		return nil // locked or gone: not a reason to stop
	}
	defer in.Close()
	tmp := to + ".copying"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, to)
}

// UserDataBase: the folder per-user data folders are made in (%LOCALAPPDATA% on Windows).
func UserDataBase() string { return userDataBase() }
