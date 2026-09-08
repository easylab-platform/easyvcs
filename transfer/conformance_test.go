package transfer

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/store"
)

// seedTransferRepo builds a repo with a single snapshot tree (one blob + one
// nested tree) so Collect/CollectObjectsWhere/round-trips can be exercised.
func seedTreeRepo(t *testing.T, repo *store.Repo) object.ID {
	t.Helper()
	b1 := &object.Object{Kind: object.KindBlob, Blob: []byte("hello")}
	b2 := &object.Object{Kind: object.KindBlob, Blob: []byte("world")}
	_ = repo.WriteObject(b1)
	_ = repo.WriteObject(b2)
	sub := object.NewTree()
	sub.Entries["n.txt"] = object.Entry{Name: "n.txt", Kind: object.KindBlob, ID: b2.ID()}
	root := object.NewTree()
	root.Entries["a.txt"] = object.Entry{Name: "a.txt", Kind: object.KindBlob, ID: b1.ID()}
	root.Entries["dir"] = object.Entry{Name: "dir", Kind: object.KindTree, ID: sub.ID()}
	_ = repo.WriteObject(&object.Object{Kind: object.KindTree, Tree: sub})
	_ = repo.WriteObject(&object.Object{Kind: object.KindTree, Tree: root})
	return root.ID()
}

// TestCollectObjectsWhere verifies the delta-selection object collector: with a
// hasObject that already knows one blob, that object is skipped.
func TestConformanceCollectObjectsWhere(t *testing.T) {
	home := t.TempDir()
	t.Setenv("EASYVCS_HOME", home)
	cs, _ := store.OpenDefault()
	defer cs.Close()
	repo, _ := cs.Create(store.RepoRef{Namespace: "n", Name: "r"})
	rootID := seedTreeRepo(t, repo)

	// hasObject knows the a.txt blob => it should NOT be re-sent (but sub/root
	// trees and the world blob still are).
	known := map[string]bool{}
	known[object.BlobID([]byte("hello")).String()] = true
	recs, err := CollectObjectsWhere(repo, rootID, func(id object.ID) bool { return known[id.String()] })
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 {
		t.Fatalf("expected 3 objects (world blob + 2 trees) after skipping known, got %d", len(recs))
	}
	// With no skip, collect all reachable objects (b1+b2 blobs + sub+root).
	all, err := CollectObjectsWhere(repo, rootID, func(object.ID) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("expected 4 reachable objects, got %d", len(all))
	}
}

// TestCollectWithHaveSkipsKnownRevisions verifies Collect omits revisions the
// destination already has.
func TestCollectWithHaveSkipsKnownRevisions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("EASYVCS_HOME", home)
	cs, _ := store.OpenDefault()
	defer cs.Close()
	repo, _ := cs.Create(store.RepoRef{Namespace: "n", Name: "r"})
	treeID := seedTreeRepo(t, repo)
	s := &store.Snapshot{RevisionID: "revA", TreeID: treeID, Author: store.Author{Name: "t"}}
	s.RevisionHash = store.SnapshotHashFor(s)
	_ = repo.PutSnapshot(s)
	_ = repo.PutRevision(&store.Revision{ID: "revA", Hash: s.RevisionHash})
	_ = repo.PutRef(&store.Ref{Name: "main", Kind: store.RefBranch, Target: "revA"})

	// have=revA -> Collect returns empty revisions.
	b, err := Collect(repo, []string{"revA"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Revisions) != 0 {
		t.Fatalf("should skip known revision, got %d", len(b.Revisions))
	}
	// have empty -> returns revA.
	b2, _ := Collect(repo, nil, nil)
	if len(b2.Revisions) != 1 {
		t.Fatalf("expected revA when have empty, got %d", len(b2.Revisions))
	}
}

// TestFilterBundleByWant keeps only wanted revisions + their snapshots.
func TestFilterBundleByWant(t *testing.T) {
	home := t.TempDir()
	t.Setenv("EASYVCS_HOME", home)
	cs, _ := store.OpenDefault()
	defer cs.Close()
	repo, _ := cs.Create(store.RepoRef{Namespace: "n", Name: "r"})
	treeID := seedTreeRepo(t, repo)
	s := &store.Snapshot{RevisionID: "revA", TreeID: treeID, Author: store.Author{Name: "t"}}
	s.RevisionHash = store.SnapshotHashFor(s)
	_ = repo.PutSnapshot(s)
	_ = repo.PutRevision(&store.Revision{ID: "revA", Hash: s.RevisionHash})

	b, _ := Collect(repo, nil, nil)
	kept := FilterBundleWant(b, map[string]bool{"revA": true})
	if len(kept.Revisions) != 1 {
		t.Fatalf("expected kept 1 revision, got %d", len(kept.Revisions))
	}
	// A want set that matches nothing yields no revisions.
	empty := FilterBundleWant(b, map[string]bool{"nope": true})
	if len(empty.Revisions) != 0 {
		t.Fatalf("expected 0 revisions for unmatched want, got %d", len(empty.Revisions))
	}
}

// TestBundleBinaryRoundTrip verifies MarshalBinary/UnmarshalBinary/Compress
// preserve the bundle (revisions + refs + objects) — including the new
// ExpectedRefs and that the wire frame is not broken.
func TestBundleBinaryRoundTrip(t *testing.T) {
	b := &Bundle{Version: Version, Repo: store.RepoRef{Namespace: "n", Name: "r"},
		Revisions:  []*store.Revision{{ID: "revA", Hash: object.BlobID([]byte("h"))}},
		Refs:       []*store.Ref{{Name: "main", Kind: store.RefBranch, Target: "revA"}},
		Objects:    []ObjectRecord{{Kind: object.KindBlob, Content: []byte("data")}},
		ExpectedRefs: []*store.Ref{{Name: "main", Kind: store.RefBranch, Target: "expected"}},
	}
	// JSON round-trip (uses json tag "changes" for BackwardCompat).
	raw, _ := json.Marshal(b)
	var back Bundle
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Revisions) != 1 || len(back.ExpectedRefs) != 1 {
		t.Fatalf("json round-trip mismatch: %+v", back)
	}
	// Binary + gzip round-trip.
	enc, err := CompressBundle(b)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := UnmarshalBinary(enc)
	if err != nil {
		t.Fatal(err)
	}
	if len(dec.Revisions) != 1 || len(dec.Objects) != 1 || len(dec.ExpectedRefs) != 1 {
		t.Fatalf("binary round-trip mismatch: %+v", dec)
	}
	if dec.Revisions[0].ID != "revA" {
		t.Fatalf("revision id lost")
	}
	if dec.Objects[0].Content == nil || !bytes.Equal(dec.Objects[0].Content, []byte("data")) {
		t.Fatalf("object content lost")
	}
}
