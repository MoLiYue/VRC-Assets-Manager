package core

import (
	"os"
	"path/filepath"
	"testing"
)

// MioVRCA's data comes over once, into the per-user folder only: the library, the covers and the logins;
// not the browsers' profiles, the log or half-written files. MioVRCA's own folder is left as it was.
func TestImportUpstreamData(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "MioVRC_AssetManager")
	for f, body := range map[string]string{"library.json": `{"assets":[]}`, "covers/a.jpg": "jpg", "booth-session.dat": "s",
		"webview/EBWebView/x": "big", "library.log": "log", "wishlist.json.tmp": "half", "recipes/r.json": "{}"} {
		p := filepath.Join(src, filepath.FromSlash(f))
		_ = os.MkdirAll(filepath.Dir(p), 0755)
		_ = os.WriteFile(p, []byte(body), 0644)
	}
	dst := AppDataFolder(base)
	if from, err := ImportUpstreamData(base, filepath.Join(base, "elsewhere")); from != "" || err != nil {
		t.Errorf("a folder of its own was filled: %q %v", from, err)
	}
	from, err := ImportUpstreamData(base, dst)
	if err != nil || from != src {
		t.Fatalf("%q %v", from, err)
	}
	for _, f := range []string{"library.json", "covers/a.jpg", "booth-session.dat", "recipes/r.json"} {
		if !FileExists(filepath.Join(dst, filepath.FromSlash(f))) {
			t.Errorf("not copied: %s", f)
		}
	}
	for _, f := range []string{"webview", "library.log", "wishlist.json.tmp"} {
		if FileExists(filepath.Join(dst, f)) {
			t.Errorf("copied: %s", f)
		}
	}
	if !FileExists(filepath.Join(src, "library.json")) || !FileExists(filepath.Join(src, "webview", "EBWebView", "x")) {
		t.Error("MioVRCA's own data was touched")
	}
	// once only: a library emptied here is not filled again
	_ = os.Remove(filepath.Join(dst, "library.json"))
	if from, _ := ImportUpstreamData(base, dst); from != "" {
		t.Error("copied a second time")
	}
}
