package ai

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"vrclib/internal/core"
	"vrclib/internal/naming"
)

// Recipes (装配方案).
//
// What a pipeline run did, written down so that it can be applied to another project or another model, and
// shared as a file. The file is the contract — it is what another player's program reads — so the shapes
// here are the file's, not the program's: format, schema, the base body, the assets with the prefab each
// was put in the scene from, and the menu that was built.
//
// The id is this computer's own: it names the file in the data folder and is left out of what is exported.

const (
	recipeFormat = "miovrca-recipe"
	recipeSchema = 1
	recipeExt    = ".miovrca-recipe.json"
)

// RecipeBase: the model a recipe was made on.
type RecipeBase struct {
	Name  string `json:"name,omitempty"`
	Key   string `json:"key,omitempty"` // the library card, when it was known
	Booth string `json:"boothId,omitempty"`
	// the whole model's prefab, when the run put it in the scene: applied to a scene without one, it is put
	// in first
	Prefab string `json:"prefab,omitempty"`
}

// RecipeAsset: one asset the recipe puts on the model.
type RecipeAsset struct {
	Name   string   `json:"name"`
	Booth  string   `json:"boothId,omitempty"`
	Key    string   `json:"key,omitempty"`
	Prefab string   `json:"prefab,omitempty"` // where it was taken from in the project
	Kind   string   `json:"kind,omitempty"`   // 衣服, 头发 …
	Object string   `json:"object,omitempty"` // what it is called in the scene
	Parts  []string `json:"parts,omitempty"`
	Colors []string `json:"colors,omitempty"`
}

// RecipeMenuItem: one control of the menu that was built.
type RecipeMenuItem struct {
	Kind    string   `json:"kind,omitempty"` // outfit, toggle, colour …
	Path    []string `json:"path,omitempty"` // where it sits in the menu
	Label   string   `json:"label,omitempty"`
	Objects []string `json:"objects,omitempty"`
	// as build_menu takes them
	Default       bool   `json:"default,omitempty"`
	Parameter     string `json:"parameter,omitempty"`
	MaterialsFrom string `json:"materialsFrom,omitempty"` // a skin: the prefab its materials are taken from
}

// RecipeMenu: a page of the menu.
type RecipeMenu struct {
	Label string           `json:"label,omitempty"`
	Items []RecipeMenuItem `json:"items,omitempty"`
}

// Recipe: a saved run, as the file holds it.
type Recipe struct {
	Format   string        `json:"format"`
	Schema   int           `json:"schema"`
	ID       string        `json:"id,omitempty"` // this computer's own name for it (not exported)
	Name     string        `json:"name"`
	Note     string        `json:"note,omitempty"`
	Base     RecipeBase    `json:"base"`
	Assets   []RecipeAsset `json:"assets,omitempty"`
	Menus    []RecipeMenu  `json:"menus,omitempty"`
	Made     int64         `json:"made,omitempty"`
	Project  string        `json:"project,omitempty"` // the project it was made in (not exported)
	Imported bool          `json:"imported,omitempty"`
}

// RecipeNeed: an asset a recipe wants.
type RecipeNeed struct {
	Name  string `json:"name"`
	Kind  string `json:"kind,omitempty"`
	Key   string `json:"key,omitempty"`
	Have  bool   `json:"have"`  // it is in the library
	InPrj bool   `json:"inPrj"` // and already in the project
	Link  string `json:"link,omitempty"`
}

// RecipePreview: what applying a recipe to a project would do.
type RecipePreview struct {
	Recipe  *Recipe      `json:"recipe,omitempty"`
	Needs   []RecipeNeed `json:"needs,omitempty"`
	Ready   bool         `json:"ready"`
	Base    string       `json:"base,omitempty"`
	Warning string       `json:"warning,omitempty"`
}

func recipeDir() string { return filepath.Join(core.DataDir, "recipes") }

// okAssetPath: a path a recipe names in a project — under Assets, with nothing that leads out of it.
func okAssetPath(p string) error {
	if p == "" {
		return nil
	}
	s := strings.ReplaceAll(p, `\`, "/")
	if s != path.Clean(s) || strings.HasPrefix(s, "/") || strings.Contains(s, "..") {
		return errors.New("方案中的路径无效：" + p)
	}
	if !strings.HasPrefix(s, "Assets/") {
		return errors.New("方案中的路径不在工程的 Assets 中：" + p)
	}
	return nil
}

// recipeFile: where a recipe of this id is kept. It refuses an id that is not one of ours, so that nothing
// can be read or written outside the folder.
func recipeFile(id string) (string, error) {
	if id == "" || len(id) > 64 || strings.ContainsAny(id, `/\.:`) {
		return "", errors.New("方案不存在")
	}
	return filepath.Join(recipeDir(), id+".json"), nil
}

func newRecipeID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ListRecipes: the recipes saved on this computer, the newest first.
func ListRecipes() []Recipe {
	out := []Recipe{}
	ents, err := os.ReadDir(recipeDir())
	if err != nil {
		return out
	}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if r, err := readRecipe(filepath.Join(recipeDir(), e.Name())); err == nil {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Made > out[j].Made })
	return out
}

func readRecipe(p string) (*Recipe, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var r Recipe
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	if r.ID == "" {
		r.ID = strings.TrimSuffix(filepath.Base(p), ".json")
	}
	return &r, nil
}

// LoadRecipe: one of them.
func LoadRecipe(id string) (*Recipe, error) {
	p, err := recipeFile(id)
	if err != nil {
		return nil, err
	}
	r, err := readRecipe(p)
	if err != nil {
		return nil, errors.New("方案不存在")
	}
	return r, nil
}

func writeRecipe(r *Recipe) error {
	p, err := recipeFile(r.ID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(recipeDir(), 0755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(b, '\n'), 0644)
}

// RenameRecipe gives one another name and note.
func RenameRecipe(id, name, note string) (*Recipe, error) {
	r, err := LoadRecipe(id)
	if err != nil {
		return nil, err
	}
	if n := strings.TrimSpace(name); n != "" {
		r.Name = n
	}
	r.Note = strings.TrimSpace(note)
	if err := writeRecipe(r); err != nil {
		return nil, err
	}
	core.BumpRev()
	return r, nil
}

// DeleteRecipe drops one.
func DeleteRecipe(id string) error {
	p, err := recipeFile(id)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		return errors.New("方案不存在")
	}
	core.BumpRev()
	return nil
}

// ExportRecipe writes one into dir as a file to share, and answers the file's path. What this computer
// calls it by (the id, the project it was made in) is left out.
func ExportRecipe(id, dir string) (string, error) {
	r, err := LoadRecipe(id)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(dir) == "" {
		return "", errors.New("请选择保存位置")
	}
	if !filepath.IsAbs(dir) {
		return "", errors.New("保存位置无效")
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return "", errors.New("保存位置不存在：" + dir)
	}
	out := *r
	out.ID, out.Project, out.Imported = "", "", false
	out.Format, out.Schema = recipeFormat, recipeSchema
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	name := core.SafeName(out.Name, 60)
	if name == "" {
		name = "recipe"
	}
	p := uniqueFile(filepath.Join(dir, name+recipeExt))
	if err := os.WriteFile(p, append(b, '\n'), 0644); err != nil {
		return "", err
	}
	return p, nil
}

func uniqueFile(p string) string {
	if !core.StatOK(p) {
		return p
	}
	base := strings.TrimSuffix(p, recipeExt)
	for i := 2; ; i++ {
		q := base + " (" + itoa(i) + ")" + recipeExt
		if !core.StatOK(q) {
			return q
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// ImportRecipe reads a recipe file (save: keep it, instead of only looking at it).
func ImportRecipe(data []byte, save bool) (*Recipe, error) {
	var r Recipe
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, errors.New("这不是一个方案文件（无法解析）")
	}
	if r.Format != recipeFormat {
		return nil, errors.New("这不是 MioVRCA 的方案文件")
	}
	if r.Schema > recipeSchema {
		return nil, errors.New("该方案由更新版本的软件导出，请先更新软件")
	}
	if strings.TrimSpace(r.Name) == "" {
		r.Name = "未命名方案"
	}
	// every path in it has to be one inside a project's Assets folder: a file from somebody else is not
	// going to have this program reach anywhere else on this computer
	for _, a := range r.Assets {
		if err := okAssetPath(a.Prefab); err != nil {
			return nil, err
		}
	}
	if err := okAssetPath(r.Base.Prefab); err != nil {
		return nil, err
	}
	for _, m := range r.Menus {
		for _, it := range m.Items {
			if err := okAssetPath(it.MaterialsFrom); err != nil {
				return nil, err
			}
		}
	}
	r.ID, r.Imported, r.Project = newRecipeID(), true, ""
	if r.Made == 0 {
		r.Made = time.Now().Unix()
	}
	if !save {
		r.ID = "" // only looked at: nothing is kept, and it has no name here
		return &r, nil
	}
	if err := writeRecipe(&r); err != nil {
		return nil, err
	}
	core.BumpRev()
	return &r, nil
}

// RecipeNeeds: the assets a recipe wants, and whether this library has them.
func RecipeNeeds(st *core.Store, r *Recipe) []RecipeNeed {
	out := []RecipeNeed{}
	if r == nil {
		return out
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	for _, a := range r.Assets {
		n := RecipeNeed{Name: a.Name, Kind: a.Kind, Key: a.Key}
		for _, have := range st.Assets {
			if (a.Key != "" && have.Key == a.Key) || (a.Booth != "" && have.BoothID == a.Booth) ||
				naming.NormKey(have.Name) == naming.NormKey(a.Name) {
				n.Have, n.Key = true, have.Key
				for _, u := range have.Usage {
					if u.Status == "used" {
						n.InPrj = true
					}
				}
				break
			}
		}
		if !n.Have && a.Booth != "" {
			n.Link = core.BoothWebBase() + "/ja/items/" + a.Booth
		}
		out = append(out, n)
	}
	return out
}
