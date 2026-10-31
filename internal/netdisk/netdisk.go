// Package netdisk reads what a netdisk share holds: the share link a seller sends, the listing behind it,
// and what the listing says about the products inside (which base body, which category, and whether the
// share is a collection of several products).
package netdisk

import (
	"regexp"
	"strings"
	"sync"

	"vrclib/internal/cloudshare"
	"vrclib/internal/core"
)

// TaskPan: reading the shares of the cards that have one, as the window's task list shows it.
var TaskPan = &core.Task{Name: "pan", Label: "读取网盘分享"}

// The queue of shares waiting to be read, and whether one is being read. PanMu guards both.
var (
	PanMu     sync.Mutex
	PanActive bool
	PanQueue  []string
)

// PanPart: one product inside a share — the folder (or the single file) it lives in.
type PanPart struct {
	Path     string   `json:"path"` // the path inside the share ("" = the share itself)
	Name     string   `json:"name"`
	Category string   `json:"category,omitempty"`
	Bases    []string `json:"bases,omitempty"`
	BoothID  string   `json:"boothId,omitempty"`
	Files    int      `json:"files,omitempty"`
	Size     int64    `json:"size,omitempty"`
}

// PanInfo: what a share's listing says about what is in it.
type PanInfo struct {
	Category string    `json:"category,omitempty"`
	Bases    []string  `json:"bases,omitempty"`
	Parts    []PanPart `json:"parts,omitempty"` // the products inside, when it holds more than one
	Files    int       `json:"files,omitempty"`
	Size     int64     `json:"size,omitempty"`
}

var (
	// a Baidu Netdisk share, as a link or in the message a seller sends. The share id kept by the program is
	// the one in "/s/<surl>" (it begins with 1); the init pages carry it without that 1.
	reBaiduS    = regexp.MustCompile(`(?i)(?:pan|yun)\.baidu\.com/s/(1[A-Za-z0-9_-]+)`)
	reBaiduInit = regexp.MustCompile(`(?i)(?:pan|yun)\.baidu\.com/(?:share|wap)/init\?[^\s"'<>]*surl=([A-Za-z0-9_-]+)`)
	// the extraction code as sellers write it ("提取码: abcd", "密码：abcd", "pwd=abcd")
	rePwdWord = regexp.MustCompile(`(?i)(?:提取码|提取密码|密码|访问码|訪問碼|pwd|password|code)[^A-Za-z0-9]{0,4}([A-Za-z0-9]{4})`)
	rePwdURL  = regexp.MustCompile(`(?i)[?&]pwd=([A-Za-z0-9]{4})`)
)

// halfWidth turns the full-width characters a chat sends ("ｐａｎ．ｂａｉｄｕ．ｃｏｍ／ｓ／…") into plain ones.
func halfWidth(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 0xFF01 && r <= 0xFF5E:
			return r - 0xFEE0
		case r == 0x3000:
			return ' '
		}
		return r
	}, s)
}

// ShareSurl: the Baidu share id in a link or in the text a seller sends, "" when there is none.
func ShareSurl(text string) string {
	half := halfWidth(text)
	if m := reBaiduS.FindStringSubmatch(half); m != nil {
		return m[1]
	}
	if m := reBaiduInit.FindStringSubmatch(half); m != nil {
		return "1" + strings.TrimPrefix(m[1], "1")
	}
	return ""
}

// ShareID: the share id of a card's 分享链接 field ("" when it is not a Baidu share).
func ShareID(url string) string { return ShareSurl(url) }

// SharePwdFromURL: the extraction code the link, or the text around it, carries.
func SharePwdFromURL(link string) string {
	half := halfWidth(link)
	if m := rePwdURL.FindStringSubmatch(half); m != nil {
		return m[1]
	}
	if m := rePwdWord.FindStringSubmatch(half); m != nil {
		return m[1]
	}
	return ""
}

// CloudFirst: of the shares a text mentions, the Google Drive or Dropbox one comes first. A message with a
// Baidu link before it is that share's, and the cloud link in it is something else.
func CloudFirst(text string) bool {
	half := halfWidth(text)
	cloud := cloudshare.Parse(half)
	if cloud == nil {
		return false
	}
	at := -1
	if m := reBaiduS.FindStringIndex(half); m != nil {
		at = m[0]
	}
	if m := reBaiduInit.FindStringIndex(half); m != nil && (at < 0 || m[0] < at) {
		at = m[0]
	}
	if at < 0 {
		return true
	}
	i := strings.Index(half, cloud.URL)
	return i >= 0 && i < at
}

// ShareKey: the key a share's listing is kept under in st.Pan — the Baidu share id, or the whole card key
// for the clouds ("gd:…", "db:…").
func ShareKey(cardKey string) string {
	share, _, _ := strings.Cut(cardKey, "#")
	if s, ok := strings.CutPrefix(share, "pan:"); ok {
		return s
	}
	return share
}

// PanCardKeys: the keys of the cards that are a netdisk share, not an asset on disk.
func PanCardKeys(st *core.Store) []string {
	var out []string
	for _, key := range core.SortedKeys(st.User) {
		if core.IsNetdiskKey(key) {
			out = append(out, key)
		}
	}
	return out
}

// PanSub: the listing of the share a key belongs to, and the product inside it that the key names (nil when
// the key is the share itself).
func PanSub(st *core.Store, key string) (*core.PanListing, *PanPart) {
	if !core.IsNetdiskKey(key) {
		return nil, nil
	}
	l := st.Pan[ShareKey(key)]
	_, path, inside := strings.Cut(key, "#")
	if l == nil || !inside {
		return l, nil
	}
	for _, p := range SplitPanCached(l) {
		if p.Path == path {
			part := p
			return l, &part
		}
	}
	return l, nil
}
