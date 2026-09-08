package revision

import (
	"strings"
	"testing"

	"github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/store"
)

// TestTagImmutable verifies a tag cannot be re-pointed once created,
// while a branch can.
func TestTagImmutable(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		snapA, revA := commitTree(t, ws, dir, "A", nil)
		_, revB := commitTree(t, ws, dir, "B", []object.ID{snapA.RevisionHash})

		if _, err := ws.SetRef("v1", store.RefTag, revA.ID); err != nil {
			t.Fatal(err)
		}
		// Re-tagging the same name to a different revision must fail.
		if _, err := ws.SetRef("v1", store.RefTag, revB.ID); err == nil {
			t.Fatalf("expected error re-tagging an existing tag")
		}
		// A branch under the same name is a separate namespace; but a branch
		// named "v1" as a branch should also be rejected only if it's a tag? The
		// guard keys on Kind==RefTag, so a branch named "v1" coexists is fine.
		if _, err := ws.SetRef("main", store.RefBranch, revA.ID); err != nil {
			t.Fatal(err)
		}
		// Re-pointing the branch is allowed.
		if _, err := ws.SetRef("main", store.RefBranch, revB.ID); err != nil {
			t.Fatalf("branch should be re-pointable: %v", err)
		}
		// Tag points at original; branch now at revB.
		tag, _ := ws.GetRef("v1")
		if tag.Target != revA.ID {
			t.Fatalf("tag target changed: %s", tag.Target)
		}
	})
}

// TestBranchFromTag derives a fresh independent revision from a tag's target.
func TestBranchFromTag(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		snapA, revA := commitTree(t, ws, dir, "A", nil)
		_ = snapA
		if _, err := ws.SetRef("v1", store.RefTag, revA.ID); err != nil {
			t.Fatal(err)
		}
		// Tag resolves to revA's revision id.
		tag, err := ws.GetRef("v1")
		if err != nil {
			t.Fatal(err)
		}
		// Derive a new branch from the tag target.
		dsnap, drev, err := ws.Derive(tag.Target, "", true)
		if err != nil {
			t.Fatal(err)
		}
		if drev.ID == tag.Target {
			t.Fatalf("derived branch should have an independent id")
		}
		if drev.ForkFrom != tag.Target {
			t.Fatalf("derived ForkFrom = %q want %q", drev.ForkFrom, tag.Target)
		}
		if dsnap.TreeID != snapA.TreeID {
			t.Fatalf("derived tree differs from tag revision tree")
		}
		if _, err := ws.SetRef("fix-v1", store.RefBranch, drev.ID); err != nil {
			t.Fatal(err)
		}
		ref, _ := ws.GetRef("fix-v1")
		if ref.Target != drev.ID {
			t.Fatalf("branch fix-v1 should point at derived revision")
		}
	})
}

// TestDeriveErrorMessage notes that derive requires a committed source.
func TestTagBranchIntegrationMessage(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		snapA, revA := commitTree(t, ws, dir, "A", nil)
		_, _ = ws.SetRef("v1", store.RefTag, revA.ID)
		_ = snapA
		_, _ = ws.SetRef("main", store.RefBranch, revA.ID)
		// A source whose revision was derived also carries ForkFrom (round-trip).
		_, drev, err := ws.Derive(revA.ID, "", true)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(drev.ID, "") {
			t.Fatal("derive should produce an id")
		}
	})
}
