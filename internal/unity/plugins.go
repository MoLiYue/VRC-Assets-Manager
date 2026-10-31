package unity

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"vrclib/internal/core"
)

// UnityWant: the Unity version VRChat asks for, and the Hub link that installs it.
const (
	UnityWant     = "2022.3.22f1"
	unityWantHash = "887be4894c44"
)

// plugin: one package the new-project form offers.
type plugin struct {
	Pkg     string `json:"pkg"`
	Label   string `json:"label"`
	Note    string `json:"note,omitempty"`
	Repo    string `json:"-"`                 // the id of the VPM repository it comes from (vpmRepos)
	URL     string `json:"-"`                 // a git plugin: the repository Unity fetches
	Path    string `json:"-"`                 // the folder inside that repository, when the package is not at its root
	Default bool   `json:"default,omitempty"` // ticked to begin with
	Fixed   bool   `json:"fixed,omitempty"`   // the pipeline needs it: the tick cannot be taken off
	Git     bool   `json:"git,omitempty"`     // installed by its git link, not from a VPM repository
}

// basePlugins: the packages a new project gets from the VPM repositories. The SDK and Modular Avatar are
// what the pipeline cannot do without.
var basePlugins = []plugin{
	{Pkg: "com.vrchat.avatars", Label: "VRChat SDK - Avatars", Note: "上传模型必需",
		Repo: "com.vrchat.repos.official", Default: true, Fixed: true},
	{Pkg: "nadena.dev.modular-avatar", Label: "Modular Avatar", Note: "穿戴与菜单必需",
		Repo: "dev.nadena.vpm", Default: true, Fixed: true},
	{Pkg: "com.anatawa12.avatar-optimizer", Label: "AAO: Avatar Optimizer", Note: "降低性能评级",
		Repo: "com.anatawa12.vpm", Default: true},
	{Pkg: "jp.lilxyzw.liltoon", Label: "lilToon", Note: "常用卡通着色器",
		Repo: "io.github.lilxyzw.vpm", Default: true},
	{Pkg: "com.vrcfury.vrcfury", Label: "VRCFury", Note: "部分素材需要",
		Repo: "com.vrcfury.vcc"},
}

// vpmRepos: the VPM repositories, by id, and where their listings are. VRChat's own two are always read:
// the SDK is in the official one, and a plugin's dependencies may be in either.
var vpmRepos = map[string]string{
	"com.vrchat.repos.official": "https://packages.vrchat.com/official?download",
	"com.vrchat.repos.curated":  "https://packages.vrchat.com/curated?download",
	"dev.nadena.vpm":            "https://vpm.nadena.dev/vpm.json",
	"io.github.lilxyzw.vpm":     "https://lilxyzw.github.io/vpm-repos/vpm.json",
	"com.anatawa12.vpm":         "https://vpm.anatawa12.com/vpm.json",
	"com.vrcfury.vcc":           "https://vcc.vrcfury.com",
}

// gitPlugins: the packages Unity fetches itself, by the git link their README gives. UnitySkills is one of
// them: the VPM repositories do not carry it.
var gitPlugins = []plugin{
	{Pkg: SkillsPkg, Label: "UnitySkills", Note: "AI 操作 Unity 所需，由 Git 安装",
		URL: "https://github.com/Besty0728/Unity-Skills.git", Path: "/SkillsForUnity", Default: true, Git: true},
}

// isGitPlugin: this package is one Unity fetches by a git link.
func isGitPlugin(pkg string) bool {
	for _, p := range gitPlugins {
		if p.Pkg == pkg {
			return true
		}
	}
	return false
}

// pluginOf: the package of that name among the ones offered.
func pluginOf(pkg string) (plugin, bool) {
	for _, list := range [][]plugin{basePlugins, gitPlugins} {
		for _, p := range list {
			if p.Pkg == pkg {
				return p, true
			}
		}
	}
	return plugin{}, false
}

// gitReaches: whether this computer's git can reach a repository — git is installed and the repository
// answers. The second value says why not, in the words the note uses. Tests put their own in.
var gitReaches = func(ctx context.Context, repo string) (bool, string) {
	exe, err := exec.LookPath("git")
	if err != nil {
		return false, "本机未安装 Git"
	}
	c, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(c, exe, "ls-remote", "--exit-code", "-h", repo, "HEAD")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=echo")
	if out, err := cmd.CombinedOutput(); err != nil {
		core.Logf("git 无法连接 %s：%v %s", repo, err, strings.TrimSpace(string(out)))
		return false, "Git 无法连接 GitHub"
	}
	return true, ""
}

// unityModules: the modules a fresh VRChat project has in its Packages/manifest.json.
var unityModules = []string{
	"com.unity.ide.rider", "com.unity.ide.visualstudio", "com.unity.modules.animation",
	"com.unity.modules.audio", "com.unity.modules.imgui", "com.unity.modules.imageconversion",
	"com.unity.modules.jsonserialize", "com.unity.modules.particlesystem", "com.unity.modules.physics",
	"com.unity.modules.ui", "com.unity.modules.uielements", "com.unity.modules.umbra",
	"com.unity.modules.unitywebrequest", "com.unity.modules.unitywebrequesttexture",
}

// writeUnityManifest writes the Packages/manifest.json of a project that is being made.
func writeUnityManifest(p string) error {
	deps := map[string]string{}
	for _, m := range unityModules {
		deps[m] = "1.0.0"
	}
	b, err := json.MarshalIndent(map[string]any{"dependencies": deps}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(p, "Packages"), 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(p, "Packages", "manifest.json"), append(b, '\n'), 0644)
}

// manifestDep adds (or replaces) one dependency of Packages/manifest.json, leaving the rest as it is.
func manifestDep(p, pkg, version string) error {
	file := filepath.Join(p, "Packages", "manifest.json")
	var m map[string]any
	if b, err := os.ReadFile(file); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	if m == nil {
		m = map[string]any{}
	}
	deps, _ := m["dependencies"].(map[string]any)
	if deps == nil {
		deps = map[string]any{}
	}
	deps[pkg] = version
	m["dependencies"] = deps
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(file, append(b, '\n'), 0644)
}

// installSkills puts UnitySkills into a project being made and answers the note the window shows. By the
// git link of its README when this computer's git reaches GitHub — Unity fetches it when it opens the
// project, and it can be updated in the Package Manager afterwards; otherwise the copy the program carries,
// with the reason.
func installSkills(ctx context.Context, p string) string {
	pl, ok := pluginOf(SkillsPkg)
	if !ok {
		return ""
	}
	if reach, why := gitReaches(ctx, pl.URL); reach {
		url := pl.URL
		if pl.Path != "" {
			url += "?path=" + pl.Path
		}
		if err := manifestDep(p, SkillsPkg, url); err != nil {
			core.Logf("写入 UnitySkills 的 Git 链接失败：%v", err)
		} else {
			return "UnitySkills：通过 Git 安装，Unity 打开工程时下载，之后可在 Package Manager 中更新"
		}
	} else {
		core.Logf("UnitySkills 改用自带版本：%s", why)
		if err := putPackage(p, SkillsPkg, writeSkillsPackage); err != nil {
			return "UnitySkills 安装失败：" + core.TrimErr(err)
		}
		return "UnitySkills：" + why + "，已安装软件自带的 " + SkillsVersion
	}
	// the link could not be written: fall back to the copy the program carries
	if err := putPackage(p, SkillsPkg, writeSkillsPackage); err != nil {
		return "UnitySkills 安装失败：" + core.TrimErr(err)
	}
	return "UnitySkills：已安装软件自带的 " + SkillsVersion
}
