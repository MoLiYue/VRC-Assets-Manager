package unity

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"vrclib/internal/core"
	"vrclib/internal/semver"
)

// Installing VPM packages the way the Creator Companion does, without it.
//
// A repository is a JSON listing of packages, each with its versions; a version says where its zip is, the
// zip's SHA-256, which Unity it is made for, and the packages it needs (vpmDependencies, as version ranges).
// The newest version that fits this Unity and every range asked of it is taken, its dependencies likewise,
// and the zips are unpacked into Packages/. Packages/vpm-manifest.json then says what is there, which is
// what VCC and ALCOM read when they open the project afterwards.

// vpmVersion: one version of a package, as the listing writes it.
type vpmVersion struct {
	Name    string            `json:"name"`
	Version string            `json:"version"`
	URL     string            `json:"url"`
	SHA     string            `json:"zipSHA256"`
	Unity   string            `json:"unity"`
	Deps    map[string]string `json:"vpmDependencies"`
}

// vpmListing: a repository.
type vpmListing struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Packages map[string]struct {
		Versions map[string]vpmVersion `json:"versions"`
	} `json:"packages"`
}

// vpmBase: a stand-in for the repositories (tests): each listing is <base>/<id>.json.
func vpmBase() string { return strings.TrimRight(os.Getenv("VRCLIB_VPM_BASE"), "/") }

func repoURL(id string) string {
	if b := vpmBase(); b != "" {
		return b + "/" + id + ".json"
	}
	return vpmRepos[id]
}

// fetchListing reads a repository's listing.
func fetchListing(ctx context.Context, c *http.Client, id string) (*vpmListing, error) {
	u := repoURL(id)
	if u == "" {
		return nil, errors.New("未知的插件仓库：" + id)
	}
	b, err := httpGet(ctx, c, u, 32<<20)
	if err != nil {
		return nil, fmt.Errorf("无法读取插件仓库 %s：%v", id, err)
	}
	var l vpmListing
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, fmt.Errorf("插件仓库 %s 的内容无法读取", id)
	}
	return &l, nil
}

func httpGet(ctx context.Context, c *http.Client, u string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "VCC/2.4.0") // what the repositories expect of a client of theirs
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, max))
}

// fitsUnity: a version made for this Unity or an older one ("2022.3" fits 2022.3.22f1; "6000.0" does not).
func fitsUnity(made, have string) bool {
	if made == "" {
		return true
	}
	a, b := numsOf(made), numsOf(have)
	for i := 0; i < 2; i++ {
		x, y := 0, 0
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return x < y
		}
	}
	return true
}

// resolve works out which version of each package goes in: the ones asked for and everything they need.
func resolve(listings []*vpmListing, want []string, unityVer string) (map[string]vpmVersion, error) {
	all := map[string][]vpmVersion{}
	for _, l := range listings {
		for name, p := range l.Packages {
			for _, v := range p.Versions {
				if v.Name == "" {
					v.Name = name
				}
				all[name] = append(all[name], v)
			}
		}
	}
	ranges := map[string][]semver.VerRange{}
	picked := map[string]vpmVersion{}
	var add func(name, rng string, depth int) error
	add = func(name, rng string, depth int) error {
		if depth > 20 {
			return errors.New("插件依赖层级过深：" + name)
		}
		if rng != "" {
			r, err := semver.ParseRange(rng)
			if err != nil {
				return fmt.Errorf("%s 的版本要求无法读取：%s", name, rng)
			}
			ranges[name] = append(ranges[name], r)
		}
		vs := all[name]
		if len(vs) == 0 {
			return errors.New("插件仓库中没有 " + name)
		}
		var best *vpmVersion
		var bestV semver.Semver
		for i := range vs {
			sv, ok := semver.ParseSemver(vs[i].Version)
			if !ok || sv.Pre != "" || !fitsUnity(vs[i].Unity, unityVer) {
				continue // no prereleases in a new project, and nothing made for a later Unity
			}
			fits := true
			for _, r := range ranges[name] {
				if !r.Match(sv) {
					fits = false
					break
				}
			}
			if fits && (best == nil || sv.Cmp(bestV) > 0) {
				best, bestV = &vs[i], sv
			}
		}
		if best == nil {
			return fmt.Errorf("没有符合要求的 %s 版本", name)
		}
		if old, ok := picked[name]; ok && old.Version == best.Version {
			return nil
		}
		picked[name] = *best
		deps := make([]string, 0, len(best.Deps))
		for d := range best.Deps {
			deps = append(deps, d)
		}
		sort.Strings(deps)
		for _, d := range deps {
			if err := add(d, best.Deps[d], depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	for _, w := range want {
		if err := add(w, "", 0); err != nil {
			return nil, err
		}
	}
	return picked, nil
}

// installVPM fetches a package's zip, checks it, and unpacks it into Packages/<name>.
func installVPM(ctx context.Context, c *http.Client, project string, v vpmVersion) error {
	b, err := httpGet(ctx, c, v.URL, 1<<30)
	if err != nil {
		return fmt.Errorf("%s 下载失败：%v", v.Name, err)
	}
	if v.SHA != "" {
		sum := sha256.Sum256(b)
		if !strings.EqualFold(hex.EncodeToString(sum[:]), v.SHA) {
			return fmt.Errorf("%s 的文件校验不符，未安装", v.Name)
		}
	}
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return fmt.Errorf("%s 不是有效的 zip", v.Name)
	}
	// some zips carry everything in one folder ("GestureManager-3.9.9/package.json"): that folder is cut off
	top := ""
	for _, f := range zr.File {
		n := filepath.ToSlash(f.Name)
		if path.Base(n) == "package.json" && strings.Count(n, "/") <= 1 {
			top = path.Dir(n)
			if top == "." {
				top = ""
			}
			break
		}
	}
	dst := filepath.Join(project, "Packages", v.Name)
	_ = os.RemoveAll(dst)
	for _, f := range zr.File {
		n := filepath.ToSlash(f.Name)
		if top != "" {
			var ok bool
			if n, ok = strings.CutPrefix(n, top+"/"); !ok {
				continue
			}
		}
		if n == "" || strings.Contains(n, "..") || strings.HasPrefix(n, "/") {
			continue
		}
		out := filepath.Join(dst, filepath.FromSlash(n))
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(out, 0755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(out), 0755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		w, err := os.Create(out)
		if err == nil {
			_, err = io.Copy(w, rc)
			if cerr := w.Close(); err == nil {
				err = cerr
			}
		}
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// writeVPMManifest writes Packages/vpm-manifest.json for what went in.
func writeVPMManifest(project string, want []string, picked map[string]vpmVersion) error {
	deps, locked := map[string]any{}, map[string]any{}
	for _, w := range want {
		if v, ok := picked[w]; ok {
			deps[w] = map[string]string{"version": v.Version}
		}
	}
	for name, v := range picked {
		l := map[string]any{"version": v.Version}
		if len(v.Deps) > 0 {
			l["dependencies"] = v.Deps
		}
		locked[name] = l
	}
	b, err := json.MarshalIndent(map[string]any{"dependencies": deps, "locked": locked}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(project, "Packages", "vpm-manifest.json"), append(b, '\n'), 0644)
}

// fetchTimeout bounds one request to a repository.
const fetchTimeout = 2 * time.Minute

var _ = core.Logf
