package purchases

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"vrclib/internal/core"
	"vrclib/internal/testkit"
)

// gumFake: gumroad.com as test/mockweb.py has it — Inertia pages, 15 purchases a page, a download page per
// purchase with its files in folders, one of them a membership no longer paid for — and a file host on
// another host name, which the login must never reach.
type gumFake struct {
	mu       sync.Mutex
	n        int
	dlPages  int      // download pages read
	leaked   []string // what reached the file host with a Cookie
	site     *httptest.Server
	files    *httptest.Server
	filesURL string
}

func newGumFake(t *testing.T, n int) *gumFake {
	t.Helper()
	f := &gumFake{n: n}
	zip := testkit.ZipBytes(t, map[string][]byte{"Gum Dress/Gum Dress.unitypackage": []byte("pkg")})
	f.files = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" {
			f.mu.Lock()
			f.leaked = append(f.leaked, r.URL.Path)
			f.mu.Unlock()
		}
		_, _ = w.Write(zip)
	}))
	// another host name for the same server: the file host is not gumroad.com
	f.filesURL = strings.Replace(f.files.URL, "127.0.0.1", "localhost", 1)
	name := func(i int) string { return fmt.Sprintf("Gum Item %02d", i) }
	f.site = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := func(component string, props map[string]any) {
			b, _ := json.Marshal(map[string]any{"component": component, "props": props})
			fmt.Fprintf(w, `<!DOCTYPE html><html><body><div id="app" data-page="%s"></div></body></html>`, html.EscapeString(string(b)))
		}
		if c, _ := r.Cookie("_gumroad_app_session"); c == nil || c.Value != "gumok" {
			page("Logins/New", map[string]any{})
			return
		}
		switch {
		case r.URL.Path == "/library":
			p := 1
			_, _ = fmt.Sscanf(r.URL.Query().Get("page"), "%d", &p)
			var res []any
			f.mu.Lock()
			n := f.n
			f.mu.Unlock()
			if r.URL.Query().Get("show_archived_only") != "true" {
				for i := (p - 1) * 15; i < p*15 && i < n; i++ {
					res = append(res, map[string]any{
						"product":  map[string]any{"name": name(i), "thumbnail_url": "", "creator": map[string]any{"name": "Atelier Gum", "profile_url": "https://ateliergum.gumroad.com/"}},
						"purchase": map[string]any{"id": fmt.Sprintf("gp%02d==", i), "download_url": fmt.Sprintf("%s/d/tok%02d", f.site.URL, i), "variants": ""}})
				}
			}
			pages := (n + 14) / 15
			if pages == 0 {
				pages = 1
			}
			page("Library/Index", map[string]any{"results": res, "pagination": map[string]any{"page": p, "pages": pages, "count": n},
				"logged_in_user": map[string]any{"name": "Mio"}})
		case strings.HasPrefix(r.URL.Path, "/d/tok"):
			f.mu.Lock()
			f.dlPages++
			f.mu.Unlock()
			i := strings.TrimPrefix(r.URL.Path, "/d/tok")
			if i == "02" {
				page("UrlRedirects/MembershipInactive", map[string]any{})
				return
			}
			file := func(id, n, ext string, dl bool) map[string]any {
				m := map[string]any{"type": "file", "file_name": n, "extension": ext, "file_size": 5, "id": id, "download_url": nil}
				if dl {
					m["download_url"] = "/r/tok" + i + "/product_files?product_file_ids%5B%5D=" + id
				}
				return m
			}
			page("UrlRedirects/DownloadPage", map[string]any{"content": map[string]any{"content_items": []any{
				map[string]any{"type": "folder", "name": "Unity", "children": []any{file("gf"+i+"a", "Gum_Item_"+i, "ZIP", true)}},
				file("gf"+i+"b", "Gum_Item_"+i+"_PSD", "ZIP", true), file("gf"+i+"c", "Preview", "MP4", false)}},
				"purchase": map[string]any{"id": "gp" + i + "==", "product_long_url": "https://ateliergum.gumroad.com/l/perm" + i, "created_at": "2026-09-01T10:00:00Z"}})
		case strings.HasPrefix(r.URL.Path, "/r/tok"):
			http.Redirect(w, r, f.filesURL+"/gumfiles/"+r.URL.Query().Get("product_file_ids[]")+".zip?X-Amz-Signature=abc", 302)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.site.Close)
	t.Cleanup(f.files.Close)
	t.Setenv("VRCLIB_GUMROAD_BASE", f.site.URL)
	return f
}

func gumStore(t *testing.T) *core.Store {
	t.Helper()
	gap := pageGap
	pageGap = time.Millisecond
	t.Cleanup(func() { pageGap = gap })
	st := testkit.NewStore(t)
	st.Settings.Roots = []string{t.TempDir()}
	if err := saveSession(gumSessionFile(), []core.SavedCookie{{Name: "_gumroad_app_session", Value: "gumok"}}); err != nil {
		t.Fatal(err)
	}
	return st
}

// The library over two pages; each purchase's files, from its download page, with the folder they lie in;
// a video that only plays in the browser is no file; a membership no longer paid for says so.
func TestGumroadSync(t *testing.T) {
	f := newGumFake(t, 17)
	st := gumStore(t)
	if err := syncGumroad(st, false); err != nil {
		t.Fatal(err)
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	n := 0
	for id := range st.Purchases {
		if core.IsGumID(id) {
			n++
		}
	}
	if n != 17 {
		t.Fatalf("purchases: %v", core.SortedKeys(st.Purchases))
	}
	p := st.Purchases["gr_gp00"]
	if p == nil || p.Name != "Gum Item 00" || p.Shop != "Atelier Gum" || p.Source != "gumroad" {
		t.Fatalf("the purchase: %+v", p)
	}
	if strings.Join(p.Files, ",") != "Gum_Item_00.zip,Gum_Item_00_PSD.zip" || strings.Join(p.DLPaths, ",") != "Unity," {
		t.Errorf("files %v in %v", p.Files, p.DLPaths)
	}
	if p.PageURL != "https://ateliergum.gumroad.com/l/perm00" || len(p.Orders) != 1 || p.Orders[0].Date != "2026-09-01" {
		t.Errorf("page %q, orders %+v", p.PageURL, p.Orders)
	}
	if q := st.Purchases["gr_gp02"]; q == nil || len(q.Files) != 0 || !strings.Contains(q.Note, "订阅已失效") {
		t.Errorf("the membership no longer paid for: %+v", q)
	}
	if st.GumroadSync == 0 {
		t.Error("the time of the sync was not written down")
	}
	if a := CurrentGumroadAccount(st); !a.LoggedIn || a.Name != "Mio" || a.Count != 17 {
		t.Errorf("account: %+v", a)
	}
	if f.dlPages != 17 {
		t.Errorf("download pages read: %d", f.dlPages)
	}
}

// A sync after it reads the download pages of the new purchases only.
func TestGumroadSyncIsIncremental(t *testing.T) {
	f := newGumFake(t, 5)
	st := gumStore(t)
	if err := syncGumroad(st, false); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.dlPages, f.n = 0, 6
	f.mu.Unlock()
	if err := syncGumroad(st, false); err != nil {
		t.Fatal(err)
	}
	if f.dlPages != 1 {
		t.Errorf("download pages read the second time: %d, want only the new purchase's", f.dlPages)
	}
	f.dlPages = 0
	if err := syncGumroad(st, true); err != nil {
		t.Fatal(err)
	}
	if f.dlPages != 6 {
		t.Errorf("a full sync read %d download pages", f.dlPages)
	}
}

// The files are fetched with the Gumroad login, which does not go along to the file host; they are unpacked
// like any other purchase's.
func TestGumroadDownload(t *testing.T) {
	resetDL(t)
	f := newGumFake(t, 1)
	st := gumStore(t)
	if err := syncGumroad(st, false); err != nil {
		t.Fatal(err)
	}
	if n, err := QueueDownloads(st, "gr_gp00", []string{"gf00a"}); err != nil || n != 1 {
		t.Fatalf("queued %d: %v", n, err)
	}
	for _, j := range waitDL(t, "gr_gp00") {
		if j.Status != "done" {
			t.Fatalf("job: %+v", j)
		}
	}
	got := testkit.ListTree(filepath.Join(st.Settings.Roots[0], "Gum Item 00"))
	if !core.ContainsStr(got, "Gum Dress/Gum Dress.unitypackage") {
		t.Errorf("in the folder: %v", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.leaked) != 0 {
		t.Errorf("the Gumroad login reached the file host: %v", f.leaked)
	}
}

// A login Gumroad no longer accepts: it is dropped, and the player is told to log in again.
func TestGumroadLoginGone(t *testing.T) {
	newGumFake(t, 3)
	st := gumStore(t)
	_ = saveSession(gumSessionFile(), []core.SavedCookie{{Name: "_gumroad_app_session", Value: "stale"}})
	if err := syncGumroad(st, false); err == nil || !strings.Contains(err.Error(), "重新登录") {
		t.Fatalf("err: %v", err)
	}
	if LoadGumSession() != nil {
		t.Error("the login that no longer works was kept")
	}
}
