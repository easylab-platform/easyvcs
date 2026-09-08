package revision

import (
	"testing"

	"github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/store"
)

func TestDeriveIndependentIDAndForkFrom(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		// Build a source revision with a message.
		snap, src, err := ws.CommitFromChanges(object.ID{}, []FileChangeSpec{
			{Path: "a.txt", Content: []byte("a\n")},
		}, "source", store.Author{Name: "t"}, "")
		if err != nil {
			t.Fatal(err)
		}
		// Note: CommitFromChanges stores the message on the Snapshot, not the
		// Revision; verify via the snapshot.
		if snap.Description != "source" {
			t.Fatalf("source desc = %q", snap.Description)
		}

		// Derive a branch revision.
		dsnap, drev, err := ws.Derive(src.ID, "", true)
		if err != nil {
			t.Fatal(err)
		}
		// Independent id.
		if drev.ID == src.ID {
			t.Fatalf("derived id must differ from source: %s", drev.ID)
		}
		// ForkFrom records the source.
		if drev.ForkFrom != src.ID {
			t.Fatalf("ForkFrom = %q want %q", drev.ForkFrom, src.ID)
		}
		// Same tree (content clone).
		if dsnap.TreeID != snap.TreeID {
			t.Fatalf("derived tree differs: %s vs %s", dsnap.TreeID, snap.TreeID)
		}
		// Same parents.
		if len(dsnap.Parents) != len(snap.Parents) {
			t.Fatalf("derived parents mismatch")
		}
		// Different snapshot hash (revision id participates in the hash).
		if dsnap.RevisionHash == snap.RevisionHash {
			t.Fatalf("derived snapshot hash must differ (revision id in hash)")
		}
		// Message inherited from source when description not given.
		if dsnap.Description != "source" {
			t.Fatalf("derived desc = %q want source", dsnap.Description)
		}
	})
}

func TestDeriveRequiresMessage(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		// Source with empty commit message (uncommitted placeholder).
		snap, src, err := ws.CommitFromChanges(object.ID{}, []FileChangeSpec{
			{Path: "b.txt", Content: []byte("b\n")},
		}, "", store.Author{Name: "t"}, "")
		if err != nil {
			t.Fatal(err)
		}
		_ = snap

		// Without message and without auto_commit -> error.
		if _, _, err := ws.Derive(src.ID, "", false); err == nil {
			t.Fatalf("expected error for uncommitted source without auto_commit")
		}

		// With explicit message, auto_commit allows finalizing the fork point.
		dsnap, drev, err := ws.Derive(src.ID, "fork msg", true)
		if err != nil {
			t.Fatalf("derive with auto_commit + message should succeed: %v", err)
		}
		if dsnap.Description != "fork msg" {
			t.Fatalf("derived desc = %q", dsnap.Description)
		}
		if drev.ForkFrom != src.ID {
			t.Fatalf("ForkFrom = %q", drev.ForkFrom)
		}

		// No message anywhere, even with auto_commit -> error.
		if _, _, err := ws.Derive(src.ID, "", true); err == nil {
			t.Fatalf("expected error when no message can be resolved")
		}
	})
}

func TestSetDescriptionRewritesMessageKeepsID(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		snap, rev, err := ws.CommitFromChanges(object.ID{}, []FileChangeSpec{
			{Path: "x.txt", Content: []byte("x\n")},
		}, "old message", store.Author{Name: "t"}, "")
		if err != nil {
			t.Fatal(err)
		}
		ns, nrev, err := ws.SetDescription(rev.ID, "new message")
		if err != nil {
			t.Fatal(err)
		}
		// id stable
		if nrev.ID != rev.ID {
			t.Fatalf("id changed: %s != %s", nrev.ID, rev.ID)
		}
		// message updated
		if ns.Description != "new message" {
			t.Fatalf("desc = %q", ns.Description)
		}
		// snapshot hash changed (description in hash)
		if ns.RevisionHash == snap.RevisionHash {
			t.Fatalf("hash should change when message changes")
		}
		// tree unchanged
		if ns.TreeID != snap.TreeID {
			t.Fatalf("tree changed unexpectedly")
		}
		// repointed revision points at new snapshot
		got, err := ws.store.GetRevision(rev.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Hash != ns.RevisionHash {
			t.Fatalf("revision not repointed: %s != %s", got.Hash, ns.RevisionHash)
		}
	})
}
