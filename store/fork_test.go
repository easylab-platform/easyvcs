package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/easylab-platform/easyvcs/object"
)

// seedForkSource creates a source repo with two revisions chained (r1 ->
// r2/r3 divergent) plus a branch and a tag, so a fork can be validated for
// identity remapping and content preservation.
func seedForkSource(t *testing.T, cs *CentralStore) (*Repo, *object.ID, *object.ID) {
	t.Helper()
	repo, err := cs.Create(RepoRef{Namespace: "fork", Name: "src"})
	if err != nil {
		t.Fatal(err)
	}
	// Two blobs + a tree referencing them.
	b1 := &object.Object{Kind: object.KindBlob, Blob: []byte("hello\n")}
	b2 := &object.Object{Kind: object.KindBlob, Blob: []byte("world\n")}
	_ = repo.WriteObject(b1)
	_ = repo.WriteObject(b2)
	tree := object.NewTree()
	tree.Entries["a.txt"] = object.Entry{Name: "a.txt", Kind: object.KindBlob, ID: b1.ID()}
	tree.Entries["b.txt"] = object.Entry{Name: "b.txt", Kind: object.KindBlob, ID: b2.ID()}
	treeID := tree.ID()
	_ = repo.WriteObject(&object.Object{Kind: object.KindTree, Tree: tree})

	// Root snapshot + revision.
	s1 := &Snapshot{RevisionID: "rev-root", TreeID: treeID, Description: "root",
		Author: Author{Name: "t", Email: "t@x"}, CommitTime: time.Now().UTC()}
	s1.RevisionHash = SnapshotHashFor(s1)
	if err := repo.PutSnapshot(s1); err != nil {
		t.Fatal(err)
	}
	if err := repo.PutRevision(&Revision{ID: "rev-root", Hash: s1.RevisionHash, Created: time.Now().UTC(), ChangedPaths: []string{"a.txt"}}); err != nil {
		t.Fatal(err)
	}
	// Child snapshot + revision pointing at root.
	s2 := &Snapshot{RevisionID: "rev-child", TreeID: treeID, Parents: []object.ID{s1.RevisionHash},
		Description: "child", Author: Author{Name: "t", Email: "t@x"}, CommitTime: time.Now().UTC()}
	s2.RevisionHash = SnapshotHashFor(s2)
	if err := repo.PutSnapshot(s2); err != nil {
		t.Fatal(err)
	}
	if err := repo.PutRevision(&Revision{ID: "rev-child", Hash: s2.RevisionHash, Created: time.Now().UTC(), ChangedPaths: []string{"b.txt"}}); err != nil {
		t.Fatal(err)
	}
	// Refs.
	if err := repo.PutRef(&Ref{Name: "main", Kind: RefBranch, Target: "rev-child"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.PutRef(&Ref{Name: "v1", Kind: RefTag, Target: "rev-root"}); err != nil {
		t.Fatal(err)
	}
	return repo, &s1.RevisionHash, &s2.RevisionHash
}

func TestForkRemapsIdentities(t *testing.T) {
	cs := newTestCentral(t)
	_ = cs.SetWAL()
	src, _, _ := seedForkSource(t, cs)

	dst, err := cs.Fork(src.RepoRef(), RepoRef{Namespace: "fork", Name: "dst"})
	if err != nil {
		t.Fatal(err)
	}

	srcRevIDs, _ := src.ListRevisions()
	dstRevIDs, _ := dst.ListRevisions()

	// No revision id may be shared between the fork and its parent.
	seen := map[string]bool{}
	for _, r := range srcRevIDs {
		seen[r.ID] = true
	}
	for _, r := range dstRevIDs {
		if seen[r.ID] {
			t.Fatalf("fork shares revision id %s with source (identity not remapped)", r.ID)
		}
	}
	if len(dstRevIDs) != len(srcRevIDs) {
		t.Fatalf("fork revision count %d != source %d", len(dstRevIDs), len(srcRevIDs))
	}
	// Content must be preserved: reach the tree of a forked revision and verify
	// it reads a.txt/b.txt blobs.
	dstRev, err := dst.GetRevision(dstRevIDs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := dst.GetSnapshot(dstRev.Hash)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := dst.getTree(snap.TreeID)
	if err != nil {
		t.Fatalf("forked tree not readable: %v", err)
	}
	if _, ok := tree.Entries["a.txt"]; !ok {
		t.Fatal("fork lost a.txt")
	}
	if _, ok := tree.Entries["b.txt"]; !ok {
		t.Fatal("fork lost b.txt")
	}
}

func TestForkRefsPointAtNewIDs(t *testing.T) {
	cs := newTestCentral(t)
	_ = cs.SetWAL()
	src, _, _ := seedForkSource(t, cs)
	dst, err := cs.Fork(src.RepoRef(), RepoRef{Namespace: "fork", Name: "dst"})
	if err != nil {
		t.Fatal(err)
	}
	mainRef, err := dst.GetRef("main")
	if err != nil || mainRef == nil {
		t.Fatalf("fork main ref missing: %v", err)
	}
	// The target must be a fork revision id (not the source's "rev-child").
	if mainRef.Target == "rev-child" {
		t.Fatal("fork ref still points at source revision id")
	}
	// The referenced revision must actually exist in the fork.
	if _, err := dst.GetRevision(mainRef.Target); err != nil {
		t.Fatalf("fork main ref points at %s which does not exist: %v", mainRef.Target, err)
	}
	tagRef, err := dst.GetRef("v1")
	if err != nil || tagRef == nil {
		t.Fatalf("fork v1 tag missing: %v", err)
	}
	if _, err := dst.GetRevision(tagRef.Target); err != nil {
		t.Fatalf("fork v1 tag target %s invalid: %v", tagRef.Target, err)
	}
}

func TestForkSnapshotHashesDiffer(t *testing.T) {
	cs := newTestCentral(t)
	_ = cs.SetWAL()
	src, sh1, sh2 := seedForkSource(t, cs)
	_ = sh1
	_ = sh2
	dst, err := cs.Fork(src.RepoRef(), RepoRef{Namespace: "fork", Name: "dst"})
	if err != nil {
		t.Fatal(err)
	}
	// Every forked snapshot hash must differ from source snapshot hashes.
	srcSnaps := map[string]bool{}
	revs, _ := src.ListRevisions()
	for _, r := range revs {
		srcSnaps[r.Hash.String()] = true
	}
	drevs, _ := dst.ListRevisions()
	count := 0
	for _, r := range drevs {
		if srcSnaps[r.Hash.String()] {
			t.Fatalf("fork snapshot hash %s equals a source hash", r.Hash)
		}
		count++
	}
	if count == 0 {
		t.Fatal("fork has no revisions")
	}
}

// getTree is a test-only helper to read a tree object.
func (r *Repo) getTree(id object.ID) (*object.Tree, error) {
	o, err := r.ReadObject(id)
	if err != nil {
		return nil, err
	}
	if o.Kind != object.KindTree || o.Tree == nil {
		return nil, fmt.Errorf("not a tree")
	}
	return o.Tree, nil
}
