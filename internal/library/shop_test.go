package library

import (
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"vrclib/internal/core"
	"vrclib/internal/testkit"
)

// shopCard: a card of Booth's browse page, in the markup test/mockweb.py has (the shape of the live page).
func shopCard(id, name, sub, shop, cat string, price int) string {
	return fmt.Sprintf(`<li class="item-card l-card " data-product-brand="%s" data-product-category="209" data-product-id="%s" data-product-name="%s..." data-product-price="%d"><div class="item-card__wrap" id="item_%s"><div class="item-card__thumbnail js-thumbnail"><div class="item-card__thumbnail-images"><a class="js-thumbnail-image item-card__thumbnail-image" data-original="https://booth.pximg.net/%s.jpg" href="https://booth.pm/ja/items/%s"></a></div></div>
<div class="item-card__summary"><div class="item-card__category"><a class="item-card__category-anchor nav-reverse" href="#">%s</a></div><div class="item-card__title"><a target="_self" class="item-card__title-anchor--multiline nav" href="https://booth.pm/ja/items/%s">%s</a></div>
<div class="item-card__shop-info"><a class="item-card__shop-name-anchor nav" href="https://%s.booth.pm/"><div class="flex items-center"><img alt="%s" class="user-avatar at-item-footer" src="x"></div></a></div></div></div></li>`,
		sub, id, html.EscapeString([]string{name}[0][:min(20, len(name))]), price, id, id, id, cat, id, html.EscapeString(name), sub, html.EscapeString(shop))
}

type shopFake struct {
	mu  sync.Mutex
	got []*url.URL
	n   int // cards on the page
}

func newShopFake(t *testing.T, n int) *shopFake {
	t.Helper()
	f := &shopFake{n: n}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.got = append(f.got, r.URL)
		n := f.n
		f.mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/ja/items/") && strings.HasSuffix(r.URL.Path, ".json") {
			fmt.Fprint(w, `{"name":"【8アバター対応】Moon Dress","price":"¥ 1,500","description":"説明","category":{"name":"3D衣装"},
				"shop":{"name":"Luna Works","url":"https://lunaworks.booth.pm/"},"tags":[{"name":"VRChat"}],
				"images":[{"original":"https://booth.pximg.net/o.jpg","resized":"https://booth.pximg.net/c/72x72/r.jpg"}]}`)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/ja/items/") {
			fmt.Fprint(w, "<html></html>")
			return
		}
		var b strings.Builder
		b.WriteString("<html><body><ul>")
		b.WriteString(shopCard("9100001", "【8アバター対応】Moon Dress", "lunaworks", "Luna Works", "3D衣装", 1500))
		b.WriteString(shopCard("9000003", "Bought Dress", "lunaworks", "Luna & Co", "3D衣装", 2500))
		b.WriteString(shopCard("8099091", "AONAMI セーラー服", "aonami", "AONAMI shop", "3D衣装", 2500))
		for i := 3; i < n; i++ {
			b.WriteString(shopCard(fmt.Sprint(9500000+i), fmt.Sprintf("Item No.%d", i), "shop", "Shop", "3D衣装", 300+i))
		}
		b.WriteString("</ul></body></html>")
		_, _ = w.Write([]byte(b.String()))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("VRCLIB_BOOTH_WEB", srv.URL)
	return f
}

func (f *shopFake) last() *url.URL {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.got[len(f.got)-1]
}

// A page of Booth's: its cards, each with what the library says of it; the query made of the words, the
// base bodies and the style tags picked, in the category picked, sorted as asked.
func TestSearchShop(t *testing.T) {
	f := newShopFake(t, 60)
	st := testkit.NewStore(t)
	st.Settings.Styles = []string{"甜美=ロリータ|姫"}
	st.Purchases["9000003"] = &core.Purchase{ID: "9000003", Name: "Bought Dress"}
	st.Assets = []*core.Asset{{Key: "name:aonami", Name: "AONAMI", BoothID: "8099091"}}
	items, more, err := SearchShop(st, ShopQuery{Q: "dress", Cats: []string{"衣服"}, Bases: []string{"Manuka"}, Styles: []string{"甜美"}, Sort: "new", Page: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 60 || !more {
		t.Fatalf("%d items, more %v", len(items), more)
	}
	u := f.last()
	if p, _ := url.PathUnescape(u.Path); p != "/ja/browse/3D衣装" || u.Query().Get("q") != "dress Manuka ロリータ" || u.Query().Get("sort") != "new" || u.Query().Get("page") != "2" {
		t.Errorf("asked for %s", u)
	}
	it := items[0]
	if it.ID != "9100001" || it.Name != "【8アバター対応】Moon Dress" || it.Shop != "Luna Works" || it.ShopSub != "lunaworks" ||
		it.Category != "3D衣装" || it.Price != "¥ 1,500" || it.Thumb != "https://booth.pximg.net/9100001.jpg" {
		t.Errorf("card: %+v", it)
	}
	if items[1].Shop != "Luna & Co" {
		t.Errorf("a shop name with an entity in it: %q", items[1].Shop)
	}
	if it.Bought || it.Owned || !items[1].Bought || items[1].Owned || !items[2].Owned {
		t.Errorf("bought / owned: %+v %+v %+v", it, items[1], items[2])
	}
	// nothing picked: all of 3D models, by popularity, and a short page is the last one
	f.mu.Lock()
	f.n = 20
	f.mu.Unlock()
	if _, more, err := SearchShop(st, ShopQuery{}); err != nil || more {
		t.Errorf("a short page: more %v, %v", more, err)
	}
	if u := f.last(); !strings.HasSuffix(u.Path, "/ja/browse/3D%E3%83%A2%E3%83%87%E3%83%AB") && !strings.HasSuffix(u.EscapedPath(), "/ja/browse/3D%E3%83%A2%E3%83%87%E3%83%AB") || u.Query().Get("sort") != "wish_lists" || u.Query().Has("q") {
		t.Errorf("nothing picked: %s", u)
	}
}

// One product in full, through the item JSON.
func TestShopItemDetail(t *testing.T) {
	newShopFake(t, 3)
	st := testkit.NewStore(t)
	st.Purchases["9100001"] = &core.Purchase{ID: "9100001"}
	it, err := ShopItemDetail(st, "9100001")
	if err != nil {
		t.Fatal(err)
	}
	if it.Name != "【8アバター対応】Moon Dress" || it.Shop != "Luna Works" || it.Category != "3D衣装" || len(it.Tags) != 1 || len(it.Images) != 1 || !it.Bought {
		t.Errorf("item: %+v", it)
	}
	for _, bad := range []string{"", "abc", "../1"} {
		if _, err := ShopItemDetail(st, bad); err == nil {
			t.Errorf("id %q accepted", bad)
		}
	}
}
