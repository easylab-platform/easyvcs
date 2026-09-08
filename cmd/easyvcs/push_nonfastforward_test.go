package main

import (
	"testing"

	"github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/revision"
	"github.com/easylab-platform/easyvcs/store"
	"github.com/easylab-platform/easyvcs/transfer"
)

// TestLocalPushRejectsNonFastForward verifies that pushing to a local remote
// that would move a branch backwards is rejected with a NonFastForwardError.
func TestLocalPushRejectsNonFastForward(t *testing.T) {
	srcDir, _, srcRepo := stubLocalRemote(t, "team", "app", "a\n", "c1")

	homeB := t.TempDir()
	t.Setenv("EASYVCS_HOME", homeB)
	csB, _ := store.OpenDefault()
	repoB, _ := csB.Create(store.RepoRef{Namespace: "cl", Name: "app"})
	dirB := t.TempDir()
	_ = store.WriteMarker(dirB, &store.WorkspaceMarker{Repo: repoB.RepoRef(), Home: homeB})
	_ = repoB.PutRemote("origin", srcDir, "")

	// Pull the single revision (c1) so repoB has main -> c1.
	if err := doPull(repoB, []string{"origin", "main"}); err != nil {
		t.Fatalf("pull: %v", err)
	}
	wsB := revision.NewWorkspace(repoB)
	mainRef, _ := wsB.GetRef("main")

	// Now the remote (source) advances: add a child c2 so its main moves forward.
	wsA := revision.NewWorkspace(srcRepo)
	srcMain, _ := wsA.GetRef("main")
	srcSnap, _ := wsA.GetSnapshot(srcMainHash(t, wsA, srcMain.Target))
	_, childRev, err := wsA.CommitFromChanges(srcSnap.RevisionHash, []revision.FileChangeSpec{{Path: "c.txt", Content: []byte("c\n")}}, "c2", store.Author{Name: "t"}, "")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = wsA.SetRef("main", store.RefBranch, childRev.ID)

	// repoB is behind (main is still c1). Pushing its main (c1) onto a remote
	// whose main is at c2 (child of c1) is a fast-forward (c1 is ancestor of c2),
	// so it should NOT be rejected. To get a genuine rejection, repoB must
	// advance on a side branch, then push main -> its tip while the remote main
	// points elsewhere WITHOUT ancestry. The simplest genuine non-FF: push from
	// repoB a branch whose target is NOT an ancestor of the remote's current
	// target. Create a divergent commit in repoB.
	divSnap, _ := wsA.GetSnapshot(srcMainHash(t, wsA, childRev.ID)) // source is ahead
	_ = divSnap
	// repoB creates a divergent revision on top of c1 (editing a file).
	_, divRev, err := wsB.CommitFromChanges(mainRefTargetToHash(t, wsB, mainRef.Target), []revision.FileChangeSpec{{Path: "z.txt", Content: []byte("z\n")}}, "div", store.Author{Name: "t"}, "")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = wsB.SetRef("main", store.RefBranch, divRev.ID)

	rem, _ := repoB.GetRemote("origin")
	target, err := openLocalRepo(rem.URL)
	if err != nil {
		t.Fatal(err)
	}
	bundle, _ := transfer.CollectAll(repoB)
	conflicts := transfer.CheckNonFastForward(target, serverRefsOf(target), bundle.Refs, bundle)
	if len(conflicts) == 0 {
		// The remote main is at cp2 (child of c1 which is ancestor of divRev? no —
		// divRev's parent is c1, remote main is c2 whose parent is c1. divRev and
		// c2 are siblings, neither is ancestor => non-FF. Assert at least one.
		t.Fatalf("expected non-fast-forward conflict for divergent local/remote main")
	}
	// Also assert the doPushLocal path surfaces the same error.
	err = doPushLocal(repoB, rem)
	if err == nil {
		t.Fatalf("doPushLocal should reject non-fast-forward")
	}
	if _, ok := err.(*transfer.NonFastForwardError); !ok {
		t.Fatalf("expected NonFastForwardError, got %T %v", err, err)
	}
}

func srcMainHash(t *testing.T, ws *revision.Workspace, revID string) object.ID {
	t.Helper()
	rev, err := ws.GetRevision(revID)
	if err != nil {
		t.Fatal(err)
	}
	return rev.Hash
}

func serverRefsOf(repo *store.Repo) []*store.Ref {
	refs, _ := repo.ListRefs()
	return refs
}
