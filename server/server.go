// Package server is the authoritative EasyVCS VCS protocol server, expressed as
// a reusable library so it can be embedded by easylab or run as a standalone
// binary (see cmd/server). It exposes the change-native smart protocol
// (advertise / fetch / push) against the central store over HTTP:

//	POST /repo/{namespace}/{name}/advertise   (read-only)
//	POST /repo/{namespace}/{name}/fetch       (read-only)
//	POST /repo/{namespace}/{name}/push        (auth + non-fast-forward check)
//
// By default it speaks HTTP/1.1 and cleartext HTTP/2, so the easyvcs CLI (h1+h2
// dual-stack) and the easylab aggregator both reach it. Write endpoints require
// a bearer token from EASYVCS_TOKEN (comma-separated); when unset, write is
// open (like the CLI-driven easylab default).
package server

import (
	"bytes"
	"compress/gzip"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/store"
	"github.com/easylab-platform/easyvcs/transfer"
)

// AdvertiseReq/Resp mirror the CLI's smart protocol request/response.
type AdvertiseReq struct {
	Have        []string `json:"have"`
	HaveObjects []string `json:"have_objects"`
	WantChanges []string `json:"want_changes,omitempty"`
}

type AdvertiseResp struct {
	Repo    string       `json:"repo"`
	Changes []string     `json:"changes"`
	Refs    []*store.Ref `json:"refs"`
}

// Server is the VCS protocol handler. It is scope-free; callers supply the
// central store and an optional token set.
type Server struct {
	cs     *store.CentralStore
	tokens map[string]bool
}

// New builds a Server over the given central store. tokens is a set of accepted
// bearer tokens; an empty set means write is open (auth disabled).
func New(cs *store.CentralStore, tokens map[string]bool) *Server {
	if tokens == nil {
		tokens = map[string]bool{}
	}
	return &Server{cs: cs, tokens: tokens}
}

// Handler returns the HTTP router for this server. It may be mounted directly
// (e.g. http.Server{Handler: s.Handler()} or embedded in a larger mux).
func (s *Server) Handler() http.Handler {
	return s.router()
}

// Router returns the *http.ServeMux (same as Handler but typed).
func (s *Server) Router() *http.ServeMux { return s.router() }

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authOK(r) {
			writeErr(w, http.StatusUnauthorized, fmt.Errorf("unauthorized: missing or invalid token"))
			return
		}
		next(w, r)
	}
}

func (s *Server) authOK(r *http.Request) bool {
	if len(s.tokens) == 0 {
		return true // open server (CLI default)
	}
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return false
	}
	token := strings.TrimPrefix(header, "Bearer ")
	if token == "" {
		return false
	}
	for valid := range s.tokens {
		if subtle.ConstantTimeCompare([]byte(token), []byte(valid)) == 1 {
			return true
		}
	}
	return false
}

func (s *Server) router() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /repo/{ns}/{name}/advertise", s.handleAdvertise)
	mux.HandleFunc("POST /repo/{ns}/{name}/fetch", s.handleFetch)
	mux.HandleFunc("POST /repo/{ns}/{name}/push", s.requireAuth(s.handlePush))
	return mux
}

func (s *Server) repo(w http.ResponseWriter, r *http.Request) (*store.Repo, bool) {
	repo, err := s.cs.OpenRepo(store.RepoRef{Namespace: r.PathValue("ns"), Name: r.PathValue("name")})
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return nil, false
	}
	return repo, true
}

func (s *Server) handleAdvertise(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.repo(w, r)
	if !ok {
		return
	}
	changes, err := repo.ListRevisions()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	refs, err := repo.ListRefs()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	var ids []string
	for _, c := range changes {
		ids = append(ids, c.ID)
	}
	writeJSON(w, http.StatusOK, AdvertiseResp{Repo: repo.String(), Changes: ids, Refs: refs})
}

func (s *Server) handleFetch(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.repo(w, r)
	if !ok {
		return
	}
	req, err := decodeJSONBody[AdvertiseReq](r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	haveObj := map[string]bool{}
	for _, o := range req.HaveObjects {
		haveObj[o] = true
	}
	b, err := transfer.Collect(repo, req.Have, func(id object.ID) bool {
		return haveObj[id.String()]
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if len(req.WantChanges) > 0 {
		wanted := changeSetWithAncestors(repo, req.WantChanges, req.Have)
		b = transfer.FilterBundleWant(b, wanted)
	}
	writeBundle(w, b)
}

func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.repo(w, r)
	if !ok {
		return
	}
	b, err := decodeBundleBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// Reject a non-fast-forward update: compare the server's current refs to
	// the incoming bundle's refs. The authoritative check is server-side against
	// the store (the bundle carries the incoming objects needed to resolve a
	// fast-forward whose tip is not yet in the repo).
	serverRefs, err := repo.ListRefs()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	conflicts := transfer.CheckNonFastForward(repo, serverRefs, b.Refs, b)
	if len(conflicts) > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":            "non-fast-forward",
			"conflicting_refs": conflicts,
			"message":          "non-fast-forward update rejected; pull and merge before pushing",
		})
		return
	}
	n, err := transfer.Apply(repo, b)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"applied": n, "repo": repo.String()})
}

func changeSetWithAncestors(repo *store.Repo, wants []string, have []string) map[string]bool {
	result := map[string]bool{}
	changes, err := repo.ListRevisions()
	if err != nil {
		return result
	}
	byID := map[string]*store.Revision{}
	snapOwner := map[string]string{}
	for _, c := range changes {
		byID[c.ID] = c
		snapOwner[c.Hash.String()] = c.ID
	}
	haveSet := map[string]bool{}
	for _, h := range have {
		haveSet[h] = true
	}
	var visit func(id string)
	visit = func(id string) {
		if haveSet[id] || result[id] {
			return
		}
		result[id] = true
		ch := byID[id]
		if ch == nil {
			return
		}
		if snap, err := repo.GetSnapshot(ch.Hash); err == nil {
			for _, p := range snap.Parents {
				if owner := snapOwner[p.String()]; owner != "" {
					visit(owner)
				}
			}
		}
	}
	for _, w := range wants {
		visit(w)
	}
	return result
}

func writeBundle(w http.ResponseWriter, b *transfer.Bundle) {
	encoded, err := transfer.CompressBundle(b)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	// A write failure while streaming the bundle is best-effort; headers are set
	// and the client may simply have disconnected.
	_, _ = w.Write(encoded)
}

func decodeBundleBody(r *http.Request) (*transfer.Bundle, error) {
	reader := r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		gr, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, err
		}
		defer gr.Close()
		reader = gr
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if bytes.HasPrefix(data, []byte("EVCSBUN")) {
		return transfer.UnmarshalBinary(data)
	}
	var b transfer.Bundle
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

func decodeJSONBody[T any](r *http.Request) (T, error) {
	var v T
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		return v, err
	}
	return v, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	// Encode errors are best-effort for a response writer (client disconnect).
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"error": err.Error()})
}

