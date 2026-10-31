package library

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"vrclib/internal/core"
	"vrclib/internal/testkit"
)

func guid(prefix string, i int) string { return fmt.Sprintf("%s%031d", prefix, i) }

// guidPkg writes a unitypackage of these GUIDs.
func guidPkg(t *testing.T, p string, guids []string) {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, g := range guids {
		for _, f := range []string{"pathname", "asset"} {
			_ = tw.WriteHeader(&tar.Header{Name: g + "/" + f, Mode: 0644, Size: 1, Typeflag: tar.TypeReg})
			_, _ = tw.Write([]byte("x"))
		}
	}
	_ = tw.Close()
	_ = zw.Close()
	_ = os.MkdirAll(filepath.Dir(p), 0755)
	if err := os.WriteFile(p, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
}

// imported: what Unity leaves in a project for these GUIDs — a file and its .meta.
func imported(t *testing.T, project, folder string, guids []string) {
	t.Helper()
	for i, g := range guids {
		f := filepath.Join(project, "Assets", filepath.FromSlash(folder), fmt.Sprintf("f%d.asset", i))
		testkit.WriteFile(t, f, 1)
		if err := os.WriteFile(f+".meta", []byte("fileFormatVersion: 2\nguid: "+g+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

func project(t *testing.T, dir string) {
	t.Helper()
	for _, d := range []string{"Assets", "ProjectSettings"} {
		_ = os.MkdirAll(filepath.Join(dir, d), 0755)
	}
}

func guids(prefix string, n int) []string {
	var out []string
	for i := 0; i < n; i++ {
		out = append(out, guid(prefix, i))
	}
	return out
}

// An asset is in a project when the project holds its GUIDs: most of them "used", some "partial". The
// shader that outfits carry along does not make every one of them seem to be in every project. Projects are
// found below the project folder, a folder per model with versions inside, and named by their path there.
func TestUsageScan(t *testing.T) {
	st := testkit.NewStore(t)
	lib, prjs := t.TempDir(), t.TempDir()
	st.Settings.Roots, st.Settings.ProjectRoots = []string{lib}, []string{prjs}
	shader := guids("5", 6) // lilToon, in every outfit
	dress, hair, tail := guids("d", 10), guids("a", 10), guids("e", 10)
	guidPkg(t, filepath.Join(lib, "Moon Dress", "Moon Dress.unitypackage"), append(append([]string{}, dress...), shader...))
	guidPkg(t, filepath.Join(lib, "Twin Hair", "Twin Hair.unitypackage"), append(append([]string{}, hair...), shader...))
	guidPkg(t, filepath.Join(lib, "Wolf Tail", "Wolf Tail.unitypackage"), append(append([]string{}, tail...), shader...))
	a, b := filepath.Join(prjs, "Manuka", "v2"), filepath.Join(prjs, "Kipfel")
	project(t, a)
	project(t, b)
	project(t, filepath.Join(a, "Assets", "Nested")) // a project inside a project is not another one
	imported(t, a, "Shop/Moon", dress)               // all of the dress
	imported(t, a, "Shop/Hair", hair[:3])            // three files of the hair
	imported(t, a, "lilToon", shader)                // and the shader
	imported(t, b, "lilToon", shader)                // only the shader
	RunFolderScan(st, &core.Task{})
	RunUsageScan(st, &core.Task{})

	if len(st.Projects) != 2 || st.Projects[0].Name != "Kipfel" || st.Projects[1].Name != "Manuka/v2" {
		t.Fatalf("projects: %+v", st.Projects)
	}
	by := map[string]*core.Asset{}
	for _, x := range st.Assets {
		by[x.Name] = x
	}
	d := by["Moon Dress"]
	if len(d.Usage) != 1 || d.Usage[0].Project != "Manuka/v2" || d.Usage[0].Status != "used" || d.Usage[0].Matched != 10 ||
		d.Usage[0].Total != 10 || d.Usage[0].Folder != "Assets/Shop/Moon" || d.GuidCount != 10 {
		t.Errorf("the dress: %+v (guids %d)", d.Usage, d.GuidCount)
	}
	if h := by["Twin Hair"]; len(h.Usage) != 1 || h.Usage[0].Status != "partial" {
		t.Errorf("the hair: %+v", h.Usage)
	}
	if w := by["Wolf Tail"]; len(w.Usage) != 0 {
		t.Errorf("the tail is in no project (only the shader it carries is): %+v", w.Usage)
	}
	if st.Projects[1].Assets != 2 || st.Projects[0].Assets != 0 || st.LastUsage == 0 {
		t.Errorf("project counts: %+v", st.Projects)
	}
	if !core.StatOK(filepath.Join(core.DataDir, "guidcache.json")) {
		t.Error("the packages' GUIDs are not kept")
	}
	// the project's name is what the cards and the usage records go by
	st.Mu.RLock()
	name := st.UsageName(a)
	st.Mu.RUnlock()
	if name != "Manuka/v2" {
		t.Errorf("usage name: %q", name)
	}
	// an asset taken out of the project: no longer in it
	_ = os.RemoveAll(filepath.Join(a, "Assets", "Shop", "Moon"))
	RunUsageScan(st, &core.Task{})
	if len(by["Moon Dress"].Usage) != 0 {
		t.Errorf("still used after it was removed: %+v", by["Moon Dress"].Usage)
	}
}

// A project added by hand (a folder that is itself a project) is found, and one that is no longer there is
// dropped.
func TestUsageProjectsByHand(t *testing.T) {
	st := testkit.NewStore(t)
	p := filepath.Join(t.TempDir(), "Solo")
	project(t, p)
	gone := filepath.Join(t.TempDir(), "Gone")
	st.Settings.ProjectRoots = []string{p}
	st.Projects = []core.ProjectInfo{{Name: "Gone", Path: gone}}
	RunUsageScan(st, &core.Task{})
	if len(st.Projects) != 1 || st.Projects[0].Path != p || st.Projects[0].Name != "Solo" {
		t.Errorf("projects: %+v", st.Projects)
	}
}
