package store

import (
	"testing"
)

func claimFixture(t *testing.T, s *CentralStore, owner int64, ns string) {
	t.Helper()
	if _, err := s.Create(RepoRef{Owner: owner, Namespace: ns, Name: "seed"}); err != nil {
		t.Fatalf("seed repo %s/seed: %v", ns, err)
	}
}

// First publish under a namespace the user really owns claims the name;
// another user publishing the same name is refused.
func TestPackageClaimAndCrossUserRefusal(t *testing.T) {
	s := openTenantStore(t)
	alice, _ := s.CreateUser("alice", "Alice")
	bob, _ := s.CreateUser("bob", "Bob")
	claimFixture(t, s, alice.ID, "acme")
	claimFixture(t, s, bob.ID, "acme2")

	ctx := t.Context()
	// alice claims @acme/ui (she owns an acme namespace).
	if err := s.AuthorizePublish(ctx, "npm", "@acme/ui", alice.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// bob tries too; the NAME is already claimed by alice.
	if err := s.AuthorizePublish(ctx, "npm", "@acme/ui", bob.ID); err == nil {
		t.Fatal("cross-user publish must be refused")
	}
	// bob can claim a different name under his own scope.
	if err := s.AuthorizePublish(ctx, "npm", "@acme2/lib", bob.ID); err != nil {
		t.Fatalf("bob claim @acme2/lib: %v", err)
	}
}

// A scope with no namespace evidence in the user is refused (no squatting).
func TestPackageScopeRequiresNamespaceEvidence(t *testing.T) {
	s := openTenantStore(t)
	alice, _ := s.CreateUser("alice", "Alice")
	ctx := t.Context()
	if err := s.AuthorizePublish(ctx, "npm", "@ghost/pkg", alice.ID); err == nil {
		t.Fatal("publishing under an unknown scope must be refused")
	}
}

// Private visibility: owning user reads, others don't; public is default.
func TestPackageVisibility(t *testing.T) {
	s := openTenantStore(t)
	alice, _ := s.CreateUser("alice", "Alice")
	bob, _ := s.CreateUser("bob", "Bob")
	claimFixture(t, s, alice.ID, "acme")
	ctx := t.Context()
	if err := s.AuthorizePublish(ctx, "npm", "@acme/priv", alice.ID); err != nil {
		t.Fatal(err)
	}
	if !s.CanRead(ctx, "npm", "@acme/priv", bob.ID) {
		t.Fatal("public by default: other user must read")
	}
	if err := s.SetPackageVisibility("npm", "@acme/priv", alice.ID, "private"); err != nil {
		t.Fatal(err)
	}
	if s.CanRead(ctx, "npm", "@acme/priv", bob.ID) {
		t.Fatal("private: other user must NOT read")
	}
	if !s.CanRead(ctx, "npm", "@acme/priv", alice.ID) {
		t.Fatal("private: owner must read")
	}
	// Only the owner can flip visibility.
	if err := s.SetPackageVisibility("npm", "@acme/priv", bob.ID, "public"); err == nil {
		t.Fatal("non-owner visibility change must fail")
	}
}

// Namespace extraction follows each protocol's native convention.
func TestNamespaceOfName(t *testing.T) {
	cases := []struct{ format, repo, want string }{
		{"npm", "@acme/ui", "acme"},
		{"npm", "lodash", ""},
		{"oci", "acme/api", "acme"},
		{"generic", "acme", ""},
	}
	for _, c := range cases {
		if got := NamespaceOfName(c.format, c.repo); got != c.want {
			t.Fatalf("NamespaceOfName(%q,%q) = %q, want %q", c.format, c.repo, got, c.want)
		}
	}
}

// A package whose name maps to a repository inherits that repo's roles: the
// repo maintainer may publish, a developer may not, and a private package is
// readable by repo members only.
func TestPackageRolesInheritFromRepo(t *testing.T) {
	s := openTenantStore(t)
	owner, _ := s.CreateUser("owner", "Owner")
	maint, _ := s.CreateUser("maint", "Maintainer")
	dev, _ := s.CreateUser("dev", "Developer")
	outsider, _ := s.CreateUser("outsider", "Outsider")
	ctx := t.Context()

	// Repo acme/api; npm name @acme/api maps to it.
	repo, err := s.Create(RepoRef{Owner: owner.ID, Namespace: "acme", Name: "api"})
	if err != nil {
		t.Fatal(err)
	}
	if ref, ok := s.RepoForPackage("npm", "@acme/api"); !ok || ref.Name != "api" {
		t.Fatalf("RepoForPackage npm @acme/api = %+v %v", ref, ok)
	}
	if err := s.SetRepoMember(repo.RepoID(), maint.ID, RoleMaintainer, &owner.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRepoMember(repo.RepoID(), dev.ID, RoleDeveloper, &owner.ID); err != nil {
		t.Fatal(err)
	}

	// Owner + maintainer may claim/publish; developer may not.
	if err := s.AuthorizePublish(ctx, "npm", "@acme/api", maint.ID); err != nil {
		t.Fatalf("maintainer claim: %v", err)
	}
	if err := s.AuthorizePublish(ctx, "npm", "@acme/api", dev.ID); err == nil {
		t.Fatal("developer publish must be refused")
	}
	// Owner may re-publish (already claimed, still maintainer+).
	if err := s.AuthorizePublish(ctx, "npm", "@acme/api", owner.ID); err != nil {
		t.Fatalf("owner re-publish: %v", err)
	}

	// Private: repo members read, outsider does not.
	if err := s.SetPackageVisibilityAuthorized("npm", "@acme/api", owner.ID, "private"); err != nil {
		t.Fatal(err)
	}
	if !s.CanRead(ctx, "npm", "@acme/api", maint.ID) || !s.CanRead(ctx, "npm", "@acme/api", owner.ID) {
		t.Fatal("members must read private package")
	}
	if s.CanRead(ctx, "npm", "@acme/api", outsider.ID) {
		t.Fatal("outsider must NOT read a private package")
	}
	// A maintainer can flip visibility; a developer cannot.
	if err := s.SetPackageVisibilityAuthorized("npm", "@acme/api", maint.ID, "public"); err != nil {
		t.Fatalf("maintainer visibility flip: %v", err)
	}
	if err := s.SetPackageVisibilityAuthorized("npm", "@acme/api", dev.ID, "private"); err == nil {
		t.Fatal("developer visibility flip must be refused")
	}
}

// An unclaimed name (pull-through cache) is readable by everyone and grants no
// management rights.
func TestUnclaimedPackageIsPublicReadOnly(t *testing.T) {
	s := openTenantStore(t)
	alice, _ := s.CreateUser("alice", "Alice")
	ctx := t.Context()
	if !s.CanRead(ctx, "npm", "lodash", alice.ID) {
		t.Fatal("unclaimed package must be public-readable")
	}
	if role, _ := s.PackageScopeRole("npm", "lodash", alice.ID); role.CanMerge() {
		t.Fatalf("unclaimed package must grant no management, got %q", role)
	}
}
