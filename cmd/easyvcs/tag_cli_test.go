package main

import (
	"os"
	"testing"

	"github.com/easylab-platform/easyvcs/revision"
	"github.com/easylab-platform/easyvcs/store"
)

// TestCLITagImmutableAndBranchFromTag exercises the tag CLI: creating a tag,
// refusing to re-tag the same name, and deriving a branch from the tag.
func TestCLITagImmutableAndBranchFromTag(t *testing.T) {
	dir := t.TempDir()
	c, repo := newCLIRepo(t, dir)

	write(t, dir, "a.txt", "a\n")
	os.Args = []string{"easyvcs", "commit"}
	cmdCommit(c)
	rid := newestRevisionID(t, repo)

	// Create a tag.
	os.Args = []string{"easyvcs", "tag", "v1", rid}
	captureStdout(t, func() { cmdTag(c) })
	tag, err := repo.GetRef("v1")
	if err != nil || tag == nil || tag.Kind != store.RefTag {
		t.Fatalf("tag v1 not created: %v", err)
	}

	// Re-tagging the same name against a second (new) revision fails. cmdTag
	// calls os.Exit on error, so assert the immutability via the revision API
	// (also covered by revision.TestTagImmutable).
	write(t, dir, "b.txt", "b\n")
	os.Args = []string{"easyvcs", "commit"}
	cmdCommit(c)
	rid2 := newestRevisionID(t, repo)
	if _, err := revision.NewWorkspace(repo).SetRef("v1", store.RefTag, rid2); err == nil {
		t.Fatalf("expected re-tag to fail")
	}

	// Branch from the tag: `branch fix-v1 v1` derives an independent revision.
	os.Args = []string{"easyvcs", "branch", "fix-v1", "v1"}
	captureStdout(t, func() { cmdBranch(c) })
	b, err := repo.GetRef("fix-v1")
	if err != nil || b == nil || b.Kind != store.RefBranch {
		t.Fatalf("branch fix-v1 not created: %v", err)
	}
	if b.Target == tag.Target {
		t.Fatalf("branch from tag should have an independent id, got same as tag")
	}
	ws := revision.NewWorkspace(repo)
	brev, err := ws.GetRevision(b.Target)
	if err != nil {
		t.Fatal(err)
	}
	if brev.ForkFrom != tag.Target {
		t.Fatalf("branch-from-tag ForkFrom = %q want %q", brev.ForkFrom, tag.Target)
	}
}
