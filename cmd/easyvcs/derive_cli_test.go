package main

import (
	"os"
	"strings"
	"testing"

	"github.com/easylab-platform/easyvcs/store"
)

// newCLIRepo initializes a repo + workspace in a temp dir and returns the ctx
// plus the repo handle and the newest revision id (via commit).
func newCLIRepo(t *testing.T, dir string) (*ctx, *store.Repo) {
	t.Helper()
	setHome(t)
	orig, _ := os.Getwd()
	_ = os.Chdir(dir)
	t.Cleanup(func() { _ = os.Chdir(orig) })

	c := &ctx{cs: mustOpen(t)}
	os.Args = []string{"easyvcs", "init", "team/app"}
	cmdInit(c)
	repo := openFirstRepo(t, c.cs)
	return c, repo
}

// openFirstRepo opens the first repository in the central store.
func openFirstRepo(t *testing.T, cs *store.CentralStore) *store.Repo {
	t.Helper()
	repos, err := cs.List()
	if err != nil || len(repos) == 0 {
		t.Fatalf("no repos: %v", err)
	}
	repo, err := cs.OpenRepo(repos[0])
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

// newestRevisionID returns the newest revision's id (list order = newest first).
func newestRevisionID(t *testing.T, repo *store.Repo) string {
	t.Helper()
	revs, err := repo.ListRevisions()
	if err != nil || len(revs) == 0 {
		t.Fatalf("no revisions: %v", err)
	}
	return revs[0].ID
}

// TestCLIBranchDerivesIndependentRevision: `branch` on a fresh name derives a
// new, independent revision id (ForkFrom the source) instead of pointing at the
// source, so in-place amends don't leak across branches.
func TestCLIBranchDerivesIndependentRevision(t *testing.T) {
	dir := t.TempDir()
	c, repo := newCLIRepo(t, dir)

	write(t, dir, "a.txt", "1\n")
	os.Args = []string{"easyvcs", "commit"}
	cmdCommit(c)
	srcID := newestRevisionID(t, repo)

	os.Args = []string{"easyvcs", "branch", "feature", srcID}
	out := captureStdout(t, func() { cmdBranch(c) })
	if !strings.Contains(out, "feature") {
		t.Fatalf("branch output missing name: %q", out)
	}

	ref, err := repo.GetRef("feature")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Target == srcID {
		t.Fatalf("branch should derive a distinct revision, but points at source %s", srcID)
	}
	derived, err := repo.GetRevision(ref.Target)
	if err != nil {
		t.Fatal(err)
	}
	if derived.ForkFrom != srcID {
		t.Fatalf("derived ForkFrom = %q, want %q", derived.ForkFrom, srcID)
	}
}

// TestCLIBranchWithMessageOverridesContentMessage checks that deriving a branch
// with an explicit --message overrides the (inherited) source message.
func TestCLIBranchWithMessageOverrides(t *testing.T) {
	dir := t.TempDir()
	c, repo := newCLIRepo(t, dir)

	write(t, dir, "a.txt", "1\n")
	os.Args = []string{"easyvcs", "commit"}
	cmdCommit(c)
	srcID := newestRevisionID(t, repo)

	os.Args = []string{"easyvcs", "branch", "feature2", srcID, "--message", "custom message"}
	captureStdout(t, func() { cmdBranch(c) })

	ref, err := repo.GetRef("feature2")
	if err != nil {
		t.Fatal(err)
	}
	derived, err := repo.GetRevision(ref.Target)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := repo.GetSnapshot(derived.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Description != "custom message" {
		t.Fatalf("derived desc = %q, want override", snap.Description)
	}
}

// TestCLIBranchRepointKeepsTarget verifies re-running `branch` on an existing
// name does NOT derive a second revision; it repoints to the same target.
func TestCLIBranchRepointKeepsTarget(t *testing.T) {
	dir := t.TempDir()
	c, repo := newCLIRepo(t, dir)

	write(t, dir, "a.txt", "1\n")
	os.Args = []string{"easyvcs", "commit"}
	cmdCommit(c)
	srcID := newestRevisionID(t, repo)

	os.Args = []string{"easyvcs", "branch", "feature", srcID}
	captureStdout(t, func() { cmdBranch(c) })
	ref1, _ := repo.GetRef("feature")
	derived1 := ref1.Target

	// Second branch on the same name: should keep derived1, not create a new one.
	os.Args = []string{"easyvcs", "branch", "feature", srcID}
	captureStdout(t, func() { cmdBranch(c) })
	ref2, _ := repo.GetRef("feature")
	if ref2.Target != derived1 {
		t.Fatalf("repoint should keep target: %s != %s", ref2.Target, derived1)
	}
}

// TestCLIMessageRewritesMessage: `message` keeps the id and updates the text.
func TestCLIMessageRewritesMessage(t *testing.T) {
	dir := t.TempDir()
	c, repo := newCLIRepo(t, dir)

	write(t, dir, "a.txt", "1\n")
	os.Args = []string{"easyvcs", "commit"}
	cmdCommit(c)
	srcID := newestRevisionID(t, repo)

	before, _ := repo.GetRevision(srcID)
	snapBefore, _ := repo.GetSnapshot(before.Hash)

	os.Args = []string{"easyvcs", "message", srcID, "new title"}
	out := captureStdout(t, func() { cmdMessage(c) })
	if !strings.Contains(out, "id unchanged") {
		t.Fatalf("message output: %q", out)
	}

	after, _ := repo.GetRevision(srcID)
	if after.ID != before.ID {
		t.Fatalf("id changed: %s != %s", after.ID, before.ID)
	}
	snapAfter, _ := repo.GetSnapshot(after.Hash)
	if snapAfter.Description != "new title" {
		t.Fatalf("message not updated: %q", snapAfter.Description)
	}
	if snapAfter.TreeID != snapBefore.TreeID {
		t.Fatalf("tree should not change on message rewrite")
	}
}
