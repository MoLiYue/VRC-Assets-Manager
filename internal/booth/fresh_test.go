package booth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"vrclib/internal/core"
	"vrclib/internal/testkit"
)

// A sync asks Booth only about the purchases nothing is known about yet. What an earlier sync fetched is left
// alone, however long ago that was: the items of a library of a few hundred used to be asked about again on
// every sync, two requests and a pause each. Asking again is 「重新获取」 (force), or a RefreshOld round.
func TestBoothFetchLeavesWhatIsKnownAlone(t *testing.T) {
	gap := boothGap
	boothGap = time.Millisecond
	defer func() { boothGap = gap }()
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/ja/items/"), ".json")
		if !strings.HasSuffix(r.URL.Path, ".json") { // the item page, read for the description blocks
			fmt.Fprint(w, "<html></html>")
			return
		}
		mu.Lock()
		asked = append(asked, id)
		mu.Unlock()
		fmt.Fprintf(w, `{"name":"Item %s","price":"¥ 100"}`, id)
	}))
	defer srv.Close()
	t.Setenv("VRCLIB_BOOTH_WEB", srv.URL)
	took := func() string {
		mu.Lock()
		defer mu.Unlock()
		got := strings.Join(asked, " ")
		asked = nil
		return got
	}

	st := testkit.NewStore(t)
	long := time.Now().Unix() - 30*86400
	// newest purchase first (When(), falling back to First)
	st.Purchases["7000001"] = &core.Purchase{ID: "7000001", Name: "new", First: 3000}    // nothing known yet
	st.Purchases["7000002"] = &core.Purchase{ID: "7000002", Name: "known", First: 2000}  // fetched a month ago
	st.Purchases["7000003"] = &core.Purchase{ID: "7000003", Name: "gone", First: 1000}   // delisted
	st.Purchases["7000004"] = &core.Purchase{ID: "7000004", Name: "oldinfo", First: 500} // info an older version wrote
	st.Booth["7000002"] = &core.BoothInfo{ID: "7000002", Name: "known", Fetched: long, Ver: BoothInfoVer}
	st.Booth["7000003"] = &core.BoothInfo{ID: "7000003", Name: "gone", Fetched: long, Ver: BoothInfoVer, Gone: true}
	st.Booth["7000004"] = &core.BoothInfo{ID: "7000004", Name: "oldinfo", Fetched: long, Ver: BoothInfoVer - 1}

	prog := &core.Task{}
	RunBoothFetch(st, prog, false, nil)
	if got := took(); got != "7000001 7000004" {
		t.Fatalf("a sync asked for %q, wanted the new purchase and the one with info of an older version", got)
	}
	// the same sync run again: there is nothing left to ask about, and Booth is not touched
	RunBoothFetch(st, prog, false, nil)
	if got, msg := took(), prog.Snapshot().Msg; got != "" || !strings.Contains(msg, "暂无需要获取") {
		t.Fatalf("a second sync asked for %q, status %q", got, msg)
	}

	// a round meant to look for changes the shops made takes in what was fetched long ago — not the delisted one
	RefreshOld = true
	RunBoothFetch(st, prog, false, nil)
	RefreshOld = false
	if got := took(); got != "7000002" {
		t.Fatalf("a RefreshOld round asked for %q", got)
	}
	// 「重新获取」 asks again about everything, the delisted item included
	RunBoothFetch(st, prog, true, nil)
	if got := took(); got != "7000001 7000002 7000003 7000004" {
		t.Fatalf("force asked for %q", got)
	}
}
