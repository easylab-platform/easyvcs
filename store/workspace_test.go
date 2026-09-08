package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/easylab-platform/easyvcs/object"
)

func TestMarkers(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := LoadMarker(dir); err == nil {
		t.Fatal("expected ErrNotFound when no marker")
	}
	m := &WorkspaceMarker{Repo: RepoRef{Namespace: "n", Name: "r"}, CurrentRevision: "rev1", Branch: "main"}
	if err := WriteMarker(dir, m); err != nil {
		t.Fatal(err)
	}
	got, err := LookupMarker(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Repo != m.Repo || got.CurrentRevision != "rev1" {
		t.Fatalf("marker mismatch: %+v", got)
	}
	// Nested dir loads parent's marker.
	sub := filepath.Join(dir, "sub")
	_ = os.MkdirAll(sub, 0o755)
	foundDir, got2, err := LoadMarker(sub)
	if err != nil {
		t.Fatal(err)
	}
	if foundDir != dir || got2.CurrentRevision != "rev1" {
		t.Fatalf("nested marker: %s %+v", foundDir, got2)
	}
}

func TestWorkspaceResolve(t *testing.T) {
	cs := newTestCentral(t)
	repo, _ := cs.Create(RepoRef{Namespace: "n", Name: "r"})
	dir := t.TempDir()
	if err := WriteMarker(dir, &WorkspaceMarker{Repo: repo.RepoRef(), CurrentRevision: "r1"}); err != nil {
		t.Fatal(err)
	}
	marker, gotRepo, err := cs.ResolveWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	if gotRepo.RepoRef() != repo.RepoRef() || marker.CurrentRevision != "r1" {
		t.Fatalf("resolve mismatch: %+v %+v", marker, gotRepo)
	}
	// Missing repo
	dir2 := t.TempDir()
	if err := WriteMarker(dir2, &WorkspaceMarker{Repo: RepoRef{Namespace: "no", Name: "repo"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cs.ResolveWorkspace(dir2); err == nil {
		t.Fatal("expected error resolving missing repo")
	}
}

func TestWriteMarkerRecordsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("EASYVCS_HOME", home)
	dir := t.TempDir()
	m := &WorkspaceMarker{Repo: RepoRef{Namespace: "n", Name: "r"}}
	if err := WriteMarker(dir, m); err != nil {
		t.Fatal(err)
	}
	if m.Home != home {
		t.Fatalf("WriteMarker should record HomeDir, got %q", m.Home)
	}
	got, err := LookupMarker(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Home != home {
		t.Fatalf("marker after write should carry home, got %q", got.Home)
	}
}

func TestOpenStoreForMarkerRequiresLocation(t *testing.T) {
	// A marker with no Home/DSN must be rejected (no silent fallback).
	if _, err := OpenStoreForMarker(&WorkspaceMarker{Repo: RepoRef{Namespace: "n", Name: "r"}}); err == nil {
		t.Fatal("expected error for marker without store location")
	}
	if _, err := OpenStoreForMarker(nil); err == nil {
		t.Fatal("expected error for nil marker")
	}
	// With a valid Home it opens the store at <home>/easyvcs.db.
	home := t.TempDir()
	t.Setenv("EASYVCS_HOME", home)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	cs, err := OpenStoreForMarker(&WorkspaceMarker{Repo: RepoRef{Namespace: "n", Name: "r"}, Home: home})
	if err != nil {
		t.Fatalf("expected open with home: %v", err)
	}
	_ = cs.Close()
}

func TestOpenStoreForMarkerPrecedence(t *testing.T) {
	home := t.TempDir()
	other := t.TempDir()
	t.Setenv("EASYVCS_HOME", other)
	// DSN wins over Home.
	dsn := filepath.Join(home, "custom.db")
	cs, err := OpenStoreForMarker(&WorkspaceMarker{Repo: RepoRef{Namespace: "n", Name: "r"}, Home: home, DSN: dsn})
	if err != nil {
		t.Fatalf("open with dsn: %v", err)
	}
	if cs.root != filepath.Dir(dsn) {
		t.Fatalf("dsn should route to %s, got %s", filepath.Dir(dsn), cs.root)
	}
	_ = cs.Close()
}

func TestRepoCloseAndObjectIDs(t *testing.T) {
	cs := newTestCentral(t)
	repo, _ := cs.Create(RepoRef{Namespace: "n", Name: "r"})
	_ = repo.WriteObject(&object.Object{Kind: object.KindBlob, Blob: []byte("a")})
	ids, err := repo.ObjectIDs()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatalf("object ids: %d", len(ids))
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestListEmpty(t *testing.T) {
	cs := newTestCentral(t)
	repos, err := cs.List()
	if err != nil || len(repos) != 0 {
		t.Fatalf("expected empty list: %v %d", err, len(repos))
	}
}
