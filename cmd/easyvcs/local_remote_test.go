package main

import (
	"testing"

	"github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/revision"
	"github.com/easylab-platform/easyvcs/store"
	"github.com/easylab-platform/easyvcs/transfer"
)

func TestIsLocalURL(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"/abs/path/repo", true},
		{"./rel/repo", true},
		{"../up/repo", true},
		{"~/repo", true},
		{"bare-path", false},
		{"http://host:18160", false},
		{"https://host/repo", false},
		{"host:18160", false},        // has a colon but no scheme; treated as a host:port network target
		{"git@host:path", false},
	}
	for _, c := range cases {
		if got := isLocalURL(c.url); got != c.want {
			t.Errorf("isLocalURL(%q)=%v want %v", c.url, got, c.want)
		}
	}
}

func TestLocalRemotePushFetchPull(t *testing.T) {
	// Home A: a "remote" workspace repo with one committed revision + branch.
	dirA := t.TempDir()
	homeA := t.TempDir()
	t.Setenv("EASYVCS_HOME", homeA)
	csA, err := store.OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	repoA, err := csA.Create(store.RepoRef{Namespace: "team", Name: "app"})
	if err != nil {
		t.Fatal(err)
	}
	wsA := revision.NewWorkspace(repoA)
	snap, rev, err := wsA.CommitFromChanges(object.ID{}, []revision.FileChangeSpec{{Path: "a.txt", Content: []byte("a\n")}}, "first", store.Author{Name: "t", Email: "t@x"}, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = snap
	if _, err := wsA.SetRef("main", store.RefBranch, rev.ID); err != nil {
		t.Fatal(err)
	}
	// Marker records the store home so openLocalRepo can reopen repoA.
	if err := store.WriteMarker(dirA, &store.WorkspaceMarker{Repo: repoA.RepoRef(), Home: homeA}); err != nil {
		t.Fatal(err)
	}

	// Home B: the consumer workspace with a different home.
	dirB := t.TempDir()
	homeB := t.TempDir()
	t.Setenv("EASYVCS_HOME", homeB)
	csB, err := store.OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	repoB, err := csB.Create(store.RepoRef{Namespace: "local", Name: "clone"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteMarker(dirB, &store.WorkspaceMarker{Repo: repoB.RepoRef(), Home: homeB}); err != nil {
		t.Fatal(err)
	}

	// Configure origin on repoB pointing at the "remote" workspace dirA.
	remName := "origin"
	if err := repoB.PutRemote(remName, dirA, ""); err != nil {
		t.Fatal(err)
	}

	// Fetch from the local remote: records remote refs (origin.main), which feed
	// the branch resolution in pull.
	if err := doFetch(repoB, []string{remName}); err != nil {
		t.Fatalf("fetch from local remote: %v", err)
	}
	rrefs, err := repoB.ListRemoteRefs(remName)
	if err != nil || len(rrefs) == 0 {
		t.Fatalf("no remote refs recorded from local remote: %v", err)
	}
	foundRemoteMain := false
	for _, rr := range rrefs {
		if rr.Name == "main" && rr.Target != "" {
			foundRemoteMain = true
		}
	}
	if !foundRemoteMain {
		t.Fatalf("origin.main not recorded: %v", rrefs)
	}

	// Pull it, then confirm the consumer's "main" branch points at rev.
	if err := doPull(repoB, []string{remName, "main"}); err != nil {
		t.Fatalf("pull from local remote: %v", err)
	}
	mainRef, err := repoB.GetRef("main")
	if err != nil || mainRef == nil {
		t.Fatalf("main branch missing after pull: %v", err)
	}
	if mainRef.Target != rev.ID {
		t.Fatalf("pulled main points at %s want %s", mainRef.Target, rev.ID)
	}

	// Confirm the consumer repo actually has the revision (transferred).
	if _, err := repoB.GetRevision(rev.ID); err != nil {
		t.Fatalf("revision %s not transferred: %v", rev.ID, err)
	}

	// Push from B back and verify A's main got the rev (already has it -> idempotent).
	rem, err := repoB.GetRemote(remName)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := transfer.CollectAll(repoB)
	if err != nil {
		t.Fatal(err)
	}
	target, err := openLocalRepo(rem.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transfer.Apply(target, bundle); err != nil {
		t.Fatalf("push to local remote: %v", err)
	}
	repoA2, _ := csA.OpenRepo(store.RepoRef{Namespace: "team", Name: "app"})
	mainA, err := repoA2.GetRef("main")
	if err != nil || mainA == nil {
		t.Fatalf("remote main missing after push: %v", err)
	}
	if mainA.Target != rev.ID {
		t.Fatalf("remote main points at %s want %s after push", mainA.Target, rev.ID)
	}
}
