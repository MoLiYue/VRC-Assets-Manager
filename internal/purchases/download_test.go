package purchases

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"vrclib/internal/core"
	"vrclib/internal/testkit"
	"vrclib/internal/unity"
)

// a store that answers the download addresses the library page gave
type dlFake struct {
	mu    sync.Mutex
	files map[string][]byte // download id → what it holds
	names map[string]string // download id → the name it offers in Content-Disposition
	got   []string
	deny  bool // answer as the store does when the login is no longer good
}

func (f *dlFake) serve() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/downloadables/")
		f.mu.Lock()
		f.got = append(f.got, id)
		b, ok := f.files[id]
		name, deny := f.names[id], f.deny
		f.mu.Unlock()
		if deny {
			w.WriteHeader(403)
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		if name != "" {
			w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		}
		http.ServeContent(w, r, name, time.Now(), strings.NewReader(string(b)))
	}))
}

func (f *dlFake) asked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.got...)
}

// resetDL puts the queue back to empty: the worker goroutine lives as long as the program, so each test
// starts from nothing of its own.
func resetDL(t *testing.T) {
	t.Helper()
	CancelDownloads()
	// what an earlier test left running ends first: it reads the data folder, which this test replaces
	for i := 0; i < 400 && dlBusy.Load(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	dlMu.Lock()
	dlJobs, dlQ = nil, nil
	dlPending = map[string]unity.ImportReq{}
	dlMu.Unlock()
}

// dlSetup: a store with a download folder, a purchase of two files, and a server that answers them.
func dlSetup(t *testing.T, files map[string][]byte) (*core.Store, *dlFake, *httptest.Server, string) {
	t.Helper()
	f := &dlFake{files: files, names: map[string]string{}}
	srv := f.serve()
	t.Cleanup(srv.Close)
	st := testkit.NewStore(t)
	dir := t.TempDir()
	st.Settings.Roots = []string{dir}
	t.Setenv("VRCLIB_BOOTH_BASE", srv.URL)
	if err := saveSession(boothSessionFile(), []core.SavedCookie{{Name: "_plaza_session_nktz7u", Value: "ok"}}); err != nil {
		t.Fatal(err)
	}
	var dls, names []string
	for id := range files {
		dls = append(dls, srv.URL+"/downloadables/"+id)
		names = append(names, id)
	}
	sortTwo(names, dls)
	st.Purchases["9000001"] = &core.Purchase{ID: "9000001", Name: "Moon Dress", Files: names, Downloads: dls}
	return st, f, srv, dir
}

// sortTwo sorts the names and keeps the addresses beside them (map order is not fixed).
func sortTwo(names, dls []string) {
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
			dls[j], dls[j-1] = dls[j-1], dls[j]
		}
	}
}

// waitDL waits until nothing of a purchase is on its way any more.
func waitDL(t *testing.T, item string) []DLJob {
	t.Helper()
	for i := 0; i < 600; i++ {
		jobs, busy := DLSnapshot(), false
		for _, j := range jobs {
			if j.Item == item && (j.Status == "queued" || j.Status == "running" || j.Status == "unpacking") {
				busy = true
			}
		}
		if !busy && len(jobs) > 0 {
			time.Sleep(60 * time.Millisecond) // let what follows the last file finish
			return DLSnapshot()
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the downloads did not finish: %+v", DLSnapshot())
	return nil
}

// The files of a purchase land in a folder of their own, and what can be unpacked is unpacked; the archive
// then goes to the Recycle Bin and the part file is gone.
func TestQueueDownloadsAndUnpack(t *testing.T) {
	resetDL(t)
	zip := testkit.ZipBytes(t, map[string][]byte{"Moon Dress/dress.unitypackage": []byte("pkg")})
	st, f, _, dir := dlSetup(t, map[string][]byte{"d1.zip": zip, "d2.unitypackage": []byte("standalone")})
	n, err := QueueDownloads(st, "9000001", nil)
	if err != nil || n != 2 {
		t.Fatalf("queued %d: %v", n, err)
	}
	jobs := waitDL(t, "9000001")
	for _, j := range jobs {
		if j.Status != "done" {
			t.Fatalf("job %s: %+v", j.ID, j)
		}
	}
	into := filepath.Join(dir, "Moon Dress")
	got := testkit.ListTree(into)
	if !core.ContainsStr(got, "d2.unitypackage") {
		t.Errorf("the file that is no archive: %v", got)
	}
	if !core.ContainsStr(got, "Moon Dress/dress.unitypackage") {
		t.Errorf("what came out of the archive: %v", got)
	}
	if core.ContainsStr(got, "d1.zip") {
		t.Errorf("the archive was not cleared away: %v", got)
	}
	for _, p := range got {
		if strings.HasSuffix(p, ".part") {
			t.Errorf("a part file was left behind: %v", got)
		}
	}
	if len(f.asked()) != 2 {
		t.Errorf("asked for: %v", f.asked())
	}
}

// Only the files that were ticked are fetched.
func TestQueueDownloadsOnlyChosen(t *testing.T) {
	resetDL(t)
	st, f, _, dir := dlSetup(t, map[string][]byte{"d1.bin": []byte("one"), "d2.bin": []byte("two")})
	if n, err := QueueDownloads(st, "9000001", []string{"d2.bin"}); err != nil || n != 1 {
		t.Fatalf("queued %d: %v", n, err)
	}
	waitDL(t, "9000001")
	if got := f.asked(); len(got) != 1 || got[0] != "d2.bin" {
		t.Errorf("asked for: %v", got)
	}
	if got := testkit.ListTree(filepath.Join(dir, "Moon Dress")); len(got) != 1 || got[0] != "d2.bin" {
		t.Errorf("in the folder: %v", got)
	}
	// queueing the same file again while it is there is not an error worth a second download
	waitDL(t, "9000001")
	if _, err := QueueDownloads(st, "9000001", []string{"d2.bin"}); err != nil {
		t.Errorf("again: %v", err)
	}
	waitDL(t, "9000001")
	if got := f.asked(); len(got) != 2 {
		t.Errorf("the second time it is fetched again (the file may have changed): %v", got)
	}
}

// "保留压缩包" leaves the archives packed.
func TestQueueDownloadsKeepsArchivesWhenAsked(t *testing.T) {
	resetDL(t)
	zip := testkit.ZipBytes(t, map[string][]byte{"a/x.unitypackage": []byte("pkg")})
	st, _, _, dir := dlSetup(t, map[string][]byte{"d1.zip": zip})
	st.Settings.NoExtract = true
	if _, err := QueueDownloads(st, "9000001", nil); err != nil {
		t.Fatal(err)
	}
	waitDL(t, "9000001")
	if got := testkit.ListTree(filepath.Join(dir, "Moon Dress")); len(got) != 1 || got[0] != "d1.zip" {
		t.Errorf("packed as it was: %v", got)
	}
}

// A file whose name is taken gets a number: an older version the player keeps is not replaced.
func TestQueueDownloadsDoesNotReplace(t *testing.T) {
	resetDL(t)
	st, _, _, dir := dlSetup(t, map[string][]byte{"d1.bin": []byte("new")})
	into := filepath.Join(dir, "Moon Dress")
	if err := os.MkdirAll(into, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(into, "d1.bin"), []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := QueueDownloads(st, "9000001", nil); err != nil {
		t.Fatal(err)
	}
	waitDL(t, "9000001")
	got := testkit.ListTree(into)
	if len(got) != 2 || !core.ContainsStr(got, "d1 (2).bin") {
		t.Errorf("in the folder: %v", got)
	}
	if b, _ := os.ReadFile(filepath.Join(into, "d1.bin")); string(b) != "old" {
		t.Errorf("what was there was replaced: %q", b)
	}
}

// A login the store no longer accepts: the file waits for the player instead of failing, and the window is
// told; stopping the downloads lets it go.
func TestQueueDownloadsWaitsForLogin(t *testing.T) {
	resetDL(t)
	st, f, _, _ := dlSetup(t, map[string][]byte{"d1.bin": []byte("x")})
	f.mu.Lock()
	f.deny = true
	f.mu.Unlock()
	if _, err := QueueDownloads(st, "9000001", nil); err != nil {
		t.Fatal(err)
	}
	ok := false
	for i := 0; i < 200 && !ok; i++ {
		time.Sleep(50 * time.Millisecond)
		for _, j := range DLSnapshot() {
			if j.Status == "login" && j.Login {
				ok = true
			}
		}
	}
	if !ok || !DLNeedLogin() {
		t.Fatalf("waiting for a login: %+v", DLSnapshot())
	}
	CancelDownloads()
	for i := 0; i < 200; i++ {
		time.Sleep(50 * time.Millisecond)
		done := true
		for _, j := range DLSnapshot() {
			if j.Status == "login" || j.Status == "running" || j.Status == "queued" {
				done = false
			}
		}
		if done {
			return
		}
	}
	t.Errorf("not let go of: %+v", DLSnapshot())
}

// What "下载并导入" asked for is handed on once the files are in — and a download does not wait on it.
func TestPendingImportHandedOnAfterDownload(t *testing.T) {
	resetDL(t)
	st, _, _, _ := dlSetup(t, map[string][]byte{"d1.bin": []byte("x")})
	var got string
	old := ImportAfterDownload
	ImportAfterDownload = func(_ *core.Store, req unity.ImportReq) error { got = req.Key + "|" + req.Project; return nil }
	t.Cleanup(func() { ImportAfterDownload = old })
	SetPendingImport("9000001", unity.ImportReq{Project: `C:\Proj`, Recycle: true})
	if _, err := QueueDownloads(st, "9000001", nil); err != nil {
		t.Fatal(err)
	}
	waitDL(t, "9000001")
	if got != "purchase:9000001|C:\\Proj" {
		t.Errorf("the import that was asked for: %q", got)
	}
	// and it is handed on once only
	got = ""
	if _, err := QueueDownloads(st, "9000001", nil); err != nil {
		t.Fatal(err)
	}
	waitDL(t, "9000001")
	if got != "" {
		t.Errorf("handed on twice: %q", got)
	}
}
