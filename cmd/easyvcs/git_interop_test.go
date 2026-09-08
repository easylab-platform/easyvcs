package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCLIGitPullLocalRepo exercises cmdGitPull against a *local* git repository
// (no network required) using `git clone <path>`. It clones a real git dir into
// a temp working tree, then pulls it into the easyvcs workspace as a single
// revision.
func TestCLIGitPullLocalRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	setHome(t)
	dir := t.TempDir()
	orig, _ := os.Getwd()
	_ = os.Chdir(dir)
	defer func() { _ = os.Chdir(orig) }()

	c := &ctx{cs: mustOpen(t)}
	os.Args = []string{"easyvcs", "init", "team/app"}
	cmdInit(c)

	// Create a local git repo with one file.
	gitDir := t.TempDir()
	runGit(t, gitDir, "init")
	runGit(t, gitDir, "config", "user.email", "t@x")
	runGit(t, gitDir, "config", "user.name", "t")
	write(t, gitDir, "f.txt", "hello\n")
	runGit(t, gitDir, "add", "-A")
	runGit(t, gitDir, "commit", "-q", "-m", "c1")

	os.Args = []string{"easyvcs", "git-pull", gitDir}
	captureStdout(t, func() { cmdGitPull(c) })

	repo := openFirstRepo(t, c.cs)
	revs, _ := repo.ListRevisions()
	if len(revs) == 0 {
		t.Fatalf("no revisions after git-pull")
	}
	// The pulled revision's tree should contain f.txt.
	ws := revisionNewWorkspace(repo)
	snap, err := ws.GetSnapshot(revs[0].Hash)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := ws.FindEntry(ws.MustTree(snap.TreeID), "f.txt")
	if err != nil || entry.Kind != objectKindBlob() {
		t.Fatalf("f.txt should be present after git-pull: %v %v", err, entry.Kind)
	}
}

// TestCLIGitPushLocalRepo exercises cmdGitPush into a bare local git repo.
func TestCLIGitPushLocalRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	setHome(t)
	dir := t.TempDir()
	orig, _ := os.Getwd()
	_ = os.Chdir(dir)
	defer func() { _ = os.Chdir(orig) }()

	c := &ctx{cs: mustOpen(t)}
	os.Args = []string{"easyvcs", "init", "team/app"}
	cmdInit(c)
	write(t, dir, "x.txt", "x\n")
	os.Args = []string{"easyvcs", "commit"}
	cmdCommit(c)

	// A bare git repo as the destination.
	bareDir := t.TempDir()
	bare := filepath.Join(bareDir, "dest.git")
	_ = os.MkdirAll(bare, 0o755)
	runGit(t, bare, "init", "--bare", "-q")

	os.Args = []string{"easyvcs", "git-push", bare}
	captureStdout(t, func() { cmdGitPush(c) })

	// Verify the bare repo now has a commit + the file.
	ls := runGit(t, bare, "log", "--oneline", "-1")
	if strings.TrimSpace(ls) == "" {
		t.Fatalf("bare repo has no commits after git-push")
	}
	// Clone the bare repo and confirm x.txt content.
	cloneDir := filepath.Join(t.TempDir(), "clone")
	cmd := exec.Command("git", "clone", "-q", bare, cloneDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone bare after push: %v %s", err, out)
	}
	data, _ := os.ReadFile(filepath.Join(cloneDir, "x.txt"))
	if string(data) != "x\n" {
		t.Fatalf("pushed x.txt content = %q", data)
	}
}
