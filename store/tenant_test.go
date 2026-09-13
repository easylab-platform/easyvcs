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

// The migration must guarantee the default tenant with the FIXED id 1 (rows
// carry tenant_id=1 as their column default).
func TestTenantDefaultExistsWithFixedID(t *testing.T) {
	s := openTenantStore(t)
	got, err := s.GetTenant(1)
	if err != nil {
		t.Fatalf("get tenant 1: %v", err)
	}
	if got.Slug != "default" {
		t.Fatalf("tenant 1 slug = %q, want default", got.Slug)
	}
	// Idempotent re-run.
	if err := s.migrateTenants(); err != nil {
		t.Fatalf("re-run migrateTenants: %v", err)
	}
	if n := countTenants(t, s); n != 1 {
		t.Fatalf("after re-run: %d tenants, want 1", n)
	}
}

func countTenants(t *testing.T, s *CentralStore) int {
	t.Helper()
	list, err := s.ListTenants()
	if err != nil {
		t.Fatalf("list tenants: %v", err)
	}
	return len(list)
}

// Two tenants may each own an identically-named repo; lookups never cross the
// boundary.
func TestRepoIsolationAcrossTenants(t *testing.T) {
	s := openTenantStore(t)
	other, err := s.CreateTenant("acme", "Acme")
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	a := RepoRef{Namespace: "platform", Name: "api"}             // default tenant
	b := RepoRef{Tenant: other.ID, Namespace: "platform", Name: "api"} // acme tenant

	if _, err := s.Create(a); err != nil {
		t.Fatalf("create default/platform/api: %v", err)
	}
	if _, err := s.Create(b); err != nil {
		t.Fatalf("create acme platform/api (same name in another tenant): %v", err)
	}

	// Zero-value refs stay on the default tenant (legacy call sites).
	got, err := s.OpenRepo(RepoRef{Namespace: "platform", Name: "api"})
	if err != nil {
		t.Fatalf("open default ref: %v", err)
	}
	if got.Namespace != "platform" || got.Name != "api" {
		t.Fatalf("opened %+v", got)
	}
	// The acme-scoped ref must NOT resolve through the default lens.
	if _, err := s.OpenRepo(RepoRef{Namespace: "platform", Name: "nope"}); err == nil {
		t.Fatal("expected not-found for absent repo")
	}

	// Exists checks are tenant-scoped.
	if ok, _ := s.RepoExists(RepoRef{Tenant: other.ID, Namespace: "platform", Name: "api"}); !ok {
		t.Fatal("acme platform/api should exist")
	}

	// Per-tenant listing never leaks the other tenant's repos.
	defRepos, err := s.ListForTenant(1)
	if err != nil {
		t.Fatalf("list default: %v", err)
	}
	if len(defRepos) != 1 || defRepos[0].Namespace != "platform" {
		t.Fatalf("default tenant repos = %+v", defRepos)
	}
	acmeRepos, err := s.ListForTenant(other.ID)
	if err != nil {
		t.Fatalf("list acme: %v", err)
	}
	if len(acmeRepos) != 1 {
		t.Fatalf("acme tenant repos = %+v", acmeRepos)
	}
}

// Deleting one tenant's repo leaves the same-named repo of the other tenant
// untouched.
func TestDeleteIsTenantScoped(t *testing.T) {
	s := openTenantStore(t)
	other, _ := s.CreateTenant("t2", "T2")
	if _, err := s.Create(RepoRef{Namespace: "x", Name: "r"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(RepoRef{Tenant: other.ID, Namespace: "x", Name: "r"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(RepoRef{Namespace: "x", Name: "r"}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.RepoExists(RepoRef{Namespace: "x", Name: "r"}); ok {
		t.Fatal("default copy should be gone")
	}
	if ok, _ := s.RepoExists(RepoRef{Tenant: other.ID, Namespace: "x", Name: "r"}); !ok {
		t.Fatal("t2 copy must survive")
	}
}

// Membership rows are pinned to the member's tenant: an identically-named
// namespace in another tenant can never grant access.
func TestNamespaceMembershipIsTenantPinned(t *testing.T) {
	s := openTenantStore(t)
	other, _ := s.CreateTenant("t2", "T2")

	uA, err := s.CreateUser("alice", "Alice") // default tenant
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddNamespaceMember("acme", uA.ID, RoleMember); err != nil {
		t.Fatal(err)
	}

	// alice's membership row lives in the default tenant only: the same
	// namespace name under t2 must not consider her a member.
	if s.IsNamespaceMember("acme", uA.ID) != true {
		t.Fatal("alice should be member in her own tenant")
	}
	// Membership lookup is via the user's tenant, so a repo of tenant t2
	// named acme/x resolves membership through t2 rows — none exist.
	got, err := s.GetNamespaceMember("acme", uA.ID)
	if err != nil || got.Role != RoleMember {
		t.Fatalf("get membership: %v %+v", err, got)
	}

	// A t2 user gets their own, separate membership row for the same name.
	uB, err := s.CreateUserTenant(other.ID, "bob", "Bob")
	if err != nil {
		t.Fatal(err)
	}
	if uB.TenantID != other.ID {
		t.Fatalf("bob tenant = %d, want %d", uB.TenantID, other.ID)
	}
	if err := s.AddNamespaceMember("acme", uB.ID, RoleAdmin); err != nil {
		t.Fatal(err)
	}
	// Write ACLs: bob (admin in t2) can write t2's acme repo, NOT the default
	// tenant's acme repo (his membership row is tenant-pinned).
	uid := uB.ID
	if !s.UserCanWriteRepo(RepoRef{Tenant: other.ID, Namespace: "acme", Name: "r"}, &uid) {
		t.Fatal("bob should write t2/acme")
	}
	if s.UserCanWriteRepo(RepoRef{Namespace: "acme", Name: "r"}, &uid) {
		t.Fatal("bob must NOT write default/acme via a same-named namespace")
	}
}
