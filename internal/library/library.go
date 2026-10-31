// Package library is the asset library itself: it scans the asset folders, works out what each asset is
// (its category, the base bodies it fits, the Booth product it belongs to), builds the cards the window
// shows, and carries the tools that work on the library as a whole — tidying, moving it to another
// computer, taking over another manager's records, and the covers inside unitypackages.
package library

import (
	"vrclib/internal/core"
)

// Categories: the categories a card can be in, in the order the window lists them.
// They are the ones the window has colours for (web/app.js CAT_COLOR) and naming.Classify gives.
var Categories = []string{"素体", "衣服", "头发", "配饰", "道具", "材质", "面捕", "插件", "动作", "音效", "字体", "其他"}

// ShopCat: one of the Booth categories the shop page offers.
type ShopCat struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	ID    string `json:"id,omitempty"` // Booth's own category number
}

// ImageExt: the picture formats a cover may be in, by lower-case extension.
var ImageExt = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".bmp": true}

// TaskPkgCover: reading the preview pictures out of unitypackages, as the task list shows it.
var TaskPkgCover = &core.Task{Name: "pkgcover", Label: "提取素材封面"}

// ImportBusy: whether something is writing into the library, so that bringing in another copy of it has to
// wait. The server fills it in (it knows about the downloads and imports).
var ImportBusy = func() bool { return false }

// AssetView: one card of the library, as the window reads it. It is the asset as scanned, together with
// what the player wrote down, what Booth says, and where it is used.
type AssetView struct {
	Key       string          `json:"key"`
	Name      string          `json:"name"`
	RawName   string          `json:"rawName,omitempty"`
	NameZh    string          `json:"nameZh,omitempty"`
	Category  string          `json:"category,omitempty"`
	Bases     []string        `json:"bases,omitempty"`
	Tags      []string        `json:"tags,omitempty"`
	Styles    []string        `json:"styles,omitempty"`
	Notes     string          `json:"notes,omitempty"`
	Cover     string          `json:"cover,omitempty"`
	CoverPkg  bool            `json:"coverPkg,omitempty"` // the cover is Unity's 128-pixel preview out of a unitypackage
	Size      int64           `json:"size,omitempty"`
	Files     int             `json:"files,omitempty"`
	MTime     int64           `json:"mtime,omitempty"`
	Added     int64           `json:"added,omitempty"`
	Hidden    bool            `json:"hidden,omitempty"`
	Fav       bool            `json:"fav,omitempty"`
	Locations []core.Location `json:"locations,omitempty"`
	Usage     []core.Usage    `json:"usage,omitempty"`
	Packages  []string        `json:"packages,omitempty"`
	Archives  []string        `json:"archives,omitempty"`
	// the Booth product it belongs to, and how that was worked out ("user", "url", "name", "library", "auto")
	BoothID   string          `json:"boothId,omitempty"`
	BoothFrom string          `json:"boothFrom,omitempty"`
	Booth     *core.BoothInfo `json:"booth,omitempty"`
	// a netdisk share card: the share and what it holds
	Share    string           `json:"share,omitempty"`
	Pan      *core.PanListing `json:"pan,omitempty"`
	Purchase *core.Purchase   `json:"purchase,omitempty"`
	// a card that holds several products (合集包): how many, and whether they are still inside archives
	Bundle       int  `json:"bundle,omitempty"`
	BundlePacked bool `json:"bundlePacked,omitempty"`
}

// OldCopy: a folder of an earlier version of an asset, as the update that replaced it left it behind.
type OldCopy struct {
	Path  string `json:"path"`
	Name  string `json:"name,omitempty"`
	Size  int64  `json:"size,omitempty"`
	Files int    `json:"files,omitempty"`
}

// UpdateNote: what is new for an asset — the files its Booth page or its share has that the library does not.
type UpdateNote struct {
	Key      string    `json:"key"`
	Name     string    `json:"name,omitempty"`
	Added    []string  `json:"added,omitempty"`    // files that are new
	Replaced []string  `json:"replaced,omitempty"` // files of another size than the ones on disk
	Old      []OldCopy `json:"old,omitempty"`      // where the version on disk lives
	Source   string    `json:"source,omitempty"`   // "booth", "gumroad", "pan", "gd", "db"
	// where the new files are fetched from: a share (the card key of the share, and what of it is new) or
	// a purchase (its item number, and the ids of the downloads that are new)
	PanKey  string   `json:"panKey,omitempty"`
	Paths   []string `json:"paths,omitempty"`
	Item    string   `json:"item,omitempty"`
	DLs     []string `json:"dls,omitempty"`
	Gumroad bool     `json:"gumroad,omitempty"` // the purchase is a Gumroad one
	CanDL   bool     `json:"canDl,omitempty"`   // the program can fetch it (not a share linked by hand)
	Version string   `json:"version,omitempty"` // what the new one calls itself, when it says
	Seen    int64    `json:"seen,omitempty"`
	Found   int64    `json:"found,omitempty"`
}

// ---------- the scanning pipeline ----------

// ---------- netdisk shares ----------

// ---------- updates ----------

// ---------- collections (合集包) ----------

// BundleInfo: what marking a folder as a collection found.
type BundleInfo struct {
	Containers []string `json:"containers,omitempty"` // the folder levels that hold the products
	Products   int      `json:"products,omitempty"`
}

// ---------- the Booth shop page ----------

// ---------- covers ----------

// ---------- tidying ----------

// TidyFile: one file a tidy scan found.
type TidyFile struct {
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	Key   string `json:"key,omitempty"`  // the card it belongs to
	Name  string `json:"name,omitempty"` // and what that is called
	Keep  bool   `json:"keep,omitempty"` // the one of a group that is kept
	Why   string `json:"why,omitempty"`
	MTime int64  `json:"mtime,omitempty"`
}

// TidyGroup: files that are the same, or an archive and what was unpacked out of it.
type TidyGroup struct {
	Files []TidyFile `json:"files"`
	Size  int64      `json:"size,omitempty"` // what moving all but the kept one would free
}

// TidyScan: the result of one scan, with the number that says which result it is.
type TidyScan struct {
	Seq    int64       `json:"seq"`
	Groups []TidyGroup `json:"groups,omitempty"`
	Files  int         `json:"files,omitempty"`
	Size   int64       `json:"size,omitempty"`
	Cut    bool        `json:"cut,omitempty"` // the list was cut short
	Ran    int64       `json:"ran,omitempty"`
}

// TidyRun: how a tidy scan is going.
type TidyRun struct {
	Running bool   `json:"running"`
	Done    int    `json:"done,omitempty"`
	Total   int    `json:"total,omitempty"`
	Msg     string `json:"msg,omitempty"`
	Err     string `json:"err,omitempty"`
}

// TidyOne: one of the tidy pages, as the window reads it — how the scan is going, and its last result.
type TidyOne struct {
	Run TidyRun   `json:"run"`
	Dup *TidyScan `json:"dup,omitempty"` // 重复文件
	Arc *TidyScan `json:"arc,omitempty"` // 可清理的压缩包
}

// TidyView: all the tidy pages.
type TidyView struct {
	Dup   TidyOne `json:"dup"`
	Arc   TidyOne `json:"arc"`
	Space TidyOne `json:"space"`
	Fit   TidyOne `json:"fit"`
}

// TidyResult: what moving files to the Recycle Bin did.
type TidyResult struct {
	Removed int      `json:"removed"`
	Freed   int64    `json:"freed"`
	Failed  []string `json:"failed,omitempty"`
}

// ---------- moving the library to another computer ----------

// Transfer: how an export or an import is going.
type Transfer struct {
	Stage   string `json:"stage,omitempty"` // export, import, done, failed
	Msg     string `json:"msg,omitempty"`
	Done    int    `json:"done,omitempty"`
	Total   int    `json:"total,omitempty"`
	File    string `json:"file,omitempty"`
	Err     string `json:"err,omitempty"`
	CanUndo bool   `json:"canUndo,omitempty"`
}

// ExportInfo: what an export file holds, read before anything is brought in.
type ExportInfo struct {
	Path    string   `json:"path"`
	Version string   `json:"version,omitempty"`
	Made    int64    `json:"made,omitempty"`
	Assets  int      `json:"assets,omitempty"`
	Cards   int      `json:"cards,omitempty"`
	Roots   []string `json:"roots,omitempty"`   // the asset folders of the computer it came from
	Missing []string `json:"missing,omitempty"` // of those, the ones this computer does not have
	Covers  int      `json:"covers,omitempty"`
	Err     string   `json:"err,omitempty"`
}

// ImportOptions: how an export file is brought in.
type ImportOptions struct {
	Path string            `json:"path"`
	Mode string            `json:"mode,omitempty"` // "replace" = instead of merging into what is here
	Map  map[string]string `json:"map,omitempty"`  // the folders of the other computer → this one's
}

// ImportResult: what bringing one in did.
type ImportResult struct {
	Cards   int      `json:"cards,omitempty"`
	Covers  int      `json:"covers,omitempty"`
	Pending []string `json:"pending,omitempty"` // folders still to be mapped
	CanUndo bool     `json:"canUndo,omitempty"`
	Err     string   `json:"err,omitempty"`
}

// ---------- another manager's library (Avatar Explorer, KonoAsset) ----------
