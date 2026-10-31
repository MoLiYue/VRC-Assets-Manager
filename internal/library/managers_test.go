package library

import (
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vrclib/internal/core"
	"vrclib/internal/testkit"
)

func writeJSON(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0644); err != nil {
		t.Fatal(err)
	}
}

// Avatar Explorer V1 unpacked into an asset folder: its ItemsData.json (as Models/Item.cs writes it, with a
// BOM), its assets in Datas/Items. The scan takes its assets and none of its database; 「导入」 fills in what
// the cards have nothing of yet.
func TestAvatarExplorerV1(t *testing.T) {
	st := testkit.NewStore(t)
	root := t.TempDir()
	st.Settings.Roots = []string{root}
	ae := filepath.Join(root, "Avatar Explorer")
	testkit.WriteFile(t, filepath.Join(ae, "Datas", "Items", "Kipfel", "Kipfel.unitypackage"), 100)
	testkit.WriteFile(t, filepath.Join(ae, "Datas", "Items", "Moon Dress", "Moon Dress.unitypackage"), 100)
	testkit.WriteFile(t, filepath.Join(ae, "Datas", "Backup", "2026-01-01", "old.unitypackage"), 100)
	_ = os.MkdirAll(filepath.Join(ae, "Datas", "Thumbnail"), 0755)
	_ = os.WriteFile(filepath.Join(ae, "Datas", "Thumbnail", "moon.jpg"), testkit.JPEG(8, 8, color.RGBA{9, 9, 9, 255}), 0644)
	writeJSON(t, filepath.Join(ae, "Datas", "ItemsData.json"), "\ufeff"+`[
	 {"Title":"Kipfel","BoothId":5813187,"ItemPath":"Datas\\Items\\Kipfel","Type":0,"SupportedAvatar":[],"Tags":[],"ItemMemo":""},
	 {"Title":"Moon Dress","BoothId":9100001,"ItemPath":"Datas\\Items\\Moon Dress","ImagePath":"Datas\\Thumbnail\\moon.jpg","Type":1,
	  "SupportedAvatar":["Datas\\Items\\Kipfel"],"Tags":["dress","moon"],"ItemMemo":"for Kipfel"},
	 {"Title":"Gone","BoothId":-1,"ItemPath":"Datas\\Items\\Gone","Type":10}]`)
	RunFolderScan(st, &core.Task{})
	var names []string
	for _, a := range st.Assets {
		names = append(names, a.Name)
	}
	if strings.Join(names, ",") != "Kipfel,Moon Dress" {
		t.Fatalf("scanned: %v (its backups and database are not assets)", names)
	}
	found := false
	for _, d := range AEFolders() {
		found = found || d == ae
	}
	_ = found // (where AEFolders looks depends on the machine: only the preview of a given folder is asserted)
	pv, err := PreviewAE(st, ae)
	if err != nil {
		t.Fatal(err)
	}
	if pv.Version != 1 || pv.Items != 3 || pv.Matched != 2 || pv.Missing != 1 || pv.Outside != 0 || pv.AddRoot != "" {
		t.Errorf("preview: %+v", pv)
	}
	// the player wrote a note of their own on the dress already: it stands
	dress := viewOf(st, "Moon Dress").Key
	st.User[dress] = &core.UserData{Notes: "mine"}
	res, err := ApplyAE(st, ae, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Matched != 2 || res.Booth != 2 || res.Category != 2 || res.Bases != 1 || res.Tags != 1 || res.Notes != 0 || res.Covers != 1 || res.Skipped != 1 {
		t.Errorf("result: %+v", res)
	}
	u := st.User[dress]
	if u.BoothURL != "https://booth.pm/ja/items/9100001" || u.Category != "衣服" || strings.Join(u.Bases, ",") != "Kipfel" ||
		strings.Join(u.Tags, ",") != "dress,moon" || u.Notes != "mine" || filepath.Dir(u.Cover) != OwnCoverDir() {
		t.Errorf("the dress: %+v", u)
	}
	if k := st.User[viewOf(st, "Kipfel").Key]; k == nil || k.Category != "素体" {
		t.Errorf("the base body: %+v", k)
	}
	// Avatar Explorer's own data is not written to
	if b, _ := os.ReadFile(filepath.Join(ae, "Datas", "ItemsData.json")); !strings.Contains(string(b), `"ItemMemo":"for Kipfel"`) {
		t.Error("ItemsData.json was changed")
	}
}

// Avatar Explorer outside the asset folders: its folder of assets is offered to be added, and adding it
// comes first; nothing else is done until it is scanned.
func TestAvatarExplorerAddRoot(t *testing.T) {
	st := testkit.NewStore(t)
	st.Settings.Roots = []string{t.TempDir()}
	ae := filepath.Join(t.TempDir(), "AE")
	testkit.WriteFile(t, filepath.Join(ae, "Datas", "Items", "Tail", "Tail.unitypackage"), 100)
	writeJSON(t, filepath.Join(ae, "Datas", "ItemsData.json"), `[{"Title":"Tail","BoothId":1,"ItemPath":"Datas\\Items\\Tail","Type":4}]`)
	pv, err := PreviewAE(st, ae)
	if err != nil {
		t.Fatal(err)
	}
	items := filepath.Join(ae, "Datas", "Items")
	if pv.Outside != 1 || pv.AddRoot != items || pv.InFolder != 1 {
		t.Fatalf("preview: %+v", pv)
	}
	res, err := ApplyAE(st, ae, true)
	if err != nil || res.AddRoot != items || res.Matched != 0 || !core.ContainsStr(st.Settings.Roots, items) {
		t.Fatalf("add the folder: %+v %v", res, err)
	}
	RunFolderScan(st, &core.Task{})
	if res, _ := ApplyAE(st, ae, false); res.Matched != 1 || res.Booth != 1 {
		t.Errorf("after the scan: %+v", res)
	}
	if _, err := PreviewAE(st, t.TempDir()); err == nil || !strings.Contains(err.Error(), "ItemsData.json") {
		t.Errorf("not a folder of it: %v", err)
	}
	bad := t.TempDir()
	writeJSON(t, filepath.Join(bad, "Datas", "ItemsData.json"), `{"not":"a list"}`)
	if _, err := PreviewAE(st, bad); err == nil || !strings.Contains(err.Error(), "格式无法识别") {
		t.Errorf("a file of another shape: %v", err)
	}
}

// KonoAsset: metadata/*.json as its loader reads them — the current {"version":3,"data":[…]} and a bare
// array an older one wrote — its assets in data/<id>, their pictures in images/.
func TestKonoAsset(t *testing.T) {
	st := testkit.NewStore(t)
	root := t.TempDir()
	st.Settings.Roots = []string{root}
	ka := filepath.Join(root, "KonoAsset")
	idA, idW := "6f1c0d2e-0000-4000-8000-000000000001", "6f1c0d2e-0000-4000-8000-000000000002"
	testkit.WriteFile(t, filepath.Join(ka, "data", idA, "Manuka.unitypackage"), 100)
	testkit.WriteFile(t, filepath.Join(ka, "data", idW, "Sailor.unitypackage"), 100)
	_ = os.MkdirAll(filepath.Join(ka, "images"), 0755)
	_ = os.WriteFile(filepath.Join(ka, "images", "sailor.png"), testkit.JPEG(8, 8, color.RGBA{1, 1, 1, 255}), 0644)
	writeJSON(t, filepath.Join(ka, "metadata", "avatars.json"), `{"version":3,"data":[{"id":"`+idA+`","description":{"name":"マヌカ","creator":"ying","imageFilename":null,"tags":[],"memo":null,"boothItemId":5058077,"dependencies":[],"createdAt":1,"publishedAt":null}}]}`)
	writeJSON(t, filepath.Join(ka, "metadata", "avatarWearables.json"), `[{"id":"`+idW+`","description":{"name":"セーラー服","creator":"x","imageFilename":"sailor.png","tags":["制服"],"boothItemId":8099091,"createdAt":1},"category":"衣装","supportedAvatars":["マヌカ"]}]`)
	writeJSON(t, filepath.Join(ka, "metadata", "worldObjects.json"), `{"version":3,"data":[]}`)
	RunFolderScan(st, &core.Task{})
	if len(st.Assets) != 2 {
		t.Fatalf("scanned: %+v (its metadata and images are not assets)", st.Assets)
	}
	pv, err := PreviewKA(st, ka)
	if err != nil {
		t.Fatal(err)
	}
	if pv.Items != 2 || pv.Matched != 2 {
		t.Errorf("preview: %+v", pv)
	}
	res, err := ApplyKA(st, ka, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Matched != 2 || res.Booth != 2 || res.Category != 2 || res.Bases != 1 || res.Tags != 1 || res.Covers != 1 {
		t.Errorf("result: %+v", res)
	}
	for _, a := range st.Assets {
		u := st.User[a.Key]
		if strings.Contains(a.Locations[0].Path, idW) && (u.Category != "衣服" || strings.Join(u.Bases, ",") != "マヌカ" || u.BoothURL == "") {
			t.Errorf("the wearable: %+v", u)
		}
	}
	writeJSON(t, filepath.Join(ka, "metadata", "otherAssets.json"), `{"version":9,"data":[]}`)
	if _, err := PreviewKA(st, ka); err == nil || !strings.Contains(err.Error(), "格式无法识别") {
		t.Errorf("a version from later: %v", err)
	}
	if _, err := PreviewKA(st, t.TempDir()); err == nil || !strings.Contains(err.Error(), "avatars.json") {
		t.Errorf("not a folder of it: %v", err)
	}
}
