package purchases

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"vrclib/internal/booth"
	"vrclib/internal/core"
)

// Reading the Gumroad library.
//
// Gumroad's pages are Inertia pages: the HTML carries what the page shows as JSON in the data-page attribute
// of its root element, which is what is read here (asking for the JSON itself needs the version of their
// front end, which changes with every release of theirs).
//
//	/library?page=N                      the purchases, 15 a page, with where each is downloaded from
//	/library?show_archived_only=true     the ones the player archived
//	/d/<token>                           a purchase's download page: its files, in folders, and its date
//
// A purchase's id here is "gr_" and Gumroad's own purchase id. Its files are fetched with the Gumroad login
// from gumroad.com, which sends the browser on to the file host; the login does not go along (a redirect to
// another host drops the Cookie header).
//
// Like the Booth sync, this one is incremental: a purchase already read keeps what its download page said,
// and only new purchases have theirs read. SyncAll reads every download page again.

const gumMaxPages = 200

var reDataPage = regexp.MustCompile(`data-page="([^"]*)"`)

// inertiaPage: what an Inertia page carries.
type inertiaPage struct {
	Component string          `json:"component"`
	Props     json.RawMessage `json:"props"`
}

// gumPage reads one page and answers what it carries.
func gumPage(c *http.Client, u, cookie string) (*inertiaPage, error) {
	body, status, err := fetchPage(c, u, cookie)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("Gumroad 返回错误（HTTP %d）", status)
	}
	m := reDataPage.FindSubmatch(body)
	if m == nil {
		return nil, errors.New("无法读取 Gumroad 页面（页面结构可能已变化）")
	}
	var p inertiaPage
	if err := json.Unmarshal([]byte(html.UnescapeString(string(m[1]))), &p); err != nil {
		return nil, errors.New("无法读取 Gumroad 页面内容")
	}
	if p.Component == "Logins/New" {
		return nil, ErrStoreLogin
	}
	return &p, nil
}

type gumLibrary struct {
	Results []struct {
		Product struct {
			Name    string `json:"name"`
			Thumb   string `json:"thumbnail_url"`
			Creator struct {
				Name string `json:"name"`
				URL  string `json:"profile_url"`
			} `json:"creator"`
		} `json:"product"`
		Purchase struct {
			ID       string `json:"id"`
			Download string `json:"download_url"`
			Variants string `json:"variants"`
			Archived bool   `json:"is_archived"`
		} `json:"purchase"`
	} `json:"results"`
	Pagination struct {
		Page  int `json:"page"`
		Pages int `json:"pages"`
		Count int `json:"count"`
	} `json:"pagination"`
	User *struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"logged_in_user"`
}

type gumItem struct {
	Type     string    `json:"type"`
	Name     string    `json:"name"`
	FileName string    `json:"file_name"`
	Ext      string    `json:"extension"`
	Size     int64     `json:"file_size"`
	ID       string    `json:"id"`
	Download *string   `json:"download_url"`
	Children []gumItem `json:"children"`
}

type gumDownloadPage struct {
	Content struct {
		Items []gumItem `json:"content_items"`
	} `json:"content"`
	Purchase struct {
		ID      string `json:"id"`
		Name    string `json:"product_name"`
		LongURL string `json:"product_long_url"`
		Created string `json:"created_at"`
	} `json:"purchase"`
}

// StartGumroadSync reads the Gumroad library in the background, reading the download pages of the new
// purchases only. It answers false when a sync is already running.
func StartGumroadSync(st *core.Store) bool { return startGum(st, false) }

// StartFullGumroadSync reads every purchase's download page again.
func StartFullGumroadSync(st *core.Store) bool { return startGum(st, true) }

func startGum(st *core.Store, full bool) bool {
	if !GumBusy.CompareAndSwap(false, true) {
		return false
	}
	GumCancel.Store(false)
	core.BumpRev()
	go func() {
		defer func() {
			GumBusy.Store(false)
			core.BumpRev()
		}()
		core.RunTask(TaskGumroad, func() {
			if err := syncGumroad(st, full); err != nil {
				TaskGumroad.Set(1, 1, core.TrimErr(err))
				core.Logf("同步 Gumroad 已购失败：%v", err)
			}
		})
	}()
	return true
}

func syncGumroad(st *core.Store, full bool) error {
	cookie := cookieHeader(LoadGumSession())
	if cookie == "" {
		return errors.New("尚未登录 Gumroad")
	}
	c := core.HTTPClient(st)
	known := knownItems(st)
	added, read := 0, 0
	for _, archived := range []bool{false, true} {
		for page := 1; page <= gumMaxPages; page++ {
			if GumCancel.Load() || core.Quitting.Load() {
				return errors.New("已停止")
			}
			TaskGumroad.Set(page-1, page, fmt.Sprintf("正在读取 Gumroad 已购第 %d 页", page))
			u := core.GumroadLibraryURL() + fmt.Sprintf("?page=%d", page)
			if archived {
				u += "&show_archived_only=true"
			}
			p, err := gumPage(c, u, cookie)
			if err != nil {
				if errors.Is(err, ErrStoreLogin) {
					_ = LogoutGumroad()
					return errors.New("Gumroad 登录已失效，请重新登录后同步")
				}
				return err
			}
			var lib gumLibrary
			if err := json.Unmarshal(p.Props, &lib); err != nil {
				return errors.New("无法读取 Gumroad 已购列表")
			}
			if lib.User != nil {
				gumAccount.Store(&GumroadAccount{LoggedIn: true, Name: lib.User.Name})
			}
			for _, r := range lib.Results {
				if r.Purchase.ID == "" {
					continue
				}
				id := core.GumPrefix + gumID(r.Purchase.ID)
				isNew := !known[id]
				st.Mu.Lock()
				pp := st.Purchases[id]
				if pp == nil {
					pp = &core.Purchase{ID: id, First: time.Now().Unix()}
					st.Purchases[id] = pp
					added++
				}
				pp.Name, pp.Shop, pp.ShopURL, pp.Thumb = r.Product.Name, r.Product.Creator.Name, r.Product.Creator.URL, r.Product.Thumb
				pp.Source, pp.DLPage, pp.Variant = "gumroad", absGum(r.Purchase.Download), r.Purchase.Variants
				st.Mu.Unlock()
				if (isNew || full) && pp.DLPage != "" {
					if err := readGumDownloads(c, cookie, st, id, pp.DLPage); err != nil {
						if errors.Is(err, ErrStoreLogin) {
							return errors.New("Gumroad 登录已失效，请重新登录后同步")
						}
						core.Logf("Gumroad 已购 %s 的下载页读取失败：%v", r.Product.Name, err)
					}
					read++
					time.Sleep(pageGap)
				}
				if isNew && r.Product.Thumb != "" {
					if p, err := booth.DownloadTo(c, r.Product.Thumb, "purchase_"+core.SafeName(id, 40)); err == nil {
						st.Mu.Lock()
						pp.Cover = p
						st.Mu.Unlock()
					}
				}
			}
			if page >= lib.Pagination.Pages || len(lib.Results) == 0 {
				break
			}
			time.Sleep(pageGap)
		}
	}
	st.Mu.Lock()
	st.GumroadSync = time.Now().Unix()
	n := 0
	for id := range st.Purchases {
		if core.IsGumID(id) {
			n++
		}
	}
	st.Mu.Unlock()
	_ = st.Save()
	core.BumpRev()
	msg := fmt.Sprintf("共 %d 件 Gumroad 已购，新增 %d 件", n, added)
	TaskGumroad.Set(1, 1, msg)
	core.Logf("Gumroad 已购同步完成：%s（读取 %d 个下载页）", msg, read)
	return nil
}

// readGumDownloads reads a purchase's download page: its files, the folders they lie in, and its date.
func readGumDownloads(c *http.Client, cookie string, st *core.Store, id, page string) error {
	p, err := gumPage(c, page, cookie)
	if err != nil {
		return err
	}
	var files, dls, folders []string
	note := ""
	switch p.Component {
	case "UrlRedirects/DownloadPage":
		var d gumDownloadPage
		if err := json.Unmarshal(p.Props, &d); err != nil {
			return errors.New("无法读取下载页")
		}
		var walk func([]gumItem, string)
		walk = func(items []gumItem, at string) {
			for _, it := range items {
				switch it.Type {
				case "folder":
					walk(it.Children, strings.TrimPrefix(at+"/"+it.Name, "/"))
				case "file":
					if it.Download == nil || *it.Download == "" {
						continue // a file only for watching in the browser (a video)
					}
					name := it.FileName
					if ext := strings.ToLower(it.Ext); ext != "" && !strings.HasSuffix(strings.ToLower(name), "."+ext) {
						name += "." + ext
					}
					files, dls, folders = append(files, name), append(dls, absGum(*it.Download)), append(folders, at)
				}
			}
		}
		walk(d.Content.Items, "")
		st.Mu.Lock()
		if pp := st.Purchases[id]; pp != nil {
			if d.Purchase.LongURL != "" {
				pp.PageURL = d.Purchase.LongURL
			}
			if len(d.Purchase.Created) >= 10 {
				pp.Orders = []core.PurchaseOrder{{ID: d.Purchase.ID, Date: d.Purchase.Created[:10]}}
			}
		}
		st.Mu.Unlock()
	case "UrlRedirects/MembershipInactive":
		note = "订阅已失效，无法下载"
	default:
		note = "无法读取下载页"
	}
	st.Mu.Lock()
	if pp := st.Purchases[id]; pp != nil {
		pp.Files, pp.Downloads, pp.DLPaths, pp.Note = files, dls, folders, note
	}
	st.Mu.Unlock()
	return nil
}

// gumID: Gumroad's purchase id as part of a key ("gp00==" carries characters a card key need not).
func gumID(s string) string { return strings.TrimRight(s, "=") }

// absGum: an address the page gives, made whole against gumroad.com.
func absGum(u string) string {
	if u == "" || strings.Contains(u, "://") {
		return u
	}
	base, err := url.Parse(core.GumroadBase() + "/")
	if err != nil {
		return u
	}
	ref, err := url.Parse(u)
	if err != nil {
		return u
	}
	return base.ResolveReference(ref).String()
}
