package netdisk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	"vrclib/internal/core"
)

// Reading a Baidu Netdisk share, the way its web page does (the reading of AList's baidu_share driver).
//
//	GET  /s/<surl>                       the share's page; its data is JSON in the page: a script
//	                                     id="locals-data" (the layout a visitor gets) or locals.mset({…}) (the
//	                                     one a logged-in visitor gets) — share_uk, shareid, file_list
//	                                     A share with an extraction code sends the visitor to /share/init.
//	POST /share/verify?surl=<surl less 1> pwd=…: errno 0 sets the cookie BDCLND, which opens the share
//	GET  /share/list?shareid&uk&dir=…    what a folder of it holds, a page at a time
//
// Nothing here needs a login: a share is read as a visitor. Saving it into the player's own netdisk and
// downloading it does (internal/pandl).

const (
	baiduApp      = "250528" // the web client's app id, which every request of the page carries
	baiduPageSize = 100
	baiduMaxFiles = 5000 // a share is listed this far; the rest is said to be cut off
	baiduMaxDepth = 8
)

// BaiduError: what Baidu answered with — its errno, and what that means for the player.
type BaiduError struct {
	Errno int
	Msg   string
}

func (e *BaiduError) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	return fmt.Sprintf("百度网盘返回错误（%d）", e.Errno)
}

// Typed errors a caller may act on.
var (
	ErrSharePwd     = errors.New("提取码错误")
	ErrShareNeedPwd = errors.New("该分享需要提取码")
	ErrShareGone    = errors.New("分享已失效（已被删除、取消或过期）")
	ErrShareCaptcha = errors.New("百度要求输入验证码，请稍后重试或在网盘页面中打开该分享")
)

// baiduMsg: what an errno of the share's pages means.
func baiduMsg(errno int) error {
	switch errno {
	case 0:
		return nil
	case -9, -12:
		return ErrSharePwd
	case -62, -63:
		return ErrShareCaptcha
	case 2, -7, 105, 115, 145:
		return ErrShareGone
	}
	return &BaiduError{Errno: errno}
}

// baiduShare: a share as its page describes it.
type baiduShare struct {
	UK      json.Number `json:"share_uk"`
	ShareID json.Number `json:"shareid"`
	Files   []baiduFile `json:"file_list"`
	Title   string      `json:"title"`
}

type baiduFile struct {
	Name  string      `json:"server_filename"`
	Path  string      `json:"path"`
	IsDir json.Number `json:"isdir"`
	Size  json.Number `json:"size"`
	FsID  json.Number `json:"fs_id"`
}

func (f baiduFile) dir() bool { return f.IsDir.String() == "1" }

var (
	reLocalsScript = regexp.MustCompile(`(?s)<script[^>]*id="locals-data"[^>]*>\s*(\{.*?\})\s*</script>`)
	reLocalsMset   = regexp.MustCompile(`(?s)locals\.mset\((\{.*?\})\);`)
	reShareGone    = regexp.MustCompile(`你来晚了|分享的文件已经被|链接不存在|分享已过期|此链接分享内容可能因为涉及侵权`)
)

// BaiduClient: a visitor's session with Baidu — its cookies (BAIDUID, and BDCLND once a code was taken).
type BaiduClient struct {
	c         *http.Client
	base      string
	lastToken string // the bdstoken the last share page gave (a logged-in visitor's)
	// Ask: when Baidu wants a captcha before it takes the extraction code — the picture, and whether the
	// last answer was wrong; it answers what the player read ("" for another picture). nil: not asked,
	// ErrShareCaptcha instead.
	Ask func(ctx context.Context, img []byte, bad bool) (string, error)
}

// CaptchaAsker: what OpenBaiduShare asks the player with (BaiduClient.Ask).
type CaptchaAsker = func(ctx context.Context, img []byte, bad bool) (string, error)

// NewBaiduClient: a visitor's session over the program's own client (its proxy, its timeouts).
func NewBaiduClient(c *http.Client) *BaiduClient {
	jar, _ := cookiejar.New(nil)
	cc := *c
	cc.Jar = jar
	return &BaiduClient{c: &cc, base: core.PanBase()}
}

func (b *BaiduClient) get(ctx context.Context, u string) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", core.UA)
	req.Header.Set("Referer", b.base+"/")
	resp, err := b.c.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp, body, err
}

// jsonOf reads an answer of Baidu's (which may begin with blank lines).
func jsonOf(b []byte, v any) error {
	return json.Unmarshal([]byte(strings.TrimSpace(string(b))), v)
}

// open: the share's data, after the extraction code when it has one.
func (b *BaiduClient) open(ctx context.Context, surl, pwd string) (*baiduShare, error) {
	page := b.base + "/s/" + url.PathEscape(surl)
	resp, body, err := b.get(ctx, page)
	if err != nil {
		return nil, err
	}
	if strings.Contains(resp.Request.URL.Path, "/share/init") {
		if pwd == "" {
			return nil, ErrShareNeedPwd
		}
		if err := b.verify(ctx, surl, pwd); err != nil {
			return nil, err
		}
		if resp, body, err = b.get(ctx, page); err != nil {
			return nil, err
		}
		if strings.Contains(resp.Request.URL.Path, "/share/init") {
			return nil, ErrSharePwd
		}
	}
	if reShareGone.Match(body) {
		return nil, ErrShareGone
	}
	m := reLocalsScript.FindSubmatch(body)
	if m == nil {
		m = reLocalsMset.FindSubmatch(body)
	}
	if m == nil {
		return nil, errors.New("无法读取百度网盘分享页面（页面结构可能已变化）")
	}
	dec := json.NewDecoder(strings.NewReader(string(m[1])))
	dec.UseNumber()
	var s baiduShare
	if err := dec.Decode(&s); err != nil {
		return nil, errors.New("无法读取百度网盘分享页面的内容")
	}
	var tok struct {
		Token string `json:"bdstoken"`
	}
	_ = json.Unmarshal(m[1], &tok)
	b.lastToken = tok.Token
	return &s, nil
}

// verify gives the share its extraction code — and, when Baidu asks for one first (errno -62), the captcha
// the player reads (errno -63: read wrong, another picture).
func (b *BaiduClient) verify(ctx context.Context, surl, pwd string) error {
	vcode, vstr := "", ""
	bad := false
	for round := 0; round < 12; round++ {
		errno, randsk, err := b.postVerify(ctx, surl, pwd, vcode, vstr)
		if err != nil {
			return err
		}
		switch errno {
		case 0:
			// the cookie that opens the share; set by the answer, and kept here in case it was not
			if randsk != "" {
				if u, err := url.Parse(b.base + "/"); err == nil {
					b.c.Jar.SetCookies(u, []*http.Cookie{{Name: "BDCLND", Value: randsk, Path: "/"}})
				}
			}
			return nil
		case -62, -63:
			if b.Ask == nil {
				return ErrShareCaptcha
			}
			bad = errno == -63 && vcode != ""
			for {
				img, str, err := b.captcha(ctx)
				if err != nil {
					return err
				}
				code, err := b.Ask(ctx, img, bad)
				if err != nil {
					return err
				}
				if code != "" {
					vcode, vstr = code, str
					break
				}
				bad = false // another picture, asked for by the player
			}
		default:
			return baiduMsg(errno)
		}
	}
	return ErrShareCaptcha
}

func (b *BaiduClient) postVerify(ctx context.Context, surl, pwd, vcode, vstr string) (int, string, error) {
	q := url.Values{"surl": {strings.TrimPrefix(surl, "1")}, "channel": {"chunlei"}, "web": {"1"}, "app_id": {baiduApp}, "clienttype": {"0"}}
	form := url.Values{"pwd": {pwd}, "vcode": {vcode}, "vcode_str": {vstr}}
	req, err := http.NewRequestWithContext(ctx, "POST", b.base+"/share/verify?"+q.Encode(), strings.NewReader(form.Encode()))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("User-Agent", core.UA)
	req.Header.Set("Referer", b.base+"/share/init?surl="+q.Get("surl"))
	resp, err := b.c.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var r struct {
		Errno  int    `json:"errno"`
		Randsk string `json:"randsk"`
	}
	if err := jsonOf(body, &r); err != nil {
		return 0, "", errors.New("百度网盘的回应无法读取")
	}
	return r.Errno, r.Randsk, nil
}

// captcha: a new captcha's picture, and the string that names it in the answer.
func (b *BaiduClient) captcha(ctx context.Context) ([]byte, string, error) {
	q := url.Values{"prod": {"shareverify"}, "web": {"1"}, "channel": {"chunlei"}, "app_id": {baiduApp}, "clienttype": {"0"}}
	_, body, err := b.get(ctx, b.base+"/api/getcaptcha?"+q.Encode())
	if err != nil {
		return nil, "", err
	}
	var r struct {
		Errno int    `json:"errno"`
		Str   string `json:"vcode_str"`
		Img   string `json:"vcode_img"`
	}
	if jsonOf(body, &r) != nil || r.Str == "" {
		return nil, "", errors.New("无法获取百度网盘的验证码")
	}
	img := r.Img
	if img == "" {
		img = b.base + "/genimage?" + r.Str
	}
	// only Baidu's own picture: the address the answer gives is not followed elsewhere
	if iu, err := url.Parse(img); err != nil || (iu.Host != "" && !strings.HasSuffix(iu.Hostname(), "baidu.com") && !strings.HasPrefix(img, b.base)) {
		img = b.base + "/genimage?" + r.Str
	}
	_, pic, err := b.get(ctx, img)
	if err != nil {
		return nil, "", err
	}
	if ct := http.DetectContentType(pic); !strings.HasPrefix(ct, "image/") {
		return nil, "", errors.New("百度网盘返回的验证码不是图片")
	}
	return pic, r.Str, nil
}

// list: what a folder of the share holds, every page of it.
func (b *BaiduClient) list(ctx context.Context, s *baiduShare, dir string) ([]baiduFile, error) {
	var out []baiduFile
	for page := 1; page < 1000; page++ {
		q := url.Values{"shareid": {s.ShareID.String()}, "uk": {s.UK.String()}, "dir": {dir}, "page": {strconv.Itoa(page)},
			"num": {strconv.Itoa(baiduPageSize)}, "order": {"name"}, "desc": {"0"}, "showempty": {"0"}, "web": {"1"},
			"channel": {"chunlei"}, "app_id": {baiduApp}, "clienttype": {"0"}}
		_, body, err := b.get(ctx, b.base+"/share/list?"+q.Encode())
		if err != nil {
			return nil, err
		}
		dec := json.NewDecoder(strings.NewReader(strings.TrimSpace(string(body))))
		dec.UseNumber()
		var r struct {
			Errno int         `json:"errno"`
			List  []baiduFile `json:"list"`
		}
		if dec.Decode(&r) != nil {
			return nil, errors.New("百度网盘的回应无法读取")
		}
		if err := baiduMsg(r.Errno); err != nil {
			return nil, err
		}
		out = append(out, r.List...)
		if len(r.List) < baiduPageSize {
			break
		}
	}
	return out, nil
}

// FetchBaiduShare reads what a share holds, as a listing (Surl: the share's id).
func FetchBaiduShare(ctx context.Context, c *http.Client, surl, pwd string) (*core.PanListing, error) {
	b := NewBaiduClient(c)
	s, err := b.open(ctx, surl, pwd)
	if err != nil {
		return nil, err
	}
	l := &core.PanListing{Surl: surl}
	n := 0
	var build func(fs []baiduFile, depth int) ([]*core.PanFile, error)
	build = func(fs []baiduFile, depth int) ([]*core.PanFile, error) {
		var out []*core.PanFile
		for _, f := range fs {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			pf := &core.PanFile{Name: f.Name, Dir: f.dir()}
			if !pf.Dir {
				pf.Size, _ = f.Size.Int64()
				l.Size += pf.Size
				n++
			} else if depth >= baiduMaxDepth || n >= baiduMaxFiles {
				pf.Partial, l.Truncated = true, true
			} else {
				kids, err := b.list(ctx, s, f.Path)
				if err != nil {
					return nil, err
				}
				if pf.Children, err = build(kids, depth+1); err != nil {
					return nil, err
				}
			}
			out = append(out, pf)
			if n >= baiduMaxFiles {
				l.Truncated = true
				break
			}
		}
		return out, nil
	}
	if l.Files, err = build(s.Files, 0); err != nil {
		return nil, err
	}
	l.Count = n
	l.Title = s.Title
	if l.Title == "" && len(s.Files) > 0 {
		l.Title = s.Files[0].Name
		if len(s.Files) > 1 {
			l.Title += " 等"
		}
	}
	return l, nil
}

// ---------- a share, opened to be saved ----------

// BaiduShare: a share opened with the player's login, for saving into their netdisk (internal/pandl).
type BaiduShare struct {
	UK, ShareID string
	Token       string // bdstoken: what the page gives a logged-in visitor
	root        string // the share's own path prefix ("/sharelink<uk>-<shareid>")
	top         []baiduFile
	b           *BaiduClient
}

// ShareFile: one entry of a share, by its id there.
type ShareFile struct {
	Path  string // in the share, from its root: "/Folder/file.zip"
	Name  string
	FsID  int64
	Dir   bool
	Size  int64
	share string // its path in the share's own terms
}

// OpenBaiduShare opens a share with these cookies (the player's login among them).
func OpenBaiduShare(ctx context.Context, c *http.Client, surl, pwd string, cookies []*http.Cookie, ask CaptchaAsker) (*BaiduShare, error) {
	b := NewBaiduClient(c)
	b.Ask = ask
	if u, err := url.Parse(b.base + "/"); err == nil && len(cookies) > 0 {
		b.c.Jar.SetCookies(u, cookies)
	}
	s, err := b.open(ctx, surl, pwd)
	if err != nil {
		return nil, err
	}
	out := &BaiduShare{UK: s.UK.String(), ShareID: s.ShareID.String(), top: s.Files, b: b}
	if len(s.Files) > 0 {
		out.root = path.Dir(s.Files[0].Path)
	}
	out.Token = b.lastToken
	return out, nil
}

func (s *BaiduShare) file(f baiduFile) ShareFile {
	size, _ := f.Size.Int64()
	id, _ := f.FsID.Int64()
	rel := strings.TrimPrefix(f.Path, s.root)
	if !strings.HasPrefix(rel, "/") {
		rel = "/" + rel
	}
	return ShareFile{Path: rel, Name: f.Name, FsID: id, Dir: f.dir(), Size: size, share: f.Path}
}

// Top: what the share holds at its root.
func (s *BaiduShare) Top() []ShareFile {
	var out []ShareFile
	for _, f := range s.top {
		out = append(out, s.file(f))
	}
	return out
}

// List: what a folder of the share holds.
func (s *BaiduShare) List(ctx context.Context, dir ShareFile) ([]ShareFile, error) {
	fs, err := s.b.list(ctx, &baiduShare{UK: json.Number(s.UK), ShareID: json.Number(s.ShareID)}, dir.share)
	if err != nil {
		return nil, err
	}
	var out []ShareFile
	for _, f := range fs {
		out = append(out, s.file(f))
	}
	return out, nil
}

// Find: the entry at a path of the share ("/Folder/file.zip"; "/" is not an entry: Top is).
func (s *BaiduShare) Find(ctx context.Context, p string) (ShareFile, error) {
	p = path.Clean("/" + strings.TrimPrefix(p, "/"))
	level := s.Top()
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i, name := range parts {
		var hit *ShareFile
		for j := range level {
			if level[j].Name == name {
				hit = &level[j]
				break
			}
		}
		if hit == nil {
			return ShareFile{}, errors.New("分享中已没有「" + p + "」")
		}
		if i == len(parts)-1 {
			return *hit, nil
		}
		if !hit.Dir {
			return ShareFile{}, errors.New("分享中已没有「" + p + "」")
		}
		kids, err := s.List(ctx, *hit)
		if err != nil {
			return ShareFile{}, err
		}
		level = kids
	}
	return ShareFile{}, errors.New("分享中已没有「" + p + "」")
}
