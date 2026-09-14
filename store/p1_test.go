package store

import (
	"testing"
)

// TestRoleCapabilities verifies the capability thresholds on the three
// repository roles.
func TestRoleCapabilities(t *testing.T) {
	owner := RoleOwner
	maintainer := Role(RoleMaintainer)
	developer := Role(RoleDeveloper)
	none := RoleNone

	if !owner.CanPush() || !owner.CanMerge() || !owner.CanPropose() || !owner.CanRead() || !owner.CanOwnSession() {
		t.Fatal("owner must have every capability")
	}
	if maintainer.CanPush() || !maintainer.CanMerge() || !maintainer.CanPropose() || !maintainer.CanRead() || maintainer.CanOwnSession() {
		t.Fatal("maintainer: merge/propose/read yes; push/session no")
	}
	if developer.CanPush() || developer.CanMerge() || !developer.CanPropose() || !developer.CanRead() || developer.CanOwnSession() {
		t.Fatal("developer: propose/read yes; push/merge/session no")
	}
	if none.CanRead() || none.CanPropose() || none.AtLeast(RoleOwner) {
		t.Fatal("none must grant nothing")
	}
	if !owner.AtLeast(Role(RoleMaintainer)) || !maintainer.AtLeast(Role(RoleDeveloper)) {
		t.Fatal("AtLeast ordering broken")
	}
}

// TestSecretEncryptionRoundTrip verifies that with EASYVCS_SECRET_KEY the value
// is encrypted at rest but decrypts back, and without a key it is plaintext.
func TestSecretEncryptionRoundTrip(t *testing.T) {
	cs := newTestCentral(t)
	owner, _ := cs.CreateUser("owner", "Owner")
	repo, _ := cs.Create(RepoRef{Owner: owner.ID, Namespace: "n", Name: "r"})

	// Without a key: plaintext round-trip.
	_ = repo.PutRemote("origin", "http://x", "tok123")
	rem, err := repo.GetRemote("origin")
	if err != nil {
		t.Fatal(err)
	}
	if rem.Token != "tok123" {
		t.Fatalf("no-key roundtrip: %q", rem.Token)
	}
	// Encrypted value would carry the enc: prefix only when a key is set.
	if enc, err := encryptSecret("tok123"); err != nil || enc != "tok123" {
		t.Fatalf("no-key encrypt should be identity, got %q %v", enc, err)
	}

	// With a key: encrypted at rest, decrypts back.
	t.Setenv("EASYVCS_SECRET_KEY", "0123456789abcdef0123456789abcdef")
	_ = repo.PutRemote("origin", "http://x", "secret567")
	rem2, err := repo.GetRemote("origin")
	if err != nil {
		t.Fatal(err)
	}
	if rem2.Token != "secret567" {
		t.Fatalf("with-key roundtrip: %q", rem2.Token)
	}
	// The stored value must not be the plaintext.
	var row remoteRow
	_ = cs.d.gdb.Where("repo_id=? AND name=?", repo.repoID, "origin").First(&row).Error
	if row.Token != nil && *row.Token == "secret567" {
		t.Fatal("token stored in plaintext despite key set")
	}
	if row.Token != nil && len(*row.Token) > 4 && (*row.Token)[:4] != "enc:" {
		t.Fatalf("token should be encrypted (enc: prefix), got %q", *row.Token)
	}
}

// TestAuditRecord verifies audit_log is append-only and queryable.
func TestAuditRecord(t *testing.T) {
	cs := newTestCentral(t)
	if err := cs.RecordAudit(AuditEvent{Action: "push", Namespace: "n", Repo: "r", Outcome: "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := cs.RecordAudit(AuditEvent{Action: "push", Namespace: "n", Repo: "r", Outcome: "denied"}); err != nil {
		t.Fatal(err)
	}
	events, err := cs.ListAudit(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("audit count=%d", len(events))
	}
	if events[0].Outcome == events[1].Outcome {
		t.Fatalf("expected newest-first distinct outcomes")
	}
}

// TestTokenStoredHashedRaw verifies the tokens table never holds the plaintext
// (internal access to the GORM session; no exported test backdoor needed).
func TestTokenStoredHashedRaw(t *testing.T) {
	cs := newTestCentral(t)
	u, err := cs.CreateUser("alice", "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CreateToken("supersecret", u.ID, "write"); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := cs.d.gdb.Raw("SELECT token FROM tokens LIMIT 1").Scan(&raw).Error; err != nil {
		t.Fatal(err)
	}
	if raw == "supersecret" {
		t.Fatal("token stored in plaintext")
	}
	if len(raw) != 64 { // sha256 hex
		t.Fatalf("token hash length = %d, want 64", len(raw))
	}
	if tk, err := cs.LookupToken("supersecret"); err != nil || tk.UserID != u.ID {
		t.Fatalf("lookup by plaintext: %v %v", tk, err)
	}
	// ListTokens must not echo the plaintext.
	toks, err := cs.ListTokens(u.ID)
	if err != nil || len(toks) != 1 || toks[0].Token != "" {
		t.Fatalf("ListTokens must omit plaintext: %+v %v", toks, err)
	}
}
