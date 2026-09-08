package store

import (
	"testing"

	"github.com/easylab-platform/easyvcs/object"
)

// TestUpdateRevisionHashCAS verifies the optimistic-concurrency guard: a CAS
// repoint succeeds when the revision still points at the expected hash and
// returns ErrRevisionChanged when it does not (so a concurrent amend is detected
// instead of being silently overwritten).
func TestUpdateRevisionHashCAS(t *testing.T) {
	cs := newTestCentral(t)
	repo, _ := cs.Create(RepoRef{Namespace: "n", Name: "r"})

	tree := object.BlobID([]byte("t"))
	s1 := &Snapshot{RevisionID: "r1", TreeID: tree, Author: Author{Name: "a"}}
	s1.RevisionHash = SnapshotHashFor(s1)
	_ = repo.PutSnapshot(s1)
	_ = repo.PutRevision(&Revision{ID: "r1", Hash: s1.RevisionHash, Created: now(), ChangedPaths: []string{"f"}})

	// SetDescription/rebase would expect to start from the current hash (s1).
	next := &Snapshot{RevisionID: "r1", TreeID: tree, Author: Author{Name: "a"}, Description: "v2"}
	next.RevisionHash = SnapshotHashFor(next)
	if err := repo.PutSnapshot(next); err != nil {
		t.Fatal(err)
	}

	// CAS with the correct expected hash succeeds.
	if err := repo.UpdateRevisionHashCAS("r1", s1.RevisionHash, next.RevisionHash); err != nil {
		t.Fatalf("CAS with matching expected hash should succeed: %v", err)
	}
	got, _ := repo.GetRevision("r1")
	if got.Hash != next.RevisionHash {
		t.Fatalf("after CAS hash=%s want %s", got.Hash, next.RevisionHash)
	}

	// CAS with a stale expected hash (another writer changed it) must fail.
	stale := &Snapshot{RevisionID: "r1", TreeID: tree, Author: Author{Name: "a"}, Description: "v3"}
	stale.RevisionHash = SnapshotHashFor(stale)
	if err := repo.PutSnapshot(stale); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateRevisionHashCAS("r1", s1.RevisionHash, stale.RevisionHash); err != ErrRevisionChanged {
		t.Fatalf("expected ErrRevisionChanged for stale CAS, got %v", err)
	}
	// The revision is unchanged (still the v2 hash committed by the first CAS).
	got2, _ := repo.GetRevision("r1")
	if got2.Hash != next.RevisionHash {
		t.Fatalf("stale CAS should not mutate revision; hash=%s", got2.Hash)
	}
}
