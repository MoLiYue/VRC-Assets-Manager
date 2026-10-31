package testkit

import (
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// MockWeb starts test/mockweb.py — the project's stand-in for Baidu's pages and other services, shaped after
// the live ones — and answers its address. Without Python and Pillow the test is left out.
//
// The port is picked here and handed over, so another package's stand-in started at the same moment may
// take it first: then this one cannot listen and ends at once, while the other one answers on that port. So
// it counts as up only while this very process runs, and is started again on another port when it ended.
func MockWeb(t *testing.T, root string) string {
	t.Helper()
	return startPy(t, root, "mockweb.py", true, "/disk/main")
}

// MockLLM starts test/mockllm.py — the project's stand-in for an OpenAI- and Anthropic-compatible service —
// and answers its address. Its key is "sk-good-key-1234"; GET /__faults?set=429,500,cut,context sets what the
// next requests fail with.
func MockLLM(t *testing.T, root string) string {
	t.Helper()
	return startPy(t, root, "mockllm.py", false, "/__faults")
}

// startPy starts one of the test/ stand-ins on a port of its own (as an argument, or in MOCK_PORT), and
// answers once this very process answers probe.
func startPy(t *testing.T, root, name string, portArg bool, probe string) string {
	t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	if exec.Command(py, "-c", "import PIL").Run() != nil {
		t.Skip("Pillow not installed")
	}
	script, _ := filepath.Abs(filepath.Join(root, "test", name))
	for try := 0; try < 20; try++ {
		// a port below the system's range for ports given out on request (Linux: 32768 and up, Windows:
		// 49152 and up): the test servers of other packages (httptest, port 0) are never given it, so nothing
		// of theirs can answer here — or be sent this one's probe — while the stand-in starts
		port := 20000 + rand.Intn(10000)
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue // taken
		}
		ln.Close()
		cmd := exec.Command(py, script)
		if portArg {
			cmd.Args = append(cmd.Args, fmt.Sprint(port))
		} else {
			cmd.Env = append(os.Environ(), fmt.Sprintf("MOCK_PORT=%d", port))
		}
		cmd.Dir = filepath.Dir(script)
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		exited := make(chan struct{})
		go func() { _ = cmd.Wait(); close(exited) }()
		base := fmt.Sprintf("http://127.0.0.1:%d", port)
		up := false
		for i := 0; i < 100 && !up; i++ {
			select {
			case <-exited:
				i = 100 // it could not listen: another port
				continue
			case <-time.After(50 * time.Millisecond):
			}
			if resp, err := http.Get(base + probe); err == nil {
				resp.Body.Close()
				up = true
			}
		}
		if up {
			// answered — by this process, if it is still running a moment later
			select {
			case <-exited:
				up = false
			case <-time.After(150 * time.Millisecond):
			}
		}
		if !up {
			_ = cmd.Process.Kill()
			<-exited
			continue
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); <-exited })
		return base
	}
	t.Fatal(name + " did not come up")
	return ""
}
