package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/revision"
	"github.com/easylab-platform/easyvcs/store"
)

func TestCLIFullWorkflow(t *testing.T) {
	setHome(t)
	dir := t.TempDir()
	orig, _ := os.Getwd()
	_ = os.Chdir(dir)
	defer func() { _ = os.Chdir(orig) }()

	c := &ctx{cs: mustOpen(t)}
	os.Args = []string{"easyvcs", "init", "team/app"}
	cmdInit(c)

	write(t, dir, "a.txt", "1\n")
	os.Args = []string{"easyvcs", "commit"}
	cmdCommit(c)

	os.Args = []string{"easyvcs", "log"}
	captureStdout(t, func() { cmdLog(c) })

	snap := firstSnapshotHash(t, c.cs)
	os.Args = []string{"easyvcs", "show", snap}
	captureStdout(t, func() { cmdShow(c) })

	write(t, dir, "a.txt", "1\n2\n")
	os.Args = []string{"easyvcs", "amend"}
	captureStdout(t, func() { cmdAmend(c) })

	os.Args = []string{"easyvcs", "log", "a.txt"}
	captureStdout(t, func() { cmdLogPath(c, "a.txt", false) })
	os.Args = []string{"easyvcs", "log", "--count", "a.txt"}
	captureStdout(t, func() { cmdLogPath(c, "a.txt", true) })

	rid := firstRevisionID(t, c.cs)
	os.Args = []string{"easyvcs", "branch", "main", rid}
	captureStdout(t, func() { cmdBranch(c) })
	os.Args = []string{"easyvcs", "tag", "v1", rid}
	captureStdout(t, func() { cmdTag(c) })
	os.Args = []string{"easyvcs", "refs"}
	captureStdout(t, func() { cmdRefs(c) })

	write(t, dir, "b.txt", "b\n")
	os.Args = []string{"easyvcs", "commit"}
	cmdCommit(c)
	rid2 := firstRevisionID(t, c.cs)
	os.Args = []string{"easyvcs", "rebase", rid2, "--onto", snap}
	captureStdout(t, func() { cmdRebase(c) })
	os.Args = []string{"easyvcs", "squash", rid2}
	captureStdout(t, func() { cmdSquash(c) })
}

func TestCLIDiffMerge(t *testing.T) {
	setHome(t)
	dir := t.TempDir()
	orig, _ := os.Getwd()
	_ = os.Chdir(dir)
	defer func() { _ = os.Chdir(orig) }()

	c := &ctx{cs: mustOpen(t)}
	os.Args = []string{"easyvcs", "init", "team/app"}
	cmdInit(c)
	write(t, dir, "f.txt", "one\n")
	os.Args = []string{"easyvcs", "commit"}
	cmdCommit(c)
	snap := firstSnapshotHash(t, c.cs)

	os.Args = []string{"easyvcs", "diff", snap, snap}
	captureStdout(t, func() { cmdDiff(c) })
	// (diff "bad" would os.Exit(1); skip to keep test process alive)

	os.Args = []string{"easyvcs", "merge", "--base", snap, "--ours", snap, "--theirs", snap}
	captureStdout(t, func() { cmdMerge(c) })
}

func TestCLIWorkspaceAndRepo(t *testing.T) {
	setHome(t)
	dir := t.TempDir()
	orig, _ := os.Getwd()
	_ = os.Chdir(dir)
	defer func() { _ = os.Chdir(orig) }()

	c := &ctx{cs: mustOpen(t)}
	os.Args = []string{"easyvcs", "init", "team/app"}
	cmdInit(c)

	os.Args = []string{"easyvcs", "workspace", "list"}
	captureStdout(t, func() { cmdWorkspaceList(c) })
	os.Args = []string{"easyvcs", "workspace", "attach"}
	captureStdout(t, func() { cmdWorkspaceAttach(c) })
	os.Args = []string{"easyvcs", "repositories"}
	captureStdout(t, func() { cmdRepositories(c) })
}

func TestCLIExportImport(t *testing.T) {
	setHome(t)
	dir := t.TempDir()
	orig, _ := os.Getwd()
	_ = os.Chdir(dir)
	defer func() { _ = os.Chdir(orig) }()

	c := &ctx{cs: mustOpen(t)}
	os.Args = []string{"easyvcs", "init", "team/app"}
	cmdInit(c)
	write(t, dir, "f.txt", "hello\n")
	os.Args = []string{"easyvcs", "commit"}
	cmdCommit(c)

	bundle := filepath.Join(t.TempDir(), "app.evcs")
	os.Args = []string{"easyvcs", "export", "team/app", "-o", bundle}
	captureStdout(t, func() { cmdExport(c) })
	if _, err := os.Stat(bundle); err != nil {
		t.Fatalf("export failed: %v", err)
	}
	os.Args = []string{"easyvcs", "import", bundle}
	captureStdout(t, func() { cmdImport(c) })
}

func TestCLIRemote(t *testing.T) {
	setHome(t)
	dir := t.TempDir()
	orig, _ := os.Getwd()
	_ = os.Chdir(dir)
	defer func() { _ = os.Chdir(orig) }()

	c := &ctx{cs: mustOpen(t)}
	os.Args = []string{"easyvcs", "init", "team/app"}
	cmdInit(c)
	write(t, dir, "f.txt", "x\n")
	os.Args = []string{"easyvcs", "commit"}
	cmdCommit(c)

	os.Args = []string{"easyvcs", "remote", "add", "origin", "http://127.0.0.1:1", "--token", "tok"}
	captureStdout(t, func() { cmdRemote(c) })
	os.Args = []string{"easyvcs", "remote"}
	captureStdout(t, func() { cmdRemote(c) })
	os.Args = []string{"easyvcs", "remote", "origin"}
	captureStdout(t, func() { cmdRemote(c) })
	os.Args = []string{"easyvcs", "remote", "remove", "origin"}
	captureStdout(t, func() { cmdRemote(c) })
}

// TestCLICloneLocal verifies `clone` from a local workspace directory into a
// fresh destination repo + workspace, registering origin and pulling the source
// default branch. It uses two independent EASYVCS_HOME stores.
func TestCLICloneLocal(t *testing.T) {
	// Source repo in homeA.
	srcHome := t.TempDir()
	t.Setenv("EASYVCS_HOME", srcHome)
	csA, _ := store.OpenDefault()
	srcRepo, err := csA.Create(store.RepoRef{Namespace: "team", Name: "app"})
	if err != nil {
		t.Fatal(err)
	}
	wsA := revision.NewWorkspace(srcRepo)
	snap, rev, err := wsA.CommitFromChanges(object.ID{}, []revision.FileChangeSpec{{Path: "a.txt", Content: []byte("hi\n")}}, "c1", store.Author{Name: "t"}, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = snap
	if _, err := wsA.SetRef("main", store.RefBranch, rev.ID); err != nil {
		t.Fatal(err)
	}
	srcDir := t.TempDir()
	if err := store.WriteMarker(srcDir, &store.WorkspaceMarker{Repo: srcRepo.RepoRef(), Home: srcHome}); err != nil {
		t.Fatal(err)
	}

	// Clone into homeB.
	dstHome := t.TempDir()
	t.Setenv("EASYVCS_HOME", dstHome)
	c := &ctx{cs: mustOpen(t)}
	dstDir := t.TempDir()
	orig, _ := os.Getwd()
	_ = os.Chdir(dstDir)
	defer func() { _ = os.Chdir(orig) }()
	os.Args = []string{"easyvcs", "clone", srcDir}
	captureStdout(t, func() { cmdClone(c) })

	wsB := revision.NewWorkspace(openFirstRepo(t, c.cs))
	mainRef, err := wsB.GetRef("main")
	if err != nil || mainRef == nil {
		t.Fatalf("main branch missing after clone: %v", err)
	}
	if mainRef.Target != rev.ID {
		t.Fatalf("clone main points at %s want %s", mainRef.Target, rev.ID)
	}
	if _, err := wsB.GetRevision(rev.ID); err != nil {
		t.Fatalf("cloned revision missing: %v", err)
	}
}
