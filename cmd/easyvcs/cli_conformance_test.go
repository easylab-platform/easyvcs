package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/revision"
	"github.com/easylab-platform/easyvcs/store"
)

// TestCLICheckoutAndResolve covers two previously-untested commands: checkout
// (materialize a snapshot into a dir + repoint the workspace marker) and
// resolve (pick a conflict side, revision id stable).
func TestCLICheckoutAndResolve(t *testing.T) {
	dir := t.TempDir()
	c, repo := newCLIRepo(t, dir)

	write(t, dir, "a.txt", "1\n")
	os.Args = []string{"easyvcs", "commit"}
	cmdCommit(c)
	rev := newestRevisionID(t, repo)
	ws := revision.NewWorkspace(repo)
	rv, _ := ws.GetRevision(rev)
	snap, _ := ws.GetSnapshot(rv.Hash)

	// checkout into a sibling dir and verify the file materialized.
	dest := t.TempDir()
	os.Args = []string{"easyvcs", "checkout", snap.RevisionHash.String(), dest}
	captureStdout(t, func() { cmdCheckout(c) })
	data, err := os.ReadFile(filepath.Join(dest, "a.txt"))
	if err != nil || string(data) != "1\n" {
		t.Fatalf("checkout file: %v %q", err, data)
	}

	// Make a conflict: create base, ours, theirs which collide on the same path,
	// rebase ours onto theirs to produce a conflict, then resolve to side 0.
	// We drive the revision API directly (rebase produces a conflict object).
	base := object.BlobID([]byte("base\n"))
	ours := object.BlobID([]byte("ours\n"))
	theirs := object.BlobID([]byte("theirs\n"))
	confID := mustWriteConflict(t, ws, object.NewConflictFrom3Way(base, ours, theirs))
	// Build a tree holding the conflict and commit it as a revision.
	confTree := object.NewTree()
	confTree.Entries["conflict.txt"] = object.Entry{Name: "conflict.txt", Kind: object.KindConflict, ID: confID}
	cid := mustWriteTree(t, ws, confTree)
	snap2, ch2, err := ws.Commit(revision.CommitParams{TreeID: cid, Description: "conf", Author: store.Author{Name: "t"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = snap2
	// Verify the tree carries a conflict, then resolve via the CLI.
	atoms, err := ws.ConflictsInTree(cid)
	if err != nil || len(atoms) == 0 {
		t.Fatalf("expected conflict atoms: %v %v", atoms, err)
	}
	os.Args = []string{"easyvcs", "resolve", ch2.ID, "conflict.txt", "--side", "0"}
	captureStdout(t, func() { cmdResolve(c) })
	// After resolve the revision is repointed to a new snapshot; re-fetch it.
	fresh, err := ws.GetRevision(ch2.ID)
	if err != nil {
		t.Fatal(err)
	}
	rsnap, err := ws.GetSnapshot(fresh.Hash)
	if err != nil {
		t.Fatal(err)
	}
	tree := ws.MustTree(rsnap.TreeID)
	entry, err := ws.FindEntry(tree, "conflict.txt")
	if err != nil || entry.Kind != object.KindBlob {
		t.Fatalf("expected resolved blob, got %v %v", entry.Kind, err)
	}
}

// TestCLIFetchPullPushLocal drives fetch/pull/push over a local remote entirely
// through the CLI commands, using a real second store as the remote.
func TestCLIFetchPullPushLocal(t *testing.T) {
	// Remote store: create a workspace dir with a committed revision + main.
	srcHome := t.TempDir()
	t.Setenv("EASYVCS_HOME", srcHome)
	csA, _ := store.OpenDefault()
	_ = csA.SetWAL()
	srcRepo, _ := csA.Create(store.RepoRef{Namespace: "team", Name: "app"})
	wsA := revision.NewWorkspace(srcRepo)
	snapA, revA, err := wsA.CommitFromChanges(object.ID{}, []revision.FileChangeSpec{{Path: "a.txt", Content: []byte("a\n")}}, "cA", store.Author{Name: "t"}, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = snapA
	_, _ = wsA.SetRef("main", store.RefBranch, revA.ID)
	srcDir := t.TempDir()
	_ = store.WriteMarker(srcDir, &store.WorkspaceMarker{Repo: srcRepo.RepoRef(), Home: srcHome})

	// Client store.
	dstHome := t.TempDir()
	t.Setenv("EASYVCS_HOME", dstHome)
	c := &ctx{cs: mustOpen(t)}
	dir := t.TempDir()
	orig, _ := os.Getwd()
	_ = os.Chdir(dir)
	defer func() { _ = os.Chdir(orig) }()

	os.Args = []string{"easyvcs", "init", "team/app"}
	cmdInit(c)
	os.Args = []string{"easyvcs", "remote", "add", "origin", srcDir}
	captureStdout(t, func() { cmdRemote(c) })

	// fetch (records origin.main), then pull (creates main), then push (idempotent).
	os.Args = []string{"easyvcs", "fetch", "origin"}
	captureStdout(t, func() { cmdFetch(c) })
	os.Args = []string{"easyvcs", "pull", "origin", "main"}
	captureStdout(t, func() { cmdPull(c) })

	repo := openFirstRepo(t, c.cs)
	mainRef, err := repo.GetRef("main")
	if err != nil || mainRef == nil {
		t.Fatalf("main not created after pull: %v", err)
	}
	if mainRef.Target != revA.ID {
		t.Fatalf("pull main = %s want %s", mainRef.Target, revA.ID)
	}

	// Push a new local revision back to the remote and verify it lands there.
	ws := revision.NewWorkspace(repo)
	cur, _ := ws.GetRevision(mainRef.Target)
	csnap, _ := ws.GetSnapshot(cur.Hash)
	_, newRev, err := ws.CommitFromChanges(csnap.RevisionHash, []revision.FileChangeSpec{{Path: "b.txt", Content: []byte("b\n")}}, "cB", store.Author{Name: "t"}, "")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = ws.SetRef("main", store.RefBranch, newRev.ID)
	os.Args = []string{"easyvcs", "push", "origin"}
	captureStdout(t, func() { cmdPush(c) })

	// Remote should now have the pushed revision.
	srcRepo2, _ := csA.OpenRepo(store.RepoRef{Namespace: "team", Name: "app"})
	if _, err := srcRepo2.GetRevision(newRev.ID); err != nil {
		t.Fatalf("remote missing pushed revision %s: %v", newRev.ID, err)
	}
}

