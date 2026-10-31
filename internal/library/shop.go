package library

import (
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"vrclib/internal/booth"
	"vrclib/internal/core"
)

// The Booth page of the program: Booth's own browse pages, read and shown as cards, with what the library
// says about each product (bought, already in the library).
//
// Booth has no search interface of its own to offer: what is read is the page a browser gets,
// booth.pm/ja/browse/<category>?q=…&sort=…&page=N, 60 cards a page. A product's own page is read through
// the item JSON the rest of the program uses (booth.FetchBoothInfo).

// ShopCats: the categories the Booth page lets the player browse, by what the window calls them and what
// Booth does.
var ShopCats = []ShopCat{
	{Key: "3D衣装", Label: "衣服"},
	{Key: "3Dキャラクター", Label: "素体"},
	{Key: "3D装飾品", Label: "配饰"},
	{Key: "3D小道具", Label: "道具"},
	{Key: "3Dテクスチャ", Label: "材质"},
	{Key: "3Dツール・システム", Label: "插件"},
	{Key: "3Dモーション・アニメーション", Label: "动作"},
	{Key: "3D環境・ワールド", Label: "世界"},
	{Key: "3Dモデル（その他）", Label: "其他"},
}

const shopAll = "3Dモデル" // what is browsed when no category is picked
const shopPage = 60     // cards on one of Booth's browse pages

// ShopQuery: what the Booth page asks for.
type ShopQuery struct {
	Q      string   `json:"q,omitempty"`
	Cats   []string `json:"cats,omitempty"`   // by the window's names
	Bases  []string `json:"bases,omitempty"`  // base bodies: searched for by name
	Styles []string `json:"styles,omitempty"` // style tags: searched for by their first word
	Sort   string   `json:"sort,omitempty"`   // popular, new, cheap, pricey
	Page   int      `json:"page,omitempty"`
	Adult  bool     `json:"adult,omitempty"`
}

// ShopItem: one product, as a card of the Booth page shows it, or in full.
type ShopItem struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Shop     string   `json:"shop,omitempty"`
	ShopSub  string   `json:"shopSub,omitempty"`
	ShopURL  string   `json:"shopUrl,omitempty"`
	URL      string   `json:"url,omitempty"`
	Category string   `json:"category,omitempty"`
	Price    string   `json:"price,omitempty"`
	Thumb    string   `json:"thumb,omitempty"`
	Images   []string `json:"images,omitempty"`
	Desc     string   `json:"desc,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	Adult    bool     `json:"adult,omitempty"`
	Bought   bool     `json:"bought,omitempty"` // among the purchases
	Owned    bool     `json:"owned,omitempty"`  // in the library
}

var shopSorts = map[string]string{"popular": "wish_lists", "new": "new", "cheap": "price_asc", "pricey": "price_desc"}

// shopURL: the browse page a query asks for.
func shopURL(st *core.Store, q ShopQuery) string {
	cat := shopAll
	if len(q.Cats) == 1 { // Booth browses one category at a time; several are the whole of 3D models
		for _, c := range ShopCats {
			if c.Label == q.Cats[0] || c.Key == q.Cats[0] {
				cat = c.Key
			}
		}
	}
	words := strings.Fields(q.Q)
	words = append(words, q.Bases...)
	st.Mu.RLock()
	styles := st.Settings.Styles
	st.Mu.RUnlock()
	for _, want := range q.Styles {
		for _, s := range styles {
			name, kw, ok := strings.Cut(s, "=")
			if ok && strings.TrimSpace(name) == want {
				if w := strings.TrimSpace(strings.SplitN(kw, "|", 2)[0]); w != "" {
					words = append(words, w)
				}
			}
		}
	}
	v := url.Values{}
	if len(words) > 0 {
		v.Set("q", strings.Join(words, " "))
	}
	sort := shopSorts[q.Sort]
	if sort == "" {
		sort = shopSorts["popular"]
	}
	v.Set("sort", sort)
	if q.Page > 1 {
		v.Set("page", strconv.Itoa(q.Page))
	}
	if q.Adult {
		v.Set("adult", "include")
	}
	return core.BoothWebBase() + "/ja/browse/" + url.PathEscape(cat) + "?" + v.Encode()
}

// SearchShop: the products of a page of the shop, and whether there are more after it.
func SearchShop(st *core.Store, q ShopQuery) ([]ShopItem, bool, error) {
	if q.Page < 1 {
		q.Page = 1
	}
	req, err := http.NewRequest("GET", shopURL(st, q), nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("User-Agent", core.UA)
	req.Header.Set("Accept-Language", "ja,zh-CN;q=0.8")
	if q.Adult {
		req.Header.Set("Cookie", "adult=t")
	}
	resp, err := core.HTTPClient(st).Do(req)
	if err != nil {
		return nil, false, errors.New("无法连接 Booth：" + core.FriendlyNetErr(err))
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == 429 || resp.StatusCode == 403:
		return nil, false, fmt.Errorf("Booth 暂时限制了访问（HTTP %d），请稍后重试", resp.StatusCode)
	case resp.StatusCode != 200:
		return nil, false, fmt.Errorf("Booth 返回错误（HTTP %d）", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	items := parseShopCards(string(body))
	markOwned(st, items)
	return items, len(items) >= shopPage, nil
}

var (
	reShopCard  = regexp.MustCompile(`(?s)<li[^>]*\bclass="[^"]*\bitem-card\b[^"]*"(.*?)</li>`)
	reCardAttr  = regexp.MustCompile(`data-product-(id|brand|price|name)="([^"]*)"`)
	reCardThumb = regexp.MustCompile(`data-original="([^"]+)"`)
	reCardTitle = regexp.MustCompile(`(?s)item-card__title-anchor[^>]*>(.*?)</a>`)
	reCardCat   = regexp.MustCompile(`(?s)item-card__category-anchor[^>]*>(.*?)</a>`)
	reCardShop  = regexp.MustCompile(`(?s)item-card__shop-name-anchor[^>]*href="([^"]+)".*?alt="([^"]*)"`)
	reShopTag   = regexp.MustCompile(`(?s)<[^>]+>`)
)

// parseShopCards reads the cards of a browse page.
func parseShopCards(page string) []ShopItem {
	var out []ShopItem
	seen := map[string]bool{}
	for _, m := range reShopCard.FindAllStringSubmatch(page, -1) {
		card := m[0]
		it := ShopItem{}
		for _, a := range reCardAttr.FindAllStringSubmatch(card, -1) {
			v := html.UnescapeString(a[2])
			switch a[1] {
			case "id":
				it.ID = v
			case "brand":
				it.ShopSub = v
			case "price":
				if n, err := strconv.Atoi(v); err == nil {
					it.Price = booth.PriceText("JPY", int64(n))
				}
			case "name":
				it.Name = v // cut short by Booth; the title below is whole
			}
		}
		if it.ID == "" || seen[it.ID] {
			continue
		}
		seen[it.ID] = true
		if t := reCardTitle.FindStringSubmatch(card); t != nil {
			it.Name = strings.TrimSpace(html.UnescapeString(reShopTag.ReplaceAllString(t[1], "")))
		}
		if c := reCardCat.FindStringSubmatch(card); c != nil {
			it.Category = strings.TrimSpace(html.UnescapeString(reShopTag.ReplaceAllString(c[1], "")))
		}
		if s := reCardShop.FindStringSubmatch(card); s != nil {
			it.ShopURL, it.Shop = html.UnescapeString(s[1]), html.UnescapeString(s[2])
		}
		if th := reCardThumb.FindStringSubmatch(card); th != nil {
			it.Thumb = html.UnescapeString(th[1])
		}
		it.URL = "https://booth.pm/ja/items/" + it.ID
		out = append(out, it)
	}
	return out
}

// markOwned says of each product whether it was bought and whether the library holds it.
func markOwned(st *core.Store, items []ShopItem) {
	st.Mu.RLock()
	owned := OwnedBoothIDs(st)
	for i := range items {
		_, items[i].Owned = owned[items[i].ID]
		items[i].Bought = st.Purchases[items[i].ID] != nil
	}
	st.Mu.RUnlock()
}

// ShopItemDetail: one product of the shop, read in full.
func ShopItemDetail(st *core.Store, id string) (*ShopItem, error) {
	id = strings.TrimSpace(id)
	if id == "" || strings.Trim(id, "0123456789") != "" {
		return nil, errors.New("商品编号无效")
	}
	bi, err := booth.FetchBoothInfo(core.HTTPClient(st), id, false)
	if err != nil && (bi == nil || bi.Name == "") {
		return nil, err
	}
	it := &ShopItem{ID: id, Name: bi.Name, Shop: bi.Shop, ShopURL: bi.ShopURL, URL: bi.URL, Category: bi.Category,
		Price: bi.Price, Images: bi.Images, Desc: bi.Desc, Tags: bi.Tags, Adult: bi.Adult}
	if bi.ImageURL != "" && (len(it.Images) == 0 || it.Images[0] != bi.ImageURL) {
		it.Thumb = bi.ImageURL
	}
	one := []ShopItem{*it}
	markOwned(st, one)
	it.Owned, it.Bought = one[0].Owned, one[0].Bought
	return it, nil
}
