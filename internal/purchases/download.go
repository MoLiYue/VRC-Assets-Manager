package purchases

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"vrclib/internal/archive"
	"vrclib/internal/core"
	"vrclib/internal/library"
	"vrclib/internal/naming"
	"vrclib/internal/unity"
)

// Downloading the files of a purchase.
//
// One file at a time, in the order they were queued: a store turns several at once away sooner than it is
// faster, and on one line it is not faster anyway. The files of a purchase go into a folder of their own
// under the download folder, named after the product; a file whose name is taken already gets a number
// rather than replacing what is there.
//
// Once every file of a purchase is down, what can be unpacked is unpacked — only the files of this batch,
// never what the folder held before (that is someone's older version, kept on purpose). The archives then go
// to the Recycle Bin unless the settings say to keep them.
//
// Importing into Unity is not part of this: a download fills the library, and the import is a step the
// player asks for separately. What "下载并导入" sets is remembered here (SetPendingImport) and handed on when
// the files are in.

const (
	dlRetry    = 2                // tries per file before it is called failed
	dlLoginFor = 30 * time.Minute // how long a file waits for the player to log in again
	dlTick     = 700 * time.Millisecond
)

// dlTask: one file waiting to be fetched.
type dlTask struct {
	Item string // the purchase
	ID   string // the download's id at the store
	URL  string
	File string // what the page called it ("" = whatever the store answers with)
}

var (
	dlQ    []dlTask
	dlStop atomic.Bool
	dlBusy atomic.Bool // a file is being fetched (or waits for a login) right now
	// dlGen counts the times the downloads were stopped: a file waiting for a login belongs to the round it
	// was queued in, and lets go when that round was stopped — even when another has been started since.
	dlGen      atomic.Int64
	dlPending  = map[string]unity.ImportReq{}
	dlStore    atomic.Pointer[core.Store]
	dlWake     = make(chan struct{}, 1)
	dlStarted  sync.Once
	dlUnpacked []string // the folders the last unpacking made, for the log
)

// ImportAfterDownload: what to do with the import "下载并导入" asked for, once the files are in. It is a var
// so that the import can be plugged in where it lives, and so that a download does not wait on it.
var ImportAfterDownload = func(st *core.Store, req unity.ImportReq) error {
	item := strings.TrimPrefix(req.Key, "purchase:")
	dir := ""
	for _, j := range DLSnapshot() {
		if j.Item == item && j.Dir != "" {
			dir = j.Dir
		}
	}
	if dir == "" {
		return errors.New("找不到下载的文件夹")
	}
	// the folder becomes a card once it is scanned; that card is what is imported
	library.RunPipeline(st, true, false, false, false, nil)
	st.Mu.RLock()
	key := ""
	for _, a := range st.Assets {
		for _, l := range a.Locations {
			if core.PathKey(l.Path) == core.PathKey(dir) || strings.HasPrefix(core.PathKey(l.Path), core.PathKey(dir)+string(filepath.Separator)) {
				key = a.Key
			}
		}
	}
	st.Mu.RUnlock()
	if key == "" {
		return errors.New("下载的文件夹中没有可导入的素材")
	}
	req.Key = key
	return unity.StartImport(st, req)
}

// QueueDownloads queues the files of a purchase (ids empty: all of them) and returns how many were queued.
func QueueDownloads(st *core.Store, item string, ids []string) (int, error) {
	if strings.TrimSpace(item) == "" {
		return 0, errors.New("未指定要下载的已购商品")
	}
	st.Mu.RLock()
	p := st.Purchases[item]
	var tasks []dlTask
	if p != nil {
		for i, u := range p.Downloads {
			id := downloadID(u)
			if len(ids) > 0 && !core.ContainsStr(ids, id) {
				continue
			}
			name := ""
			if i < len(p.Files) {
				name = p.Files[i]
			}
			tasks = append(tasks, dlTask{Item: item, ID: id, URL: u, File: name})
		}
	}
	st.Mu.RUnlock()
	if p == nil {
		return 0, errors.New("未找到该已购商品")
	}
	if len(tasks) == 0 {
		return 0, errors.New("该商品没有可下载的文件")
	}
	dlStore.Store(st)
	dlStop.Store(false)
	n := 0
	dlMu.Lock()
	for _, t := range tasks {
		if waiting(t.Item, t.ID) {
			continue // already queued or running: not twice
		}
		dlQ = append(dlQ, t)
		dlJobs = append(dlJobs, DLJob{ID: t.ID, Item: t.Item, Name: p.Name, File: t.File, Status: "queued"})
		n++
	}
	dlMu.Unlock()
	if n == 0 {
		return 0, errors.New("这些文件已在下载队列中")
	}
	core.Downloading.Add(int32(n))
	dlStarted.Do(func() { go dlLoop() })
	select {
	case dlWake <- struct{}{}:
	default:
	}
	core.BumpRev()
	return n, nil
}

// sessionOf: the login a purchase's files are fetched with.
func sessionOf(item string) []core.SavedCookie {
	if core.IsGumID(item) {
		return LoadGumSession()
	}
	return LoadBoothSession()
}

// downloadID: what names a file at its store — Booth's downloadable number, Gumroad's product file id.
func downloadID(u string) string {
	if pu, err := url.Parse(u); err == nil {
		if v := pu.Query().Get("product_file_ids[]"); v != "" {
			return v
		}
		return path.Base(strings.TrimRight(pu.Path, "/"))
	}
	return path.Base(strings.TrimRight(u, "/"))
}

// waiting: this file is in the queue already. dlMu is held.
func waiting(item, id string) bool {
	for _, j := range dlJobs {
		if j.Item == item && j.ID == id && (j.Status == "queued" || j.Status == "running" || j.Status == "login" || j.Status == "unpacking") {
			return true
		}
	}
	return false
}

// CancelDownloads stops the downloads that are queued or running.
func CancelDownloads() {
	dlStop.Store(true)
	dlGen.Add(1)
	dlMu.Lock()
	left := int32(0)
	for i := range dlJobs {
		if dlJobs[i].Status == "queued" || dlJobs[i].Status == "login" {
			dlJobs[i].Status, dlJobs[i].Ended = "cancelled", time.Now().Unix()
			left++
		}
	}
	dlQ = nil
	dlMu.Unlock()
	if left > 0 {
		core.Downloading.Add(-left)
	}
	core.BumpRev()
}

// SetPendingImport: once this purchase's files are downloaded and unpacked, import them ("下载并导入").
func SetPendingImport(item string, req unity.ImportReq) {
	dlMu.Lock()
	dlPending[item] = req
	dlMu.Unlock()
}

// takePending: the import this purchase was to have, if any.
func takePending(item string) (unity.ImportReq, bool) {
	dlMu.Lock()
	defer dlMu.Unlock()
	req, ok := dlPending[item]
	delete(dlPending, item)
	return req, ok
}

// dlLoop runs for as long as the program does, one file at a time.
func dlLoop() {
	for {
		t, ok := nextTask()
		if !ok {
			select {
			case <-dlWake:
			case <-time.After(2 * time.Second):
			}
			if core.Quitting.Load() {
				return
			}
			continue
		}
		st := dlStore.Load()
		if st == nil {
			continue
		}
		dlBusy.Store(true)
		core.RunTask(TaskDownload, func() { runTask(st, t) })
		if lastOfItem(t.Item) {
			finishItem(st, t.Item)
		}
		dlBusy.Store(false)
	}
}

func nextTask() (dlTask, bool) {
	dlMu.Lock()
	defer dlMu.Unlock()
	if len(dlQ) == 0 {
		return dlTask{}, false
	}
	t := dlQ[0]
	dlQ = dlQ[1:]
	return t, true
}

// lastOfItem: nothing of this purchase is left to fetch.
func lastOfItem(item string) bool {
	dlMu.Lock()
	defer dlMu.Unlock()
	for _, t := range dlQ {
		if t.Item == item {
			return false
		}
	}
	return true
}

// setJob changes one file's row, by purchase and download id.
func setJob(item, id string, f func(*DLJob)) {
	dlMu.Lock()
	for i := range dlJobs {
		if dlJobs[i].Item == item && dlJobs[i].ID == id {
			f(&dlJobs[i])
		}
	}
	dlMu.Unlock()
}

// runTask fetches one file.
func runTask(st *core.Store, t dlTask) {
	if dlStop.Load() || core.Quitting.Load() {
		setJob(t.Item, t.ID, func(j *DLJob) { j.Status, j.Ended = "cancelled", time.Now().Unix() })
		core.Downloading.Add(-1)
		return
	}
	dir, err := itemDir(st, t.Item)
	if err != nil {
		fail(t, err)
		return
	}
	setJob(t.Item, t.ID, func(j *DLJob) {
		j.Status, j.Dir, j.Started, j.Err, j.Login = "running", dir, time.Now().Unix(), "", false
	})
	core.BumpRev()
	c := core.HTTPClient(st)
	var last error
	for try := 0; try < dlRetry; try++ {
		p, err := fetchFile(c, st, t, dir)
		switch {
		case err == nil:
			// the last file of a purchase is not done until what came down is unpacked: the rows say so
			// from here on, so that nothing (the window, an update into projects) takes it for finished
			// in between
			status := "done"
			if lastOfItem(t.Item) {
				status = "unpacking"
			}
			setJob(t.Item, t.ID, func(j *DLJob) {
				j.Status, j.Path, j.Ended, j.Done = status, p, time.Now().Unix(), j.Total
			})
			core.Downloading.Add(-1)
			core.Logf("已下载 %s", p)
			core.BumpRev()
			return
		case errors.Is(err, ErrStoreLogin):
			if !waitForLogin(t) {
				return
			}
		case dlStop.Load(), core.Quitting.Load():
			setJob(t.Item, t.ID, func(j *DLJob) { j.Status, j.Ended = "cancelled", time.Now().Unix() })
			core.Downloading.Add(-1)
			return
		default:
			last = err
			time.Sleep(time.Second)
		}
	}
	if last == nil {
		last = errors.New("下载失败")
	}
	fail(t, last)
}

func fail(t dlTask, err error) {
	setJob(t.Item, t.ID, func(j *DLJob) {
		j.Status, j.Err, j.Ended = "failed", core.TrimErr(err), time.Now().Unix()
	})
	core.Downloading.Add(-1)
	core.Logf("下载失败 %s：%v", t.File, err)
	core.BumpRev()
}

// waitForLogin keeps a file waiting while the player logs in to the store again.
func waitForLogin(t dlTask) bool {
	setJob(t.Item, t.ID, func(j *DLJob) { j.Status, j.Login, j.Err = "login", true, "" })
	core.BumpRev()
	gen := dlGen.Load()
	before := cookieHeader(sessionOf(t.Item))
	for end := time.Now().Add(dlLoginFor); time.Now().Before(end); {
		time.Sleep(time.Second)
		if dlStop.Load() || core.Quitting.Load() || dlGen.Load() != gen {
			setJob(t.Item, t.ID, func(j *DLJob) { j.Status, j.Ended = "cancelled", time.Now().Unix() })
			core.Downloading.Add(-1)
			return false
		}
		if now := cookieHeader(sessionOf(t.Item)); now != "" && now != before {
			setJob(t.Item, t.ID, func(j *DLJob) { j.Status, j.Login = "running", false })
			core.BumpRev()
			return true
		}
	}
	fail(t, errors.New("等待登录超时"))
	return false
}

// itemDir: the folder a purchase's files go into, made if it is not there.
func itemDir(st *core.Store, item string) (string, error) {
	st.Mu.RLock()
	dl := DownloadDir(st)
	name := ""
	if p := st.Purchases[item]; p != nil {
		name = core.SafeName(p.Name, 70)
	}
	st.Mu.RUnlock()
	if dl == "" {
		return "", errors.New("请先在设置中选择素材文件夹或下载位置")
	}
	if name == "" || naming.IsGenericName(name) {
		name = item // nothing to call it by: the item number does
	}
	dir := filepath.Join(dl, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	return dir, nil
}

// fetchFile fetches one file into dir and answers where it went. A part already there is carried on from
// when the store allows it.
var fetchFile = func(c *http.Client, st *core.Store, t dlTask, dir string) (string, error) {
	req, err := http.NewRequest("GET", t.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", core.UA)
	// the login of the store the purchase is from; a redirect to the file host does not carry it along
	if ck := cookieHeader(sessionOf(t.Item)); ck != "" {
		req.Header.Set("Cookie", ck)
	}
	// what was fetched before, to carry on from
	tmp := filepath.Join(dir, "."+t.ID+".part")
	var at int64
	if fi, err := os.Stat(tmp); err == nil {
		at = fi.Size()
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", at))
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403 || strings.Contains(resp.Request.URL.Path, "/users/sign_in"):
		return "", ErrStoreLogin
	case resp.StatusCode == 416: // the part is the whole file already
	case resp.StatusCode == 206:
	case resp.StatusCode == 200:
		at = 0 // the store sent it from the start
	default:
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	total := resp.ContentLength
	if total > 0 {
		total += at
	}
	setJob(t.Item, t.ID, func(j *DLJob) { j.Total, j.Done = total, at })
	flag := os.O_CREATE | os.O_WRONLY
	if at > 0 {
		flag |= os.O_APPEND
	} else {
		flag |= os.O_TRUNC
	}
	f, err := os.OpenFile(tmp, flag, 0644)
	if err != nil {
		return "", err
	}
	done, err := copyWatched(f, resp.Body, t, at)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	name := fileName(t, resp)
	dst := archive.UniquePath(filepath.Join(dir, name))
	if err := os.Rename(tmp, dst); err != nil {
		return "", err
	}
	setJob(t.Item, t.ID, func(j *DLJob) { j.Done, j.File = done, filepath.Base(dst) })
	return dst, nil
}

// copyWatched copies the body over, telling the window how far it has got.
func copyWatched(w io.Writer, r io.Reader, t dlTask, at int64) (int64, error) {
	buf := make([]byte, 256<<10)
	done, t0, last := at, time.Now(), time.Now()
	for {
		if dlStop.Load() || core.Quitting.Load() {
			return done, errors.New("已停止")
		}
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return done, werr
			}
			done += int64(n)
			if time.Since(last) > dlTick {
				last = time.Now()
				speed := int64(float64(done-at) / time.Since(t0).Seconds())
				setJob(t.Item, t.ID, func(j *DLJob) { j.Done, j.Speed = done, speed })
				core.BumpRev()
			}
		}
		if err == io.EOF {
			return done, nil
		}
		if err != nil {
			return done, err
		}
	}
}

// fileName: what to save a file as — what the library page called it, else what the store says, else the
// download's id.
func fileName(t dlTask, resp *http.Response) string {
	if n := core.SafeName(t.File, 120); n != "" {
		return n
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if i := strings.Index(strings.ToLower(cd), "filename="); i >= 0 {
			n := strings.Trim(strings.TrimSpace(cd[i+len("filename="):]), `";`)
			if n = core.SafeName(n, 120); n != "" {
				return n
			}
		}
	}
	if n := core.SafeName(path.Base(resp.Request.URL.Path), 120); n != "" && n != "/" && n != "." {
		return n
	}
	return t.ID + ".bin"
}

// finishItem: every file of a purchase is in. What can be unpacked is unpacked — only these files — and the
// import "下载并导入" asked for is handed on.
func finishItem(st *core.Store, item string) {
	var places []string
	dlMu.Lock()
	dir := ""
	for _, j := range dlJobs {
		if j.Item == item && (j.Status == "done" || j.Status == "unpacking") && j.Path != "" {
			places, dir = append(places, j.Path), j.Dir
		}
	}
	dlMu.Unlock()
	if len(places) == 0 {
		return
	}
	defer setItemJobs(item, "done", "") // whatever happens below, the rows end as done
	st.Mu.RLock()
	noExtract, keepZip := st.Settings.NoExtract, st.Settings.KeepZip
	st.Mu.RUnlock()
	req, wanted := takePending(item)
	if !noExtract {
		setItemJobs(item, "unpacking", "正在解压")
		remove := core.RecycleOnly
		if keepZip {
			remove = func([]string) error { return nil }
		}
		res := archive.UnpackAll(places, req.Pwd, remove, func(name string, i, n int) {
			TaskDownload.Set(i, n, "正在解压 "+name)
		})
		dlUnpacked = res.Done
		for f, why := range res.Failed {
			core.Logf("解压失败 %s：%s", f, why)
		}
		if len(res.Done) > 0 {
			core.Logf("已解压 %d 个压缩包到 %s", len(res.Done), dir)
		}
		setItemJobs(item, "done", "")
	}
	st.Mu.Lock()
	if u := st.User["purchase:"+item]; u != nil {
		u.Downloaded, u.Updated = dir, time.Now().Unix()
	}
	st.Mu.Unlock()
	_ = st.Save()
	core.Notify(core.Notice{Kind: "download", What: item})
	core.BumpRev()
	if wanted {
		req.Key = "purchase:" + item
		if err := ImportAfterDownload(st, req); err != nil {
			core.Logf("下载并导入：%v", err)
		}
	}
}

// setItemJobs moves every finished row of a purchase to another state (the unpacking is one step for all of
// them, and the window shows it on the rows).
func setItemJobs(item, status, msg string) {
	dlMu.Lock()
	for i := range dlJobs {
		if dlJobs[i].Item == item && (dlJobs[i].Status == "done" || dlJobs[i].Status == "unpacking") {
			dlJobs[i].Status = status
			if msg != "" {
				dlJobs[i].Err = ""
			}
		}
	}
	dlMu.Unlock()
	if msg != "" {
		TaskDownload.Set(0, 0, msg)
	}
	core.BumpRev()
}
