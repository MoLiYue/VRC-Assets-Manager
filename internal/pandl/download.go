package pandl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"vrclib/internal/archive"
	"vrclib/internal/cloudshare"
	"vrclib/internal/core"
	"vrclib/internal/library"
	"vrclib/internal/netdisk"
	"vrclib/internal/purchases"
	"vrclib/internal/unity"
)

// Downloading a netdisk share into the library.
//
// Google Drive and Dropbox files are fetched straight from the share (internal/cloudshare). A Baidu share
// cannot be: Baidu gives files only out of the player's own netdisk. So what was picked of it is saved
// there first, into a folder of its own under /MioVRCA, and then fetched through the PCS — with the
// player's login, as their own files, at whatever speed Baidu gives their account. Nothing here goes round
// Baidu's limits.
//
// One share at a time. A file that breaks off is carried on from where it stopped (a .part beside it), and
// a download stopped or cut short by closing the program carries on the same way next time. Once every file
// is in, what can be unpacked is unpacked — only these files, never what the folder held before.

const (
	pcsUA      = "pan.baidu.com" // the PCS gives files to Baidu's own client only
	dlTries    = 4
	diskRoot   = "/MioVRCA"
	saveBatch  = 100 // entries saved in one request
	maxListing = 20000
)

var (
	qMu     sync.Mutex
	queue   []*panTask
	running bool
	nextID  int64
	cancel  context.CancelFunc
	qWake   = make(chan struct{}, 1)
)

// panTask: one download, with what it was asked for.
type panTask struct {
	id    int64
	key   string
	paths []string
	imp   *unity.ImportReq
	disk  string // a folder or file of the player's own netdisk, instead of a share
}

func init() {
	wakeQueue = func() {
		select {
		case qWake <- struct{}{}:
		default:
		}
	}
}

// setJob changes the row of a download.
func setJob(id int64, f func(*PanJob)) {
	jobMu.Lock()
	for i := range jobs {
		if jobs[i].ID == id {
			f(&jobs[i])
		}
	}
	jobMu.Unlock()
	core.BumpRev()
}

// QueuePanDownload queues what of a share is to be fetched (paths empty: all of it); imp is the import to
// run once it is unpacked ("下载并导入").
func QueuePanDownload(st *core.Store, key string, paths []string, imp *unity.ImportReq) error {
	if !core.IsNetdiskKey(key) {
		return errors.New("不是网盘分享")
	}
	share, _, _ := strings.Cut(key, "#")
	st.Mu.RLock()
	u := st.User[share]
	name := ""
	if u != nil {
		name = u.Name
	}
	if l := st.Pan[netdisk.ShareKey(share)]; name == "" && l != nil {
		name = l.Title
	}
	st.Mu.RUnlock()
	if u == nil || u.ShareURL == "" {
		return errors.New("该卡片没有网盘分享链接")
	}
	jobMu.Lock()
	for _, j := range jobs {
		if j.Key == key && !over(j.Stage) {
			jobMu.Unlock()
			return errors.New("该分享已在下载队列中")
		}
	}
	jobMu.Unlock()
	return enqueue(st, &panTask{key: key, paths: cleanPaths(paths), imp: imp}, name)
}

// QueueDiskDownload queues a folder or a file of the player's own netdisk.
func QueueDiskDownload(st *core.Store, p string) error {
	p = path.Clean("/" + strings.TrimSpace(p))
	if p == "/" {
		return errors.New("请选择要下载的文件夹或文件")
	}
	return enqueue(st, &panTask{key: "disk:" + p, disk: p}, path.Base(p))
}

func cleanPaths(ps []string) []string {
	var out []string
	for _, p := range ps {
		if p = path.Clean("/" + strings.TrimSpace(p)); p == "/" {
			return nil // all of it
		}
		out = append(out, p)
	}
	return out
}

func over(stage string) bool { return stage == "done" || stage == "failed" || stage == "cancelled" }

func enqueue(st *core.Store, t *panTask, name string) error {
	qMu.Lock()
	nextID++
	t.id = nextID
	queue = append(queue, t)
	start := !running
	running = true
	qMu.Unlock()
	jobMu.Lock()
	jobs = append(jobs, PanJob{ID: t.id, Key: t.key, Name: name, Stage: "queued", Paths: t.paths, Started: time.Now().Unix()})
	jobMu.Unlock()
	core.Downloading.Add(1)
	core.BumpRev()
	if start {
		go worker(st)
	}
	return nil
}

func worker(st *core.Store) {
	for {
		qMu.Lock()
		if len(queue) == 0 || core.Quitting.Load() {
			running = false
			qMu.Unlock()
			return
		}
		t := queue[0]
		queue = queue[1:]
		ctx, c := context.WithCancel(context.Background())
		cancel = c
		qMu.Unlock()
		core.RunTask(TaskPanDL, func() { run(ctx, st, t) })
		c()
		core.Downloading.Add(-1)
	}
}

// run does one download, and says how it ended.
func run(ctx context.Context, st *core.Store, t *panTask) {
	var files []string
	var dir, saved string
	var err error
	switch {
	case t.disk != "":
		dir, files, err = runDisk(ctx, st, t)
	case cloudshare.IsCloudKey(t.key):
		dir, files, err = runCloud(ctx, st, t)
	default:
		dir, files, saved, err = runBaidu(ctx, st, t)
	}
	if ctx.Err() != nil {
		setJob(t.id, func(j *PanJob) { j.Stage, j.Msg, j.Ended = "cancelled", "已取消", time.Now().Unix() })
		return
	}
	if err != nil {
		core.Logf("网盘下载失败 %s：%v", t.key, err)
		setJob(t.id, func(j *PanJob) { j.Stage, j.Err, j.Ended = "failed", core.TrimErr(err), time.Now().Unix() })
		return
	}
	failed := finish(st, t, dir, files)
	setJob(t.id, func(j *PanJob) {
		j.Stage, j.Dir, j.Files, j.Ended = "done", dir, len(files), time.Now().Unix()
		j.Msg = fmt.Sprintf("已下载 %d 个文件", len(files))
		j.Failed, j.Saved = failed, saved
	})
	core.Notify(core.Notice{Kind: "pandownload", What: t.key})
	if t.imp != nil {
		importAfter(st, dir, *t.imp)
	}
}

// localDir: where a download goes — a folder of the download folder named after the share.
func localDir(st *core.Store, name, key string) (string, error) {
	st.Mu.RLock()
	dl := purchases.DownloadDir(st)
	st.Mu.RUnlock()
	if dl == "" {
		return "", errors.New("请先在设置中选择素材文件夹或下载位置")
	}
	n := core.SafeName(name, 70)
	if n == "" {
		n = core.SafeName(strings.NewReplacer(":", "_", "/", "_").Replace(key), 70)
	}
	dir := filepath.Join(dl, n)
	return dir, os.MkdirAll(dir, 0755)
}

func jobName(id int64) string {
	jobMu.Lock()
	defer jobMu.Unlock()
	for _, j := range jobs {
		if j.ID == id {
			return j.Name
		}
	}
	return ""
}

// ---------- Baidu ----------

func loginWait(ctx context.Context, t *panTask) (*BaiduSession, error) {
	for {
		if s := LoadBaiduSession(); s != nil {
			return s, nil
		}
		setJob(t.id, func(j *PanJob) { j.Stage, j.Login = "login", true })
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-qWake:
		case <-time.After(30 * time.Second):
		}
	}
}

func runBaidu(ctx context.Context, st *core.Store, t *panTask) (dir string, files []string, saved string, err error) {
	s, err := loginWait(ctx, t)
	if err != nil {
		return "", nil, "", err
	}
	setJob(t.id, func(j *PanJob) { j.Stage, j.Login, j.Msg = "fetch", false, "正在读取分享" })
	c := NewBDClient(st, s)
	name, vip, err := c.Whoami() // the level as it is now: a membership may have ended, or begun
	if err != nil {
		if errors.Is(err, ErrBaiduLogin) {
			ForgetBaiduSession()
		}
		return "", nil, "", err
	}
	if vip != s.VIP || name != s.Name {
		s.Name, s.VIP = name, vip
		_ = SaveBaiduSession(s)
		core.BumpRev()
	}
	t0 := time.Now()
	defer func() {
		if err == nil {
			var total int64
			for _, f := range files {
				if fi, e := os.Stat(f); e == nil {
					total += fi.Size()
				}
			}
			secs := time.Since(t0).Seconds()
			core.Logf("百度网盘下载完成：%d 个文件，%.1f MB，平均 %.0f KB/s（%s）", len(files), float64(total)/1e6, float64(total)/1024/max(secs, 0.001), VIPLabel(vip))
		}
	}()
	share, _, _ := strings.Cut(t.key, "#")
	st.Mu.RLock()
	u := st.User[share]
	link, pwd := u.ShareURL, u.SharePwd
	st.Mu.RUnlock()
	if pwd == "" {
		pwd = netdisk.SharePwdFromURL(link)
	}
	surl := netdisk.ShareSurl(link)
	sh, err := netdisk.OpenBaiduShare(ctx, c.http(), surl, pwd, httpCookies(s.Cookies), asker(t.id, t.key))
	if err != nil {
		return "", nil, "", err
	}
	// what was picked, by the share's own entries
	var pick []netdisk.ShareFile
	if len(t.paths) == 0 {
		pick = sh.Top()
	} else {
		for _, p := range t.paths {
			f, err := sh.Find(ctx, p)
			if err != nil {
				return "", nil, "", err
			}
			pick = append(pick, f)
		}
	}
	if len(pick) == 0 {
		return "", nil, "", errors.New("分享中没有文件")
	}
	// saved into a folder of the player's netdisk
	setJob(t.id, func(j *PanJob) { j.Stage, j.Msg = "save", "正在转存到网盘" })
	dest, err := c.freshDir(ctx, diskRoot+"/"+remoteName(jobName(t.id), surl))
	if err != nil {
		return "", nil, "", err
	}
	if err := c.save(ctx, sh, pick, dest); err != nil {
		return "", nil, "", err
	}
	// and fetched from there
	dir, err = localDir(st, jobName(t.id), t.key)
	if err != nil {
		return "", nil, "", err
	}
	files, err = c.fetchTree(ctx, t.id, dest, dir)
	return dir, files, dest, err
}

func httpCookies(cs []core.SavedCookie) []*http.Cookie {
	var out []*http.Cookie
	for _, c := range cs {
		out = append(out, &http.Cookie{Name: c.Name, Value: c.Value, Path: "/"})
	}
	return out
}

// reNotBaidu: what Baidu does not take in a folder name (error -7): the characters a file system does not,
// and emoji and other symbols.
var reNotBaidu = regexp.MustCompile(`[\\/:*?"<>|\x00-\x1f]|[\x{1F000}-\x{1FFFF}\x{2600}-\x{27BF}\x{FE0F}\x{200D}]`)

// remoteName: the name of the folder in the player's netdisk — the share's, without what Baidu refuses;
// the share's id when nothing is left.
func remoteName(name, surl string) string {
	n := strings.TrimSpace(reNotBaidu.ReplaceAllString(name, ""))
	if r := []rune(n); len(r) > 60 {
		n = string(r[:60])
	}
	if n == "" || n == "." || n == ".." {
		return surl
	}
	return n
}

// mkdir makes a folder of the player's netdisk (a folder that is there already is used as it is).
func (c *BDClient) mkdir(ctx context.Context, p string) (string, error) {
	got, _, err := c.mkdirNew(ctx, p)
	return got, err
}

// freshDir makes a folder no earlier download has used: a folder of that name that is there already holds
// the copies saved then, and Baidu would rename what is saved beside them ("Dress(1).zip"), which would then
// come down too. "<name> (2)", "<name> (3)" … instead.
func (c *BDClient) freshDir(ctx context.Context, p string) (string, error) {
	for i := 1; i <= 50; i++ {
		try := p
		if i > 1 {
			try = fmt.Sprintf("%s (%d)", p, i)
		}
		got, existed, err := c.mkdirNew(ctx, try)
		if err != nil {
			return "", err
		}
		if !existed {
			return got, nil
		}
	}
	return "", errors.New("无法在网盘中创建文件夹：同名文件夹过多，请清理网盘中的 " + diskRoot + " 文件夹")
}

// mkdirNew: mkdir, saying whether the folder was there already.
func (c *BDClient) mkdirNew(ctx context.Context, p string) (string, bool, error) {
	if c.token == "" {
		if _, _, err := c.Whoami(); err != nil {
			return "", false, err
		}
	}
	var r struct {
		Errno int    `json:"errno"`
		Path  string `json:"path"`
	}
	form := url.Values{"path": {p}, "isdir": {"1"}, "block_list": {"[]"}}
	if err := c.call(ctx, "POST", "/api/create", url.Values{"a": {"commit"}, "bdstoken": {c.token}}, form, &r); err != nil {
		return "", false, err
	}
	switch r.Errno {
	case 0:
		if r.Path != "" {
			return r.Path, false, nil // (Baidu's own name for it, when it changed it)
		}
		return p, false, nil
	case -8:
		return p, true, nil
	case -6:
		return "", false, ErrBaiduLogin
	case -7:
		return "", false, errors.New("百度网盘不接受该文件夹名称（错误 -7）")
	}
	return "", false, fmt.Errorf("无法在网盘中创建文件夹（百度网盘错误 %d）", r.Errno)
}

// save saves these entries of the share into dest. An account may save only so many files at once (errno
// 12): a folder that is too many is saved a level down, its entries one batch after another.
func (c *BDClient) save(ctx context.Context, sh *netdisk.BaiduShare, pick []netdisk.ShareFile, dest string) error {
	for start := 0; start < len(pick); start += saveBatch {
		batch := pick[start:min(start+saveBatch, len(pick))]
		errno, err := c.transfer(ctx, sh, batch, dest)
		if err != nil {
			return err
		}
		switch errno {
		case 0:
			continue
		case 12:
			// too many files: one entry at a time, and a folder's entries into a folder of that name
			for _, f := range batch {
				if n, err := c.transfer(ctx, sh, []netdisk.ShareFile{f}, dest); err != nil {
					return err
				} else if n == 0 {
					continue
				} else if n != 12 || !f.Dir {
					return saveErr(n)
				}
				sub, err := c.mkdir(ctx, dest+"/"+f.Name)
				if err != nil {
					return err
				}
				kids, err := sh.List(ctx, f)
				if err != nil {
					return err
				}
				if err := c.save(ctx, sh, kids, sub); err != nil {
					return err
				}
			}
		default:
			return saveErr(errno)
		}
	}
	return nil
}

func saveErr(errno int) error {
	switch errno {
	case -6:
		return ErrBaiduLogin
	case 2:
		return errors.New("转存到网盘失败（百度网盘错误 2）：分享中的文件已变化，或网盘中的目标文件夹已不存在，请重新读取分享后再试")
	case 4, -33:
		return errors.New("转存到网盘失败：网盘空间不足")
	case 12:
		return errors.New("转存到网盘失败：文件数量超过账号单次转存的上限")
	}
	return fmt.Errorf("转存到网盘失败（百度网盘错误 %d）", errno)
}

func (c *BDClient) transfer(ctx context.Context, sh *netdisk.BaiduShare, files []netdisk.ShareFile, dest string) (int, error) {
	ids := make([]int64, 0, len(files))
	for _, f := range files {
		ids = append(ids, f.FsID)
	}
	b, _ := json.Marshal(ids)
	var r struct {
		Errno int `json:"errno"`
	}
	q := url.Values{"shareid": {sh.ShareID}, "from": {sh.UK}, "bdstoken": {c.token}, "ondup": {"newcopy"}, "async": {"1"}}
	if err := c.call(ctx, "POST", "/share/transfer", q, url.Values{"fsidlist": {string(b)}, "path": {dest}}, &r); err != nil {
		return 0, err
	}
	return r.Errno, nil
}

// diskEntry: one row of /api/list.
type diskEntry struct {
	Path  string `json:"path"`
	Name  string `json:"server_filename"`
	IsDir int    `json:"isdir"`
	Size  int64  `json:"size"`
	MTime int64  `json:"server_mtime"`
}

// list reads a folder of the player's netdisk, every page of it.
func (c *BDClient) list(ctx context.Context, dir string) ([]diskEntry, error) {
	var out []diskEntry
	for start := 0; start < maxListing; start += 1000 {
		var r struct {
			Errno int         `json:"errno"`
			List  []diskEntry `json:"list"`
		}
		q := url.Values{"dir": {dir}, "start": {fmt.Sprint(start)}, "num": {"1000"}, "order": {"name"}}
		if err := c.call(ctx, "GET", "/api/list", q, nil, &r); err != nil {
			return nil, err
		}
		switch r.Errno {
		case 0:
		case -6:
			return nil, ErrBaiduLogin
		case -9:
			return nil, errors.New("网盘中没有该文件夹：" + dir)
		default:
			return nil, fmt.Errorf("无法读取网盘文件夹（百度网盘错误 %d）", r.Errno)
		}
		out = append(out, r.List...)
		if len(r.List) < 1000 {
			break
		}
	}
	return out, nil
}

// fetchTree fetches a folder (or a file) of the player's netdisk into local, keeping its folders.
func (c *BDClient) fetchTree(ctx context.Context, id int64, remote, local string) ([]string, error) {
	type one struct {
		remote, local string
		size          int64
	}
	var all []one
	var walk func(r, l string, depth int) error
	walk = func(r, l string, depth int) error {
		if depth > 12 {
			return nil
		}
		ents, err := c.list(ctx, r)
		if err != nil {
			return err
		}
		for _, e := range ents {
			name := core.SafeName(e.Name, 120)
			if e.IsDir == 1 {
				if err := walk(e.Path, filepath.Join(l, name), depth+1); err != nil {
					return err
				}
				continue
			}
			all = append(all, one{e.Path, filepath.Join(l, name), e.Size})
		}
		return nil
	}
	if err := walk(remote, local, 0); err != nil {
		return nil, err
	}
	var total int64
	for _, f := range all {
		total += f.size
	}
	setJob(id, func(j *PanJob) { j.Stage, j.Msg, j.Total, j.Done = "download", "", total, 0 })
	var done int64
	var out []string
	for _, f := range all {
		setJob(id, func(j *PanJob) { j.File = path.Base(f.remote) })
		got, err := c.fetchFile(ctx, id, f.remote, f.local, f.size, done)
		if err != nil {
			return out, err
		}
		done += f.size
		out = append(out, got)
	}
	return out, nil
}

// fetchFile fetches one file of the netdisk through the PCS, carrying on from a part already there.
func (c *BDClient) fetchFile(ctx context.Context, id int64, remote, local string, size, before int64) (string, error) {
	if err := os.MkdirAll(filepath.Dir(local), 0755); err != nil {
		return "", err
	}
	if fi, err := os.Stat(local); err == nil && fi.Size() == size {
		return local, nil // there already, whole (an earlier attempt that ended after it)
	}
	part := local + ".part"
	u := core.PanBase() + "/rest/2.0/pcs/file?" + url.Values{"method": {"download"}, "app_id": {"250528"}, "path": {remote}}.Encode()
	var last error
	for try := 0; try < dlTries; try++ {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		var at int64
		if fi, err := os.Stat(part); err == nil {
			at = fi.Size()
			if at > size {
				_ = os.Remove(part) // not this file's: begun again
				at = 0
			}
		}
		if at == size && size > 0 {
			break
		}
		var err error
		if c.segmented(size) && at == 0 {
			err = c.fetchSegments(ctx, id, u, part, size, before)
			if errors.Is(err, errNoSegments) {
				core.Logf("百度网盘不接受多连接下载 %s，改用单个连接", path.Base(remote))
				c.noSeg = true
				continue
			}
		} else {
			err = c.fetchRange(ctx, id, u, part, at, size, before)
		}
		if err == nil {
			last = nil
			break
		}
		last = err
		if errors.Is(err, ErrBaiduLogin) || ctx.Err() != nil {
			return "", err
		}
		time.Sleep(time.Duration(try+1) * 500 * time.Millisecond)
	}
	if last != nil {
		return "", fmt.Errorf("%s 下载失败：%v", path.Base(remote), last)
	}
	if fi, err := os.Stat(part); err != nil || fi.Size() != size {
		return "", fmt.Errorf("%s 下载不完整", path.Base(remote))
	}
	dst := archive.UniquePath(local)
	if err := os.Rename(part, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// pcsAsked counts the requests made of the PCS (what the tests look at to see a file carried on).
var pcsAsked sync.Map // remote path → requests

func (c *BDClient) fetchRange(ctx context.Context, id int64, u, part string, at, size, before int64) error {
	if pu, err := url.Parse(u); err == nil {
		n, _ := pcsAsked.LoadOrStore(pu.Query().Get("path"), new(int))
		*n.(*int)++
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", pcsUA)
	req.Header.Set("Cookie", cookieHeader(c.s.Cookies))
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-", at))
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 206:
	case 200:
		at = 0 // sent from the start
	case 401, 403:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if strings.Contains(string(b), "31045") || strings.Contains(string(b), "user not exists") {
			return ErrBaiduLogin
		}
		return fmt.Errorf("HTTP %d：%s", resp.StatusCode, strings.TrimSpace(string(b)))
	default:
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	flag := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if at == 0 {
		flag = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	}
	f, err := os.OpenFile(part, flag, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 256<<10)
	got, t0, tick := at, time.Now(), time.Now()
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
				return err
			}
			got += int64(n)
			if time.Since(tick) > 700*time.Millisecond {
				tick = time.Now()
				speed := int64(float64(got-at) / time.Since(t0).Seconds())
				setJob(id, func(j *PanJob) { j.Done, j.Speed = before+got, speed })
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	if got < size {
		return io.ErrUnexpectedEOF // the line dropped: carried on from here
	}
	setJob(id, func(j *PanJob) { j.Done = before + got })
	return nil
}

// ---------- the player's own netdisk ----------

func runDisk(ctx context.Context, st *core.Store, t *panTask) (string, []string, error) {
	s, err := loginWait(ctx, t)
	if err != nil {
		return "", nil, err
	}
	c := NewBDClient(st, s)
	dir, err := localDir(st, path.Base(t.disk), t.key)
	if err != nil {
		return "", nil, err
	}
	setJob(t.id, func(j *PanJob) { j.Stage, j.Login, j.Msg = "fetch", false, "正在读取网盘" })
	// a file: fetched alone; a folder: everything in it
	parent, err := c.list(ctx, path.Dir(t.disk))
	if err != nil {
		return "", nil, err
	}
	for _, e := range parent {
		if e.Path == t.disk && e.IsDir == 0 {
			setJob(t.id, func(j *PanJob) { j.Stage, j.Total, j.File = "download", e.Size, e.Name })
			got, err := c.fetchFile(ctx, t.id, e.Path, filepath.Join(dir, core.SafeName(e.Name, 120)), e.Size, 0)
			if err != nil {
				return "", nil, err
			}
			return dir, []string{got}, nil
		}
	}
	files, err := c.fetchTree(ctx, t.id, t.disk, dir)
	return dir, files, err
}

// ---------- Google Drive and Dropbox ----------

func runCloud(ctx context.Context, st *core.Store, t *panTask) (string, []string, error) {
	share, _, _ := strings.Cut(t.key, "#")
	st.Mu.RLock()
	u := st.User[share]
	link := u.ShareURL
	l := st.Pan[netdisk.ShareKey(share)]
	st.Mu.RUnlock()
	lk := cloudshare.Parse(link)
	if lk == nil {
		return "", nil, errors.New("不是 Google Drive 或 Dropbox 分享链接")
	}
	if l == nil || len(l.Files) == 0 {
		setJob(t.id, func(j *PanJob) { j.Stage, j.Msg = "fetch", "正在读取分享" })
		var err error
		if l, err = cloudshare.FetchListingCtx(ctx, st, link); err != nil {
			return "", nil, err
		}
	}
	dir, err := localDir(st, jobName(t.id), t.key)
	if err != nil {
		return "", nil, err
	}
	type one struct {
		f   *core.PanFile
		rel string
	}
	var all []one
	var walk func([]*core.PanFile, string)
	walk = func(fs []*core.PanFile, at string) {
		for _, f := range fs {
			p := at + "/" + f.Name
			if f.Dir {
				walk(f.Children, p)
				continue
			}
			if picked(t.paths, p) {
				all = append(all, one{f, p})
			}
		}
	}
	walk(l.Files, "")
	if len(all) == 0 {
		return "", nil, errors.New("分享中没有所选的文件")
	}
	var total int64
	for _, o := range all {
		total += o.f.Size
	}
	setJob(t.id, func(j *PanJob) { j.Stage, j.Msg, j.Total, j.Done = "download", "", total, 0 })
	c := cloudshare.Client(st)
	var out []string
	var done int64
	for _, o := range all {
		if ctx.Err() != nil {
			return "", nil, ctx.Err()
		}
		setJob(t.id, func(j *PanJob) { j.File = o.f.Name })
		local := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(o.rel, "/")))
		got, err := cloudFile(ctx, t.id, c, cloudshare.SourceFor(lk, o.f), local, done)
		if err != nil {
			return "", nil, err
		}
		if fi, err := os.Stat(got); err == nil {
			done += fi.Size()
		}
		out = append(out, got)
	}
	return dir, out, nil
}

// picked: a file of the share is among what was asked for (nothing asked: all of it).
func picked(paths []string, p string) bool {
	if len(paths) == 0 {
		return true
	}
	for _, q := range paths {
		if p == q || strings.HasPrefix(p, q+"/") {
			return true
		}
	}
	return false
}

func cloudFile(ctx context.Context, id int64, c *http.Client, src cloudshare.Source, local string, before int64) (string, error) {
	if err := os.MkdirAll(filepath.Dir(local), 0755); err != nil {
		return "", err
	}
	part := local + ".part"
	var last error
	for try := 0; try < dlTries; try++ {
		var at int64
		if fi, err := os.Stat(part); err == nil {
			at = fi.Size()
		}
		resp, err := src.Open(ctx, c, at, "")
		if err != nil {
			if cloudshare.ErrRange(err) {
				_ = os.Remove(part)
				continue
			}
			last = err
			if ctx.Err() != nil || !cloudshare.Temporary(err) {
				return "", err
			}
			time.Sleep(time.Duration(try+1) * 500 * time.Millisecond)
			continue
		}
		flag := os.O_CREATE | os.O_WRONLY | os.O_APPEND
		if resp.StatusCode == 200 {
			flag, at = os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0
		}
		f, err := os.OpenFile(part, flag, 0644)
		if err != nil {
			resp.Body.Close()
			return "", err
		}
		n, err := io.Copy(f, resp.Body)
		f.Close()
		resp.Body.Close()
		setJob(id, func(j *PanJob) { j.Done = before + at + n })
		if err == nil {
			dst := archive.UniquePath(local)
			if err := os.Rename(part, dst); err != nil {
				return "", err
			}
			return dst, nil
		}
		last = err
	}
	return "", fmt.Errorf("%s 下载失败：%v", filepath.Base(local), last)
}

// ---------- after the download ----------

// finish unpacks what came down (only these files) and writes down where the share now is. It answers the
// archives that could not be unpacked.
func finish(st *core.Store, t *panTask, dir string, files []string) []string {
	st.Mu.RLock()
	noExtract, keepZip := st.Settings.NoExtract, st.Settings.KeepZip
	st.Mu.RUnlock()
	var failed []string
	if !noExtract {
		setJob(t.id, func(j *PanJob) { j.Stage, j.Msg = "unpack", "正在解压" })
		remove := core.RecycleOnly
		if keepZip {
			remove = func([]string) error { return nil }
		}
		pwd := ""
		if t.imp != nil {
			pwd = t.imp.Pwd
		}
		res := archive.UnpackAll(files, pwd, remove, func(name string, i, n int) {
			setJob(t.id, func(j *PanJob) { j.Msg = "正在解压 " + name })
		})
		for f, why := range res.Failed {
			failed = append(failed, filepath.Base(f)+"："+why)
		}
	}
	if t.disk == "" {
		share, _, _ := strings.Cut(t.key, "#")
		st.Mu.Lock()
		if u := st.User[share]; u != nil {
			u.Downloaded, u.Updated = dir, time.Now().Unix()
			if len(t.paths) == 0 {
				u.PanGot = []string{"/"}
			} else {
				for _, p := range t.paths {
					if !core.ContainsStr(u.PanGot, p) {
						u.PanGot = append(u.PanGot, p)
					}
				}
			}
		}
		st.Mu.Unlock()
		_ = st.Save()
	}
	library.StartPipeline(st, true, true, false, false, nil)
	return failed
}

// importAfter: the folder becomes a card once scanned; that card is what is imported.
func importAfter(st *core.Store, dir string, req unity.ImportReq) {
	for i := 0; i < 600 && library.PipelineBusy(); i++ {
		time.Sleep(100 * time.Millisecond)
	}
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
		core.Logf("下载并导入：下载的文件夹中没有可导入的素材")
		return
	}
	req.Key = key
	if err := unity.StartImport(st, req); err != nil {
		core.Logf("下载并导入：%v", err)
	}
}

// ---------- stopping ----------

// CancelPanDownloads stops the downloads that are queued or running.
func CancelPanDownloads() {
	qMu.Lock()
	left := queue
	queue = nil
	if cancel != nil {
		cancel()
	}
	qMu.Unlock()
	for _, t := range left {
		core.Downloading.Add(-1)
		setJob(t.id, func(j *PanJob) { j.Stage, j.Msg, j.Ended = "cancelled", "已取消", time.Now().Unix() })
	}
}

// DismissPanJob: the window has shown this card's result; forget it.
func DismissPanJob(key string) {
	jobMu.Lock()
	var keep []PanJob
	for _, j := range jobs {
		if j.Key == key && over(j.Stage) {
			continue
		}
		keep = append(keep, j)
	}
	jobs = keep
	jobMu.Unlock()
	core.BumpRev()
}

// CancelAll stops everything on the way out (what was fetched is kept, and carried on from next time).
func CancelAll() { CancelPanDownloads() }
