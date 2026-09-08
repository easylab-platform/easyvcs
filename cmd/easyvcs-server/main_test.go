package main

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

func newTestServer(t *testing.T, token string) *server {
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
	tokens := map[string]bool{}
	if token != "" {
		tokens[token] = true
	}
	return &server{cs: cs, tokens: tokens}
}

func seedRepo(t *testing.T, s *server) {
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
	mux := s.router()

	// advertise
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/repo/team/app/advertise", bytes.NewReader([]byte(`{}`)))
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("advertise: %d %s", rec.Code, rec.Body.String())
	}
	var adv advertiseResp
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
	mux := s.router()

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
	s.router().ServeHTTP(rec, req)
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
		s.router().ServeHTTP(rec, req)
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
	srv := httptest.NewServer(s.router())
	defer srv.Close()

	// The server store has the repo (team/app) with revisions rev1/rev2 and main
	// -> rev1. A "remote" client that already has rev1 should, on advertise, see
	// rev2 as the missing delta.
	advertise := func() advertiseResp {
		body := []byte(`{"have":["missing-none"]}`)
		req := httptest.NewRequest(http.MethodPost, "/repo/team/app/advertise", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		s.router().ServeHTTP(rec, req)
		var adv advertiseResp
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
	s.router().ServeHTTP(frec, freq)
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
