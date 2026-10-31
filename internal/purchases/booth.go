package purchases

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"vrclib/internal/booth"
	"vrclib/internal/core"
	"vrclib/internal/naming"
	"vrclib/internal/webpane"
)

// Reading the Booth library.
//
// Three of the player's own pages carry it, and none of them is an interface Booth offers — they are read as
// a browser gets them, which is why a change to their markup can break this:
//
//	accounts.booth.pm/library?page=N        every product bought, newest first, with its files and the
//	                                        address each file is downloaded from
//	accounts.booth.pm/library/gifts?page=N  the ones that were a gift
//	accounts.booth.pm/orders?page=N         the orders, for the date a product was bought; an order that
//	                                        does not list its products is read from accounts.booth.pm/orders/<id>
//
// A sync is incremental. The library lists what was bought newest first, so the walk ends on the first page
// where every product is one the store already knows: buying something new costs one page, not the whole
// library. Orders are the same, and the detail page of an order is only ever read once — an order never
// changes after it was placed.
//
// What an incremental sync does not see is a shop adding files to a product bought long ago (the product
// stays where it is in the list). Finding that out is what a full sync is for (SyncAll), which the update
// check runs; the button in the window asks for the incremental one.
const (
	maxLibPages   = 400 // a safety stop: 400 pages is far more than anyone's library
	maxOrderPages = 400
)

// pageGap: between two pages, so that the store is not hammered. Tests shorten it.
var pageGap = 400 * time.Millisecond

// libEntry: one product of the library page.
type libEntry struct {
	ID      string
	Name    string
	Shop    string
	ShopURL string
	Thumb   string
	Files   []string // the file names, in the order the page lists them
	DLs     []string // and where each is downloaded from
	Gift    bool
}

// orderBlock: one order of the orders page.
type orderBlock struct {
	ID    string
	Date  string   // "2026-09-20"
	Items []string // the products it lists, when it lists them
}

var (
	syncMu      sync.Mutex
	syncRunning bool
)

// StartPurchaseSync reads the Booth library in the background, up to the first page that holds nothing new.
// It answers false when a sync is already running.
func StartPurchaseSync(st *core.Store) bool { return startSync(st, false) }

// StartFullPurchaseSync reads all of it, including the file list of every product bought earlier: what finds
// out that a shop has added or replaced files.
func StartFullPurchaseSync(st *core.Store) bool { return startSync(st, true) }

func startSync(st *core.Store, full bool) bool {
	syncMu.Lock()
	if syncRunning {
		syncMu.Unlock()
		return false
	}
	syncRunning = true
	syncMu.Unlock()
	PurchaseCancel.Store(false)
	core.PurchaseBusy.Store(true)
	core.BumpRev()
	go func() {
		defer func() {
			syncMu.Lock()
			syncRunning = false
			syncMu.Unlock()
			core.PurchaseBusy.Store(false)
			core.BumpRev()
		}()
		if len(LoadBoothSession()) == 0 {
			// not logged in: the player does it on Booth's own page in the pane, and the sync begins then
			if err := openBoothLogin(st, full); err != nil {
				TaskPurchase.Set(1, 1, "无法打开 Booth 登录页面："+core.TrimErr(err))
			}
			return
		}
		core.RunTask(TaskPurchase, func() {
			if err := syncBooth(st, full); err != nil {
				TaskPurchase.Set(1, 1, core.TrimErr(err))
				core.Logf("同步 Booth 已购失败：%v", err)
			}
		})
	}()
	return true
}

// openBoothLogin shows Booth's library page in the pane and waits for the login there.
func openBoothLogin(st *core.Store, full bool) error {
	if err := webpane.Pane.Open(core.LibraryURL(false), "booth", true); err != nil {
		return err
	}
	go watchLogin("booth", webpane.BoothCookieURLs(), boothSessionFile(), func() {
		core.RunTask(TaskPurchase, func() {
			if err := syncBooth(st, full); err != nil {
				TaskPurchase.Set(1, 1, core.TrimErr(err))
				core.Logf("同步 Booth 已购失败：%v", err)
			}
		})
	})
	return nil
}

// syncBooth walks the library, the gifts and the orders.
func syncBooth(st *core.Store, full bool) error {
	cookie := cookieHeader(LoadBoothSession())
	if cookie == "" {
		return errors.New("尚未登录 Booth")
	}
	c := core.HTTPClient(st)
	known := knownItems(st)
	added, updated, pages := 0, 0, 0
	for _, gift := range []bool{false, true} {
		for page := 1; page <= maxLibPages; page++ {
			if PurchaseCancel.Load() || core.Quitting.Load() {
				return errors.New("已停止")
			}
			what := "已购"
			if gift {
				what = "礼物"
			}
			TaskPurchase.Set(pages, pages+1, fmt.Sprintf("正在读取 %s 第 %d 页", what, page))
			entries, err := libraryPage(c, cookie, page, gift)
			if err != nil {
				if errors.Is(err, ErrStoreLogin) {
					_ = ForgetBoothLogin()
					return errors.New("Booth 登录已失效，请重新登录后同步")
				}
				if page > 1 { // what was read is kept; the rest waits for the next sync
					core.Logf("Booth %s 第 %d 页读取失败：%v", what, page, err)
					break
				}
				return err
			}
			pages++
			if len(entries) == 0 {
				break // past the last page
			}
			a, u := applyEntries(st, entries, gift)
			added, updated = added+a, updated+u
			// incremental: a page on which nothing was new ends the walk (the list is newest first)
			if !full && a == 0 && u == 0 && allKnown(entries, known) {
				core.Logf("Booth %s 第 %d 页全部已收录，本次同步到此", what, page)
				break
			}
			time.Sleep(pageGap)
		}
	}
	orders, err := syncOrders(st, c, cookie, full)
	if err != nil {
		core.Logf("读取 Booth 订单失败：%v", err)
	}
	st.Mu.Lock()
	st.PurchaseSync = time.Now().Unix()
	n := len(st.Purchases)
	st.Mu.Unlock()
	_ = st.Save()
	core.BumpRev()
	msg := fmt.Sprintf("共 %d 件已购，新增 %d 件", n, added)
	if updated > 0 {
		msg += fmt.Sprintf("，%d 件有新文件", updated)
	}
	if orders > 0 {
		msg += fmt.Sprintf("，读取 %d 个订单", orders)
	}
	TaskPurchase.Set(1, 1, msg)
	core.Logf("Booth 已购同步完成：%s（读取 %d 页）", msg, pages)
	return nil
}

// knownItems: the products the store holds already, by item number.
func knownItems(st *core.Store) map[string]bool {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	out := make(map[string]bool, len(st.Purchases))
	for id := range st.Purchases {
		out[id] = true
	}
	return out
}

func allKnown(entries []libEntry, known map[string]bool) bool {
	for _, e := range entries {
		if !known[e.ID] {
			return false
		}
	}
	return true
}

// libraryPage reads one page of the library (or of the gifts).
func libraryPage(c *http.Client, cookie string, page int, gift bool) ([]libEntry, error) {
	u := core.LibraryURL(gift)
	if page > 1 {
		u += fmt.Sprintf("?page=%d", page)
	}
	body, status, err := fetchPage(c, u, cookie)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("Booth 返回错误（HTTP %d）", status)
	}
	entries := parseLibrary(string(body))
	for i := range entries {
		entries[i].Gift = gift
	}
	return entries, nil
}

var (
	// the picture of a product in the library, which every entry of the page has exactly one of
	reLibThumb = regexp.MustCompile(`l-library-item-thumbnail`)
	reItemLink = regexp.MustCompile(`booth\.pm/(?:[a-z]{2}(?:[-_][a-z]{2,4})?/)?items/([0-9]{2,10})`)
	reAlt      = regexp.MustCompile(`alt="([^"]*)"`)
	reImgSrc   = regexp.MustCompile(`<img[^>]+src="([^"]+)"`)
	reShopLink = regexp.MustCompile(`href="(https?://([a-z0-9][a-z0-9-]*)\.booth\.pm/?)"[^>]*>(?:\s*<[^>]+>)*\s*([^<]{1,80})`)
	// a file and the address it is downloaded from, and the text nodes between them
	reDLOrText = regexp.MustCompile(`data-href="([^"]*/downloadables/[0-9]+)"|>([^<>]{1,300})<`)
	reMainCut  = regexp.MustCompile(`(?s)<main[^>]*>(.*)</main>`)
)

// parseLibrary reads the products out of a library page.
//
// The page is cut to its <main> first: the header and the footer link to products too (Booth recommends
// some), and those are no purchases. What is left is split at the picture every entry has, and the product
// an entry is of is the last item link before that picture — the one the picture is wrapped in.
func parseLibrary(page string) []libEntry {
	if m := reMainCut.FindStringSubmatch(page); m != nil {
		page = m[1]
	}
	marks := reLibThumb.FindAllStringIndex(page, -1)
	var out []libEntry
	for i, mk := range marks {
		ids := reItemLink.FindAllStringSubmatch(page[:mk[0]], -1)
		if len(ids) == 0 {
			continue
		}
		id := ids[len(ids)-1][1]
		end := len(page)
		if i+1 < len(marks) {
			end = marks[i+1][0]
		}
		seg := page[mk[1]:end]
		// the picture's address: the <img> the mark sits in begins before it, so a window around it is read
		e := libEntry{ID: id, Thumb: absURL(firstGroup(reImgSrc, page[max(0, mk[0]-400):min(mk[0]+600, len(page))]))}
		if m := reAlt.FindStringSubmatch(page[mk[1]:min(mk[1]+400, len(page))]); m != nil {
			e.Name = strings.TrimSpace(unescape(m[1]))
		}
		if m := reShopLink.FindStringSubmatch(seg); m != nil {
			e.ShopURL, e.Shop = m[1], strings.TrimSpace(unescape(m[3]))
			if e.Shop == "" {
				e.Shop = m[2]
			}
		}
		e.Files, e.DLs = parseDownloads(seg)
		if e.Name == "" {
			e.Name = "Booth #" + id
		}
		out = append(out, e)
	}
	return out
}

// parseDownloads: the files of one entry, each with the address it is downloaded from. The name of a file is
// the text written before its button.
func parseDownloads(seg string) (files, dls []string) {
	last := ""
	for _, m := range reDLOrText.FindAllStringSubmatch(seg, -1) {
		if m[1] != "" {
			files, dls = append(files, last), append(dls, unescape(m[1]))
			last = ""
			continue
		}
		if t := strings.TrimSpace(unescape(m[2])); t != "" && strings.Contains(t, ".") && !strings.Contains(t, " ") {
			last = t // a file name: it has an extension and no spaces around it
		}
	}
	return files, dls
}

// applyEntries writes what a page said into the store and answers how many products are new, and how many
// got files they did not have.
func applyEntries(st *core.Store, entries []libEntry, gift bool) (added, updated int) {
	now := time.Now().Unix()
	var covers []libEntry
	st.Mu.Lock()
	for _, e := range entries {
		p := st.Purchases[e.ID]
		if p == nil {
			p = &core.Purchase{ID: e.ID, First: now}
			st.Purchases[e.ID] = p
			added++
			covers = append(covers, e)
		} else if len(newOnes(e.Files, p.Files)) > 0 {
			updated++
		}
		p.Name, p.Shop, p.ShopURL, p.Thumb, p.Gift = e.Name, e.Shop, e.ShopURL, e.Thumb, gift
		p.Files, p.Downloads = e.Files, e.DLs
		if p.PageURL == "" {
			p.PageURL = core.BoothWebBase() + "/ja/items/" + e.ID
		}
	}
	st.Mu.Unlock()
	// the picture of a product that is new, saved next to the covers of the library
	if len(covers) > 0 {
		c := core.HTTPClient(st)
		for _, e := range covers {
			if e.Thumb == "" || PurchaseCancel.Load() {
				continue
			}
			if p, err := booth.DownloadTo(c, e.Thumb, "purchase_"+e.ID); err == nil {
				st.Mu.Lock()
				if pp := st.Purchases[e.ID]; pp != nil {
					pp.Cover = p
				}
				st.Mu.Unlock()
			}
		}
	}
	return added, updated
}

// newOnes: the names in have that were not in had.
func newOnes(have, had []string) []string {
	old := map[string]bool{}
	for _, f := range had {
		old[f] = true
	}
	var out []string
	for _, f := range have {
		if !old[f] {
			out = append(out, f)
		}
	}
	return out
}

// ---------- the orders, for the date a product was bought ----------

func syncOrders(st *core.Store, c *http.Client, cookie string, full bool) (int, error) {
	seen := knownOrders(st)
	read := 0
	for page := 1; page <= maxOrderPages; page++ {
		if PurchaseCancel.Load() || core.Quitting.Load() {
			return read, errors.New("已停止")
		}
		TaskPurchase.Set(page, page+1, fmt.Sprintf("正在读取订单第 %d 页", page))
		u := core.BoothAccountsBase() + "/orders"
		if page > 1 {
			u += fmt.Sprintf("?page=%d", page)
		}
		body, status, err := fetchPage(c, u, cookie)
		if err != nil {
			return read, err
		}
		if status != 200 {
			return read, fmt.Errorf("Booth 返回错误（HTTP %d）", status)
		}
		blocks := parseOrders(string(body))
		if len(blocks) == 0 {
			break
		}
		fresh := 0
		for _, b := range blocks {
			if seen[b.ID] && !full {
				continue // an order never changes after it was placed: read once, never again
			}
			fresh++
			items := b.Items
			if len(items) == 0 {
				// the list page does not always say what an order held: its own page does
				got, err := orderItems(c, cookie, b.ID)
				if err != nil {
					core.Logf("订单 %s 的明细读取失败：%v", b.ID, err)
					continue
				}
				items = got
				time.Sleep(pageGap)
			}
			noteOrder(st, b.ID, b.Date, items)
			read++
		}
		if fresh == 0 && !full {
			break // this page is all orders known already, and so is everything after it
		}
		time.Sleep(pageGap)
	}
	return read, nil
}

// knownOrders: the orders already written down against a product.
func knownOrders(st *core.Store) map[string]bool {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	out := map[string]bool{}
	for _, p := range st.Purchases {
		for _, o := range p.Orders {
			out[o.ID] = true
		}
	}
	return out
}

func noteOrder(st *core.Store, id, date string, items []string) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	for _, item := range items {
		p := st.Purchases[item]
		if p == nil {
			continue // bought, then taken out of the library: nothing of ours
		}
		found := false
		for i := range p.Orders {
			if p.Orders[i].ID == id {
				p.Orders[i].Date, found = date, true
			}
		}
		if !found {
			p.Orders = append(p.Orders, core.PurchaseOrder{ID: id, Date: date})
		}
	}
}

var (
	reOrderLink = regexp.MustCompile(`/orders/([0-9]{3,20})`)
	reOrderDate = regexp.MustCompile(`([0-9]{4})[/\-年]([0-9]{1,2})[/\-月]([0-9]{1,2})`)
)

// parseOrders reads the orders out of the orders page: each block begins at the link to the order's own
// page, and what follows it up to the next block is that order's date and products.
func parseOrders(page string) []orderBlock {
	if m := reMainCut.FindStringSubmatch(page); m != nil {
		page = m[1]
	}
	marks := reOrderLink.FindAllStringSubmatchIndex(page, -1)
	var out []orderBlock
	for i, mk := range marks {
		id := page[mk[2]:mk[3]]
		if len(out) > 0 && out[len(out)-1].ID == id {
			continue // the same order linked twice in its block
		}
		end := len(page)
		if i+1 < len(marks) {
			end = marks[i+1][0]
		}
		b := orderBlock{ID: id}
		seg := page[mk[1]:end]
		if m := reOrderDate.FindStringSubmatch(seg); m != nil {
			b.Date = fmt.Sprintf("%s-%02s-%02s", m[1], pad2(m[2]), pad2(m[3]))
		}
		for _, im := range reItemLink.FindAllStringSubmatch(seg, -1) {
			if !core.ContainsStr(b.Items, im[1]) {
				b.Items = append(b.Items, im[1])
			}
		}
		out = append(out, b)
	}
	return out
}

// orderItems: the products one order held, from the order's own page.
func orderItems(c *http.Client, cookie, id string) ([]string, error) {
	body, status, err := fetchPage(c, core.OrderURL(id), cookie)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("HTTP %d", status)
	}
	page := string(body)
	if m := reMainCut.FindStringSubmatch(page); m != nil {
		page = m[1]
	}
	var out []string
	for _, m := range reItemLink.FindAllStringSubmatch(page, -1) {
		if !core.ContainsStr(out, m[1]) {
			out = append(out, m[1])
		}
	}
	return out, nil
}

// ---------- small helpers ----------

func firstGroup(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); m != nil {
		return unescape(m[1])
	}
	return ""
}

// absURL: a picture's address as the page writes it — whole, protocol-relative ("//booth.pximg.net/…") or
// relative to the library page.
func absURL(u string) string {
	switch {
	case u == "" || strings.Contains(u, "://"):
		return u
	case strings.HasPrefix(u, "//"):
		return "https:" + u
	case strings.HasPrefix(u, "/"):
		return core.BoothAccountsBase() + u
	}
	return core.BoothAccountsBase() + "/" + u
}

func pad2(s string) string {
	if len(s) == 1 {
		return "0" + s
	}
	return s
}

// unescape: the few entities a page's attributes carry.
func unescape(s string) string {
	for _, p := range [][2]string{{"&amp;", "&"}, {"&quot;", `"`}, {"&#39;", "'"}, {"&lt;", "<"}, {"&gt;", ">"}} {
		s = strings.ReplaceAll(s, p[0], p[1])
	}
	return s
}

// boothIDOf: the item number in a text, for the places that take one from a name.
func boothIDOf(s string) string { return naming.BoothIDFromName(s) }
