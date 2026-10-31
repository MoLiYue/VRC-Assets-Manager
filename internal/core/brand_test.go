package core

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This program is MioVRCA new, a modified version of MioVRCA: none of the original's own channels (its
// website, groups, author's mail, the update repository and registry keys it used) may come back into the
// program, the installer or the manual. The attribution to MioVRCA and its project address stay (LICENSE.md).
func TestNoUpstreamChannels(t *testing.T) {
	root, _ := filepath.Abs("../..")
	banned := []string{"miovrc.com", "qm.qq.com", "discord.gg", "Coko_Iya@", "VRCAssetLibrary", "vrchat.com/home/user", "反馈与建议", "formsubmit.co"}
	skipDir := map[string]bool{".git": true, "third_party": true, ".kiro": true, "dist": true, "unitykit": true}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDir[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		switch {
		case rel == "CHANGELOG-upstream.md", rel == "CHANGELOG.md", strings.HasSuffix(rel, "brand_test.go"):
			return nil // the history, and this list
		}
		switch strings.ToLower(filepath.Ext(p)) {
		case ".go", ".js", ".html", ".css", ".txt", ".md", ".nsi", ".rc", ".manifest", ".json", ".sh", ".bat", ".yml", ".cs":
		default:
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		for _, w := range banned {
			if strings.Contains(string(b), w) {
				t.Errorf("%s: %q", rel, w)
			}
		}
		return nil
	})
	if AppName == "MioVRCA" || AppID == "MioVRC_AssetManager" {
		t.Error("the original's own name")
	}
}
