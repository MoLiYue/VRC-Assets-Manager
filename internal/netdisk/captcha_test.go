package netdisk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Baidu asks for a captcha before it takes the code (errno -62); a wrong reading gets -63 and another
// picture; the right one opens the share. Without anyone to ask, it says a captcha is wanted.
func TestBaiduShareCaptcha(t *testing.T) {
	var mu sync.Mutex
	pics, verifies := 0, 0
	var pic bytes.Buffer
	_ = png.Encode(&pic, image.NewRGBA(image.Rect(0, 0, 4, 4)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/s/1Cap":
			if c, _ := r.Cookie("BDCLND"); c == nil {
				http.Redirect(w, r, "/share/init?surl=Cap", 302)
				return
			}
			fmt.Fprint(w, `<script id="locals-data" type="application/json">{"share_uk":"1","shareid":2,"file_list":[{"server_filename":"a.zip","path":"/s/a.zip","isdir":0,"size":3,"fs_id":9}]}</script>`)
		case "/share/init":
			fmt.Fprint(w, "请输入提取码")
		case "/share/verify":
			verifies++
			_ = r.ParseForm()
			switch {
			case r.PostForm.Get("pwd") != "ab12":
				fmt.Fprint(w, `{"errno":-9}`)
			case r.PostForm.Get("vcode") == "":
				fmt.Fprint(w, `{"errno":-62}`)
			case r.PostForm.Get("vcode") != "RIGHT" || r.PostForm.Get("vcode_str") != fmt.Sprint("str", pics):
				fmt.Fprint(w, `{"errno":-63}`)
			default:
				http.SetCookie(w, &http.Cookie{Name: "BDCLND", Value: "ok", Path: "/"})
				fmt.Fprint(w, `{"errno":0,"randsk":"ok"}`)
			}
		case "/api/getcaptcha":
			pics++
			b, _ := json.Marshal(map[string]any{"errno": 0, "vcode_str": fmt.Sprint("str", pics), "vcode_img": "https://evil.example/x.png"})
			_, _ = w.Write(b)
		case "/genimage":
			_, _ = w.Write(pic.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("VRCLIB_PAN_BASE", srv.URL)
	c := &http.Client{Timeout: 10 * time.Second}
	ctx := context.Background()
	// no one to ask
	if _, err := FetchBaiduShare(ctx, c, "1Cap", "ab12"); !errors.Is(err, ErrShareCaptcha) {
		t.Fatalf("without asking: %v", err)
	}
	// the player: a wrong reading, then "看不清", then the right one
	var asked []string
	answers := []string{"WRONG", "", "RIGHT"}
	ask := func(_ context.Context, img []byte, bad bool) (string, error) {
		if http.DetectContentType(img) != "image/png" {
			t.Errorf("not the picture: %q", img)
		}
		a := answers[len(asked)]
		asked = append(asked, fmt.Sprintf("%v>%s", bad, a))
		return a, nil
	}
	sh, err := OpenBaiduShare(ctx, c, "1Cap", "ab12", nil, ask)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(asked); got != "[false>WRONG true>RIGHT]" && got != "[false>WRONG true> false>RIGHT]" {
		t.Errorf("asked: %v", asked)
	}
	if len(sh.Top()) != 1 || sh.Top()[0].Name != "a.zip" {
		t.Errorf("the share: %+v", sh.Top())
	}
}
