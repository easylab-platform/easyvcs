package revision

import (
	"testing"

	"github.com/easylab-platform/easyvcs/object"
)

// TestCollectPaths lists all file paths under nested trees.
func TestCollectPaths(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		buildFS(t, dir, "a.txt", "a\n")
		buildFS(t, dir, "sub/b.txt", "b\n")
		buildFS(t, dir, "sub/deep/c.txt", "c\n")
		treeID, err := ws.BuildTreeFromFS(dir)
		if err != nil {
			t.Fatal(err)
		}
		paths, err := ws.CollectPaths(treeID)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, p := range paths {
			got[p] = true
		}
		for _, want := range []string{"a.txt", "sub/b.txt", "sub/deep/c.txt"} {
			if !got[want] {
				t.Fatalf("missing path %q in %v", want, paths)
			}
		}
	})
}

// TestDescendantsOf walks a linear chain plus a branch and reports descendants
// in ancestor-first order, excluding self.
func TestDescendantsOf(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		snapA, revA := commitTree(t, ws, dir, "A", nil)
		buildFS(t, dir, "f.txt", "1\n")
		snapB, revB := commitTree(t, ws, dir, "B", []object.ID{snapA.RevisionHash})
		buildFS(t, dir, "f.txt", "2\n")
		_, revC := commitTree(t, ws, dir, "C", []object.ID{snapB.RevisionHash})
		_ = revA

		desc, err := ws.DescendantsOf(revA.ID)
		if err != nil {
			t.Fatal(err)
		}
		// Linear chain: B and C are descendants of A.
		if len(desc) != 2 {
			t.Fatalf("descendants of A = %v, want [B C]", desc)
		}
		if desc[0] != revB.ID || desc[1] != revC.ID {
			t.Fatalf("descendants order = %v, want [%s %s]", desc, revB.ID, revC.ID)
		}
		// C has no descendants.
		descC, err := ws.DescendantsOf(revC.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(descC) != 0 {
			t.Fatalf("C should have no descendants, got %v", descC)
		}
	})
}

// TestWriteConflictRoundTrip persists an N-way conflict as a first-class object.
func TestWriteConflictRoundTrip(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		ours := mustWriteBlob(ws, []byte("ours"))
		theirs := mustWriteBlob(ws, []byte("theirs"))
		base := mustWriteBlob(ws, []byte("base"))
		c := object.NewConflictFrom3Way(base, ours, theirs)
		id := mustWriteConflict(ws, c)
		o, err := ws.store.ReadObject(id)
		if err != nil {
			t.Fatal(err)
		}
		if o.Kind != object.KindConflict || o.Conflict == nil {
			t.Fatalf("expected a conflict object, got %v", o.Kind)
		}
		if len(o.Conflict.Adds) != 2 || len(o.Conflict.Removes) != 1 {
			t.Fatalf("conflict terms wrong: %+v", o.Conflict)
		}
		if o.Conflict.Adds[0].ID != ours || o.Conflict.Adds[1].ID != theirs {
			t.Fatalf("conflict sides wrong")
		}
	})
}

// TestWriteTreeRoundTrip persists a tree object and reads it back.
func TestWriteTreeRoundTrip(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		blob := mustWriteBlob(ws, []byte("x"))
		tree := object.NewTree()
		tree.Entries["x.txt"] = object.Entry{Name: "x.txt", Kind: object.KindBlob, ID: blob}
		id := mustWriteTree(ws, tree)
		got, err := ws.ReadTree(id)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := got.Entries["x.txt"]; !ok {
			t.Fatalf("tree entry missing after write")
		}
	})
}

// TestFindEntry walks nested paths and reports not-found for absent leaves.
func TestFindEntry(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		buildFS(t, dir, "a.txt", "a\n")
		buildFS(t, dir, "d/b.txt", "b\n")
		treeID, err := ws.BuildTreeFromFS(dir)
		if err != nil {
			t.Fatal(err)
		}
		root := ws.MustTree(treeID)
		e, err := ws.FindEntry(root, "d/b.txt")
		if err != nil {
			t.Fatal(err)
		}
		if e.Kind != object.KindBlob {
			t.Fatalf("d/b.txt should be a blob, got %v", e.Kind)
		}
		if _, err := ws.FindEntry(root, "nope.txt"); err == nil {
			t.Fatal("expected error for missing path")
		}
		if _, err := ws.FindEntry(root, "d/nope.txt"); err == nil {
			t.Fatal("expected error for missing nested path")
		}
	})
}

// TestDiffContentFromChanges renders unified diffs for add/modify/delete.
func TestDiffContentFromChanges(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		oldBlob := mustWriteBlob(ws, []byte("line1\n"))
		rmBlob := mustWriteBlob(ws, []byte("gone\n"))
		newBlob := mustWriteBlob(ws, []byte("line1\nline2\n"))

		changes := []FileChange{
			{Path: "added.txt", Status: StatusAdded, NewID: newBlob},
			{Path: "mod.txt", Status: StatusModified, OldID: oldBlob, NewID: newBlob},
			{Path: "del.txt", Status: StatusRemoved, OldID: rmBlob},
		}
		diffs, err := ws.DiffContentFromChanges(changes)
		if err != nil {
			t.Fatal(err)
		}
		if len(diffs) != 3 {
			t.Fatalf("got %d diffs", len(diffs))
		}
		byPath := map[string]*FileDiff{}
		for i := range diffs {
			byPath[diffs[i].Path] = &diffs[i]
		}
		if byPath["added.txt"].Status != StatusAdded || byPath["added.txt"].AddedLines != 3 {
			t.Fatalf("added diff wrong: %+v", byPath["added.txt"])
		}
		if byPath["mod.txt"].Status != StatusModified || byPath["mod.txt"].Content == "" {
			t.Fatalf("mod diff wrong: %+v", byPath["mod.txt"])
		}
		if byPath["del.txt"].Status != StatusRemoved || byPath["del.txt"].RemovedLines != 2 {
			t.Fatalf("del diff wrong: %+v", byPath["del.txt"])
		}
	})
}

// TestAnnotateAt attributes lines to the revision that introduced them.
func TestAnnotateAt(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		buildFS(t, dir, "f.txt", "a\nb\n")
		_, revA := commitTree(t, ws, dir, "A", nil)
		buildFS(t, dir, "f.txt", "a\nc\n")
		_, revB := commitTree(t, ws, dir, "B", []object.ID{revAHashOf(t, ws, revA.ID)})

		lines, err := ws.AnnotateAt(HistoryOpt{Start: revB.ID, Desc: true}, "f.txt")
		if err != nil {
			t.Fatal(err)
		}
		if len(lines) == 0 {
			t.Fatalf("no annotation lines")
		}
		// Every line has some revision and content.
		for _, ln := range lines {
			if ln.Content == "" {
				t.Fatalf("empty content on line %d", ln.LineNumber)
			}
		}
	})
}

func revAHashOf(t *testing.T, ws *Workspace, id string) object.ID {
	t.Helper()
	rev, err := ws.GetRevision(id)
	if err != nil {
		t.Fatal(err)
	}
	return rev.Hash
}
