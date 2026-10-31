package pandl

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vrclib/internal/core"
	"vrclib/internal/library"
	"vrclib/internal/testkit"
)

func waitJob(t *testing.T, key string, until func(PanJob) bool) PanJob {
	t.Helper()
	for i := 0; i < 600; i++ {
		for _, j := range PanJobsSnapshot() {
			if j.Key == key && until(j) {
				return j
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %s: %+v", key, PanJobsSnapshot())
	return PanJob{}
}

func ended(j PanJob) bool { return over(j.Stage) }

// settle waits for the queue to be done and for what a download sets off — the library's rescan — to end:
// both work in the data folder, which the next test replaces.
func settle(t *testing.T) {
	t.Helper()
	for i := 0; i < 1000; i++ {
		qMu.Lock()
		busy := running || len(queue) > 0
		qMu.Unlock()
		if !busy && !library.BackgroundBusy() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("the downloads, or the rescan after them, did not end")
}

func panStore(t *testing.T, base string, shares map[string][2]string) *core.Store {
	t.Helper()
	st := testkit.NewStore(t)
	st.Settings.Roots = []string{t.TempDir()}
	st.Settings.NoWatch = true
	for key, s := range shares {
		st.User[key] = &core.UserData{ShareURL: "https://pan.baidu.com/s/" + strings.TrimPrefix(key, "pan:"), SharePwd: s[0], Name: s[1]}
	}
	t.Cleanup(func() {
		CancelPanDownloads()
		settle(t)
		jobMu.Lock()
		jobs = nil
		jobMu.Unlock()
	})
	return st
}

func mockSwitch(t *testing.T, base, q string) {
	t.Helper()
	resp, err := http.Post(base+"/mock/bd?"+q, "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

// A share with an extraction code, the whole of it: saved into a folder of the player's netdisk, fetched
// from there through the PCS (a file that breaks off halfway carried on), unpacked, and the card told where
// it is now.
func TestBaiduDownload(t *testing.T) {
	base := mockWeb(t)
	st := panStore(t, base, map[string][2]string{"pan:1DlTestShare": {"dl12", "Moon Dress 🌙"}})
	if err := SaveBaiduSession(&BaiduSession{Name: "mio测试", Cookies: loggedIn}); err != nil {
		t.Fatal(err)
	}
	mockSwitch(t, base, "cut=readme.txt")
	if err := QueuePanDownload(st, "pan:1DlTestShare", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := QueuePanDownload(st, "pan:1DlTestShare", nil, nil); err == nil {
		t.Error("the same share was queued twice")
	}
	j := waitJob(t, "pan:1DlTestShare", ended)
	if j.Stage != "done" {
		t.Fatalf("job: %+v", j)
	}
	if !strings.HasPrefix(j.Saved, "/MioVRCA/Moon Dress") || strings.Contains(j.Saved, "🌙") {
		t.Errorf("the copy in the netdisk: %q (Baidu takes no emoji in a name)", j.Saved)
	}
	got := testkit.ListTree(j.Dir)
	for _, want := range []string{"Moon Dress 月光裙/readme.txt", "Moon Dress 月光裙/MoonDress_v1/MoonDress_Plum.unitypackage",
		"Moon Dress 月光裙/MoonDress_v1/MoonDress_Kaguya.unitypackage"} {
		if !core.ContainsStr(got, want) {
			t.Errorf("not in the folder: %s (%v)", want, got)
		}
	}
	for _, p := range got {
		if strings.HasSuffix(p, ".part") || strings.HasSuffix(p, "MoonDress_v1.zip") {
			t.Errorf("left behind: %s", p)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(j.Dir, "Moon Dress 月光裙", "readme.txt")); string(b) != "Moon Dress 使用说明" {
		t.Errorf("the file that broke off halfway: %q", b)
	}
	asked := 0
	pcsAsked.Range(func(k, v any) bool {
		if strings.HasSuffix(k.(string), "/readme.txt") {
			asked += *v.(*int)
		}
		return true
	})
	if asked < 2 {
		t.Errorf("the line that dropped was not carried on: readme.txt asked for %d times", asked)
	}
	st.Mu.RLock()
	u := st.User["pan:1DlTestShare"]
	st.Mu.RUnlock()
	if u.Downloaded != j.Dir || len(u.PanGot) != 1 || u.PanGot[0] != "/" {
		t.Errorf("the card: downloaded %q, got %v", u.Downloaded, u.PanGot)
	}
	// downloaded again: a folder of its own in the netdisk, not beside the first copy
	DismissPanJob("pan:1DlTestShare")
	if err := QueuePanDownload(st, "pan:1DlTestShare", []string{"/Moon Dress 月光裙/readme.txt"}, nil); err != nil {
		t.Fatal(err)
	}
	j2 := waitJob(t, "pan:1DlTestShare", ended)
	if j2.Stage != "done" || j2.Saved == j.Saved || j2.Files != 1 {
		t.Errorf("again: %+v (first copy %q)", j2, j.Saved)
	}
}

// More files than the account may save at once: the share is saved a level down, a part at a time.
func TestBaiduDownloadOverTheSaveLimit(t *testing.T) {
	base := mockWeb(t)
	st := panStore(t, base, map[string][2]string{"pan:1LimitShare": {"", "Hair Pack"}})
	_ = SaveBaiduSession(&BaiduSession{Cookies: loggedIn})
	mockSwitch(t, base, "limit=2")
	if err := QueuePanDownload(st, "pan:1LimitShare", nil, nil); err != nil {
		t.Fatal(err)
	}
	j := waitJob(t, "pan:1LimitShare", ended)
	if j.Stage != "done" || j.Files != 3 {
		t.Fatalf("job: %+v", j)
	}
	got := strings.Join(testkit.ListTree(j.Dir), ",")
	if got != "Hair Pack/a.txt,Hair Pack/sub/b.txt,Hair Pack/sub/c.txt" {
		t.Errorf("in the folder: %s", got)
	}
}

// Not logged in: the download waits, and carries on once the player is; a wrong code ends it, saying so.
func TestBaiduDownloadWaitsForLogin(t *testing.T) {
	base := mockWeb(t)
	st := panStore(t, base, map[string][2]string{"pan:1PickShare": {"", "Kisekae"}, "pan:1DlTestShare": {"zzzz", "Moon"}})
	if err := QueuePanDownload(st, "pan:1PickShare", []string{"/Kisekae Set/Dress_A.zip"}, nil); err != nil {
		t.Fatal(err)
	}
	waitJob(t, "pan:1PickShare", func(j PanJob) bool { return j.Stage == "login" && j.Login })
	_ = SaveBaiduSession(&BaiduSession{Cookies: loggedIn})
	wakeQueue()
	j := waitJob(t, "pan:1PickShare", ended)
	if j.Stage != "done" || j.Files != 1 {
		t.Fatalf("after the login: %+v", j)
	}
	if got := testkit.ListTree(j.Dir); !core.ContainsStr(got, "Dress_A/Dress_A_Plum.unitypackage") || len(got) != 1 {
		t.Errorf("only what was picked: %v", got)
	}
	if err := QueuePanDownload(st, "pan:1DlTestShare", nil, nil); err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, "pan:1DlTestShare", ended); j.Stage != "failed" || !strings.Contains(j.Err, "提取码") {
		t.Errorf("a wrong code: %+v", j)
	}
}

func TestRemoteName(t *testing.T) {
	for in, want := range map[string]string{"Moon Dress 🌙✨": "Moon Dress", "a/b:c": "abc", "🌙": "1Surl", "": "1Surl"} {
		if got := remoteName(in, "1Surl"); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
