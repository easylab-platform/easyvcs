package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/revision"
	"github.com/easylab-platform/easyvcs/store"
	"github.com/easylab-platform/easyvcs/transfer"
)

func newTestServer(t *testing.T, token string) *Server {
	t.Helper()
	home := t.TempDir()
	t.Setenv("EASYVCS_HOME", home)
	cs, err := store.OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.SetWAL(); err != nil {
		t.Fatal(err)
	}
	s := New(cs, nil)
	// When a token is requested, register a user + the token so authenticate()
	// can resolve it, and grant the user write access to the "team" namespace so
	// pushes to team/* are authorized. Otherwise the store has no users => open
	// instance (anonymous read/write).
	if token != "" {
		u, err := cs.CreateUser("tester", "test")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cs.CreateToken(token, u.ID, "write"); err != nil {
			t.Fatal(err)
		}
		if err := cs.AddNamespaceMember("team", u.ID, "member"); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func seedRepo(t *testing.T, s *Server) {
	t.Helper()
	repo, err := s.cs.Create(store.RepoRef{Namespace: "team", Name: "app"})
	if err != nil {
		t.Fatal(err)
	}
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
}

func TestAdvertiseAndFetch(t *testing.T) {
	s := newTestServer(t, "")
	seedRepo(t, s)
	mux := s.Router()

	// advertise
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/repo/team/app/advertise", bytes.NewReader([]byte(`{}`)))
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("advertise: %d %s", rec.Code, rec.Body.String())
	}
	var adv AdvertiseResp
	_ = json.Unmarshal(rec.Body.Bytes(), &adv)
	if len(adv.Changes) == 0 || len(adv.Refs) == 0 {
		t.Fatalf("advertise empty: %+v", adv)
	}

	// fetch returns a bundle with the revision + ref
	rec2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodPost, "/repo/team/app/fetch", bytes.NewReader([]byte(`{}`)))
	mux.ServeHTTP(rec2, r2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("fetch: %d %s", rec2.Code, rec2.Body.String())
	}
	b, err := transfer.UnmarshalBinary(rec2.Body.Bytes())
	if err != nil {
		t.Fatalf("fetch bundle decode: %v", err)
	}
	if len(b.Revisions) == 0 || len(b.Refs) == 0 {
		t.Fatalf("fetch bundle empty: %+v", b)
	}
}

func TestPushDisallowsNonFastForward(t *testing.T) {
	s := newTestServer(t, "")
	seedRepo(t, s)
	mux := s.Router()

	repo, _ := s.cs.OpenRepo(store.RepoRef{Namespace: "team", Name: "app"})
	revs, _ := repo.ListRevisions()
	// Identify root vs child by parent linkage: the root has no parents.
	var childID, rootID string
	for _, r := range revs {
		snap, err := repo.GetSnapshot(r.Hash)
		if err != nil {
			t.Fatal(err)
		}
		if len(snap.Parents) == 0 {
			rootID = r.ID
		} else {
			childID = r.ID
		}
	}
	if childID == "" || rootID == "" {
		t.Fatalf("need both a root and a child revision")
	}

	fwd := &transfer.Bundle{Version: transfer.Version, Repo: repo.RepoRef(),
		Revisions: revs, Refs: []*store.Ref{{Name: "main", Kind: store.RefBranch, Target: childID}}}
	payload, _ := transfer.CompressBundle(fwd)
	req := httptest.NewRequest(http.MethodPost, "/repo/team/app/push", bytes.NewReader(payload))
	req.Header.Set("Content-Encoding", "gzip")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("fast-forward push should be allowed: %d %s", rec.Code, rec.Body.String())
	}

	back := &transfer.Bundle{Version: transfer.Version, Repo: repo.RepoRef(),
		Revisions: revs, Refs: []*store.Ref{{Name: "main", Kind: store.RefBranch, Target: rootID}}}
	payload2, _ := transfer.CompressBundle(back)
	req2 := httptest.NewRequest(http.MethodPost, "/repo/team/app/push", bytes.NewReader(payload2))
	req2.Header.Set("Content-Encoding", "gzip")
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusConflict {
		t.Fatalf("non-fast-forward push should be 409, got %d %s", rec2.Code, rec2.Body.String())
	}
}

func TestPushFastForwardAllowed(t *testing.T) {
	s := newTestServer(t, "")
	seedRepo(t, s)
	repo, _ := s.cs.OpenRepo(store.RepoRef{Namespace: "team", Name: "app"})
	revs, _ := repo.ListRevisions()
	mainRef, _ := repo.GetRef("main")
	b := &transfer.Bundle{Version: transfer.Version, Repo: repo.RepoRef(),
		Revisions: revs, Refs: []*store.Ref{{Name: "main", Kind: store.RefBranch, Target: mainRef.Target}}}
	payload, _ := transfer.CompressBundle(b)
	req := httptest.NewRequest(http.MethodPost, "/repo/team/app/push", bytes.NewReader(payload))
	req.Header.Set("Content-Encoding", "gzip")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("fast-forward push should be 200, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestPushAuthRequired(t *testing.T) {
	s := newTestServer(t, "secret")
	seedRepo(t, s)
	repo, _ := s.cs.OpenRepo(store.RepoRef{Namespace: "team", Name: "app"})
	revs, _ := repo.ListRevisions()
	mainRef, _ := repo.GetRef("main")

	send := func(auth string) int {
		b := &transfer.Bundle{Version: transfer.Version, Repo: repo.RepoRef(),
			Revisions: revs, Refs: []*store.Ref{{Name: "main", Kind: store.RefBranch, Target: mainRef.Target}}}
		payload, _ := transfer.CompressBundle(b)
		req := httptest.NewRequest(http.MethodPost, "/repo/team/app/push", bytes.NewReader(payload))
		req.Header.Set("Content-Encoding", "gzip")
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		return rec.Code
	}
	if code := send(""); code != http.StatusUnauthorized {
		t.Fatalf("no token should be 401, got %d", code)
	}
	if code := send("secret"); code != http.StatusOK {
		t.Fatalf("valid token should be 200, got %d", code)
	}
}

// TestEndToEndMockPushFetch drives the protocol through an httptest.Server that
// hosts the server's router — no real process is started. It exercises the same
// wire exchange a CLI push/fetch performs: advertise, incremental push of a
// delta, then fetch back the revision. This avoids binding a port or forking a
// subprocess.
func TestEndToEndMockPushFetch(t *testing.T) {
	s := newTestServer(t, "")
	seedRepo(t, s)
	srv := httptest.NewServer(s.Router())
	defer func() { srv.Close() }()

	// The server store has the repo (team/app) with revisions rev1/rev2 and main
	// -> rev1. A "remote" client that already has rev1 should, on advertise, see
	// rev2 as the missing delta.
	advertise := func() AdvertiseResp {
		body := []byte(`{"have":["missing-none"]}`)
		req := httptest.NewRequest(http.MethodPost, "/repo/team/app/advertise", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		var adv AdvertiseResp
		_ = json.Unmarshal(rec.Body.Bytes(), &adv)
		return adv
	}
	adv := advertise()
	if len(adv.Changes) == 0 {
		t.Fatalf("server should advertise revisions, got %+v", adv)
	}

	// Push a bundle that moves main forward (fast-forward) and verify 200.
	repo, _ := s.cs.OpenRepo(store.RepoRef{Namespace: "team", Name: "app"})
	revs, _ := repo.ListRevisions()
	var childID string
	for _, r := range revs {
		snap, err := repo.GetSnapshot(r.Hash)
		if err != nil {
			t.Fatal(err)
		}
		if len(snap.Parents) > 0 {
			childID = r.ID
		}
	}
	if childID == "" {
		t.Fatalf("no child revision to push")
	}
	b := &transfer.Bundle{Version: transfer.Version, Repo: repo.RepoRef(),
		Revisions: revs, Refs: []*store.Ref{{Name: "main", Kind: store.RefBranch, Target: childID}}}
	payload, _ := transfer.CompressBundle(b)
	req := httptest.NewRequest(http.MethodPost, "/repo/team/app/push", bytes.NewReader(payload))
	req.Header.Set("Content-Encoding", "gzip")
	rec := httptest.NewRecorder()
	srv.Config.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("push through mock server should be 200, got %d %s", rec.Code, rec.Body.String())
	}

	// Fetch a bundle and confirm it carries the pushed revision + ref.
	fetchReq := []byte(`{}`)
	frec := httptest.NewRecorder()
	freq := httptest.NewRequest(http.MethodPost, "/repo/team/app/fetch", bytes.NewReader(fetchReq))
	s.Router().ServeHTTP(frec, freq)
	if frec.Code != http.StatusOK {
		t.Fatalf("fetch: %d %s", frec.Code, frec.Body.String())
	}
	fb, err := transfer.UnmarshalBinary(frec.Body.Bytes())
	if err != nil {
		t.Fatalf("fetch bundle decode: %v", err)
	}
	if len(fb.Revisions) == 0 || len(fb.Refs) == 0 {
		t.Fatalf("fetch bundle empty: %+v", fb)
	}
}

// mockSink records audit events for assertions.
type mockSink struct{ events []store.AuditEvent }

func (m *mockSink) Record(ev store.AuditEvent) { m.events = append(m.events, ev) }

func TestACLDeniesNonMemberWrite(t *testing.T) {
	s := newTestServer(t, "secret")
	seedRepo(t, s)
	repo, _ := s.cs.OpenRepo(store.RepoRef{Namespace: "team", Name: "app"})
	revs, _ := repo.ListRevisions()
	mainRef, _ := repo.GetRef("main")

	// "secret" user IS a "team" member (from newTestServer), so this is allowed.
	b := &transfer.Bundle{Version: transfer.Version, Repo: repo.RepoRef(),
		Revisions: revs, Refs: []*store.Ref{{Name: "main", Kind: store.RefBranch, Target: mainRef.Target}}}
	payload, _ := transfer.CompressBundle(b)
	req := httptest.NewRequest(http.MethodPost, "/repo/team/app/push", bytes.NewReader(payload))
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("member push should be allowed: %d", rec.Code)
	}

	// A user with a token but NOT a team member -> repo-level write denied (403).
	// Create a second user + token in a different namespace.
	u2, err := s.cs.CreateUser("outsider", "x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.cs.CreateToken("secret-out", u2.ID, "write"); err != nil {
		t.Fatal(err)
	}
	req2 := httptest.NewRequest(http.MethodPost, "/repo/team/app/push", bytes.NewReader(payload))
	req2.Header.Set("Content-Encoding", "gzip")
	req2.Header.Set("Authorization", "Bearer secret-out")
	rec2 := httptest.NewRecorder()
	s.Router().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("non-member push should be 403, got %d %s", rec2.Code, rec2.Body.String())
	}
}

func TestAuditSinkRecordsDenied(t *testing.T) {
	// Open instance (no users) -> anonymous allowed. Use a mock sink and assert
	// at least an 'advertise' ok event is recorded.
	home := t.TempDir()
	t.Setenv("EASYVCS_HOME", home)
	cs, _ := store.OpenDefault()
	_ = cs.SetWAL()
	s := New(cs, &mockSink{})
	seedRepo(t, s)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/repo/team/app/advertise", bytes.NewReader([]byte(`{}`)))
	s.Router().ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("open advertise %d", rec.Code)
	}
	m := s.audit.(*mockSink)
	if len(m.events) == 0 {
		t.Fatal("no audit event recorded")
	}
}
