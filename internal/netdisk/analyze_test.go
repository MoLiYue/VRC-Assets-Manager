package netdisk

import (
	"strings"
	"testing"

	"vrclib/internal/core"
	"vrclib/internal/naming"
	"vrclib/internal/testkit"
)

func defs() []naming.BaseDef { return naming.ParseBases(core.DefaultSettings().Bases) }

func dir(name string, kids ...*core.PanFile) *core.PanFile {
	return &core.PanFile{Name: name, Dir: true, Children: kids}
}
func file(name string, size int64) *core.PanFile { return &core.PanFile{Name: name, Size: size} }

func share(title string, files ...*core.PanFile) *core.PanListing {
	return &core.PanListing{Surl: "1" + naming.NormKey(title), Title: title, Fetched: 1, Files: files}
}

// partNames: what a share was split into, as "name(path)" in order.
func partNames(parts []PanPart) string {
	var out []string
	for _, p := range parts {
		out = append(out, p.Name+"("+p.Path+")")
	}
	return strings.Join(out, ", ")
}

// One product in several versions is one product: the share of an outfit that carries a package for each
// base body stays a single card, and the item number its folder is named by is the share's.
func TestSharePanOfOneProduct(t *testing.T) {
	l := share("8099091", dir("8099091",
		file("AONAMI_manuka.zip", 1000), file("AONAMI_shinano.zip", 1000)))
	info := AnalyzePan(l, defs())
	if got := partNames(info.Parts); got != "AONAMI()" {
		t.Errorf("parts: %s", got)
	}
	if info.Files != 2 || info.Size != 2000 {
		t.Errorf("files=%d size=%d", info.Files, info.Size)
	}
	if id := PanBoothID(l, info); id != "8099091" {
		t.Errorf("booth id: %q", id)
	}
	// what the analysis found is remembered: the matching asks for it on every refresh
	if got := partNames(SplitPanCached(l)); got != "AONAMI()" {
		t.Errorf("cached: %s", got)
	}
}

// Packages side by side that differ only by a colour, and a PSD pack beside the product, are the same card.
func TestSharePanVariantsAndPSD(t *testing.T) {
	colours := share("Moon Dress", file("Moon Dress_黒.unitypackage", 10), file("Moon Dress_白.unitypackage", 10))
	if got := partNames(SplitPanCached(colours)); got != "Moon Dress()" {
		t.Errorf("colours: %s", got)
	}
	psd := share("Crimson Rumor",
		dir("Crimson Rumor", file("Crimson Rumor.unitypackage", 10)),
		file("Crimson_Rumor_PSD.zip", 10))
	if got := partNames(SplitPanCached(psd)); got != "Crimson Rumor()" {
		t.Errorf("psd beside it: %s", got)
	}
	// a product's folder holding its colours as folders is one product, not one card a colour
	eyes := share("Plum Hair", dir("Plum Hair",
		file("Plum Hair.unitypackage", 10), dir("Eye_Blue", file("blue.png", 1)), dir("Eye_Red", file("red.png", 1))))
	if got := partNames(SplitPanCached(eyes)); got != "Plum Hair()" {
		t.Errorf("colour folders: %s", got)
	}
}

// A share that is several products is split into one card each, whether they lie side by side or are packed
// by category first; the item number of one of them is then not the share's.
func TestSharePanCollection(t *testing.T) {
	flat := share("衣服合集",
		dir("Moon Dress", file("Moon Dress.unitypackage", 10)),
		dir("Twintail Hair", file("Twintail.unitypackage", 10)),
		dir("Cat Ears", file("Ears.unitypackage", 10)))
	got := partNames(SplitPanCached(flat))
	if n := len(SplitPanCached(flat)); n != 3 {
		t.Errorf("flat collection: %d parts (%s)", n, got)
	}
	byCat := share("改模大全",
		dir("衣服", dir("Moon Dress", file("a.unitypackage", 10)), dir("Sailor Suit", file("b.unitypackage", 10))),
		dir("头发", dir("Twintail Hair", file("c.unitypackage", 10))))
	parts := SplitPanCached(byCat)
	if len(parts) != 3 {
		t.Fatalf("by category: %s", partNames(parts))
	}
	for _, p := range parts {
		if !strings.Contains(p.Path, "/") {
			t.Errorf("a product is to keep its path inside the share: %s", partNames(parts))
		}
	}
	// two item numbers inside and none of them the share's: nothing is claimed
	two := share("打包",
		dir("8099091", file("a.zip", 10)), dir("7770415", file("b.zip", 10)))
	if id := PanBoothID(two, AnalyzePan(two, defs())); id != "" {
		t.Errorf("two numbers: %q", id)
	}
}

// The category and the base bodies a share names are read out of its title and the names inside it.
func TestAnalyzePanCategoryAndBases(t *testing.T) {
	l := share("【衣服】Moon Dress for Manuka", dir("Moon Dress",
		file("MoonDress_manuka.unitypackage", 10), file("MoonDress_plum.unitypackage", 10)))
	info := AnalyzePan(l, defs())
	if info.Category != "衣服" {
		t.Errorf("category: %q", info.Category)
	}
	if !core.ContainsStr(info.Bases, "Manuka") || !core.ContainsStr(info.Bases, "Plum") {
		t.Errorf("bases: %v", info.Bases)
	}
}

// The share link a seller sends: the id, the extraction code, and which netdisk the message is about.
func TestShareLinks(t *testing.T) {
	for _, c := range []struct{ text, surl, pwd string }{
		{"https://pan.baidu.com/s/1abcDEF_x 提取码: 2k9m", "1abcDEF_x", "2k9m"},
		{"pan.baidu.com/s/1abcDEF?pwd=ab12", "1abcDEF", "ab12"},
		{"链接：ｈｔｔｐｓ：／／ｐａｎ．ｂａｉｄｕ．ｃｏｍ／ｓ／１ａｂｃＤＥＦ　密码：ｘｙ９８", "1abcDEF", "xy98"},
		{"https://pan.baidu.com/share/init?surl=abcDEF 密码 1234", "1abcDEF", "1234"},
		{"https://yun.baidu.com/s/1Share", "1Share", ""},
		{"https://example.com/s/1abcDEF", "", ""},
	} {
		if got := ShareSurl(c.text); got != c.surl {
			t.Errorf("%q: surl %q, want %q", c.text, got, c.surl)
		}
		if got := SharePwdFromURL(c.text); got != c.pwd {
			t.Errorf("%q: pwd %q, want %q", c.text, got, c.pwd)
		}
	}
	// a message with a Baidu link first is that share's, whatever cloud link it also mentions
	if !CloudFirst("https://drive.google.com/file/d/1AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA/view") {
		t.Error("a Drive link alone")
	}
	if CloudFirst("https://pan.baidu.com/s/1abcDEF 预览 https://drive.google.com/file/d/1AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA/view") {
		t.Error("a Baidu link before it")
	}
	if CloudFirst("https://pan.baidu.com/s/1abcDEF") {
		t.Error("no cloud link at all")
	}
}

// A card key names the share its listing is kept under: the Baidu id, or the whole key for the clouds.
func TestShareKeyAndCards(t *testing.T) {
	for key, want := range map[string]string{
		"pan:1abcDEF":           "1abcDEF",
		"pan:1abcDEF#/Moon.zip": "1abcDEF",
		"gd:1Folder00000000":    "gd:1Folder00000000",
		"db:abc123#/a/b.zip":    "db:abc123",
	} {
		if got := ShareKey(key); got != want {
			t.Errorf("%s: %q, want %q", key, got, want)
		}
	}
	st := testkit.NewStore(t)
	st.User["pan:1abcDEF"] = &core.UserData{}
	st.User["gd:1Folder00000000"] = &core.UserData{}
	st.User["name:moondress"] = &core.UserData{}
	if got := strings.Join(PanCardKeys(st), ","); got != "gd:1Folder00000000,pan:1abcDEF" {
		t.Errorf("share cards: %s", got)
	}
	// the product inside a share, by its path
	l := share("打包", dir("Moon Dress", file("a.unitypackage", 10)), dir("Twintail Hair", file("b.unitypackage", 10)))
	st.Pan["1abcDEF"] = l
	AnalyzePan(l, defs())
	if _, it := PanSub(st, "pan:1abcDEF#Moon Dress"); it == nil || it.Name != "Moon Dress" {
		t.Errorf("the product inside: %+v", it)
	}
	if _, it := PanSub(st, "pan:1abcDEF"); it != nil {
		t.Errorf("the share itself: %+v", it)
	}
}
