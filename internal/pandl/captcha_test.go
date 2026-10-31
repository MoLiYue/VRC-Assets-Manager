package pandl

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"strings"
	"testing"
	"time"
)

// The download stops at "captcha" with the picture; the window's answer comes back, "换一张" too; an answer
// for a download that asks for none is refused.
func TestAnswerCaptcha(t *testing.T) {
	jobMu.Lock()
	jobs = []PanJob{{ID: 77, Key: "pan:1Cap", Stage: "fetch"}}
	jobMu.Unlock()
	t.Cleanup(func() { jobMu.Lock(); jobs = nil; jobMu.Unlock() })
	var pic bytes.Buffer
	_ = png.Encode(&pic, image.NewRGBA(image.Rect(0, 0, 4, 4)))
	if err := AnswerCaptcha("pan:1Cap", "abcd", false); err == nil {
		t.Error("an answer when none is asked for")
	}
	ask := asker(77, "pan:1Cap")
	for _, c := range []struct {
		refresh bool
		code    string
		want    string
	}{{true, "", ""}, {false, " abcd ", "abcd"}} {
		got := make(chan string, 1)
		go func() { s, _ := ask(context.Background(), pic.Bytes(), false); got <- s }()
		j := waitJob(t, "pan:1Cap", func(j PanJob) bool { return j.Stage == "captcha" })
		if !strings.HasPrefix(j.Captcha, "data:image/png;base64,") || j.CaptchaN == 0 {
			t.Errorf("the job: %+v", j)
		}
		if err := AnswerCaptcha("pan:1Cap", "", false); err == nil {
			t.Error("an empty answer was taken")
		}
		if err := AnswerCaptcha("pan:1Cap", c.code, c.refresh); err != nil {
			t.Fatal(err)
		}
		select {
		case s := <-got:
			if s != c.want {
				t.Errorf("answered %q, want %q", s, c.want)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("no answer")
		}
	}
}

// The player's own netdisk: a folder's entries, folders first; a folder that is not there any more falls
// back to the top; without a login, a login is asked for.
func TestListDisk(t *testing.T) {
	base := mockWeb(t)
	st := panStore(t, base, map[string][2]string{"pan:1PickShare": {"", "Kisekae"}})
	if _, _, err := ListDisk(st, "/"); err != ErrBaiduLogin {
		t.Errorf("without a login: %v", err)
	}
	_ = SaveBaiduSession(&BaiduSession{Cookies: loggedIn})
	if err := QueuePanDownload(st, "pan:1PickShare", nil, nil); err != nil { // puts the share into the netdisk
		t.Fatal(err)
	}
	waitJob(t, "pan:1PickShare", ended)
	ents, dir, err := ListDisk(st, "/MioVRCA")
	if err != nil || dir != "/MioVRCA" || len(ents) != 1 || !ents[0].Dir || ents[0].Name != "Kisekae" {
		t.Fatalf("/MioVRCA: %+v %q %v", ents, dir, err)
	}
	ents, _, _ = ListDisk(st, "/MioVRCA/Kisekae/Kisekae Set")
	var names []string
	for _, e := range ents {
		names = append(names, e.Name)
	}
	if strings.Join(names, ",") != "PSD,Dress_A.zip,Dress_B.zip,readme_set.txt" {
		t.Errorf("entries, folders first: %v", names)
	}
	if _, dir, err := ListDisk(st, "/gone/away"); err != nil || dir != "/" {
		t.Errorf("a folder that is gone: %q %v", dir, err)
	}
	// a file of it, down into the library
	if err := QueueDiskDownload(st, "/MioVRCA/Kisekae/Kisekae Set/readme_set.txt"); err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, "disk:/MioVRCA/Kisekae/Kisekae Set/readme_set.txt", ended); j.Stage != "done" || j.Files != 1 {
		t.Errorf("from the netdisk: %+v", j)
	}
}
