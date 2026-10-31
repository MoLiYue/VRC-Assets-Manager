package pandl

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"vrclib/internal/testkit"
)

// pcsFake: the PCS for one file — ranges as asked ("bytes=a-z"), the first request of one range breaking
// off halfway, or no ranges at all.
type pcsFake struct {
	mu      sync.Mutex
	data    []byte
	ranges  []string
	noRange bool
	cutOnce map[string]bool
}

func (f *pcsFake) serve(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != pcsUA {
			w.WriteHeader(403)
			return
		}
		f.mu.Lock()
		rng := r.Header.Get("Range")
		f.ranges = append(f.ranges, rng)
		noRange := f.noRange
		cut := f.cutOnce[rng]
		delete(f.cutOnce, rng)
		f.mu.Unlock()
		if noRange || rng == "" {
			_, _ = w.Write(f.data)
			return
		}
		var a, z int64 = 0, int64(len(f.data)) - 1
		spec := strings.TrimPrefix(rng, "bytes=")
		x, y, _ := strings.Cut(spec, "-")
		fmt.Sscan(x, &a)
		if y != "" {
			fmt.Sscan(y, &z)
		}
		chunk := f.data[a : z+1]
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, z, len(f.data)))
		w.Header().Set("Content-Length", fmt.Sprint(len(chunk)))
		w.WriteHeader(206)
		if cut {
			_, _ = w.Write(chunk[:len(chunk)/2])
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					conn.Close()
				}
			}
			return
		}
		_, _ = w.Write(chunk)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("VRCLIB_PAN_BASE", srv.URL)
	return srv
}

func segClient(t *testing.T, vip int) *BDClient {
	st := testkit.NewStore(t)
	jobMu.Lock()
	jobs = []PanJob{{ID: 5, Key: "pan:1Seg", Stage: "download"}}
	jobMu.Unlock()
	t.Cleanup(func() { jobMu.Lock(); jobs = nil; jobMu.Unlock() })
	return NewBDClient(st, &BaiduSession{VIP: vip, Cookies: loggedIn})
}

// A 超级会员 account: a large file in six pieces side by side, one of which breaks off and is carried on;
// the pieces put together are the file, and none of them is left.
func TestSegmentedDownload(t *testing.T) {
	old := segMin
	segMin = 1000
	t.Cleanup(func() { segMin = old })
	data := bytes.Repeat([]byte("0123456789abcdef"), 4000) // 64000 bytes
	f := &pcsFake{data: data, cutOnce: map[string]bool{"bytes=21334-32000": true}}
	f.serve(t)
	c := segClient(t, 2)
	local := filepath.Join(t.TempDir(), "big.zip")
	got, err := c.fetchFile(context.Background(), 5, "/MioVRCA/x/big.zip", local, int64(len(data)), 0)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(got); !bytes.Equal(b, data) {
		t.Fatalf("the file put together is not the file (%d bytes)", len(b))
	}
	f.mu.Lock()
	n := len(f.ranges)
	f.mu.Unlock()
	if n != 7 { // six pieces, and the one that broke off once more
		t.Errorf("requests: %d %v", n, f.ranges)
	}
	if left, _ := filepath.Glob(local + "*.seg*"); len(left) > 0 {
		t.Errorf("pieces left: %v", left)
	}
	if j := PanJobsSnapshot()[0]; j.Done != int64(len(data)) {
		t.Errorf("progress: %d", j.Done)
	}
}

// Baidu gives no ranges: the pieces are given up and the file comes over one connection; a member that is
// not 超级会员, and a small file, use one connection from the start.
func TestSegmentedFallsBack(t *testing.T) {
	old := segMin
	segMin = 1000
	t.Cleanup(func() { segMin = old })
	data := bytes.Repeat([]byte("x"), 5000)
	f := &pcsFake{data: data, noRange: true}
	f.serve(t)
	c := segClient(t, 2)
	local := filepath.Join(t.TempDir(), "a.zip")
	got, err := c.fetchFile(context.Background(), 5, "/a.zip", local, int64(len(data)), 0)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(got); !bytes.Equal(b, data) || !c.noSeg {
		t.Errorf("one connection after the pieces were refused: %d bytes, noSeg %v", len(b), c.noSeg)
	}
	for _, c := range []struct {
		vip  int
		size int64
	}{{1, 5000}, {0, 5000}, {2, 999}} {
		if NewBDClient(nil, &BaiduSession{VIP: c.vip}).segmented(c.size) {
			t.Errorf("vip %d, %d bytes: in pieces", c.vip, c.size)
		}
	}
}
