package purchases

import (
	"fmt"
	"image/color"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"vrclib/internal/core"
	"vrclib/internal/testkit"
)

// A stand-in for the player's own Booth pages, built like the ones the program reads (test/mockbooth.py):
// the library lists the products newest first with a download button per file, the orders page carries the
// dates and sometimes not the products, and the header and footer link to products that are no purchases.
type boothFake struct {
	mu      sync.Mutex
	pages   [][]string          // the library, a page of item ids each
	items   map[string][]string // item id → its files
	gifts   []string
	orders  [][3]string         // id, date, "a,b" items ("" = the list page does not say)
	detail  map[string][]string // order id → its items
	got     []string            // the paths asked for, in order
	signOut bool                // answer as Booth does when the login is no longer good
}

// hasPrefix: one of the paths asked for begins with this.
func hasPrefix(paths []string, prefix string) bool {
	for _, p := range paths {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

func (f *boothFake) took() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string{}, f.got...)
	f.got = nil
	return out
}

func (f *boothFake) card(id string) string {
	files := f.items[id]
	rows := ""
	for i, name := range files {
		rows += fmt.Sprintf(`<div class="flex items-center gap-8"><div class="text-14">%s</div></div>
			<div class="js-download-button" data-href="https://booth.pm/downloadables/%s%d" data-test="download-button"><span>ダウンロード</span></div>`, name, id, i)
	}
	return fmt.Sprintf(`<div class="bg-white">
		<a href="https://booth.pm/ja/items/%s"><img class="l-library-item-thumbnail" src="/thumb/%s.jpg" alt="Item %s"></a>
		<a href="https://shop%s.booth.pm/"><div class="text-14">Shop %s</div></a>
		<div class="mt-16">%s</div></div>`, id, id, id, id, id, rows)
}

func (f *boothFake) shell(body string) string {
	return `<!doctype html><html><body><header><a href="https://booth.pm/ja/items/1111111">おすすめ</a></header>` +
		`<main><div class="w-full">` + body + `</div></main>` +
		`<footer><a href="https://booth.pm/ja/items/2222222">footer item</a></footer></body></html>`
}

func (f *boothFake) serve() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.got = append(f.got, r.URL.RequestURI())
		out := f.signOut
		f.mu.Unlock()
		if out {
			if r.URL.Path == "/users/sign_in" {
				_, _ = w.Write([]byte(f.shell(`<form action="/users/sign_in">ログイン</form>`)))
				return
			}
			http.Redirect(w, r, "/users/sign_in", 302)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/thumb/") {
			_, _ = w.Write(testkit.JPEG(4, 4, color.RGBA{1, 2, 3, 255}))
			return
		}
		page := 1
		_, _ = fmt.Sscanf(r.URL.Query().Get("page"), "%d", &page)
		body := ""
		switch r.URL.Path {
		case "/library":
			if page <= len(f.pages) {
				for _, id := range f.pages[page-1] {
					body += f.card(id)
				}
			}
		case "/library/gifts":
			if page == 1 {
				for _, id := range f.gifts {
					body += f.card(id)
				}
			}
		case "/orders":
			if page == 1 {
				for _, o := range f.orders {
					inner := ""
					if o[2] != "" {
						for _, it := range strings.Split(o[2], ",") {
							inner += fmt.Sprintf(`<a href="https://booth.pm/ja/items/%s">x</a>`, it)
						}
					}
					body += fmt.Sprintf(`<div class="sheet"><a href="/orders/%s"><div>注文番号 %s</div><div>注文日時 %s 12:34</div></a>%s</div>`,
						o[0], o[0], o[1], inner)
				}
			}
		default:
			if id, ok := strings.CutPrefix(r.URL.Path, "/orders/"); ok {
				for _, it := range f.detail[id] {
					body += f.card(it)
				}
			}
		}
		_, _ = w.Write([]byte(f.shell(body)))
	}))
}

// a library of two pages, one gift, and three orders of which one does not list its products
func newFake() *boothFake {
	return &boothFake{
		pages: [][]string{{"9000001", "7770415"}, {"9000004", "9000005"}},
		items: map[string][]string{
			"9000001": {"MoonDress.zip", "MoonDress_psd.zip"},
			"7770415": {"Twintail.unitypackage"},
			"9000004": {"a.zip"}, "9000005": {"b.zip"}, "9000006": {"gift.zip"}, "9000009": {"new.zip"},
		},
		gifts:  []string{"9000006"},
		orders: [][3]string{{"51000001", "2026/09/20", "9000001"}, {"51000002", "2025/12/02", ""}, {"51000003", "2025/03/08", "9000004,9000005"}},
		detail: map[string][]string{"51000002": {"7770415"}},
	}
}

func fakeStore(t *testing.T, srv *httptest.Server) *core.Store {
	t.Helper()
	gap := pageGap
	pageGap = time.Millisecond
	t.Cleanup(func() { pageGap = gap })
	st := testkit.NewStore(t)
	t.Setenv("VRCLIB_BOOTH_BASE", srv.URL)
	t.Setenv("VRCLIB_BOOTH_WEB", srv.URL)
	t.Setenv("VRCLIB_BOOTH_DL", srv.URL)
	if err := saveSession(boothSessionFile(), []core.SavedCookie{{Name: "_plaza_session_nktz7u", Value: "ok"}}); err != nil {
		t.Fatal(err)
	}
	return st
}

// The first sync reads the whole library, the gifts and the orders, and writes down for every product what
// it is called, which shop it is from, its files and when it was bought.
func TestBoothSyncReadsLibrary(t *testing.T) {
	f := newFake()
	srv := f.serve()
	defer srv.Close()
	st := fakeStore(t, srv)
	if err := syncBooth(st, false); err != nil {
		t.Fatal(err)
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if len(st.Purchases) != 5 { // two pages of two, and the gift
		t.Fatalf("purchases: %v", core.SortedKeys(st.Purchases))
	}
	p := st.Purchases["9000001"]
	if p == nil || p.Name != "Item 9000001" || p.Shop != "Shop 9000001" || !strings.HasSuffix(p.ShopURL, "shop9000001.booth.pm/") {
		t.Fatalf("the product: %+v", p)
	}
	if strings.Join(p.Files, ",") != "MoonDress.zip,MoonDress_psd.zip" {
		t.Errorf("files: %v", p.Files)
	}
	if len(p.Downloads) != 2 || !strings.Contains(p.Downloads[0], "/downloadables/90000010") {
		t.Errorf("downloads: %v", p.Downloads)
	}
	if p.Cover == "" {
		t.Error("the picture of a new purchase was not saved")
	}
	if len(p.Orders) != 1 || p.Orders[0].ID != "51000001" || p.Orders[0].Date != "2026-09-20" {
		t.Errorf("orders: %+v", p.Orders)
	}
	// an order whose products the list page does not name is read from the order's own page
	if o := st.Purchases["7770415"].Orders; len(o) != 1 || o[0].Date != "2025-12-02" {
		t.Errorf("from the order's page: %+v", o)
	}
	if !st.Purchases["9000006"].Gift {
		t.Error("the gift is not marked as one")
	}
	if st.PurchaseSync == 0 {
		t.Error("the time of the sync was not written down")
	}
	// the products the header and the footer link to are no purchases
	for _, id := range []string{"1111111", "2222222"} {
		if st.Purchases[id] != nil {
			t.Errorf("a link outside the list was taken for a purchase: %s", id)
		}
	}
}

// A sync after it, with nothing bought in between, stops at the first page: no second page of the library,
// and no order read again.
func TestBoothSyncStopsAtWhatIsKnown(t *testing.T) {
	f := newFake()
	srv := f.serve()
	defer srv.Close()
	st := fakeStore(t, srv)
	if err := syncBooth(st, false); err != nil {
		t.Fatal(err)
	}
	first := len(f.took())
	if err := syncBooth(st, false); err != nil {
		t.Fatal(err)
	}
	again := f.took()
	if n := len(again); n >= first {
		t.Errorf("the second sync asked for %d pages, the first for %d: %v", n, first, again)
	}
	for _, p := range again {
		if strings.HasPrefix(p, "/library?page=2") {
			t.Errorf("read a page past the new ones: %v", again)
		}
		if strings.HasPrefix(p, "/orders/") {
			t.Errorf("read the detail of an order it had already: %v", again)
		}
	}
	if !hasPrefix(again, "/library") {
		t.Errorf("the first page is read every time: that is where a new purchase shows up: %v", again)
	}
	// something bought now shows up at the top of the list and is picked up
	f.mu.Lock()
	f.pages[0] = append([]string{"9000009"}, f.pages[0]...)
	f.orders = append([][3]string{{"51000004", "2026/10/28", "9000009"}}, f.orders...)
	f.mu.Unlock()
	if err := syncBooth(st, false); err != nil {
		t.Fatal(err)
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	p := st.Purchases["9000009"]
	if p == nil || len(p.Files) != 1 {
		t.Fatalf("the new purchase: %+v", p)
	}
	if len(p.Orders) != 1 || p.Orders[0].Date != "2026-10-28" {
		t.Errorf("its order: %+v", p.Orders)
	}
	if len(st.Purchases) != 6 {
		t.Errorf("purchases: %v", core.SortedKeys(st.Purchases))
	}
}

// A full sync reads every page, which is what finds out that a shop added a file to a product bought long
// ago; the incremental one does not reach it.
func TestBoothFullSyncSeesNewFilesOfOldPurchases(t *testing.T) {
	f := newFake()
	srv := f.serve()
	defer srv.Close()
	st := fakeStore(t, srv)
	if err := syncBooth(st, false); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.items["9000005"] = []string{"b.zip", "b_v2.zip"} // the shop added one, on the second page
	f.mu.Unlock()
	f.took()
	if err := syncBooth(st, false); err != nil {
		t.Fatal(err)
	}
	st.Mu.RLock()
	had := len(st.Purchases["9000005"].Files)
	st.Mu.RUnlock()
	if had != 1 {
		t.Errorf("the incremental sync is not to reach the second page: %d files", had)
	}
	if err := syncBooth(st, true); err != nil {
		t.Fatal(err)
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if got := st.Purchases["9000005"].Files; len(got) != 2 {
		t.Errorf("the full sync: %v", got)
	}
}

// A login Booth no longer accepts: it is dropped, and the player is told to log in again.
func TestBoothSyncLoginGone(t *testing.T) {
	f := newFake()
	f.signOut = true
	srv := f.serve()
	defer srv.Close()
	st := fakeStore(t, srv)
	err := syncBooth(st, false)
	if err == nil || !strings.Contains(err.Error(), "重新登录") {
		t.Fatalf("err: %v", err)
	}
	if len(LoadBoothSession()) != 0 {
		t.Error("the login that no longer works was kept")
	}
}

// What the pages are read as: the products of a page, each with its files, and the orders with their dates.
func TestParseLibraryAndOrders(t *testing.T) {
	f := newFake()
	entries := parseLibrary(f.shell(f.card("9000001") + f.card("7770415")))
	if len(entries) != 2 || entries[0].ID != "9000001" || entries[1].ID != "7770415" {
		t.Fatalf("entries: %+v", entries)
	}
	if strings.Join(entries[0].Files, ",") != "MoonDress.zip,MoonDress_psd.zip" {
		t.Errorf("files: %v", entries[0].Files)
	}
	if len(parseLibrary(f.shell(""))) != 0 {
		t.Error("an empty page is not to yield a product")
	}
	blocks := parseOrders(f.shell(`<div class="sheet"><a href="/orders/51000002"><div>注文番号 51000002</div><div>注文日時 2025/12/02 12:34</div></a></div>`))
	if len(blocks) != 1 || blocks[0].ID != "51000002" || blocks[0].Date != "2025-12-02" || len(blocks[0].Items) != 0 {
		t.Errorf("orders: %+v", blocks)
	}
}
