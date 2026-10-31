// Package pandl downloads from a netdisk: the shares a seller sends (Baidu Netdisk, Google Drive, Dropbox)
// and the player's own Baidu Netdisk. It uses the player's own account with its normal permissions.
package pandl

import (
	"errors"
	"sync"

	"vrclib/internal/core"
)

// TaskPanDL: netdisk downloads, as the window's task list shows them.
var TaskPanDL = &core.Task{Name: "pandl", Label: "网盘下载"}

// ErrBaiduLogin: Baidu wants the player to log in again (the saved login is gone or was turned away).
var ErrBaiduLogin = errors.New("需要先登录百度网盘")

// PanJob: one netdisk download, as the window reads it.
type PanJob struct {
	ID         int64    `json:"id"`
	Key        string   `json:"key"` // the card it belongs to
	Name       string   `json:"name,omitempty"`
	Stage      string   `json:"stage"` // queued, save, list, download, unpack, done, failed, cancelled
	Msg        string   `json:"msg,omitempty"`
	Paths      []string `json:"paths,omitempty"` // what of the share is being fetched ("/" = all of it)
	Files      int      `json:"files,omitempty"`
	File       string   `json:"file,omitempty"` // the one being fetched right now
	Done       int64    `json:"done,omitempty"`
	Total      int64    `json:"total,omitempty"`
	Speed      int64    `json:"speed,omitempty"`
	Dir        string   `json:"dir,omitempty"`
	Captcha    string   `json:"captcha,omitempty"`    // the picture Baidu asks the player to read (a data: address)
	CaptchaN   int      `json:"captchaN,omitempty"`   // how many pictures were shown (the window opens its dialog for a new one)
	CaptchaBad bool     `json:"captchaBad,omitempty"` // the last answer was wrong
	Err        string   `json:"err,omitempty"`
	Login      bool     `json:"login,omitempty"`
	Failed     []string `json:"failed,omitempty"` // archives that could not be unpacked, and why
	Saved      string   `json:"saved,omitempty"`  // Baidu: the copy in the player's netdisk
	Started    int64    `json:"started,omitempty"`
	Ended      int64    `json:"ended,omitempty"`
}

// BaiduAccount: the Baidu Netdisk login, as the window shows it.
type BaiduAccount struct {
	LoggedIn bool   `json:"loggedIn"`
	Name     string `json:"name,omitempty"`
	VIP      int    `json:"vip,omitempty"` // 0 none, 1 会员, 2 超级会员
}

// BaiduSession: the login kept on this computer, encrypted.
type BaiduSession struct {
	Name    string             `json:"name,omitempty"`
	VIP     int                `json:"vip,omitempty"`
	Cookies []core.SavedCookie `json:"cookies,omitempty"`
	Saved   int64              `json:"saved,omitempty"`
}

// DiskEntry: one row of the player's own netdisk, for picking what to download.
type DiskEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Dir   bool   `json:"dir,omitempty"`
	Size  int64  `json:"size,omitempty"`
	MTime int64  `json:"mtime,omitempty"`
}

// BDClient talks to Baidu with one login.
type BDClient struct {
	st    *core.Store
	s     *BaiduSession
	token string // bdstoken, once asked for: what the requests that change the netdisk carry
	noSeg bool   // Baidu turned several connections away: one from now on
}

// NewBDClient: a client for this login.
func NewBDClient(st *core.Store, s *BaiduSession) *BDClient {
	return &BDClient{st: st, s: s}
}

var (
	jobMu sync.Mutex
	jobs  []PanJob
)

// PanJobsSnapshot: the downloads the window lists.
func PanJobsSnapshot() []PanJob {
	jobMu.Lock()
	defer jobMu.Unlock()
	return append([]PanJob{}, jobs...)
}

// VIPLabel: the membership level in words, for the logs and the feedback report.
func VIPLabel(vip int) string {
	switch vip {
	case 1:
		return "普通会员"
	case 2:
		return "超级会员"
	}
	return "非会员"
}
