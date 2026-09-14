package store

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestTenancyMigration simulates a pre-user-model (tenancy-era) database and
// verifies the upgrade: tenant -> user, repo ownership, agent binding moved to
// the user, namespaces created, and the retired tenant artifacts dropped.
func TestTenancyMigration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("EASYVCS_HOME", home)
	dsn := filepath.Join(home, "easyvcs.db")

	// Build a legacy database by hand.
	raw, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	legacy := []string{
		`CREATE TABLE tenants (id INTEGER PRIMARY KEY, slug TEXT, display_name TEXT, disabled NUMERIC DEFAULT 0, created INTEGER, agent_tenant TEXT DEFAULT '', agent_token TEXT DEFAULT '')`,
		`CREATE TABLE users (id INTEGER PRIMARY KEY, tenant_id INTEGER DEFAULT 1, username TEXT, display_name TEXT, created INTEGER)`,
		`CREATE TABLE repositories (id INTEGER PRIMARY KEY, tenant_id INTEGER DEFAULT 1, namespace TEXT, name TEXT, created INTEGER, description TEXT DEFAULT '', visibility TEXT DEFAULT 'public', default_branch TEXT DEFAULT 'main', kind TEXT DEFAULT 'normal', mirror_url TEXT DEFAULT '', mirror_branch TEXT DEFAULT 'main', mirror_interval INTEGER DEFAULT 300, mirror_last_rev TEXT DEFAULT '', mirror_last_sync INTEGER DEFAULT 0, mirror_last_error TEXT DEFAULT '', mirror_token TEXT DEFAULT '')`,
		`CREATE TABLE namespace_members (tenant_id INTEGER DEFAULT 1, namespace TEXT, user_id INTEGER, role TEXT DEFAULT 'member', PRIMARY KEY (tenant_id, namespace, user_id))`,
		`CREATE TABLE package_owners (id INTEGER PRIMARY KEY, format TEXT, repository TEXT, tenant_id INTEGER DEFAULT 1, visibility TEXT DEFAULT 'public', created INTEGER)`,
		// Legacy GORM indexes that name tenant_id (must be dropped before the
		// column; SQLite otherwise refuses the column drop).
		`CREATE INDEX idx_users_tenant_id ON users(tenant_id)`,
		`CREATE INDEX idx_repositories_tenant_id ON repositories(tenant_id)`,
		`INSERT INTO tenants (id, slug, display_name, created, agent_tenant, agent_token) VALUES (1,'default','Default',0,'default','agent-tok-1')`,
		`INSERT INTO tenants (id, slug, display_name, created, agent_tenant, agent_token) VALUES (2,'acme','Acme',0,'acme','agent-tok-2')`,
		`INSERT INTO users (id, tenant_id, username, display_name, created) VALUES (1,1,'operator','Operator',0)`,
		`INSERT INTO users (id, tenant_id, username, display_name, created) VALUES (2,2,'alice','Alice',0)`,
		`INSERT INTO repositories (id, tenant_id, namespace, name, created) VALUES (1,1,'ops','tools',0)`,
		`INSERT INTO repositories (id, tenant_id, namespace, name, created) VALUES (2,2,'acme','api',0)`,
		`INSERT INTO namespace_members (tenant_id, namespace, user_id, role) VALUES (1,'ops',1,'owner')`,
	}
	for _, stmt := range legacy {
		if err := raw.Exec(stmt).Error; err != nil {
			t.Fatalf("legacy DDL %q: %v", stmt, err)
		}
	}
	if sqlDB, err := raw.DB(); err == nil {
		_ = sqlDB.Close()
	}

	// Open through the store: Init runs the migration.
	s, err := OpenDefault()
	if err != nil {
		t.Fatalf("open migrated store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Repos are owned by the tenant's user.
	r1, err := s.OpenRepo(RepoRef{Namespace: "ops", Name: "tools"})
	if err != nil || r1.OwnerUserID != 1 {
		t.Fatalf("ops/tools owner = %+v (%v), want user 1", r1, err)
	}
	r2, err := s.OpenRepo(RepoRef{Namespace: "acme", Name: "api"})
	if err != nil || r2.OwnerUserID != 2 {
		t.Fatalf("acme/api owner = %+v (%v), want user 2", r2, err)
	}

	// Agent binding moved onto the user.
	if u, err := s.GetUser(2); err != nil || u.AgentTenant != "acme" || u.AgentToken != "agent-tok-2" {
		t.Fatalf("alice agent binding = %+v (%v)", u, err)
	}

	// Namespaces created for owned repos.
	if owner, err := s.OwnerOfNamespace("acme"); err != nil || owner != 2 {
		t.Fatalf("namespace acme owner = %d (%v), want 2", owner, err)
	}

	// Retired artifacts gone.
	for _, tbl := range []string{"tenants", "namespace_members", "branch_acl"} {
		if s.d.gdb.Migrator().HasTable(tbl) {
			t.Fatalf("table %s should be dropped", tbl)
		}
	}
	if s.d.gdb.Migrator().HasColumn(&repoRow{}, "tenant_id") {
		t.Fatal("repositories.tenant_id should be dropped")
	}
	if s.d.gdb.Migrator().HasColumn(&userRow{}, "tenant_id") {
		t.Fatal("users.tenant_id should be dropped")
	}

	// The migrated repo's owner resolves to the owner role.
	if role, err := s.RoleOf(r2.RepoID(), 2); err != nil || !role.CanPush() {
		t.Fatalf("alice role on her repo = %q (%v), want owner", role, err)
	}
}
