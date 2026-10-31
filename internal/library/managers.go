package library

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"vrclib/internal/core"
)

// Taking over what another asset manager recorded: Avatar Explorer and KonoAsset.
//
// Both keep, for each asset, the Booth item, a category, the base bodies it fits, tags, a memo and a
// picture. Those are added to this library's cards for the same folders — only where the card has nothing
// of its own yet, and without writing anything into the other program's data. An asset of theirs that lies
// outside this library's asset folders is left alone; when their own folder of assets is not one of ours,
// the player may add it (addRoot), and the scan that follows takes the rest over.
//
// The formats, as their sources (MIT) have them:
//
//	Avatar Explorer V1 (puk06/Avatar-Explorer): <program>/Datas/ItemsData.json, an array of items in
//	  PascalCase — Title, BoothId (-1: none), ItemPath (relative to the program's folder, or absolute),
//	  ImagePath, Type (Avatar, Clothing, Texture …, or a number), CustomCategory, SupportedAvatar (the
//	  ItemPath of each base body's own item), Tags, ItemMemo. Its assets lie in Datas/Items.
//	Avatar Explorer V2: <data>/database/items.json. Its source is not public; it is read as the same
//	  items, in whichever case the names are written.
//	KonoAsset (siloneco/KonoAsset): <data>/metadata/{avatars,avatarWearables,worldObjects,otherAssets}.json,
//	  each {"version":3,"data":[…]} (older ones: version 2, or the bare array) of {id, description{name,
//	  creator, imageFilename, tags, memo, boothItemId}, category, supportedAvatars (base bodies by name)};
//	  an asset's files are in <data>/data/<id>, its picture in <data>/images/<imageFilename>.

// aeItem: one item, as both managers come down to.
type aeItem struct {
	Name     string
	Dir      string // where its files are
	Booth    string
	Category string // ours
	Bases    []string
	Tags     []string
	Memo     string
	Image    string
}

// ---------- Avatar Explorer ----------

// aeTypes: Avatar Explorer's ItemType, in its order, as our categories.
var aeTypes = []string{"素体", "衣服", "材质", "插件", "配饰", "头发", "动作", "插件", "材质", "", "其他"}
var aeTypeNames = map[string]int{"avatar": 0, "clothing": 1, "texture": 2, "gimmick": 3, "accessory": 4, "hairstyle": 5,
	"animation": 6, "tool": 7, "shader": 8, "custom": 9, "unknown": 10}

// aeFile: where an Avatar Explorer folder keeps its items, and which version it is.
func aeFile(dir string) (string, int) {
	for _, c := range []struct {
		rel string
		ver int
	}{{filepath.Join("Datas", "ItemsData.json"), 1}, {"ItemsData.json", 1}, {filepath.Join("database", "items.json"), 2}} {
		if p := filepath.Join(dir, c.rel); core.StatOK(p) {
			return p, c.ver
		}
	}
	return "", 0
}

// AEFolders: where Avatar Explorer keeps its data, when it is to be found — in the asset folders (it is
// often unpacked into one) and where V2 keeps its data.
func AEFolders() []string {
	var out []string
	add := func(d string) {
		if p, _ := aeFile(d); p != "" && !core.ContainsStr(out, d) {
			out = append(out, d)
		}
	}
	for _, d := range managerCandidates([]string{"Avatar Explorer", "AvatarExplorer", "Avatar-Explorer"}) {
		add(d)
	}
	return out
}

// readAE reads an Avatar Explorer folder's items.
func readAE(dir string) ([]aeItem, int, error) {
	file, ver := aeFile(dir)
	if file == "" {
		return nil, 0, errors.New("该文件夹不是 Avatar Explorer 的数据文件夹（未找到 Datas\\ItemsData.json 或 database\\items.json）")
	}
	b, err := core.ReadDataFile(file)
	if err != nil {
		return nil, ver, errors.New("无法读取 " + filepath.Base(file) + "（" + core.TrimErr(err) + "）")
	}
	var raw []map[string]any
	if err := json.Unmarshal(trimBOM(b), &raw); err != nil {
		// V2 may keep them under a key of their own
		var wrap map[string]json.RawMessage
		if json.Unmarshal(trimBOM(b), &wrap) != nil || json.Unmarshal(firstArray(wrap), &raw) != nil {
			return nil, ver, errors.New(filepath.Base(file) + " 的格式无法识别")
		}
	}
	// paths are the program's: relative ones to the folder that holds Datas
	base := dir
	if ver == 1 && strings.EqualFold(filepath.Base(filepath.Dir(file)), "Datas") {
		base = filepath.Dir(filepath.Dir(file))
	}
	abs := func(p string) string {
		// written on Windows: either separator, whichever this computer uses
		p = strings.TrimSpace(strings.NewReplacer("/", string(filepath.Separator), `\`, string(filepath.Separator)).Replace(p))
		if p == "" {
			return ""
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(base, p)
		}
		return filepath.Clean(p)
	}
	// SupportedAvatar names a base body by its own item's path: by its title it is known here
	titleAt := map[string]string{}
	for _, m := range raw {
		if t := str(m, "Title"); t != "" {
			titleAt[core.PathKey(abs(str(m, "ItemPath")))] = t
		}
	}
	var out []aeItem
	for _, m := range raw {
		it := aeItem{Name: str(m, "Title"), Dir: abs(str(m, "ItemPath")), Memo: str(m, "ItemMemo"), Image: abs(str(m, "ImagePath"))}
		if n := num(m, "BoothId"); n > 0 {
			it.Booth = strconv.FormatInt(n, 10)
		}
		it.Category = aeCategory(m)
		for _, s := range strs(m, "SupportedAvatar") {
			if t := titleAt[core.PathKey(abs(s))]; t != "" {
				it.Bases = append(it.Bases, t)
			} else if !strings.ContainsAny(s, `/\\`) {
				it.Bases = append(it.Bases, s) // a name, not a path
			}
		}
		it.Tags = strs(m, "Tags")
		if it.Dir != "" {
			out = append(out, it)
		}
	}
	return out, ver, nil
}

func aeCategory(m map[string]any) string {
	i := -1
	switch v := field(m, "Type").(type) {
	case float64:
		i = int(v)
	case string:
		if n, ok := aeTypeNames[strings.ToLower(v)]; ok {
			i = n
		}
	}
	if i < 0 || i >= len(aeTypes) {
		return ""
	}
	if aeTypes[i] == "" { // Custom: the player's own name for it, when it is one of ours
		if c := str(m, "CustomCategory"); core.ContainsStr(Categories, c) {
			return c
		}
		return ""
	}
	return aeTypes[i]
}

// PreviewAE reads what an Avatar Explorer folder holds.
func PreviewAE(st *core.Store, dir string) (*AEPreview, error) {
	items, ver, err := readAE(filepath.Clean(dir))
	if err != nil {
		return nil, err
	}
	return previewOf(st, dir, "ae", ver, items, aeItemsRoot(dir)), nil
}

// aeItemsRoot: the folder Avatar Explorer keeps its assets in.
func aeItemsRoot(dir string) string {
	for _, p := range []string{filepath.Join(dir, "Datas", "Items"), filepath.Join(dir, "Items")} {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			return p
		}
	}
	return ""
}

// ApplyAE takes over what an Avatar Explorer folder records (addRoot: add its folder of assets as an asset
// folder first).
func ApplyAE(st *core.Store, dir string, addRoot bool) (*AEResult, error) {
	items, _, err := readAE(filepath.Clean(dir))
	if err != nil {
		return nil, err
	}
	return takeOver(st, items, aeItemsRoot(dir), addRoot)
}

// ---------- KonoAsset ----------

var kaFiles = []struct{ file, kind string }{
	{"avatars.json", "avatar"}, {"avatarWearables.json", "wearable"}, {"worldObjects.json", "world"}, {"otherAssets.json", "other"},
}

// KAFolders: where KonoAsset keeps its data, when it is to be found.
func KAFolders() []string {
	var out []string
	add := func(d string) {
		if core.StatOK(filepath.Join(d, "metadata", "avatars.json")) && !core.ContainsStr(out, d) {
			out = append(out, d)
		}
	}
	// its preference file says where the data is
	if la := os.Getenv("LOCALAPPDATA"); la != "" {
		if b, err := os.ReadFile(filepath.Join(la, "dev.konoasset.app", "preference.json")); err == nil {
			var p struct {
				Data string `json:"dataDirPath"`
			}
			if json.Unmarshal(trimBOM(b), &p) == nil && p.Data != "" {
				add(p.Data)
			}
		}
		add(filepath.Join(la, "dev.konoasset.app"))
	}
	for _, d := range managerCandidates([]string{"KonoAsset"}) {
		add(d)
	}
	return out
}

// readKA reads a KonoAsset folder's assets.
func readKA(dir string) ([]aeItem, error) {
	if !core.StatOK(filepath.Join(dir, "metadata", "avatars.json")) {
		return nil, errors.New("该文件夹不是 KonoAsset 的数据文件夹（未找到 metadata\\avatars.json 等文件）")
	}
	type desc struct {
		Name  string   `json:"name"`
		Image string   `json:"imageFilename"`
		Tags  []string `json:"tags"`
		Memo  *string  `json:"memo"`
		Booth *uint64  `json:"boothItemId"`
	}
	type asset struct {
		ID       string   `json:"id"`
		Desc     desc     `json:"description"`
		Category string   `json:"category"`
		Bases    []string `json:"supportedAvatars"`
	}
	var out []aeItem
	for _, f := range kaFiles {
		b, err := core.ReadDataFile(filepath.Join(dir, "metadata", f.file))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		b = trimBOM(b)
		var list []asset
		var wrapped struct {
			Version int     `json:"version"`
			Data    []asset `json:"data"`
		}
		if json.Unmarshal(b, &wrapped) == nil && wrapped.Version > 0 {
			if wrapped.Version > 3 {
				return nil, errors.New("KonoAsset 的数据文件格式无法识别（版本 " + strconv.Itoa(wrapped.Version) + "）")
			}
			list = wrapped.Data
		} else if err := json.Unmarshal(b, &list); err != nil {
			return nil, errors.New("KonoAsset 的数据文件格式无法识别（" + f.file + "）")
		}
		for _, a := range list {
			if a.ID == "" || strings.ContainsAny(a.ID, `/\.`) {
				continue
			}
			it := aeItem{Name: a.Desc.Name, Dir: filepath.Join(dir, "data", a.ID), Tags: a.Desc.Tags, Bases: a.Bases,
				Category: kaCategory(f.kind, a.Category)}
			if a.Desc.Memo != nil {
				it.Memo = *a.Desc.Memo
			}
			if a.Desc.Booth != nil && *a.Desc.Booth > 0 {
				it.Booth = strconv.FormatUint(*a.Desc.Booth, 10)
			}
			if img := a.Desc.Image; img != "" && !strings.ContainsAny(img, `/\`) {
				it.Image = filepath.Join(dir, "images", img)
			}
			out = append(out, it)
		}
	}
	return out, nil
}

// kaCategory: KonoAsset's kind of asset and its own category name, as ours.
func kaCategory(kind, cat string) string {
	if kind == "avatar" {
		return "素体"
	}
	words := map[string]string{"衣装": "衣服", "髪型": "头发", "髪": "头发", "アクセサリー": "配饰", "アクセサリ": "配饰", "テクスチャ": "材质",
		"ギミック": "插件", "ツール": "插件", "シェーダー": "插件", "アニメーション": "动作", "モーション": "动作", "小物": "道具", "小道具": "道具"}
	for w, c := range words {
		if strings.Contains(cat, w) {
			return c
		}
	}
	if core.ContainsStr(Categories, cat) {
		return cat
	}
	return ""
}

// PreviewKA does PreviewAE's work for KonoAsset.
func PreviewKA(st *core.Store, dir string) (*AEPreview, error) {
	items, err := readKA(filepath.Clean(dir))
	if err != nil {
		return nil, err
	}
	return previewOf(st, dir, "ka", 0, items, kaRoot(dir)), nil
}

func kaRoot(dir string) string {
	if p := filepath.Join(dir, "data"); core.StatOK(p) {
		return p
	}
	return ""
}

// ApplyKA does ApplyAE's work for KonoAsset.
func ApplyKA(st *core.Store, dir string, addRoot bool) (*AEResult, error) {
	items, err := readKA(filepath.Clean(dir))
	if err != nil {
		return nil, err
	}
	return takeOver(st, items, kaRoot(dir), addRoot)
}

// ---------- both ----------

// AEPreview: what another manager's data folder holds, against this library.
type AEPreview struct {
	Dir      string `json:"dir"`
	Kind     string `json:"kind"`
	Version  int    `json:"version,omitempty"` // Avatar Explorer: 1 or 2
	Items    int    `json:"items"`             // the assets it records
	Matched  int    `json:"matched"`           // that are cards of this library
	Outside  int    `json:"outside"`           // whose folders are not in the asset folders
	Missing  int    `json:"missing"`           // whose folders are gone
	AddRoot  string `json:"addRoot,omitempty"` // its folder of assets, when it is not one of ours
	InFolder int    `json:"inFolder,omitempty"`
}

// AEResult: what taking it over did.
type AEResult struct {
	Matched  int    `json:"matched"` // cards given something
	Booth    int    `json:"booth"`
	Category int    `json:"category"`
	Bases    int    `json:"bases"`
	Tags     int    `json:"tags"`
	Notes    int    `json:"notes"`
	Covers   int    `json:"covers"`
	Skipped  int    `json:"skipped"` // assets not in this library, left alone
	AddRoot  string `json:"addRoot,omitempty"`
}

// cardFor: the key of the card whose folder is this item's, "" when the library has none. The store is
// held by the caller.
func cardFor(st *core.Store, dir string) string {
	k := core.PathKey(dir)
	for _, a := range st.Assets {
		for _, l := range a.Locations {
			if p := core.PathKey(l.Path); p == k || strings.HasPrefix(p, k+string(filepath.Separator)) {
				return a.Key
			}
		}
	}
	return ""
}

func previewOf(st *core.Store, dir, kind string, ver int, items []aeItem, root string) *AEPreview {
	pv := &AEPreview{Dir: filepath.Clean(dir), Kind: kind, Version: ver, Items: len(items)}
	st.Mu.RLock()
	roots := append([]string{}, st.Settings.Roots...)
	for _, it := range items {
		switch {
		case !core.StatOK(it.Dir):
			pv.Missing++
		case cardFor(st, it.Dir) != "":
			pv.Matched++
		case !underAny(it.Dir, roots):
			pv.Outside++
			if root != "" && underOrSame(it.Dir, root) {
				pv.InFolder++
			}
		}
	}
	st.Mu.RUnlock()
	if root != "" && !underAny(root, roots) && pv.InFolder > 0 {
		pv.AddRoot = root
	}
	return pv
}

// takeOver fills in what the cards have nothing of yet.
func takeOver(st *core.Store, items []aeItem, root string, addRoot bool) (*AEResult, error) {
	res := &AEResult{}
	if addRoot && root != "" {
		st.Mu.Lock()
		if !underAny(root, st.Settings.Roots) {
			st.Settings.Roots = append(st.Settings.Roots, root)
			res.AddRoot = root
		}
		st.Mu.Unlock()
		if res.AddRoot != "" {
			// what is in it is taken over once it is scanned (the caller runs the scan, then this again)
			_ = st.Save()
			core.BumpRev()
			return res, nil
		}
	}
	now := time.Now().Unix()
	st.Mu.Lock()
	for _, it := range items {
		key := cardFor(st, it.Dir)
		if key == "" {
			res.Skipped++
			continue
		}
		u := st.User[key]
		if u == nil {
			u = &core.UserData{}
			st.User[key] = u
		}
		did := false
		if it.Booth != "" && u.BoothURL == "" && !u.NoBooth {
			u.BoothURL, did = "https://booth.pm/ja/items/"+it.Booth, true
			res.Booth++
		}
		if it.Category != "" && u.Category == "" {
			u.Category, did = it.Category, true
			res.Category++
		}
		if len(it.Bases) > 0 && len(u.Bases) == 0 {
			u.Bases, did = it.Bases, true
			res.Bases++
		}
		if added := mergeTags(u, it.Tags); added {
			did = true
			res.Tags++
		}
		if m := strings.TrimSpace(it.Memo); m != "" && u.Notes == "" {
			u.Notes, did = m, true
			res.Notes++
		}
		if it.Image != "" && u.Cover == "" && core.StatOK(it.Image) {
			if c, err := CopyOwnCover(it.Image); err == nil {
				u.Cover, did = c, true
				res.Covers++
			}
		}
		if did {
			u.Updated = now
			res.Matched++
		}
	}
	st.Mu.Unlock()
	_ = st.Save()
	core.BumpRev()
	core.Logf("已导入其他素材管理软件的素材信息：补充 %d 张卡片，关联 Booth %d 个，跳过 %d 个", res.Matched, res.Booth, res.Skipped)
	return res, nil
}

func mergeTags(u *core.UserData, tags []string) bool {
	added := false
	for _, t := range tags {
		if t = strings.TrimSpace(t); t != "" && !core.ContainsStr(u.Tags, t) {
			u.Tags, added = append(u.Tags, t), true
		}
	}
	return added
}

// managerCandidates: folders another manager may be in — inside the asset folders (one level down), and
// the usual places programs are unpacked to.
func managerCandidates(names []string) []string {
	var out []string
	var bases []string
	for _, e := range []string{"USERPROFILE", "HOME"} {
		if h := os.Getenv(e); h != "" {
			bases = append(bases, h, filepath.Join(h, "Desktop"), filepath.Join(h, "Documents"), filepath.Join(h, "Downloads"))
		}
	}
	for _, e := range []string{"LOCALAPPDATA", "APPDATA", "ProgramFiles"} {
		if d := os.Getenv(e); d != "" {
			bases = append(bases, d)
		}
	}
	for _, b := range bases {
		for _, n := range names {
			out = append(out, filepath.Join(b, n))
		}
	}
	return out
}

// ---------- reading loosely ----------

func trimBOM(b []byte) []byte { return []byte(strings.TrimPrefix(string(b), "\ufeff")) }

// field: a value by its name, in PascalCase or camelCase.
func field(m map[string]any, name string) any {
	if v, ok := m[name]; ok {
		return v
	}
	if name != "" {
		if v, ok := m[strings.ToLower(name[:1])+name[1:]]; ok {
			return v
		}
	}
	return nil
}

func str(m map[string]any, name string) string {
	s, _ := field(m, name).(string)
	return strings.TrimSpace(s)
}

func num(m map[string]any, name string) int64 {
	switch v := field(m, name).(type) {
	case float64:
		return int64(v)
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}

func strs(m map[string]any, name string) []string {
	l, _ := field(m, name).([]any)
	var out []string
	for _, x := range l {
		if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

func firstArray(m map[string]json.RawMessage) []byte {
	for _, k := range []string{"items", "Items", "data", "Data"} {
		if v, ok := m[k]; ok {
			return v
		}
	}
	return nil
}
