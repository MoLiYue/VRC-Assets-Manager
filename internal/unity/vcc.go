package unity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
)

// vccDir: where the VRChat Creator Companion keeps its settings — and, for ALCOM, where vrc-get's own
// gui-config.json sits next to them. VRCLIB_VCCDIR replaces it in tests.
func vccDir() string {
	if d := os.Getenv("VRCLIB_VCCDIR"); d != "" {
		return d
	}
	if runtime.GOOS != "windows" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		return filepath.Join(home, ".local", "share", "VRChatCreatorCompanion")
	}
	if d := os.Getenv("LOCALAPPDATA"); d != "" {
		return filepath.Join(d, "VRChatCreatorCompanion")
	}
	return ""
}

// vccSetting: one string out of VCC's settings.json ("pathToUnityHub", "pathToUnityExe" …), "" when the
// file is not there, cannot be read, or does not carry that key.
func vccSetting(key string) string {
	d := vccDir()
	if d == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(d, "settings.json"))
	if err != nil {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}
