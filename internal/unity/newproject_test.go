package unity

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vrclib/internal/core"
	"vrclib/internal/testkit"
)

// fakeVPM: the repositories a new project reads, as test/fakevpm.py has them. lilToon offers a version made
// for a later Unity, which is not to be taken; one zip carries everything in a folder of its own.
func fakeVPM(t *testing.T) *httptest.Server {
	t.Helper()
	zips := map[string][]byte{}
	var srv *httptest.Server
	pkg := func(name, version string, deps map[string]string, top, unityVer string) map[string]any {
		var b bytes.Buffer
		zw := zip.NewWriter(&b)
		for f, c := range map[string]string{"package.json": `{"name":"` + name + `","version":"` + version + `"}`, "Editor/x.cs": "// " + name} {
			w, _ := zw.Create(top + f)
			_, _ = w.Write([]byte(c))
		}
		_ = zw.Close()
		p := "/dl/" + name + "-" + version + ".zip"
		zips[p] = b.Bytes()
		sum := sha256.Sum256(b.Bytes())
		v := map[string]any{"name": name, "version": version, "url": "BASE" + p, "zipSHA256": hex.EncodeToString(sum[:]), "unity": unityVer}
		if deps != nil {
			v["vpmDependencies"] = deps
		}
		return v
	}
	listing := func(id string, pkgs map[string][]map[string]any) map[string]any {
		out := map[string]any{}
		for name, vs := range pkgs {
			vers := map[string]any{}
			for _, v := range vs {
				vers[v["version"].(string)] = v
			}
			out[name] = map[string]any{"versions": vers}
		}
		return map[string]any{"id": id, "name": id, "packages": out}
	}
	L := map[string]map[string]any{
		"com.vrchat.repos.official": listing("com.vrchat.repos.official", map[string][]map[string]any{
			"com.vrchat.base":              {pkg("com.vrchat.base", "3.10.5", nil, "", "2022.3")},
			"com.vrchat.avatars":           {pkg("com.vrchat.avatars", "3.10.5", map[string]string{"com.vrchat.base": "3.10.5"}, "", "2022.3")},
			"com.vrchat.core.vpm-resolver": {pkg("com.vrchat.core.vpm-resolver", "0.1.29", nil, "", "2022.3")}}),
		"com.vrchat.repos.curated": listing("com.vrchat.repos.curated", map[string][]map[string]any{}),
		"dev.nadena.vpm": listing("dev.nadena.vpm", map[string][]map[string]any{
			"nadena.dev.ndmf":           {pkg("nadena.dev.ndmf", "1.14.8", nil, "", "2022.3")},
			"nadena.dev.modular-avatar": {pkg("nadena.dev.modular-avatar", "1.18.7", map[string]string{"nadena.dev.ndmf": ">=1.14.7 <2.0.0-a", "com.vrchat.avatars": ">=3.7.4"}, "", "2022.3")}}),
		"io.github.lilxyzw.vpm": listing("io.github.lilxyzw.vpm", map[string][]map[string]any{
			"jp.lilxyzw.liltoon": {pkg("jp.lilxyzw.liltoon", "2.3.4", nil, "lilToon-2.3.4/", "2022.3"), pkg("jp.lilxyzw.liltoon", "3.0.0", nil, "", "6000.0")}}),
		"com.anatawa12.vpm": listing("com.anatawa12.vpm", map[string][]map[string]any{
			"com.anatawa12.avatar-optimizer": {pkg("com.anatawa12.avatar-optimizer", "1.9.20", map[string]string{"nadena.dev.ndmf": ">=1.8.0 <2.0.0", "com.vrchat.avatars": ">=3.7.0 <3.11.0"}, "", "2022.3")}}),
	}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if b, ok := zips[p]; ok {
			_, _ = w.Write(b)
			return
		}
		if id, ok := strings.CutSuffix(strings.TrimPrefix(p, "/"), ".json"); ok && L[id] != nil {
			b, _ := json.Marshal(L[id])
			_, _ = w.Write([]byte(strings.ReplaceAll(string(b), "BASE/", srv.URL+"/")))
			return
		}
		if p == "/template/ProjectSettings.asset" {
			_, _ = w.Write([]byte("PlayerSettings:\n  productName: World\n  m_ActiveColorSpace: 1\n"))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("VRCLIB_VPM_BASE", srv.URL)
	t.Setenv("VRCLIB_VCCDIR", t.TempDir())
	return srv
}

func waitNP(t *testing.T) *NewProjectJob {
	t.Helper()
	for i := 0; i < 600; i++ {
		if j := NewProjectSnapshot(); j != nil && (j.Stage == "done" || j.Stage == "failed") {
			return j
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("not finished: %+v", NewProjectSnapshot())
	return nil
}

// A new project: the SDK and the plugins picked, with what they need, in the version that fits this Unity;
// a manifest that VCC reads; the project in the list and in VCC's; nothing left of the work in progress.
func TestNewProject(t *testing.T) {
	fakeVPM(t)
	t.Cleanup(DismissNewProject)
	st := testkit.NewStore(t)
	_ = os.WriteFile(filepath.Join(vccDir(), "settings.json"), []byte(`{"userProjects":[]}`), 0644)
	parent := t.TempDir()
	err := StartNewProject(st, NewProjectReq{Name: "Kaguya_Mod", Parent: parent,
		Plugins: []string{"jp.lilxyzw.liltoon", "com.anatawa12.avatar-optimizer"}})
	if err != nil {
		t.Fatal(err)
	}
	j := waitNP(t)
	if j.Stage != "done" {
		t.Fatalf("job: %+v", j)
	}
	dir := filepath.Join(parent, "Kaguya_Mod")
	for _, pkg := range []string{"com.vrchat.avatars", "com.vrchat.base", "com.vrchat.core.vpm-resolver", "nadena.dev.modular-avatar",
		"nadena.dev.ndmf", "jp.lilxyzw.liltoon", "com.anatawa12.avatar-optimizer"} {
		if !core.StatOK(filepath.Join(dir, "Packages", pkg, "package.json")) {
			t.Errorf("%s not in Packages (the zip's own folder cut off): %v", pkg, testkit.ListTree(filepath.Join(dir, "Packages")))
		}
	}
	var man struct {
		Dependencies map[string]map[string]string `json:"dependencies"`
		Locked       map[string]map[string]any    `json:"locked"`
	}
	b, _ := os.ReadFile(filepath.Join(dir, "Packages", "vpm-manifest.json"))
	if err := json.Unmarshal(b, &man); err != nil {
		t.Fatal(err)
	}
	if man.Locked["jp.lilxyzw.liltoon"]["version"] != "2.3.4" {
		t.Errorf("lilToon: %v (the one for a later Unity was taken)", man.Locked["jp.lilxyzw.liltoon"])
	}
	if man.Dependencies["nadena.dev.modular-avatar"] == nil || man.Dependencies["nadena.dev.ndmf"] != nil || man.Locked["nadena.dev.ndmf"] == nil {
		t.Errorf("manifest: %s", b)
	}
	if v, _ := os.ReadFile(filepath.Join(dir, "ProjectSettings", "ProjectVersion.txt")); !strings.Contains(string(v), UnityWant) {
		t.Errorf("version: %s", v)
	}
	if ps, _ := os.ReadFile(filepath.Join(dir, "ProjectSettings", "ProjectSettings.asset")); !strings.Contains(string(ps), "productName: Kaguya_Mod") {
		t.Errorf("the product is not named after the project: %s", ps)
	}
	if core.StatOK(filepath.Join(dir, npMarker)) {
		t.Error("the mark of a project in the making is still there")
	}
	if !core.IsUnityProject(dir) || len(st.Projects) != 1 || st.Projects[0].Path != dir {
		t.Errorf("projects: %+v", st.Projects)
	}
	if v, _ := os.ReadFile(filepath.Join(vccDir(), "settings.json")); !strings.Contains(string(v), "Kaguya_Mod") {
		t.Errorf("not in VCC's list: %s", v)
	}
	// the name is taken now; asked again it is refused, and nothing is touched
	if !NewProjectTaken(dir) {
		t.Error("a finished project is not taken")
	}
	DismissNewProject()
	if err := StartNewProject(st, NewProjectReq{Name: "Kaguya_Mod", Parent: parent}); err == nil {
		t.Error("made over a project that is there")
	}
}

// What a failed attempt left behind is no project: a new attempt may take the folder over. The SDK cannot be
// had: the attempt fails, saying why.
func TestNewProjectFailsAndIsTakenOver(t *testing.T) {
	srv := fakeVPM(t)
	t.Cleanup(DismissNewProject)
	st := testkit.NewStore(t)
	parent := t.TempDir()
	t.Setenv("VRCLIB_VPM_BASE", srv.URL+"/nothing-here")
	if err := StartNewProject(st, NewProjectReq{Name: "Broken", Parent: parent}); err != nil {
		t.Fatal(err)
	}
	if j := waitNP(t); j.Stage != "failed" || !strings.Contains(j.Err, "插件仓库") {
		t.Fatalf("job: %+v", j)
	}
	dir := filepath.Join(parent, "Broken")
	if NewProjectTaken(dir) {
		t.Error("what a failed attempt left counts as a project")
	}
	DismissNewProject()
	t.Setenv("VRCLIB_VPM_BASE", srv.URL)
	if err := StartNewProject(st, NewProjectReq{Name: "Broken", Parent: parent}); err != nil {
		t.Fatal(err)
	}
	if j := waitNP(t); j.Stage != "done" {
		t.Fatalf("again: %+v", j)
	}
	// a folder of somebody else's of that name is not taken over
	other := filepath.Join(parent, "Theirs")
	testkit.WriteFile(t, filepath.Join(other, "notes.txt"), 3)
	DismissNewProject()
	if err := StartNewProject(st, NewProjectReq{Name: "Theirs", Parent: parent}); err == nil {
		t.Error("made into a folder that holds somebody's files")
	}
	for _, bad := range []string{"", "a/b", "a:b", "  "} {
		if err := StartNewProject(st, NewProjectReq{Name: bad, Parent: parent}); err == nil {
			t.Errorf("name %q accepted", bad)
		}
	}
}

// What the new-project form is told about this computer and the library.
func TestFindToolchain(t *testing.T) {
	fakeVPM(t)
	st := testkit.NewStore(t)
	st.Settings.ProjectRoots = []string{`D:\VRChat_Project`}
	st.Assets = []*core.Asset{{Key: "name:kipfel", Name: "Kipfel", Category: "素体", Bases: []string{"Kipfel"}}, {Key: "name:dress", Name: "Dress", Category: "衣服"}}
	tc := FindToolchain(st)
	if tc.UnityWant != UnityWant || !strings.HasPrefix(tc.HubLink, "unityhub://"+UnityWant+"/") || tc.DefaultDir != `D:\VRChat_Project` {
		t.Errorf("toolchain: %+v", tc)
	}
	if len(tc.Bases) != 1 || tc.Bases[0].Key != "name:kipfel" {
		t.Errorf("bases: %+v", tc.Bases)
	}
	fixed := 0
	for _, p := range tc.Plugins {
		if p.Fixed {
			fixed++
		}
	}
	if fixed != 2 || len(tc.Plugins) != len(basePlugins)+len(gitPlugins) {
		t.Errorf("plugins: %+v", tc.Plugins)
	}
}
