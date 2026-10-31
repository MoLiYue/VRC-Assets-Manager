package netdisk

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vrclib/internal/core"
	"vrclib/internal/testkit"
)

// mockWeb: test/mockweb.py, as the netdisk's address.
func mockWeb(t *testing.T) string {
	t.Helper()
	base := testkit.MockWeb(t, filepath.Join("..", ".."))
	t.Setenv("VRCLIB_PAN_BASE", base)
	return base
}

func names(fs []*core.PanFile) string {
	var out []string
	for _, f := range fs {
		s := f.Name
		if f.Dir {
			s += "/(" + names(f.Children) + ")"
		}
		out = append(out, s)
	}
	return strings.Join(out, " ")
}

// A share with an extraction code: opened with it, every folder listed with the files' sizes; refused with
// another code, or none; a share that is gone says so.
func TestFetchBaiduShare(t *testing.T) {
	mockWeb(t)
	c := &http.Client{Timeout: 20 * time.Second}
	ctx := context.Background()
	l, err := FetchBaiduShare(ctx, c, "1AbCdEfGhIjK", "x7k2")
	if err != nil {
		t.Fatal(err)
	}
	if got := names(l.Files); got != "Kaguya_v1.06/(Kaguya_v1.06.unitypackage Texture/(tex_0.png tex_1.png tex_2.png) README.txt)" {
		t.Errorf("listing: %s", got)
	}
	if l.Count != 5 || l.Size != 300623143+1000+1001+1002+1200 || l.Title != "Kaguya_v1.06" || l.Surl != "1AbCdEfGhIjK" {
		t.Errorf("count %d size %d title %q", l.Count, l.Size, l.Title)
	}
	// the code written as sellers write it, beside the link
	if _, err := FetchBaiduShare(ctx, c, "1AbCdEfGhIjK", SharePwdFromURL("https://pan.baidu.com/s/1AbCdEfGhIjK 提取码: x7k2")); err != nil {
		t.Errorf("the code from the text: %v", err)
	}
	if _, err := FetchBaiduShare(ctx, c, "1AbCdEfGhIjK", "zzzz"); !errors.Is(err, ErrSharePwd) {
		t.Errorf("another code: %v", err)
	}
	if _, err := FetchBaiduShare(ctx, c, "1AbCdEfGhIjK", ""); !errors.Is(err, ErrShareNeedPwd) {
		t.Errorf("no code: %v", err)
	}
	if _, err := FetchBaiduShare(ctx, c, "1NoSuchShare", ""); !errors.Is(err, ErrShareGone) {
		t.Errorf("a share that is gone: %v", err)
	}
	// a collection, a folder per category: split into its products by the analysis
	kg, err := FetchBaiduShare(ctx, c, "1KaguyaHeji", "kg88")
	if err != nil {
		t.Fatal(err)
	}
	parts := SplitPanCached(kg)
	if len(parts) < 6 {
		t.Errorf("the collection's products: %s", partNames(parts))
	}
	// as booth.AssetBooth asks: a collection's own number is its title's only (one of its products has one)
	info := AnalyzePan(kg, defs())
	info.Parts = nil
	if id := PanBoothID(kg, info); id != "" {
		t.Errorf("a collection is no one product: %q", id)
	}
}

// The layout a logged-in visitor gets: the share's data in locals.mset({…}).
func TestBaiduShareLoggedInLayout(t *testing.T) {
	page := `<html><script>locals.mset({"share_uk":"3967098798","shareid":46386138315,"bdstoken":"tok","file_list":[{"server_filename":"a.zip","path":"/sharelink1-x/a.zip","isdir":0,"size":12,"fs_id":1}]});</script></html>`
	m := reLocalsMset.FindStringSubmatch(page)
	if m == nil || !strings.Contains(m[1], `"a.zip"`) {
		t.Fatalf("not read: %v", m)
	}
}
