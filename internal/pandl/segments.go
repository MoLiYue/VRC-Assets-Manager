package pandl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Several connections for one file, for a 超级会员 account.
//
// Baidu limits each connection, not only the account, and gives a 超级会员 account its speed only over
// several of them at once — its own client downloads that way too. So a large file is fetched as six
// pieces side by side, each into a file of its own beside the download (".seg0" … ".seg5") and each carried
// on by itself after a break; once all are whole they are put together. An account that is not 超级会员 is
// limited as a whole, and gets one connection: more would gain nothing.
//
// When Baidu will not give a piece (no 206 for its range, or turned away), the pieces are dropped and the
// file is fetched over one connection as before.

var (
	segMin   int64 = 32 << 20 // files this large and larger are fetched in pieces
	segCount       = 6
)

var errNoSegments = errors.New("百度不接受多连接下载")

// segmented: whether this file is fetched in pieces.
func (c *BDClient) segmented(size int64) bool {
	return c.s != nil && c.s.VIP == 2 && size >= segMin && !c.noSeg
}

// fetchSegments fetches the file at u into part (which it leaves whole), six pieces at a time.
func (c *BDClient) fetchSegments(ctx context.Context, id int64, u, part string, size, before int64) error {
	n := int64(segCount)
	step := (size + n - 1) / n
	type seg struct {
		file       string
		start, end int64 // the bytes it holds, end included
	}
	var segs []seg
	for i := int64(0); i < n; i++ {
		start := i * step
		if start >= size {
			break
		}
		segs = append(segs, seg{fmt.Sprintf("%s.seg%d", part, i), start, min(start+step, size) - 1})
	}
	var got atomic.Int64
	for _, s := range segs {
		if fi, err := os.Stat(s.file); err == nil {
			if have := fi.Size(); have > s.end-s.start+1 {
				_ = os.Remove(s.file) // not this piece: begun again
			} else {
				got.Add(have)
			}
		}
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	t0, at0 := time.Now(), got.Load()
	tick := make(chan struct{})
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick:
				return
			case <-time.After(700 * time.Millisecond):
				speed := int64(float64(got.Load()-at0) / time.Since(t0).Seconds())
				setJob(id, func(j *PanJob) { j.Done, j.Speed = before+got.Load(), speed })
			}
		}
	}()
	defer close(tick)
	var wg sync.WaitGroup
	errs := make([]error, len(segs))
	for i, s := range segs {
		wg.Add(1)
		go func(i int, s seg) {
			defer wg.Done()
			for try := 0; try < dlTries; try++ {
				err := c.fetchPiece(ctx, u, s.file, s.start, s.end, &got)
				if err == nil {
					errs[i] = nil
					return
				}
				errs[i] = err
				if errors.Is(err, errNoSegments) || errors.Is(err, ErrBaiduLogin) || ctx.Err() != nil {
					stop() // the others need not go on
					return
				}
				time.Sleep(time.Duration(try+1) * 500 * time.Millisecond)
			}
		}(i, s)
	}
	wg.Wait()
	// what ended it: one piece's reason is the others' cancelling, so the reason is told first
	var first error
	for _, err := range errs {
		switch {
		case err == nil:
		case errors.Is(err, errNoSegments):
			for _, s := range segs {
				_ = os.Remove(s.file)
			}
			return err
		case errors.Is(err, ErrBaiduLogin):
			return err
		case first == nil || errors.Is(first, context.Canceled):
			first = err
		}
	}
	if first != nil {
		return first
	}
	// all there: put together into the part file
	out, err := os.Create(part)
	if err != nil {
		return err
	}
	for _, s := range segs {
		f, err := os.Open(s.file)
		if err != nil {
			out.Close()
			return err
		}
		_, err = io.Copy(out, f)
		f.Close()
		if err != nil {
			out.Close()
			return err
		}
	}
	if err := out.Close(); err != nil {
		return err
	}
	for _, s := range segs {
		_ = os.Remove(s.file)
	}
	setJob(id, func(j *PanJob) { j.Done = before + size })
	return nil
}

// fetchPiece fetches bytes start…end into file, carrying on from what it holds.
func (c *BDClient) fetchPiece(ctx context.Context, u, file string, start, end int64, got *atomic.Int64) error {
	have := int64(0)
	if fi, err := os.Stat(file); err == nil {
		have = fi.Size()
	}
	if start+have > end {
		return nil // whole already
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", pcsUA)
	req.Header.Set("Cookie", cookieHeader(c.s.Cookies))
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start+have, end))
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 206:
		if !rangeFrom(resp.Header.Get("Content-Range"), start+have) {
			return errNoSegments // another piece than the one asked for
		}
	case 200, 416:
		return errNoSegments // the whole file, or no ranges at all
	case 401, 403:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if strings.Contains(string(b), "31045") || strings.Contains(string(b), "user not exists") {
			return ErrBaiduLogin
		}
		return errNoSegments // turned away: this account, or this many connections, is not let in
	default:
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 128<<10)
	want := end - start - have + 1
	for want > 0 {
		k, rerr := resp.Body.Read(buf[:min(int64(len(buf)), want)])
		if k > 0 {
			if _, err := f.Write(buf[:k]); err != nil {
				return err
			}
			want -= int64(k)
			got.Add(int64(k))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	if want > 0 {
		return io.ErrUnexpectedEOF // the line dropped: carried on from here
	}
	return nil
}

// rangeFrom: a Content-Range header ("bytes 100-199/1000") begins at this byte.
func rangeFrom(h string, at int64) bool {
	h = strings.TrimPrefix(strings.TrimSpace(h), "bytes ")
	a, _, ok := strings.Cut(h, "-")
	if !ok {
		return false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(a), 10, 64)
	return err == nil && n == at
}
