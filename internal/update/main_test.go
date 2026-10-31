package update

import (
	"os"
	"testing"
)

// The tests ask a stand-in GitHub for a repository's releases; a build without a repository of its own
// (UpdateRepo "") does not ask at all (TestNoUpdateRepo).
func TestMain(m *testing.M) {
	UpdateRepo = "example/repo"
	os.Exit(m.Run())
}

func TestNoUpdateRepo(t *testing.T) {
	was := UpdateRepo
	UpdateRepo = ""
	defer func() { UpdateRepo = was }()
	if _, err := CheckUpdate(nil, true); err != ErrNoUpdateRepo {
		t.Errorf("%v", err)
	}
	if ReleasesPage() != "" {
		t.Error("a releases page with no repository")
	}
}
