package ai

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"vrclib/internal/core"
	"vrclib/internal/library"
	"vrclib/internal/unity"
)

// Covers made out of Unity.
//
// An asset that has no picture of its own, but is in a project, can have one rendered there: the plugin
// lists the prefabs in the asset's folder of the project ("prefabs"), the one with the most renderers is
// taken (the whole outfit, not one of its parts), and the plugin renders it ("prefab_shot") into a file of
// the project's Temp folder, which is copied into the data folder.

// projectOf: the project an asset is used in, and its folder there. The store is held by the caller.
func projectOf(st *core.Store, a *core.Asset) (project, folder string) {
	for _, u := range a.Usage {
		if u.Folder == "" {
			continue
		}
		for _, p := range st.Projects {
			if p.Name == u.Project || core.PathKey(p.Path) == core.PathKey(u.Project) {
				return p.Path, u.Folder
			}
		}
	}
	return "", ""
}

func assetOf(st *core.Store, key string) *core.Asset {
	for _, a := range st.Assets {
		if a.Key == key || (a.AltKey != "" && a.AltKey == key) {
			return a
		}
	}
	return nil
}

// realCover: the card shows a picture that was not made here — one the player picked, the Booth
// product's, or one lying in the asset's folder. The store is held by the caller.
func realCover(st *core.Store, a *core.Asset) bool {
	if u := st.User[a.Key]; u != nil && u.Cover != "" {
		return true
	}
	if b := st.Booth[a.BoothID]; a.BoothID != "" && b != nil && b.Cover != "" {
		return true
	}
	return len(a.Covers) > 0
}

// CoverStatusOf: what a card's cover can be made of.
func CoverStatusOf(st *core.Store, key string) CoverStatus {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	a := assetOf(st, key)
	if a == nil {
		return CoverStatus{}
	}
	s := CoverStatus{Real: realCover(st, a)}
	if p := library.GeneratedCover(key); p != "" {
		s.Generated, s.Cover = true, p
	}
	if prj, _ := projectOf(st, a); prj != "" {
		s.Can = true
	}
	return s
}

// GenerateCover renders a cover for a card in Unity and answers which prefab it used (prefab "": the one
// with the most renderers in the asset's folder).
func GenerateCover(ctx context.Context, st *core.Store, key, prefab string) (string, error) {
	st.Mu.RLock()
	a := assetOf(st, key)
	var prj, folder string
	if a != nil {
		prj, folder = projectOf(st, a)
	}
	st.Mu.RUnlock()
	switch {
	case a == nil:
		return "", errors.New("未找到该素材")
	case prj == "":
		return "", errors.New("该素材未在任何工程中使用，无法从 Unity 生成封面")
	}
	if err := unity.CheckupReady(prj); err != nil {
		return "", err
	}
	if prefab == "" {
		raw, err := unity.BridgeCall(ctx, prj, "prefabs", map[string]any{"folder": folder}, 60*time.Second)
		if err != nil {
			return "", err
		}
		var r struct {
			Prefabs []struct {
				Path      string `json:"path"`
				Renderers int    `json:"renderers"`
			} `json:"prefabs"`
		}
		if json.Unmarshal(raw, &r) != nil || len(r.Prefabs) == 0 {
			return "", errors.New("在该素材的文件夹中没有找到预制体")
		}
		best := 0
		for i, p := range r.Prefabs {
			if p.Renderers > r.Prefabs[best].Renderers {
				best = i
			}
		}
		prefab = r.Prefabs[best].Path
	}
	raw, err := unity.BridgeCall(ctx, prj, "prefab_shot", map[string]any{"prefabs": []string{prefab}, "size": 512}, 120*time.Second)
	if err != nil {
		return "", err
	}
	var shot struct {
		Shots []struct {
			Prefab string `json:"prefab"`
			File   string `json:"file"`
		} `json:"shots"`
	}
	if json.Unmarshal(raw, &shot) != nil || len(shot.Shots) == 0 || shot.Shots[0].File == "" {
		return "", errors.New("Unity 未能生成封面")
	}
	// only a picture the plugin wrote inside the project is taken
	file := filepath.Clean(shot.Shots[0].File)
	if rel, err := filepath.Rel(prj, file); err != nil || rel == "." || filepath.IsAbs(rel) || len(rel) > 1 && rel[:2] == ".." {
		return "", errors.New("Unity 返回的封面位置无效")
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", errors.New("无法读取 Unity 生成的封面：" + core.TrimErr(err))
	}
	if _, err := library.SaveGeneratedCover(key, b); err != nil {
		return "", err
	}
	_ = os.Remove(file)
	core.BumpRev()
	core.Logf("已从 Unity 生成封面：%s（%s）", key, prefab)
	return prefab, nil
}

// ---------- every cover-less asset of a project ----------

var batch struct {
	mu   sync.Mutex
	st   CoverBatch
	stop atomic.Bool
}

// StartCoverBatch begins making covers for every asset of a project that shows no picture.
func StartCoverBatch(st *core.Store, project string) error {
	st.Mu.RLock()
	var keys []string
	for _, a := range st.Assets {
		if prj, _ := projectOf(st, a); prj == "" || core.PathKey(prj) != core.PathKey(project) {
			continue
		}
		if realCover(st, a) || library.GeneratedCover(a.Key) != "" {
			continue
		}
		keys = append(keys, a.Key)
	}
	st.Mu.RUnlock()
	if len(keys) == 0 {
		return errors.New("该工程中的素材都已有封面")
	}
	batch.mu.Lock()
	if batch.st.Running {
		batch.mu.Unlock()
		return errors.New("正在生成封面，请稍候")
	}
	batch.st = CoverBatch{Running: true, Total: len(keys), Project: project}
	batch.stop.Store(false)
	batch.mu.Unlock()
	core.BumpRev()
	go func() {
		made := 0
		var last error
		for i, k := range keys {
			if batch.stop.Load() || core.Quitting.Load() {
				break
			}
			batch.mu.Lock()
			batch.st.Done, batch.st.Key = i, k
			batch.mu.Unlock()
			core.BumpRev()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			_, err := GenerateCover(ctx, st, k, "")
			cancel()
			if err != nil {
				last = err
				core.Logf("生成封面失败 %s：%v", k, err)
				continue
			}
			made++
		}
		batch.mu.Lock()
		batch.st.Running, batch.st.Done, batch.st.Made, batch.st.Key = false, len(keys), made, ""
		if made == 0 && last != nil {
			batch.st.Err = core.TrimErr(last)
		}
		batch.mu.Unlock()
		core.BumpRev()
	}()
	return nil
}

// CoverBatchSnapshot: how that is going.
func CoverBatchSnapshot() CoverBatch {
	batch.mu.Lock()
	defer batch.mu.Unlock()
	return batch.st
}

// CancelCoverBatch stops it.
func CancelCoverBatch() { batch.stop.Store(true) }
