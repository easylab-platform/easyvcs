package store

import (
	"testing"
)

// openTenantStore opens an in-memory store and runs the schema bootstrap.
func openTenantStore(t *testing.T) *CentralStore {
	t.Helper()
	t.Setenv("EASYVCS_HOME", t.TempDir())
	s, err := OpenDefault()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	return s
}

// A user owns repositories under namespaces they own; namespaces are globally
// unique, so two users cannot own the same namespace name. A second create of
// the same namespace/name is rejected.
func TestRepoNamespaceGlobalAndOwnerScoped(t *testing.T) {
	s := openTenantStore(t)
	alice, err := s.CreateUser("alice", "Alice")
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	bob, err := s.CreateUser("bob", "Bob")
	if err != nil {
		t.Fatalf("create bob: %v", err)
	}

	a := RepoRef{Owner: alice.ID, Namespace: "platform", Name: "api"}
	if _, err := s.Create(a); err != nil {
		t.Fatalf("create alice/platform/api: %v", err)
	}
	// bob cannot take the same namespace/name (global address).
	if _, err := s.Create(RepoRef{Owner: bob.ID, Namespace: "platform", Name: "api"}); err == nil {
		t.Fatal("duplicate namespace/name must be rejected")
	}
	// Repo owner is the namespace owner.
	got, err := s.OpenRepo(RepoRef{Namespace: "platform", Name: "api"})
	if err != nil || got.OwnerUserID != alice.ID {
		t.Fatalf("open: %v %+v", err, got)
	}
	// Per-owner listing reflects ownership.
	listA, err := s.ListForOwner(alice.ID)
	if err != nil || len(listA) != 1 {
		t.Fatalf("alice repos = %+v (%v)", listA, err)
	}
	listB, err := s.ListForOwner(bob.ID)
	if err != nil || len(listB) != 0 {
		t.Fatalf("bob repos = %+v (%v)", listB, err)
	}
}

// Deleting one repo leaves a different-namespaced repo of another user intact.
func TestDeleteIsOwnerScoped(t *testing.T) {
	s := openTenantStore(t)
	alice, _ := s.CreateUser("alice", "Alice")
	bob, _ := s.CreateUser("bob", "Bob")
	if _, err := s.Create(RepoRef{Owner: alice.ID, Namespace: "a", Name: "r"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(RepoRef{Owner: bob.ID, Namespace: "b", Name: "r"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(RepoRef{Namespace: "a", Name: "r"}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.RepoExists(RepoRef{Namespace: "a", Name: "r"}); ok {
		t.Fatal("a copy should be gone")
	}
	if ok, _ := s.RepoExists(RepoRef{Namespace: "b", Name: "r"}); !ok {
		t.Fatal("b copy must survive")
	}
}

// Collaboration: the owner of a repo may grant a DIFFERENT user a role, and
// that user's effective role resolves on the owner's repo (cross-user grant).
func TestRepoMemberRoles(t *testing.T) {
	s := openTenantStore(t)
	owner, _ := s.CreateUser("owner", "Owner")
	maint, _ := s.CreateUser("maint", "Maintainer")
	dev, _ := s.CreateUser("dev", "Developer")
	other, _ := s.CreateUser("other", "Other")

	repo, err := s.Create(RepoRef{Owner: owner.ID, Namespace: "acme", Name: "api"})
	if err != nil {
		t.Fatal(err)
	}

	// Owner role comes from namespace ownership.
	if role, _ := s.RoleOf(repo.RepoID(), owner.ID); !role.CanPush() {
		t.Fatalf("owner role = %q, want owner", role)
	}
	if err := s.SetRepoMember(repo.RepoID(), maint.ID, RoleMaintainer, &owner.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRepoMember(repo.RepoID(), dev.ID, RoleDeveloper, &owner.ID); err != nil {
		t.Fatal(err)
	}

	if role, _ := s.RoleOf(repo.RepoID(), maint.ID); role.CanPush() || !role.CanMerge() {
		t.Fatalf("maintainer role = %q (push=%v merge=%v)", role, role.CanPush(), role.CanMerge())
	}
	if role, _ := s.RoleOf(repo.RepoID(), dev.ID); role.CanMerge() || !role.CanPropose() {
		t.Fatalf("developer role = %q (merge=%v propose=%v)", role, role.CanMerge(), role.CanPropose())
	}
	// A private repo with no grant: no role.
	repo.UpdateRepoMeta(RepoMeta{Visibility: "private"})
	if role, _ := s.RoleOf(repo.RepoID(), other.ID); role != RoleNone {
		t.Fatalf("non-member on private repo = %q, want none", role)
	}
	// Public exposes developer-equivalent access.
	repo.UpdateRepoMeta(RepoMeta{Visibility: "public"})
	if role, _ := s.RoleOf(repo.RepoID(), other.ID); !role.CanRead() {
		t.Fatalf("public repo should be readable, got %q", role)
	}
	// Anonymous on a public repo reads.
	if role, _ := s.RoleOf(repo.RepoID(), 0); !role.CanRead() || role.CanPush() {
		t.Fatalf("anonymous public role = %q", role)
	}
}
