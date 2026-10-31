package unity

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vrclib/internal/core"
	"vrclib/internal/testkit"
)

// mkPackage: a unitypackage writing these files (pad: bytes of the first, to make it the bigger one).
func mkPackage(t *testing.T, p string, files []string, pad int) {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	put := func(name string, b []byte) {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(b)), Typeflag: tar.TypeReg})
		_, _ = tw.Write(b)
	}
	for i, f := range files {
		guid := fmt.Sprintf("%032x", len(p)*1000+i)
		put(guid+"/pathname", []byte(f))
		body := []byte("x")
		if i == 0 {
			body = bytes.Repeat([]byte("y"), pad+1)
		}
		put(guid+"/asset", body)
	}
	_ = tw.Close()
	_ = zw.Close()
	_ = os.MkdirAll(filepath.Dir(p), 0755)
	if err := os.WriteFile(p, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
}

func impProject(t *testing.T) string {
	t.Helper()
	proj := t.TempDir()
	for _, d := range []string{"Assets/Manuka", "ProjectSettings"} {
		_ = os.MkdirAll(filepath.Join(proj, filepath.FromSlash(d)), 0755)
	}
	return proj
}

func waitImp(t *testing.T, until func(*ImportJob) bool) *ImportJob {
	t.Helper()
	for i := 0; i < 400; i++ {
		if j := ImportSnapshot(); j != nil && until(j) {
			return j
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the import did not get there: %+v", ImportSnapshot())
	return nil
}

// Versions for two base bodies and a common package: the project is built on Manuka (its Assets say so), so
// Manuka's version and the common one are ticked.
func TestChoicesForBaseVersions(t *testing.T) {
	st := testkit.NewStore(t)
	st.Settings.Bases = []string{"Kaguya", "Manuka"}
	dir := t.TempDir()
	var in []pkgPick
	for _, n := range []string{"Moon_Kaguya", "Moon_Manuka", "Moon_Common"} {
		p := filepath.Join(dir, n+".unitypackage")
		mkPackage(t, p, []string{"Assets/Moon/" + n + ".prefab"}, 0)
		in = append(in, pkgPick{Path: p, Name: n + ".unitypackage", Size: 1})
	}
	cs, why := choicesFor(st, impProject(t), in)
	var got []string
	for _, c := range cs {
		got = append(got, fmt.Sprintf("%s:%v:%s", c.Name, c.Pick, strings.Join(c.Bases, ",")))
	}
	if why != "" || strings.Join(got, " ") != "Moon_Kaguya.unitypackage:false:Kaguya Moon_Manuka.unitypackage:true:Manuka Moon_Common.unitypackage:true:" {
		t.Errorf("%q %v", why, got)
	}
	// packages for no base body, writing files of their own: nothing to ask
	if cs, _ := choicesFor(st, impProject(t), in[2:]); cs != nil {
		t.Errorf("asked about one package: %v", cs)
	}
}

// Left and right hand write the same files: the bigger is ticked, the other is not and says why. A path that
// was not offered is refused; the choice made, the import goes on with it and says what it left out.
func TestImportAsksAboutOverlap(t *testing.T) {
	st := testkit.NewStore(t)
	dir := t.TempDir()
	files := []string{"Assets/Glove/Glove.prefab", "Assets/Glove/Glove.fbx", "Assets/Glove/Tex/a.png", "Assets/Glove/Mat/a.mat"}
	mkPackage(t, filepath.Join(dir, "Glove_Left.unitypackage"), files, 5000)
	mkPackage(t, filepath.Join(dir, "Glove_Right.unitypackage"), files, 0)
	mkPackage(t, filepath.Join(dir, "Glove_Extra.unitypackage"), []string{"Assets/Glove/Extra/b.prefab"}, 0)
	var locs []core.Location
	for _, n := range []string{"Glove_Left", "Glove_Right", "Glove_Extra"} {
		locs = append(locs, core.Location{Path: filepath.Join(dir, n+".unitypackage"), Kind: "unitypackage"})
	}
	st.Assets = []*core.Asset{{Key: "glove", Name: "Glove", Category: "配饰", Locations: locs}}
	proj := impProject(t)
	t.Cleanup(DismissImport)
	if err := StartImport(st, ImportReq{Key: "glove", Project: proj}); err != nil {
		t.Fatal(err)
	}
	j := waitImp(t, func(j *ImportJob) bool { return j.Stage == "choose" })
	picks := map[string]PkgChoice{}
	for _, c := range j.Choices {
		picks[c.Name] = c
	}
	if j.Why != "overlap" || !picks["Glove_Left.unitypackage"].Pick || picks["Glove_Right.unitypackage"].Pick || picks["Glove_Right.unitypackage"].Why != "alt" ||
		!strings.Contains(picks["Glove_Right.unitypackage"].Note, "Glove_Left") || !picks["Glove_Extra.unitypackage"].Pick {
		t.Fatalf("%+v", j)
	}
	if ChooseImport([]string{filepath.Join(dir, "other.unitypackage")}) || ChooseImport(nil) {
		t.Error("a path that was not offered was taken")
	}
	if !ChooseImport([]string{picks["Glove_Right.unitypackage"].Path, picks["Glove_Extra.unitypackage"].Path}) {
		t.Fatal("the choice was not taken")
	}
	j = waitImp(t, func(j *ImportJob) bool { return j.Stage == "done" || j.Stage == "failed" })
	if j.Stage != "done" || strings.Join(j.Imported, ",") != "Glove_Extra.unitypackage,Glove_Right.unitypackage" || len(j.Skipped) != 1 ||
		j.Skipped[0] != (PkgSkip{Name: "Glove_Left.unitypackage", Why: "alt"}) || !core.ContainsStr(j.Tops, "Glove/Extra") {
		t.Errorf("%+v", j)
	}
	if !core.StatOK(filepath.Join(proj, "Assets", "Glove", "Extra", "b.prefab")) {
		t.Error("not written")
	}
	// called off while it asks: it ends, and the next import is not refused as busy
	DismissImport()
	if err := StartImport(st, ImportReq{Key: "glove", Project: proj}); err != nil {
		t.Fatal(err)
	}
	waitImp(t, func(j *ImportJob) bool { return j.Stage == "choose" })
	DismissImport()
	time.Sleep(100 * time.Millisecond)
	if err := StartImport(st, ImportReq{Key: "glove", Project: proj, Paths: []string{picks["Glove_Extra.unitypackage"].Path}}); err != nil {
		t.Fatal(err)
	}
	if j := waitImp(t, func(j *ImportJob) bool { return j.Stage != "unpack" && j.Stage != "copy" }); j.Stage != "done" || len(j.Choices) != 0 {
		t.Errorf("named packages were asked about: %+v", j)
	}
}
