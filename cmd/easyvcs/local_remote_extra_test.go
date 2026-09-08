package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/revision"
	"github.com/easylab-platform/easyvcs/store"
	"github.com/easylab-platform/easyvcs/transfer"
)

// TestIsLocalURLExhaustive covers the classification edge cases for a remote URL
// vs a local filesystem path.
func TestIsLocalURLExhaustive(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"/abs/path/repo", true},
		{"//server/share/repo", true},
		{"./rel/repo", true},
		{"../up/repo", true},
		{"~/repo", true},
		{"~user/repo", true},
		{"repo-name", false},
		{"a/b/c", true},
		{"bare", false},
		{"http://host:18160", false},
		{"https://host/repo", false},
		{"ssh://host/repo", false},
		{"git@host:path", false},
		{"host:18160", false},
		{"host", false},
		{"host:port/path", false},
		{"a/b/c", true},
		{".", true},
		{"..", true},
		{"/", true},
		{"", false},
		{"   ", false},
	}
	for _, c := range cases {
		if got := isLocalURL(c.url); got != c.want {
			t.Errorf("isLocalURL(%q)=%v want %v", c.url, got, c.want)
		}
	}
}

func TestExpandLocalPath(t *testing.T) {
	old := os.Getenv("HOME")
	defer func() {
		if old == "" {
			_ = os.Unsetenv("HOME")
		} else {
			_ = os.Setenv("HOME", old)
		}
	}()
	home, _ := os.UserHomeDir()
	t.Setenv("HOME", home)

	if got := expandLocalPath("~/repo"); got != filepath.Join(home, "repo") {
		t.Fatalf("expand ~ got %q want %q", got, filepath.Join(home, "repo"))
	}
	if got := expandLocalPath("~/a/b"); got != filepath.Join(home, "a", "b") {
		t.Fatalf("expand ~/a/b got %q", got)
	}
	if got := expandLocalPath("http://x/y"); got != "http://x/y" {
		t.Fatalf("should not expand a url: %q", got)
	}
	if got := expandLocalPath("https://host/team/app"); got != "https://host/team/app" {
		t.Fatalf("should not expand an https url: %q", got)
	}
	// Absolute path stays absolute.
	abs := "/tmp/xyz"
	if got := expandLocalPath(abs); got != abs {
		t.Fatalf("abs should stay: %q", got)
	}
	// Relative ./ path becomes absolute.
	if got := expandLocalPath("./rel"); !filepath.IsAbs(got) {
		t.Fatalf("./rel should resolve to absolute: %q", got)
	}
}

func TestRepoRefFromURLPath(t *testing.T) {
	cases := []struct {
		url      string
		ns, name string
	}{
		{"http://host:18160/team/app", "team", "app"},
		{"https://host/team/app", "team", "app"},
		{"easyvcs://host/team/app", "team", "app"},
		{"/tmp/foo/team/app", "team", "app"},
		{"team/app", "team", "app"},
		{"app", "default", "app"},
		{"", "default", ""},
	}
	for _, c := range cases {
		ns, name := repoRefFromURLPath(c.url)
		if ns != c.ns || name != c.name {
			t.Errorf("repoRefFromURLPath(%q)=(%q,%q) want (%q,%q)", c.url, ns, name, c.ns, c.name)
		}
	}
}

// stubLocalRemote creates a workspace directory acting as a local remote with a
// single committed revision on branch "main".
func stubLocalRemote(t *testing.T, ns, name, content, desc string) (dir string, revID string, repo *store.Repo) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("EASYVCS_HOME", home)
	cs, err := store.OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	repo, err = cs.Create(store.RepoRef{Namespace: ns, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	ws := revision.NewWorkspace(repo)
	_, rev, err := ws.CommitFromChanges(object.ID{}, []revision.FileChangeSpec{{Path: "a.txt", Content: []byte(content)}}, desc, store.Author{Name: "t"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ws.SetRef("main", store.RefBranch, rev.ID); err != nil {
		t.Fatal(err)
	}
	dir = t.TempDir()
	if err := store.WriteMarker(dir, &store.WorkspaceMarker{Repo: repo.RepoRef(), Home: home}); err != nil {
		t.Fatal(err)
	}
	return dir, rev.ID, repo
}

func TestCloneLocalPreservesContent(t *testing.T) {
	srcDir, revID, _ := stubLocalRemote(t, "team", "app", "hello\n", "c1")

	dstHome := t.TempDir()
	t.Setenv("EASYVCS_HOME", dstHome)
	c := &ctx{cs: mustOpen(t)}
	dstDir := t.TempDir()
	orig, _ := os.Getwd()
	_ = os.Chdir(dstDir)
	defer func() { _ = os.Chdir(orig) }()

	os.Args = []string{"easyvcs", "clone", srcDir}
	captureStdout(t, func() { cmdClone(c) })

	repo := openFirstRepo(t, c.cs)
	ws := revision.NewWorkspace(repo)
	mainRef, err := ws.GetRef("main")
	if err != nil || mainRef == nil {
		t.Fatalf("main missing: %v", err)
	}
	if mainRef.Target != revID {
		t.Fatalf("main=%s want %s", mainRef.Target, revID)
	}
	snap, err := ws.GetSnapshot(mainRefTargetToHash(t, ws, mainRef.Target))
	if err != nil {
		t.Fatal(err)
	}
	blob, present, err := pathBlobHash(ws, snap.TreeID, "a.txt")
	if err != nil || !present {
		t.Fatalf("a.txt missing after clone")
	}
	data, _ := ws.ReadBlob(blob)
	if string(data) != "hello\n" {
		t.Fatalf("content mismatch: %q", data)
	}
}

func mainRefTargetToHash(t *testing.T, ws *revision.Workspace, revID string) object.ID {
	t.Helper()
	r, err := ws.GetRevision(revID)
	if err != nil {
		t.Fatal(err)
	}
	return r.Hash
}

func pathBlobHash(ws *revision.Workspace, treeID object.ID, path string) (object.ID, bool, error) {
	entry, err := ws.FindEntry(ws.MustTree(treeID), path)
	if err != nil {
		return object.ID{}, false, err
	}
	if entry.Kind != object.KindBlob {
		return object.ID{}, false, nil
	}
	return entry.ID, true, nil
}

// TestCloneLocalBranchFlag clones with an explicit -b.
func TestCloneLocalBranchFlag(t *testing.T) {
	srcDir, revID, _ := stubLocalRemote(t, "team", "app", "x\n", "c1")

	dstHome := t.TempDir()
	t.Setenv("EASYVCS_HOME", dstHome)
	c := &ctx{cs: mustOpen(t)}
	dstDir := t.TempDir()
	orig, _ := os.Getwd()
	_ = os.Chdir(dstDir)
	defer func() { _ = os.Chdir(orig) }()

	os.Args = []string{"easyvcs", "clone", srcDir, "-b", "main"}
	captureStdout(t, func() { cmdClone(c) })

	repo := openFirstRepo(t, c.cs)
	ws := revision.NewWorkspace(repo)
	mainRef, err := ws.GetRef("main")
	if err != nil || mainRef == nil {
		t.Fatalf("main missing: %v", err)
	}
	if mainRef.Target != revID {
		t.Fatalf("main=%s want %s", mainRef.Target, revID)
	}
}

// TestLocalRemotePushAddsRevision verifies push to a local remote transfers new
// local revisions into the target.
func TestLocalRemotePushAddsRevision(t *testing.T) {
	srcDir, _, srcRepo := stubLocalRemote(t, "team", "app", "a\n", "c1")

	// Consumer repo in a different home.
	homeB := t.TempDir()
	t.Setenv("EASYVCS_HOME", homeB)
	csB, _ := store.OpenDefault()
	repoB, err := csB.Create(store.RepoRef{Namespace: "cl", Name: "app"})
	if err != nil {
		t.Fatal(err)
	}
	dirB := t.TempDir()
	_ = store.WriteMarker(dirB, &store.WorkspaceMarker{Repo: repoB.RepoRef(), Home: homeB})
	if err := repoB.PutRemote("origin", srcDir, ""); err != nil {
		t.Fatal(err)
	}

	// Add a new local revision after pulling.
	if err := doPull(repoB, []string{"origin", "main"}); err != nil {
		t.Fatalf("pull: %v", err)
	}
	wsB := revision.NewWorkspace(repoB)
	mainRef, _ := wsB.GetRef("main")
	head, err := wsB.GetSnapshot(mainRefTargetToHash(t, wsB, mainRef.Target))
	if err != nil {
		t.Fatal(err)
	}
	_, newRev, err := wsB.CommitFromChanges(head.RevisionHash, []revision.FileChangeSpec{{Path: "b.txt", Content: []byte("b\n")}}, "c2", store.Author{Name: "t"}, "")
	if err != nil {
		t.Fatal(err)
	}
	// Push to the local remote (source).
	rem, _ := repoB.GetRemote("origin")
	target, err := openLocalRepo(rem.URL)
	if err != nil {
		t.Fatal(err)
	}
	bundle, _ := transfer.CollectAll(repoB)
	if _, err := transfer.Apply(target, bundle); err != nil {
		t.Fatalf("push: %v", err)
	}
	if _, err := target.GetRevision(newRev.ID); err != nil {
		t.Fatalf("target missing pushed revision %s: %v", newRev.ID, err)
	}
	// Source repo object should also be inspectable.
	if _, err := srcRepo.GetRevision(newRev.ID); err != nil {
		t.Fatalf("source repo missing revision %s: %v", newRev.ID, err)
	}
}

// TestLocalRemoteFetchRecordsRemoteRefs ensures fetch on a local remote records
// origin.<branch> remote refs.
func TestLocalRemoteFetchRecordsRemoteRefs(t *testing.T) {
	srcDir, _, _ := stubLocalRemote(t, "team", "app", "a\n", "c1")
	homeB := t.TempDir()
	t.Setenv("EASYVCS_HOME", homeB)
	csB, _ := store.OpenDefault()
	repoB, _ := csB.Create(store.RepoRef{Namespace: "cl", Name: "app"})
	dirB := t.TempDir()
	_ = store.WriteMarker(dirB, &store.WorkspaceMarker{Repo: repoB.RepoRef(), Home: homeB})
	_ = repoB.PutRemote("origin", srcDir, "")

	if err := doFetch(repoB, []string{"origin"}); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	rrefs, err := repoB.ListRemoteRefs("origin")
	if err != nil {
		t.Fatal(err)
	}
	if len(rrefs) == 0 {
		t.Fatalf("no remote refs recorded")
	}
	found := false
	for _, rr := range rrefs {
		if rr.Kind == store.RefBranch && rr.Name == "main" && rr.Target != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("origin.main not recorded: %v", rrefs)
	}
}

// TestLocalRemotePullRebaseMerges ensures pulling a remote tip onto a divergent
// local tip rebases (3-way, non-conflicting files preserved) rather than
// overwriting: remote advances on main, local advances on main, then pull merges.
func TestLocalRemotePullRebaseMerges(t *testing.T) {
	srcDir, srcHome, srcRepo := stubLocalRemote2(t, "team", "app", "a\n", "c1")
	homeB := t.TempDir()
	t.Setenv("EASYVCS_HOME", homeB)
	csB, _ := store.OpenDefault()
	repoB, _ := csB.Create(store.RepoRef{Namespace: "cl", Name: "app"})
	dirB := t.TempDir()
	_ = store.WriteMarker(dirB, &store.WorkspaceMarker{Repo: repoB.RepoRef(), Home: homeB})
	_ = repoB.PutRemote("origin", srcDir, "")

	// First pull gives the base.
	if err := doPull(repoB, []string{"origin", "main"}); err != nil {
		t.Fatalf("first pull: %v", err)
	}
	wsB := revision.NewWorkspace(repoB)
	mainRef, _ := wsB.GetRef("main")
	baseSnap, _ := wsB.GetSnapshot(mainRefTargetToHash(t, wsB, mainRef.Target))

	// LOCAL advance: add c.txt on top of base, then move local main to it.
	_, localRev, err := wsB.CommitFromChanges(baseSnap.RevisionHash, []revision.FileChangeSpec{{Path: "c.txt", Content: []byte("c\n")}}, "local", store.Author{Name: "t"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wsB.SetRef("main", store.RefBranch, localRev.ID); err != nil {
		t.Fatal(err)
	}

	// REMOTE advance: add d.txt on top of base, then move remote main to it.
	wsA := revision.NewWorkspace(srcRepo)
	srcMain, _ := wsA.GetRef("main")
	srcBaseSnap, _ := wsA.GetSnapshot(mainRefTargetToHash(t, wsA, srcMain.Target))
	_, remoteRev, err := wsA.CommitFromChanges(srcBaseSnap.RevisionHash, []revision.FileChangeSpec{{Path: "d.txt", Content: []byte("d\n")}}, "remote", store.Author{Name: "t"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wsA.SetRef("main", store.RefBranch, remoteRev.ID); err != nil {
		t.Fatal(err)
	}

	// Pull: rebase remote tip (d.txt) onto local tip (c.txt). Both files survive.
	if err := doPull(repoB, []string{"origin", "main"}); err != nil {
		t.Fatalf("second pull (rebase): %v", err)
	}
	mergedRef, _ := wsB.GetRef("main")
	mergedSnap, err := wsB.GetSnapshot(mainRefTargetToHash(t, wsB, mergedRef.Target))
	if err != nil {
		t.Fatal(err)
	}
	tree := wsB.MustTree(mergedSnap.TreeID)
	if entry, err := wsB.FindEntry(tree, "c.txt"); err != nil || entry.Kind != object.KindBlob {
		t.Fatalf("c.txt should survive rebase merge (err=%v)", err)
	}
	if entry, err := wsB.FindEntry(tree, "d.txt"); err != nil || entry.Kind != object.KindBlob {
		t.Fatalf("d.txt should survive rebase merge (err=%v)", err)
	}
	_ = srcHome
}

// stubLocalRemote2 returns the source dir, its home, and the source repo, but
// keeps the store open so the caller can advance the remote.
func stubLocalRemote2(t *testing.T, ns, name, content, desc string) (dir string, home string, repo *store.Repo) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("EASYVCS_HOME", home)
	cs, err := store.OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	repo, err = cs.Create(store.RepoRef{Namespace: ns, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	ws := revision.NewWorkspace(repo)
	_, rev, err := ws.CommitFromChanges(object.ID{}, []revision.FileChangeSpec{{Path: "a.txt", Content: []byte(content)}}, desc, store.Author{Name: "t"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ws.SetRef("main", store.RefBranch, rev.ID); err != nil {
		t.Fatal(err)
	}
	dir = t.TempDir()
	if err := store.WriteMarker(dir, &store.WorkspaceMarker{Repo: repo.RepoRef(), Home: home}); err != nil {
		t.Fatal(err)
	}
	return dir, home, repo
}

func TestMarkerNoFallbackBreaksOldFormat(t *testing.T) {
	// A marker without home/dsn must not silently open the default store.
	dir := t.TempDir()
	raw := `{"repo":{"namespace":"n","name":"r"},"current_revision":"rev1"}`
	if err := os.WriteFile(filepath.Join(dir, store.WorkspaceMarkerFile), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := store.LookupMarker(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenStoreForMarker(m); err == nil {
		t.Fatal("expected OpenStoreForMarker to reject a marker with no home/dsn")
	}
}

func TestMarkerWriteAndReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("EASYVCS_HOME", home)
	m := &store.WorkspaceMarker{
		Repo:            store.RepoRef{Namespace: "n", Name: "r"},
		CurrentRevision: "rev1",
		Branch:          "main",
		Home:            home,
		DSN:             "/tmp/custom.db",
	}
	if err := store.WriteMarker(dir, m); err != nil {
		t.Fatal(err)
	}
	got, err := store.LookupMarker(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Home != home || got.DSN != "/tmp/custom.db" || got.Branch != "main" || got.CurrentRevision != "rev1" {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if got.Repo.Namespace != "n" || got.Repo.Name != "r" {
		t.Fatalf("repo mismatch: %+v", got.Repo)
	}
}
