package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/revision"
	"github.com/easylab-platform/easyvcs/store"
	"github.com/easylab-platform/easyvcs/transfer"
)

// TestPushToEmptyServerNoDanglingRefs reproduces the CLI bug where pushing to
// an empty server sent refs without revisions: every ref target must resolve.
func TestPushToEmptyServerNoDanglingRefs(t *testing.T) {
	s := newTestServer(t, "")
	// Server store exists but has NO repo yet; create it empty.
	if _, err := s.cs.Create(store.RepoRef{Namespace: "team", Name: "app"}); err != nil {
		t.Fatal(err)
	}

	// Client-side repo with two revisions (simulated via a second store).
	home := t.TempDir()
	t.Setenv("EASYVCS_HOME", home)
	cs2, err := store.OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	_ = cs2.SetWAL()
	src, err := cs2.Create(store.RepoRef{Namespace: "team", Name: "app"})
	if err != nil {
		t.Fatal(err)
	}
	seed := seedIn(t, src)

	// Full bundle (server advertised nothing).
	b, err := transfer.Collect(src, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := transfer.CompressBundle(b)
	req := httptest.NewRequest(http.MethodPost, "/repo/team/app/push", bytes.NewReader(payload))
	req.Header.Set("Content-Encoding", "gzip")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("push to empty server: %d %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if n, _ := resp["applied"].(float64); int(n) != len(b.Revisions) {
		t.Fatalf("applied=%v want %d (no revisions may be skipped)", resp["applied"], len(b.Revisions))
	}

	// Every ref target must resolve to a stored revision (no dangling refs).
	dst, err := s.cs.OpenRepo(store.RepoRef{Namespace: "team", Name: "app"})
	if err != nil {
		t.Fatal(err)
	}
	refs, err := dst.ListRefs()
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) == 0 {
		t.Fatal("no refs landed")
	}
	for _, rf := range refs {
		if _, err := dst.GetRevision(rf.Target); err != nil {
			t.Fatalf("dangling ref %s -> %s: %v", rf.Name, rf.Target, err)
		}
	}
	_ = seed
}

// seedIn seeds a repo with two revisions and a main branch (server-side
// helper variant operating on an arbitrary repo handle).
func seedIn(t *testing.T, repo *store.Repo) string {
	t.Helper()
	ws := revision.NewWorkspace(repo)
	_, rev1, err := ws.CommitFromChanges(object.ID{}, []revision.FileChangeSpec{{Path: "a.txt", Content: []byte("hi\n")}}, "c1", store.Author{Name: "t"}, "")
	if err != nil {
		t.Fatal(err)
	}
	snap1, _ := ws.GetSnapshot(rev1.Hash)
	if _, _, err := ws.CommitFromChanges(snap1.RevisionHash, []revision.FileChangeSpec{{Path: "b.txt", Content: []byte("b\n")}}, "c2", store.Author{Name: "t"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.SetRef("main", store.RefBranch, rev1.ID); err != nil {
		t.Fatal(err)
	}
	return rev1.ID
}

// TestPrivateRepoTokenPull verifies the read path carries the remote token:
// a private repo rejects an anonymous pull but allows a token-holding member.
func TestPrivateRepoTokenPull(t *testing.T) {
	s := newTestServer(t, "secret")
	seedRepo(t, s)
	repo, _ := s.cs.OpenRepo(store.RepoRef{Namespace: "team", Name: "app"})
	if err := repo.UpdateRepoMeta(store.RepoMeta{Visibility: "private", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/repo/team/app/advertise", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous advertise on private repo should be 401/403, got %d", rec.Code)
	}

	req2 := httptest.NewRequest(http.MethodPost, "/repo/team/app/advertise", bytes.NewReader([]byte(`{}`)))
	req2.Header.Set("Authorization", "Bearer secret")
	rec2 := httptest.NewRecorder()
	s.Router().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("member advertise on private repo should be 200, got %d %s", rec2.Code, rec2.Body.String())
	}
}

// TestReadonlyRoleDeniedWrite verifies a readonly namespace member cannot push.
func TestReadonlyRoleDeniedWrite(t *testing.T) {
	s := newTestServer(t, "secret")
	seedRepo(t, s)
	// Downgrade the tester to readonly.
	u, err := s.cs.GetUserByUsername("tester")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.cs.AddNamespaceMember("team", u.ID, "readonly"); err != nil {
		t.Fatal(err)
	}
	repo, _ := s.cs.OpenRepo(store.RepoRef{Namespace: "team", Name: "app"})
	revs, _ := repo.ListRevisions()
	b := &transfer.Bundle{Version: transfer.Version, Repo: repo.RepoRef(), Revisions: revs,
		Refs: []*store.Ref{{Name: "main", Kind: store.RefBranch, Target: revs[0].ID}}}
	payload, _ := transfer.CompressBundle(b)
	req := httptest.NewRequest(http.MethodPost, "/repo/team/app/push", bytes.NewReader(payload))
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("readonly member push should be 403, got %d %s", rec.Code, rec.Body.String())
	}
}

// TestReadLevelTokenDeniedWrite verifies a read-level token cannot push even
// for a write-capable member.
func TestReadLevelTokenDeniedWrite(t *testing.T) {
	s := newTestServer(t, "secret")
	seedRepo(t, s)
	u, _ := s.cs.GetUserByUsername("tester")
	if _, err := s.cs.CreateToken("reader", u.ID, "read"); err != nil {
		t.Fatal(err)
	}
	repo, _ := s.cs.OpenRepo(store.RepoRef{Namespace: "team", Name: "app"})
	revs, _ := repo.ListRevisions()
	b := &transfer.Bundle{Version: transfer.Version, Repo: repo.RepoRef(), Revisions: revs,
		Refs: []*store.Ref{{Name: "main", Kind: store.RefBranch, Target: revs[0].ID}}}
	payload, _ := transfer.CompressBundle(b)
	req := httptest.NewRequest(http.MethodPost, "/repo/team/app/push", bytes.NewReader(payload))
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Authorization", "Bearer reader")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("read-level token push should be 403, got %d %s", rec.Code, rec.Body.String())
	}
}

// TestMirrorRepoRejectsPush verifies read-only mirror repos reject pushes.
func TestMirrorRepoRejectsPush(t *testing.T) {
	s := newTestServer(t, "")
	seedRepo(t, s)
	repo, _ := s.cs.OpenRepo(store.RepoRef{Namespace: "team", Name: "app"})
	if err := repo.UpdateMirrorMeta(store.RepoMeta{Kind: "mirror"}); err != nil {
		t.Fatal(err)
	}
	if !repo.IsMirror() {
		t.Fatal("repo should be a mirror")
	}
	revs, _ := repo.ListRevisions()
	b := &transfer.Bundle{Version: transfer.Version, Repo: repo.RepoRef(), Revisions: revs,
		Refs: []*store.Ref{{Name: "main", Kind: store.RefBranch, Target: revs[0].ID}}}
	payload, _ := transfer.CompressBundle(b)
	req := httptest.NewRequest(http.MethodPost, "/repo/team/app/push", bytes.NewReader(payload))
	req.Header.Set("Content-Encoding", "gzip")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("mirror push should be 403, got %d %s", rec.Code, rec.Body.String())
	}
}

// TestPushBodyLimit verifies oversized request bodies are rejected with 413.
func TestPushBodyLimit(t *testing.T) {
	s := newTestServer(t, "")
	seedRepo(t, s)
	s.SetMaxBody(1024)

	// A body whose decompressed size exceeds the limit after the wire bytes
	// are capped: send an uncompressed JSON bundle larger than the cap so the
	// MaxBytesReader trips mid-read.
	big := bytes.Repeat([]byte(" "), 8192)
	req := httptest.NewRequest(http.MethodPost, "/repo/team/app/push", bytes.NewReader(big))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized push should be 413, got %d %s", rec.Code, rec.Body.String())
	}
}

// TestConcurrentPushOneFastForward verifies two concurrent pushes to the same
// branch serialize: both succeed in order (the second sees the first's refs
// and must itself be a fast-forward) or one is rejected — but the store must
// never end up with a dangling ref or interleaved partial state.
func TestConcurrentPushOneFastForward(t *testing.T) {
	s := newTestServer(t, "")
	seedRepo(t, s)
	repo, _ := s.cs.OpenRepo(store.RepoRef{Namespace: "team", Name: "app"})
	revs, _ := repo.ListRevisions()
	var childID string
	for _, r := range revs {
		snap, _ := repo.GetSnapshot(r.Hash)
		if len(snap.Parents) > 0 {
			childID = r.ID
		}
	}
	b := &transfer.Bundle{Version: transfer.Version, Repo: repo.RepoRef(), Revisions: revs,
		Refs: []*store.Ref{{Name: "main", Kind: store.RefBranch, Target: childID}}}
	payload, _ := transfer.CompressBundle(b)

	var wg sync.WaitGroup
	codes := make([]int, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/repo/team/app/push", bytes.NewReader(payload))
			req.Header.Set("Content-Encoding", "gzip")
			rec := httptest.NewRecorder()
			s.Router().ServeHTTP(rec, req)
			codes[i] = rec.Code
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("identical concurrent push %d should be idempotent-200, got %d", i, c)
		}
	}
	// Ref still resolves after the storm.
	final, err := repo.GetRef("main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.GetRevision(final.Target); err != nil {
		t.Fatalf("dangling ref after concurrent pushes: %v", err)
	}
}

// TestAuditIPNotSpoofed verifies X-Forwarded-For is ignored without
// EASYVCS_TRUSTED_PROXY (the socket address is recorded instead).
func TestAuditIPNotSpoofed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("EASYVCS_HOME", home)
	cs, err := store.OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	_ = cs.SetWAL()
	sink := &mockSink{}
	s := New(cs, sink)
	seedRepo(t, s)
	req := httptest.NewRequest(http.MethodPost, "/repo/team/app/advertise", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("advertise: %d", rec.Code)
	}
	if len(sink.events) == 0 {
		t.Fatal("no audit event")
	}
	if sink.events[0].IP == "1.2.3.4" {
		t.Fatal("X-Forwarded-For must not be trusted without EASYVCS_TRUSTED_PROXY")
	}
}

// TestTokenStoredHashed verifies the tokens table never holds the plaintext.
func TestTokenStoredHashed(t *testing.T) {
	s := newTestServer(t, "secret")
	var raw string
	if err := s.cs.RawQuery("SELECT token FROM tokens LIMIT 1").Scan(&raw).Error; err != nil {
		t.Fatal(err)
	}
	if raw == "secret" {
		t.Fatal("token stored in plaintext")
	}
	if len(raw) != 64 { // sha256 hex
		t.Fatalf("token hash length = %d, want 64", len(raw))
	}
	// Lookup still resolves the plaintext.
	if tk, err := s.cs.LookupToken("secret"); err != nil || tk.UserID == 0 {
		t.Fatalf("lookup: %v %v", tk, err)
	}
}
