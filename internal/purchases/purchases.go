// Package purchases reads what the player has bought — the Booth library and the Gumroad library — and
// downloads the files of a purchase into the asset folder.
package purchases

import (
	"sync"
	"sync/atomic"

	"vrclib/internal/core"
)

// The three kinds of work this package does, as the window's task list shows them.
var (
	TaskPurchase = &core.Task{Name: "purchase", Label: "同步 Booth 已购"}
	TaskGumroad  = &core.Task{Name: "gumroad", Label: "同步 Gumroad 已购"}
	TaskDownload = &core.Task{Name: "download", Label: "下载已购文件"}
)

// Stop marks: the window asked for the work to end.
var (
	PurchaseCancel atomic.Bool
	GumCancel      atomic.Bool
	GumBusy        atomic.Bool
)

// DLJob: one file being downloaded, as the window reads it.
type DLJob struct {
	ID      string `json:"id"`   // the download's id on the store's side (Booth: the downloadable's)
	Item    string `json:"item"` // the purchase it belongs to
	Name    string `json:"name,omitempty"`
	File    string `json:"file,omitempty"`
	Status  string `json:"status"` // queued, running, done, failed, cancelled
	Done    int64  `json:"done,omitempty"`
	Total   int64  `json:"total,omitempty"`
	Speed   int64  `json:"speed,omitempty"`
	Dir     string `json:"dir,omitempty"`
	Path    string `json:"path,omitempty"` // where the file ended up (unpacked: its folder)
	Err     string `json:"err,omitempty"`
	Login   bool   `json:"login,omitempty"` // it waits for the player to log in again
	Started int64  `json:"started,omitempty"`
	Ended   int64  `json:"ended,omitempty"`
}

// GumroadAccount: the Gumroad login, as the settings page shows it.
type GumroadAccount struct {
	LoggedIn bool   `json:"loggedIn"`
	Name     string `json:"name,omitempty"`
	Count    int    `json:"count"`    // purchases read from it
	LoginURL string `json:"loginUrl"` // the page the player logs in on
	Synced   int64  `json:"synced,omitempty"`
}

var (
	dlMu   sync.Mutex
	dlJobs []DLJob
)

// DLSnapshot: the downloads the window lists.
func DLSnapshot() []DLJob {
	dlMu.Lock()
	defer dlMu.Unlock()
	return append([]DLJob{}, dlJobs...)
}

// DLNeedLogin: a download is waiting for the player to log in to the store again.
func DLNeedLogin() bool {
	dlMu.Lock()
	defer dlMu.Unlock()
	for _, j := range dlJobs {
		if j.Login {
			return true
		}
	}
	return false
}

// DownloadDir: where the files of a purchase are put — the folder the settings name, or the first asset
// folder. The store is held by the caller.
func DownloadDir(st *core.Store) string {
	if d := st.Settings.DownloadDir; d != "" {
		return d
	}
	if len(st.Settings.Roots) > 0 {
		return st.Settings.Roots[0]
	}
	return ""
}

// ---------- the Booth library ----------

// ---------- the Gumroad library ----------

// gumAccount: what is known about the Gumroad login, kept for the window to read on every poll.
var gumAccount atomic.Pointer[GumroadAccount]

// CurrentGumroadAccount: what the settings page shows about the Gumroad login. The store is held.
func CurrentGumroadAccount(st *core.Store) GumroadAccount {
	a := GumroadAccount{}
	if p := gumAccount.Load(); p != nil {
		a = *p
	}
	a.LoggedIn = len(LoadGumSession()) > 0
	a.LoginURL = core.GumroadBase() + "/login"
	a.Synced = st.GumroadSync
	a.Count = 0
	for id := range st.Purchases {
		if core.IsGumID(id) {
			a.Count++
		}
	}
	return a
}

// ---------- downloads ----------
