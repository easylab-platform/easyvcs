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
	claimFixture(t, s, bob.ID, "acme")

	ctx := t.Context()
	// alice claims @acme/ui (she owns an acme namespace).
	if err := s.AuthorizePublish(ctx, "npm", "@acme/ui", alice.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// bob has his own acme namespace, but the NAME is already claimed by alice.
	if err := s.AuthorizePublish(ctx, "npm", "@acme/ui", bob.ID); err == nil {
		t.Fatal("cross-user publish must be refused")
	}
	// bob can claim a different name under his own scope.
	if err := s.AuthorizePublish(ctx, "npm", "@acme/lib", bob.ID); err != nil {
		t.Fatalf("bob claim @acme/lib: %v", err)
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
