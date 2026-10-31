// Package ai drives the automation pipeline: it dresses a model with what the library holds, builds the
// menu, has an AI carry on from there, saves what it did as a recipe that can be applied to another
// project, and renders the covers the cards show.
package ai

import (
	"io"
	"net/http"
	"os"
	"path/filepath"

	"vrclib/internal/core"
	"vrclib/internal/unity"
)

// ShotsDir: where the screenshots of the AI's steps are kept.
func ShotsDir() string { return filepath.Join(core.DataDir, "shots") }

// ServeShot answers with one of the screenshots the AI's steps left behind (/shot?f=…). Only a plain name
// is taken, and only a file that really is a picture is answered with: these are shown on the program's own
// pages.
func ServeShot(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(filepath.Clean("/" + r.URL.Query().Get("f")))
	if name == "." || name == "/" || name == "" {
		http.NotFound(w, r)
		return
	}
	p := filepath.Join(ShotsDir(), name)
	f, err := os.Open(p)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	ct := http.DetectContentType(head[:n])
	switch ct {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
	default:
		http.NotFound(w, r)
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "max-age=86400")
	fi, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, name, fi.ModTime(), f)
}

// ---------- covers rendered out of Unity ----------

// CoverStatus: what making a cover for a card out of a project amounts to.
type CoverStatus struct {
	Can       bool   `json:"can"`       // a project holds a prefab of it, so one can be made
	Real      bool   `json:"real"`      // the card already shows a picture of its own
	Generated bool   `json:"generated"` // the picture it shows was made this way
	Cover     string `json:"cover,omitempty"`
	Prefab    string `json:"prefab,omitempty"`
	Err       string `json:"err,omitempty"`
}

// CoverBatch: making covers for every card of a project that shows no picture.
type CoverBatch struct {
	Running bool   `json:"running"`
	Done    int    `json:"done"`
	Total   int    `json:"total"`
	Key     string `json:"key,omitempty"`
	Made    int    `json:"made"`
	Project string `json:"project,omitempty"`
	Err     string `json:"err,omitempty"`
}

// ---------- recipes that need the editor ----------

// PreviewRecipe: what applying it to this project would do.
func PreviewRecipe(st *core.Store, id, project string) (*RecipePreview, error) {
	r, err := LoadRecipe(id)
	if err != nil {
		return nil, err
	}
	pv := &RecipePreview{Recipe: r, Needs: RecipeNeeds(st, r), Ready: core.IsUnityProject(project) && unity.CheckupReady(project) == nil, Base: r.Base.Name}
	for _, n := range pv.Needs {
		if !n.Have {
			pv.Warning = "方案中有素材不在素材库中，应用时将跳过"
			break
		}
	}
	if !pv.Ready {
		pv.Warning = "请先在 Unity 中打开该工程并安装流水线插件"
	}
	return pv, nil
}
