package library

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vrclib/internal/core"
	"vrclib/internal/testkit"
)

// makePkg writes a unitypackage: each entry an asset path and, when given, the preview Unity made of it (a
// picture of that many pixels a side, so that the one taken can be told by its size).
func makePkg(t *testing.T, p string, entries map[string]int) {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	put := func(name string, b []byte) {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(b)), Typeflag: tar.TypeReg})
		_, _ = tw.Write(b)
	}
	i := 0
	for asset, side := range entries {
		i++
		guid := strings.Repeat(string(rune('a'+i)), 32)
		if side > 0 { // the preview first, as Unity writes it, before the path that says what it is of
			var img bytes.Buffer
			_ = png.Encode(&img, image.NewRGBA(image.Rect(0, 0, side, side)))
			put(guid+"/preview.png", img.Bytes())
		}
		put(guid+"/pathname", []byte(asset+"\n00"))
		put(guid+"/asset", []byte("x"))
	}
	_ = tw.Close()
	_ = zw.Close()
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
}

func coverSide(t *testing.T, p string) int {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, err := png.DecodeConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Width
}

func viewOf(st *core.Store, name string) AssetView {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	for _, v := range AllViews(st) {
		if v.Name == name {
			return v
		}
	}
	return AssetView{}
}

func waitPkg(t *testing.T) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if !PkgBatchSnapshot().Running && !BackgroundBusy() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the batch did not end")
}

// A card without a picture shows the preview of its package's prefab — not a texture's, though that one is
// larger — and says it is only a preview. A Booth picture comes before it.
func TestPkgCoverChoosesThePrefab(t *testing.T) {
	st := testkit.NewStore(t)
	root := t.TempDir()
	st.Settings.Roots = []string{root}
	makePkg(t, filepath.Join(root, "Moon Dress", "Moon Dress.unitypackage"), map[string]int{
		"Assets/Moon/Moon Dress.prefab": 64, "Assets/Moon/Tex/body.png": 128, "Assets/Moon/readme.txt": 0})
	RunFolderScan(st, &core.Task{})
	key := viewOf(st, "Moon Dress").Key
	c, err := ExtractPkgCover(st, key)
	if err != nil {
		t.Fatal(err)
	}
	if coverSide(t, c) != 64 {
		t.Errorf("not the prefab's preview: %d pixels", coverSide(t, c))
	}
	if s := PkgCoverStatusOf(st, key); s.Cover == nil || s.Cover.Entry != "Assets/Moon/Moon Dress.prefab" {
		t.Errorf("status: %+v", s)
	}
	if v := viewOf(st, "Moon Dress"); v.Cover != c || !v.CoverPkg {
		t.Errorf("the card: cover %q, from a package %v", v.Cover, v.CoverPkg)
	}
	// the Booth product's picture, once linked, comes first
	booth := filepath.Join(t.TempDir(), "booth.jpg")
	_ = os.WriteFile(booth, testkit.JPEG(8, 8, color.RGBA{1, 2, 3, 255}), 0644)
	st.Mu.Lock()
	st.User[key] = &core.UserData{BoothURL: "https://booth.pm/ja/items/1234567"}
	st.Booth["1234567"] = &core.BoothInfo{ID: "1234567", Name: "Moon Dress", Cover: booth}
	st.Mu.Unlock()
	if v := viewOf(st, "Moon Dress"); v.Cover != booth || v.CoverPkg {
		t.Errorf("with a Booth picture: %q %v", v.Cover, v.CoverPkg)
	}
}

// After a scan the cards without a picture are looked into by themselves — each once, again only when its
// packages changed. One the player took the cover off is left alone; a package without previews says so.
func TestPkgCoverAfterScan(t *testing.T) {
	st := testkit.NewStore(t)
	root := t.TempDir()
	st.Settings.Roots = []string{root}
	makePkg(t, filepath.Join(root, "Alpha", "Alpha.unitypackage"), map[string]int{"Assets/A/Alpha.prefab": 32})
	makePkg(t, filepath.Join(root, "Beta", "Beta.unitypackage"), map[string]int{"Assets/B/readme.txt": 0})
	RunPipeline(st, true, false, false, false, nil)
	waitPkg(t)
	a, b := viewOf(st, "Alpha"), viewOf(st, "Beta")
	if !a.CoverPkg || a.Cover == "" {
		t.Errorf("Alpha: %+v", a)
	}
	if s := PkgCoverStatusOf(st, b.Key); b.Cover != "" || s.Failed != "unitypackage 中没有可用的预览图" {
		t.Errorf("Beta: %q, %+v", b.Cover, s)
	}
	if !core.StatOK(filepath.Join(core.DataDir, "pkgcovers.json")) {
		t.Error("what was found is not kept")
	}
	// nothing changed: the next scan does not look again
	if got := PkgBatchSnapshot(); got.Total != 2 || got.Made != 1 || got.Failed != 1 {
		t.Errorf("first batch: %+v", got)
	}
	RunPipeline(st, true, false, false, false, nil)
	waitPkg(t)
	if got := PkgBatchSnapshot(); got.Total != 2 {
		t.Errorf("looked again although nothing changed: %+v", got)
	}
	// Beta's package now has a preview: it is looked into again
	time.Sleep(1100 * time.Millisecond) // a newer time on the file
	makePkg(t, filepath.Join(root, "Beta", "Beta.unitypackage"), map[string]int{"Assets/B/Beta.prefab": 16})
	RunPipeline(st, true, false, false, false, nil)
	waitPkg(t)
	if v := viewOf(st, "Beta"); !v.CoverPkg {
		t.Errorf("Beta after its package changed: %+v", PkgBatchSnapshot())
	}
	// the player takes Alpha's off: it stays off, also when asked for every card
	RemovePkgCover(st, a.Key)
	if v := viewOf(st, "Alpha"); v.Cover != "" || v.CoverPkg {
		t.Errorf("after removing: %q", v.Cover)
	}
	if err := StartPkgBatch(st); err == nil {
		waitPkg(t)
	}
	if v := viewOf(st, "Alpha"); v.CoverPkg {
		t.Error("a cover the player removed came back by itself")
	}
	// asked for by the player, it is taken again
	if _, err := ExtractPkgCover(st, a.Key); err != nil {
		t.Fatal(err)
	}
	if v := viewOf(st, "Alpha"); !v.CoverPkg {
		t.Error("extracting by hand did not bring it back")
	}
}

// What is not a unitypackage, or is broken, is an error — not a crash, not a cover.
func TestPkgCoverBadPackage(t *testing.T) {
	st := testkit.NewStore(t)
	root := t.TempDir()
	st.Settings.Roots = []string{root}
	testkit.WriteFile(t, filepath.Join(root, "Junk", "Junk.unitypackage"), 300)
	RunFolderScan(st, &core.Task{})
	if _, err := ExtractPkgCover(st, viewOf(st, "Junk").Key); err == nil || !strings.Contains(err.Error(), "unitypackage") {
		t.Errorf("err: %v", err)
	}
	if _, err := ExtractPkgCover(st, "name:nothing"); err == nil {
		t.Error("a card that is not there")
	}
}
