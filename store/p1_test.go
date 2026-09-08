package store

import (
	"testing"
)

// TestBranchACL verifies the branch allowlist semantics: with no rows the repo
// role applies (all branches allowed); once a row exists, only listed branches
// are pushable.
func TestBranchACL(t *testing.T) {
	cs := newTestCentral(t)
	repo, _ := cs.Create(RepoRef{Namespace: "n", Name: "r"})
	user := int64(7)

	// No rows (no allowlist in effect) -> can push any branch.
	ok, err := repo.CanPushBranch(user, "main")
	if err != nil || !ok {
		t.Fatalf("no allowlist: main should be allowed, got %v %v", ok, err)
	}

	// Add an allowlist row for "main".
	if err := repo.AddBranchACL(user, "main"); err != nil {
		t.Fatal(err)
	}
	// main allowed; dev not allowed.
	if ok, _ := repo.CanPushBranch(user, "main"); !ok {
		t.Fatal("main should be allowed when listed")
	}
	if ok, _ := repo.CanPushBranch(user, "dev"); ok {
		t.Fatal("dev should be denied when allowlist active")
	}

	// Another user with no rows is unaffected (repo role applies to them).
	if ok, _ := repo.CanPushBranch(8, "dev"); !ok {
		t.Fatal("user 8 with no rows falls back to repo-wide write")
	}

	// List + remove.
	branches, err := repo.ListBranchACL(user)
	if err != nil || len(branches) != 1 || branches[0] != "main" {
		t.Fatalf("list branch acl: %v %v", branches, err)
	}
	if err := repo.RemoveBranchACL(user, "main"); err != nil {
		t.Fatal(err)
	}
	// Now no rows -> repo-wide again.
	if ok, _ := repo.CanPushBranch(user, "dev"); !ok {
		t.Fatal("after removing rows, repo role should allow dev")
	}
}

// TestSecretEncryptionRoundTrip verifies that with EASYVCS_SECRET_KEY the value
// is encrypted at rest but decrypts back, and without a key it is plaintext.
func TestSecretEncryptionRoundTrip(t *testing.T) {
	cs := newTestCentral(t)
	repo, _ := cs.Create(RepoRef{Namespace: "n", Name: "r"})

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
