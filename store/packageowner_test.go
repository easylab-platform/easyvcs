package store

import (
	"testing"
)

func claimFixture(t *testing.T, s *CentralStore, tid int64, ns string) {
	t.Helper()
	if _, err := s.Create(RepoRef{Tenant: tid, Namespace: ns, Name: "seed"}); err != nil {
		t.Fatalf("seed repo %s/seed: %v", ns, err)
	}
}

// First publish under a scope the tenant really owns claims the name;
// another tenant publishing the same name is refused.
func TestPackageClaimAndCrossTenantRefusal(t *testing.T) {
	s := openTenantStore(t)
	other, _ := s.CreateTenant("t2", "T2")
	claimFixture(t, s, 1, "acme")
	claimFixture(t, s, other.ID, "acme")

	ctx := t.Context()
	// default tenant claims @acme/ui (it has an acme org).
	if err := s.AuthorizePublish(ctx, "npm", "@acme/ui", 1); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// t2 has its own acme org, but the NAME is already claimed by default.
	if err := s.AuthorizePublish(ctx, "npm", "@acme/ui", other.ID); err == nil {
		t.Fatal("cross-tenant publish must be refused")
	}
	// t2 can claim a different name under its own scope.
	if err := s.AuthorizePublish(ctx, "npm", "@acme/lib", other.ID); err != nil {
		t.Fatalf("t2 claim @acme/lib: %v", err)
	}
}

// A scope with no org evidence in the tenant is refused (no squatting).
func TestPackageScopeRequiresOrgEvidence(t *testing.T) {
	s := openTenantStore(t)
	ctx := t.Context()
	if err := s.AuthorizePublish(ctx, "npm", "@ghost/pkg", 1); err == nil {
		t.Fatal("publishing under an unknown scope must be refused")
	}
}

// Private visibility: owning tenant reads, others don't; public is default.
func TestPackageVisibility(t *testing.T) {
	s := openTenantStore(t)
	other, _ := s.CreateTenant("t2", "T2")
	claimFixture(t, s, 1, "acme")
	ctx := t.Context()
	if err := s.AuthorizePublish(ctx, "npm", "@acme/priv", 1); err != nil {
		t.Fatal(err)
	}
	if !s.CanRead(ctx, "npm", "@acme/priv", other.ID) {
		t.Fatal("public by default: other tenant must read")
	}
	if err := s.SetPackageVisibility("npm", "@acme/priv", 1, "private"); err != nil {
		t.Fatal(err)
	}
	if s.CanRead(ctx, "npm", "@acme/priv", other.ID) {
		t.Fatal("private: other tenant must NOT read")
	}
	if !s.CanRead(ctx, "npm", "@acme/priv", 1) {
		t.Fatal("private: owner must read")
	}
	// Only the owner can flip visibility.
	if err := s.SetPackageVisibility("npm", "@acme/priv", other.ID, "public"); err == nil {
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
