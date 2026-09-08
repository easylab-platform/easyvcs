package transfer

import (
	"testing"
	"time"

	"github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/store"
)

// TestIsAncestorAndCheckNonFastForward builds a linear chain of snapshots and
// validates fast-forward vs non-fast-forward detection.
func TestIsAncestorAndCheckNonFastForward(t *testing.T) {
	home := t.TempDir()
	t.Setenv("EASYVCS_HOME", home)
	cs, err := store.OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	_ = cs.SetWAL()
	defer cs.Close()
	repo, err := cs.Create(store.RepoRef{Namespace: "n", Name: "r"})
	if err != nil {
		t.Fatal(err)
	}

	// Build a chain r1 -> r2 -> r3 (+ a divergent r4 from r2).
	seedChain(t, repo, "r1", "r2", "r3")

	// r1 is an ancestor of r3 -> chain order intact. We use exact revision ids.
	ff, err := IsAncestor(repo, "r1", "r3")
	if err != nil {
		t.Fatal(err)
	}
	if !ff {
		t.Fatal("r1 should be an ancestor of r3")
	}
	fwd, err := IsAncestor(repo, "r3", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if fwd {
		t.Fatal("r3 should not be an ancestor of r1")
	}
	// Case: forward r1 -> r3 (fast-forward) should pass.
	if conflicts := CheckNonFastForward(repo, revRefs(t, repo, [][2]string{{"main", "r1"}}), revRefs(t, repo, [][2]string{{"main", "r3"}}), nil); len(conflicts) != 0 {
		t.Fatalf("expected fast-forward allowed, conflicts=%v", conflicts)
	}
	// Case: going backwards r3 -> r1 (non-fast-forward) should be rejected.
	if conflicts := CheckNonFastForward(repo, revRefs(t, repo, [][2]string{{"main", "r3"}}), revRefs(t, repo, [][2]string{{"main", "r1"}}), nil); len(conflicts) != 1 {
		t.Fatalf("expected 1 non-fast-forward conflict, got %v", conflicts)
	}
	// Case: unchanged ref -> no conflict.
	if conflicts := CheckNonFastForward(repo, revRefs(t, repo, [][2]string{{"main", "r3"}}), revRefs(t, repo, [][2]string{{"main", "r3"}}), nil); len(conflicts) != 0 {
		t.Fatalf("expected no conflict for unchanged")
	}
	// Case: new branch -> no conflict.
	if conflicts := CheckNonFastForward(repo, revRefs(t, repo, [][2]string{{"main", "r3"}}), revRefs(t, repo, [][2]string{{"feature", "r1"}}), nil); len(conflicts) != 0 {
		t.Fatalf("expected no conflict for new branch")
	}
}

func seedChain(t *testing.T, repo *store.Repo, ids ...string) {
	t.Helper()
	var parent object.ID
	for _, id := range ids {
		tree := object.NewTree()
		tree.Entries[id] = object.Entry{Name: "f", Kind: object.KindBlob, ID: object.BlobID([]byte(id))}
		treeID := tree.ID()
		_ = repo.WriteObject(&object.Object{Kind: object.KindTree, Tree: tree})
		snap := &store.Snapshot{RevisionID: id, TreeID: treeID, CommitTime: now(), Author: store.Author{Name: "t"}}
		if !parent.IsZero() {
			snap.Parents = []object.ID{parent}
		}
		snap.RevisionHash = store.SnapshotHashFor(snap)
		if err := repo.PutSnapshot(snap); err != nil {
			t.Fatal(err)
		}
		if err := repo.PutRevision(&store.Revision{ID: id, Hash: snap.RevisionHash, Created: now()}); err != nil {
			t.Fatal(err)
		}
		parent = snap.RevisionHash
	}
}

func revRefs(t *testing.T, repo *store.Repo, pairs [][2]string) []*store.Ref {
	t.Helper()
	out := []*store.Ref{}
	for _, p := range pairs {
		out = append(out, &store.Ref{Name: p[0], Kind: store.RefBranch, Target: p[1]})
	}
	return out
}



func now() time.Time { return time.Now().UTC() }
