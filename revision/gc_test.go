package revision

import (
	"testing"

	"github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/store"
)

// TestGCPrunesOrphans builds a repo with a live revision tree plus a stray blob
// written directly (never referenced), then runs GC (real store) and asserts
// the orphan is pruned while live objects survive.
func TestGCPrunesOrphans(t *testing.T) {
	repo, cs := newTestRepo(t)
	_ = cs.SetWAL()
	ws := NewWorkspace(repo)
	// Live revision: tree root -> blob.
	blob, _ := ws.WriteBlob([]byte("live"))
	tree := object.NewTree()
	tree.Entries["a.txt"] = object.Entry{Name: "a.txt", Kind: object.KindBlob, ID: blob}
	treeID, _ := ws.WriteTree(tree)
	liveSnap, rev, err := ws.Commit(CommitParams{TreeID: treeID, Description: "live", Author: store.Author{Name: "t"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = liveSnap
	_ = cs.SetWAL()
	_ = rev

	// Orphan blob not referenced by any tree.
	orphan, _ := ws.WriteBlob([]byte("orphan"))

	// Dry-run should sweep exactly the orphan.
	res, err := GCRun(cs, GCOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Swept != 1 {
		t.Fatalf("dry-run swept=%d want 1", res.Swept)
	}

	// Real run prunes it (dry-run does not delete) and keeps live objects.
	res2, err := GCRun(cs, GCOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Swept != 1 {
		t.Fatalf("real sweep=%d want 1", res2.Swept)
	}
	ok, _ := repo.ObjectExists(orphan)
	if ok {
		t.Fatalf("orphan should be deleted after GC")
	}
	if exists, _ := repo.ObjectExists(blob); !exists {
		t.Fatalf("live blob should survive GC")
	}
}

func TestVerifyReportsCounts(t *testing.T) {
	repo, cs := newTestRepo(t)
	_ = cs.SetWAL()
	ws := NewWorkspace(repo)
	blob, _ := ws.WriteBlob([]byte("x"))
	tree := object.NewTree()
	tree.Entries["f.txt"] = object.Entry{Name: "f.txt", Kind: object.KindBlob, ID: blob}
	treeID, _ := ws.WriteTree(tree)
	if _, _, err := ws.Commit(CommitParams{TreeID: treeID, Description: "d", Author: store.Author{Name: "t"}}); err != nil {
		t.Fatal(err)
	}
	res, err := Verify(cs)
	if err != nil {
		t.Fatal(err)
	}
	if res.Snapshots != 1 || res.MissingObjects != 0 || res.BrokenSnapshots != 0 {
		t.Fatalf("verify: %+v", res)
	}
}
