package gitbridge

import (
	"os"
	"testing"

	git "github.com/go-git/go-git/v5"

	"github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/revision"
	"github.com/easylab-platform/easyvcs/store"
)

// exportBare initializes an empty bare git repo at path.
func exportBare(path string) error {
	_, err := git.PlainInit(path, true)
	return err
}

var _ = os.Getenv

func TestHeaderParse(t *testing.T) {
	msg := "revision: abc123\n\nFixed the thing"
	if id := headerRevision(msg); id != "abc123" {
		t.Fatalf("headerRevision=%q", id)
	}
	if d := headerDescription(msg); d != "Fixed the thing" {
		t.Fatalf("headerDescription=%q", d)
	}
	if headerRevision("no header") != "" {
		t.Fatalf("should be no header")
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("EASYVCS_HOME", home)
	cs, err := store.OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	// Source repo A with two chained revisions.
	repoA, _ := cs.Create(store.RepoRef{Namespace: "a", Name: "r"})
	wsA := revision.NewWorkspace(repoA)
	_, r1, err := wsA.CommitFromChanges(object.ID{}, []revision.FileChangeSpec{{Path: "a.txt", Content: []byte("1")}}, "one", store.Author{Name: "t"}, "")
	if err != nil {
		t.Fatal(err)
	}
	// walk root -> r1 by using first commit parent? r1 is root (no parent). Build r2 on top.
	s1, _ := wsA.GetSnapshot(r1.Hash)
	_, r2, err := wsA.CommitFromChanges(s1.RevisionHash, []revision.FileChangeSpec{{Path: "b.txt", Content: []byte("2")}}, "two", store.Author{Name: "t"}, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = s1

	// Export both to a bare git dir.
	dest := t.TempDir() + "/dest.git"
	if err := exportBare(dest); err != nil {
		t.Fatal(err)
	}
	shas, err := ExportRevisions(wsA, repoA, PushOptions{
		Dest: dest, Branch: "main", Revisions: []string{r1.ID, r2.ID},
		Author: store.Author{Name: "t", Email: "t@x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(shas) != 2 {
		t.Fatalf("exported %d commits, want 2", len(shas))
	}

	// Re-import into the SAME repo A: the `revision:` headers match known ids,
	// so the weak-trace reuse yields the same revision ids (idempotent).
	revs, err := ImportBranch(wsA, repoA, ImportOptions{Source: dest, Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if len(revs) != 2 {
		t.Fatalf("imported %d revisions, want 2", len(revs))
	}
	if revs[0] != r1.ID || revs[1] != r2.ID {
		t.Fatalf("weak-trace reuse failed: got %v want [%s %s]", revs, r1.ID, r2.ID)
	}
}

func TestImportIntoFreshRepoGeneratesNewIDs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("EASYVCS_HOME", home)
	cs, _ := store.OpenDefault()
	defer func() { _ = cs.Close() }()
	repoA, _ := cs.Create(store.RepoRef{Namespace: "a", Name: "r"})
	wsA := revision.NewWorkspace(repoA)
	_, r1, _ := wsA.CommitFromChanges(object.ID{}, []revision.FileChangeSpec{{Path: "a.txt", Content: []byte("1")}}, "one", store.Author{Name: "t"}, "")
	dest := t.TempDir() + "/dest.git"
	_ = exportBare(dest)
	if _, err := ExportRevisions(wsA, repoA, PushOptions{Dest: dest, Branch: "main", Revisions: []string{r1.ID}, Author: store.Author{Name: "t", Email: "t@x"}}); err != nil {
		t.Fatal(err)
	}

	// Fresh repo B: the header id is unknown -> a NEW revision id is generated.
	homeB := t.TempDir()
	t.Setenv("EASYVCS_HOME", homeB)
	csB, _ := store.OpenDefault()
	defer func() { _ = csB.Close() }()
	repoB, _ := csB.Create(store.RepoRef{Namespace: "b", Name: "r"})
	wsB := revision.NewWorkspace(repoB)
	revs, err := ImportBranch(wsB, repoB, ImportOptions{Source: dest, Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if len(revs) != 1 {
		t.Fatalf("imported %d, want 1", len(revs))
	}
	if revs[0] == r1.ID {
		t.Fatalf("fresh repo should generate a NEW id, not reuse header id %s", r1.ID)
	}
}

func TestIsScpLike(t *testing.T) {
	if !isScpLike("git@github.com:org/repo.git") {
		t.Fatal("should be scp-like")
	}
	if isScpLike("https://github.com/org/repo.git") {
		t.Fatal("https is not scp-like")
	}
	if isScpLike("/local/path") {
		t.Fatal("local path is not scp-like")
	}
}

func TestExportImportTags(t *testing.T) {
	home := t.TempDir()
	t.Setenv("EASYVCS_HOME", home)
	cs, _ := store.OpenDefault()
	defer func() { _ = cs.Close() }()
	repoA, _ := cs.Create(store.RepoRef{Namespace: "a", Name: "r"})
	wsA := revision.NewWorkspace(repoA)
	_, r1, err := wsA.CommitFromChanges(object.ID{}, []revision.FileChangeSpec{{Path: "a.txt", Content: []byte("1")}}, "one", store.Author{Name: "t"}, "")
	if err != nil {
		t.Fatal(err)
	}
	s1, _ := wsA.GetSnapshot(r1.Hash)
	_, r2, err := wsA.CommitFromChanges(s1.RevisionHash, []revision.FileChangeSpec{{Path: "b.txt", Content: []byte("2")}}, "two", store.Author{Name: "t"}, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = s1
	// tag v1 -> r2.
	if _, err := wsA.SetRef("v1", store.RefTag, r2.ID); err != nil {
		t.Fatal(err)
	}

	dest := t.TempDir() + "/dest.git"
	_ = exportBare(dest)
	if _, err := ExportRevisions(wsA, repoA, PushOptions{
		Dest: dest, Branch: "main", Revisions: []string{r1.ID, r2.ID},
		Author: store.Author{Name: "t", Email: "t@x"},
		Tags:   []TagRef{{Name: "v1", Rev: r2.ID}},
	}); err != nil {
		t.Fatal(err)
	}

	// Import into a fresh repo B; the tag should land as an easyvcs tag on the
	// revision for v1's commit.
	homeB := t.TempDir()
	t.Setenv("EASYVCS_HOME", homeB)
	csB, _ := store.OpenDefault()
	defer func() { _ = csB.Close() }()
	repoB, _ := csB.Create(store.RepoRef{Namespace: "b", Name: "r"})
	wsB := revision.NewWorkspace(repoB)
	if _, err := ImportBranch(wsB, repoB, ImportOptions{Source: dest, Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	tag, err := wsB.GetRef("v1")
	if err != nil || tag == nil || tag.Kind != store.RefTag {
		t.Fatalf("tag v1 not imported: %v", err)
	}
	// The tag must point at a real revision (it reuses the header id of r2, so
	// in a fresh repo it gets a NEW id but the tag still exists).
	if _, err := wsB.GetRevision(tag.Target); err != nil {
		t.Fatalf("tag target revision %s missing: %v", tag.Target, err)
	}
}

func TestExportSquash(t *testing.T) {
	home := t.TempDir()
	t.Setenv("EASYVCS_HOME", home)
	cs, _ := store.OpenDefault()
	defer func() { _ = cs.Close() }()
	repoA, _ := cs.Create(store.RepoRef{Namespace: "a", Name: "r"})
	wsA := revision.NewWorkspace(repoA)
	_, r1, err := wsA.CommitFromChanges(object.ID{}, []revision.FileChangeSpec{{Path: "a.txt", Content: []byte("1")}}, "one", store.Author{Name: "t"}, "")
	if err != nil {
		t.Fatal(err)
	}
	s1, _ := wsA.GetSnapshot(r1.Hash)
	_, r2, err := wsA.CommitFromChanges(s1.RevisionHash, []revision.FileChangeSpec{{Path: "b.txt", Content: []byte("2")}}, "two", store.Author{Name: "t"}, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = s1

	dest := t.TempDir() + "/dest.git"
	_ = exportBare(dest)
	shas, err := ExportRevisions(wsA, repoA, PushOptions{
		Dest: dest, Branch: "main", Revisions: []string{r1.ID, r2.ID},
		Author: store.Author{Name: "t", Email: "t@x"}, Squash: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(shas) != 1 {
		t.Fatalf("squash should export a single commit, got %d", len(shas))
	}
}
