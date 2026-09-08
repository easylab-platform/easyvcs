package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/revision"
	"github.com/easylab-platform/easyvcs/store"
	"github.com/easylab-platform/easyvcs/transfer"
)

// stubRemoteServer fakes the easylab smart-protocol endpoints used by
// collaborativePull: POST /repo/{ns}/{name}/advertise and /fetch. The advertised
// branchs/refs and the returned bundle come from a supplied remote store.
func stubRemoteServer(t *testing.T, remoteRepo *store.Repo) *httptest.Server {
	t.Helper()
	// Pre-build the bundle once (full: all revisions, refs, objects).
	bundle, err := transfer.CollectAll(remoteRepo)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := remoteRepo.ListRefs()
	if err != nil {
		t.Fatal(err)
	}
	revs, err := remoteRepo.ListRevisions()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range revs {
		ids = append(ids, r.ID)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /repo/demo/source/advertise", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(advertiseResp{Repo: "demo/source", Changes: ids, Refs: refs})
	})
	mux.HandleFunc("POST /repo/demo/source/fetch", func(w http.ResponseWriter, r *http.Request) {
		enc, err := transfer.CompressBundle(bundle)
		if err != nil {
			t.Fatalf("compress: %v", err)
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(enc)
	})
	// easylab gateway speaks HTTP/2 (h2c) — make the fake remote dual-stack.
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = protocols
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

func TestCollaborativePullMergesRemoteFirstTime(t *testing.T) {
	// ---- Remote store (the "other" easylab) ----
	setHome(t)
	csA, err := store.OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	_ = csA
	remoteRepo := createRepoIn(t, "demo", "source")
	ws := revision.NewWorkspace(remoteRepo)
	// commit base + feat on remote, then branch feature.
	tree := object.NewTree()
	tree.Entries["base.txt"] = object.Entry{Name: "base.txt", Kind: object.KindBlob, ID: mustWriteBlob(t, ws, []byte("base"))}
	s1, r1, errc := ws.Commit(revision.CommitParams{TreeID: mustWriteTree(t, ws, tree), Description: "base", Author: store.Author{Name: "t", Email: "t@x"}})
	if errc != nil {
		t.Fatal(errc)
	}
	tree2 := object.NewTree()
	tree2.Entries["base.txt"] = object.Entry{Name: "base.txt", Kind: object.KindBlob, ID: mustWriteBlob(t, ws, []byte("base"))}
	tree2.Entries["feat.txt"] = object.Entry{Name: "feat.txt", Kind: object.KindBlob, ID: mustWriteBlob(t, ws, []byte("feature-work"))}
	_, r2, errb := ws.Commit(revision.CommitParams{Parents: []object.ID{s1.RevisionHash}, TreeID: mustWriteTree(t, ws, tree2), Description: "feat", Author: store.Author{Name: "t", Email: "t@x"}})
	if errb != nil {
		t.Fatal(errb)
	}
	if _, err := ws.SetRef("feature", store.RefBranch, r2.ID); err != nil {
		t.Fatal(err)
	}
	// Keep the remote server alive with this repo.
	srv := stubRemoteServer(t, remoteRepo)
	defer func() {
		srv.Close()
		_ = r1
	}()

	// ---- Local store (the consumer) ----
	setHome(t)
	localRepo := createRepoIn(t, "demo", "source")
	lws := revision.NewWorkspace(localRepo)
	// Local has a divergent tip on main: base.txt modified.
	lt := object.NewTree()
	lt.Entries["base.txt"] = object.Entry{Name: "base.txt", Kind: object.KindBlob, ID: mustWriteBlob(t, lws, []byte("base-local"))}
	lt.Entries["local.txt"] = object.Entry{Name: "local.txt", Kind: object.KindBlob, ID: mustWriteBlob(t, lws, []byte("local-work"))}
	ls, lr, errl := lws.Commit(revision.CommitParams{TreeID: mustWriteTree(t, lws, lt), Description: "local", Author: store.Author{Name: "t", Email: "t@x"}})
	if errl != nil {
		t.Fatal(errl)
	}
	if _, serr := lws.SetRef("main", store.RefBranch, lr.ID); serr != nil {
		t.Fatal(err)
	}

	// The advertised branch feature -> tip rev id (r2.ID).
	adv := advertiseResp{}
	// We need r2.ID; fetch it from remote revs.
	rid := r2.ID

	fetchReq := advertiseReq{Have: localRevIDs(localRepo), HaveObjects: ownObjectIDs(localRepo)}
	// Register the remote so last_sync_tip can be persisted.
	if err := localRepo.PutRemote("origin", srv.URL+"/repo/demo/source", ""); err != nil {
		t.Fatal(err)
	}
	rem := &store.Remote{Name: "origin", URL: srv.URL + "/repo/demo/source"}
	full := srv.URL + "/repo/demo/source"

	// Run collaborativePull: fetch + rebase remote tip onto local main tip.
	_, _ = adv, rid
	if err := collaborativePull(&ctx{}, localRepo, rem, full, fetchReq, "main", rid); err != nil {
		t.Fatalf("collaborativePull: %v", err)
	}

	// The main branch should now point at a merged revision whose tree has
	// base.txt (local), local.txt (local), and feat.txt (remote) — i.e. the
	// remote edit was rebased onto the local line.
	mainRef, err := localRepo.GetRef("main")
	if err != nil {
		t.Fatal(err)
	}
	mrev, err := localRepo.GetRevision(mainRef.Target)
	if err != nil {
		t.Fatal(err)
	}
	msnap, err := localRepo.GetSnapshot(mrev.Hash)
	if err != nil {
		t.Fatal(err)
	}
	mergedTree := lws.MustTree(msnap.TreeID)
	names := map[string]bool{}
	for n := range mergedTree.Entries {
		names[n] = true
	}
	if !names["base.txt"] || !names["local.txt"] || !names["feat.txt"] {
		t.Fatalf("merged tree missing files: %v", names)
	}
	// local sync tip recorded.
	if tip, _ := localRepo.GetLastSyncTip("origin"); tip == "" {
		t.Fatalf("last_sync_tip should be recorded, got %q", tip)
	}
	_ = ls
}

func createRepoIn(t *testing.T, ns, name string) *store.Repo {
	t.Helper()
	home := t.TempDir()
	t.Setenv("EASYVCS_HOME", home)
	cs, err := store.OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	repo, err := cs.Create(store.RepoRef{Namespace: ns, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return repo
}

func localRevIDs(repo *store.Repo) []string {
	revs, err := repo.ListRevisions()
	if err != nil {
		return nil
	}
	var out []string
	for _, r := range revs {
		out = append(out, r.ID)
	}
	return out
}

func ownObjectIDs(repo *store.Repo) []string {
	ids, _ := transfer.EnumerateObjectIDs(repo)
	var out []string
	for _, id := range ids {
		out = append(out, id.String())
	}
	return out
}

var _ = bytes.MinRead
var _ = gzip.NewReader
var _ = io.Discard
var _ = object.ID{}
