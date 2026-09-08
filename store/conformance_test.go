package store

import (
	"testing"

	"github.com/easylab-platform/easyvcs/object"
)

// TestPushMirrorCRUD covers Put/Get/List/Delete/UpdateSync for push mirrors.
func TestPushMirrorCRUD(t *testing.T) {
	cs := newTestCentral(t)
	repo, _ := cs.Create(RepoRef{Namespace: "n", Name: "r"})

	m := &PushMirror{Name: "github", URL: "git@x:repo.git", Branch: "main", Token: "tok"}
	if err := repo.PutPushMirror(m); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetPushMirror("github")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "github" || got.URL != "git@x:repo.git" || got.Branch != "main" || got.Token != "tok" {
		t.Fatalf("push mirror mismatch: %+v", got)
	}
	// Update branch via upsert.
	m.Branch = "dev"
	if err := repo.PutPushMirror(m); err != nil {
		t.Fatal(err)
	}
	got2, _ := repo.GetPushMirror("github")
	if got2.Branch != "dev" {
		t.Fatalf("branch not updated: %+v", got2)
	}
	// UpdateSync records lastRev + lastErr.
	if err := repo.UpdatePushMirrorSync("github", "rev123", ""); err != nil {
		t.Fatal(err)
	}
	got3, _ := repo.GetPushMirror("github")
	if got3.LastRev != "rev123" {
		t.Fatalf("last_rev not updated: %+v", got3)
	}
	// List.
	all, err := repo.ListPushMirrors()
	if err != nil || len(all) != 1 {
		t.Fatalf("list mirrors: %v %d", err, len(all))
	}
	if all[0].Token != "" {
		t.Fatalf("ListPushMirrors should omit token, got %+v", all[0])
	}
	// Delete.
	if err := repo.DeletePushMirror("github"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.GetPushMirror("github"); err == nil {
		t.Fatal("expected not found after delete")
	}
	if err := repo.DeletePushMirror("github"); err == nil {
		t.Fatal("expected error deleting missing mirror")
	}
}

// TestRepoMetaRoundTrip and mirror detection.
func TestRepoMetaRoundTrip(t *testing.T) {
	cs := newTestCentral(t)
	repo, _ := cs.Create(RepoRef{Namespace: "n", Name: "r"})

	def := RepoMeta{Description: "d", Visibility: "private", DefaultBranch: "main"}
	if err := repo.UpdateRepoMeta(def); err != nil {
		t.Fatal(err)
	}
	got, err := repo.RepoMeta()
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != "d" || got.Visibility != "private" || got.DefaultBranch != "main" {
		t.Fatalf("meta mismatch: %+v", got)
	}
	if repo.IsMirror() {
		t.Fatal("normal repo should not be a mirror")
	}
	// Mirror fields are recorded by TouchMirrorSync (kind stays normal unless a
	// mirror repo is created via CreateMirror).
	if err := repo.TouchMirrorSync("revX", 12345, ""); err != nil {
		t.Fatal(err)
	}
	got3, _ := repo.RepoMeta()
	if got3.MirrorLastRev != "revX" || got3.MirrorLastSync != 12345 {
		t.Fatalf("mirror sync mismatch: %+v", got3)
	}
}

// TestRemoteRefsLifecycle covers SetRemoteRef / UpdateRemoteSyncTip /
// SetRemoteDefaultBranch / ListRemoteRefs / DeleteRemoteRefsForRemote.
func TestRemoteRefsLifecycle(t *testing.T) {
	cs := newTestCentral(t)
	repo, _ := cs.Create(RepoRef{Namespace: "n", Name: "r"})

	if err := repo.SetRemoteRef("origin", &RemoteRef{RemoteName: "origin", Kind: RefBranch, Name: "main", Target: "rev1"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetRemoteRef("origin", &RemoteRef{RemoteName: "origin", Kind: RefBranch, Name: "dev", Target: "rev2"}); err != nil {
		t.Fatal(err)
	}
	refs, err := repo.ListRemoteRefs("origin")
	if err != nil || len(refs) != 2 {
		t.Fatalf("list remote refs: %v %d", err, len(refs))
	}
	// Upsert same name updates target.
	if err := repo.SetRemoteRef("origin", &RemoteRef{RemoteName: "origin", Kind: RefBranch, Name: "main", Target: "rev1b"}); err != nil {
		t.Fatal(err)
	}
	refs, _ = repo.ListRemoteRefs("origin")
	countMain := 0
	for _, r := range refs {
		if r.Name == "main" && r.Target == "rev1b" {
			countMain++
		}
	}
	if countMain != 1 {
		t.Fatalf("main not upserted: %+v", refs)
	}
	// Default branch persists on the remote row.
	if err := repo.PutRemote("origin", "http://x", ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetRemoteDefaultBranch("origin", "main"); err != nil {
		t.Fatal(err)
	}
	if def, _ := repo.GetRemoteDefaultBranch("origin"); def != "main" {
		t.Fatalf("default branch = %q", def)
	}
	// Remote sync tip.
	if err := repo.UpdateRemoteSyncTip("origin", "tipX"); err != nil {
		t.Fatal(err)
	}
	if tip, _ := repo.GetLastSyncTip("origin"); tip != "tipX" {
		t.Fatalf("remote sync tip = %q", tip)
	}
	// Delete refs for the remote.
	if err := repo.DeleteRemoteRefsForRemote("origin"); err != nil {
		t.Fatal(err)
	}
	refs, _ = repo.ListRemoteRefs("origin")
	if len(refs) != 0 {
		t.Fatalf("remote refs not cleared: %+v", refs)
	}
}

// TestObjectExistsAndRead covers ObjectExists on a written object.
func TestObjectExistsAndRead(t *testing.T) {
	cs := newTestCentral(t)
	repo, _ := cs.Create(RepoRef{Namespace: "n", Name: "r"})
	o := &object.Object{Kind: object.KindBlob, Blob: []byte("data")}
	if err := repo.WriteObject(o); err != nil {
		t.Fatal(err)
	}
	ex, err := repo.ObjectExists(o.ID())
	if err != nil || !ex {
		t.Fatalf("ObjectExists(%s) = %v %v", o.ID(), ex, err)
	}
	got, err := repo.ReadObject(o.ID())
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Blob) != "data" {
		t.Fatalf("blob content mismatch")
	}
}
