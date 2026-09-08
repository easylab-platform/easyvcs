// Package server is the authoritative EasyVCS VCS protocol server, expressed as
// a reusable library so it can be embedded by easylab or run as a standalone
// binary (see cmd/server). It exposes the change-native smart protocol
// (advertise / fetch / push) against the central store over HTTP:

//	POST /repo/{namespace}/{name}/advertise   (read: repo read ACL)
//	POST /repo/{namespace}/{name}/fetch       (read: repo read ACL)
//	POST /repo/{namespace}/{name}/push        (write: repo + branch ACL, non-fast-forward)
//
// Authentication uses bearer tokens resolved against the store's users/tokens
// tables (store.LookupToken). Access is enforced per repository and, for push,
// per branch (see store.AddBranchACL / CanPushBranch). A write operation is
// recorded in the append-only audit_log when an AuditSink is configured.
//
// When no users/tokens are registered the instance is treated as open (CLI
// default) and anonymous read/write is allowed; a configured branch allowlist
// still applies to push.
package server

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

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

// AuditSink receives an AuditEvent for access attempts. The default sink writes
// to the store's append-only audit_log table; tests can supply a mock.
type AuditSink interface {
	Record(store.AuditEvent)
}

// Server is the VCS protocol handler. It resolves bearer tokens against the
// central store on every request and enforces repository/branch ACLs.
type Server struct {
	cs    *store.CentralStore
	audit AuditSink
}

// New builds a Server over the given central store. If sink is nil a default
// store-backed sink is used (writing audit_log). The tokens parameter is gone:
// tokens are resolved via store.LookupToken on each request.
func New(cs *store.CentralStore, sink AuditSink) *Server {
	if sink == nil {
		sink = &storeAuditSink{cs: cs}
	}
	return &Server{cs: cs, audit: sink}
}

// Handler returns the HTTP router for this server. It may be mounted directly
// (e.g. http.Server{Handler: s.Handler()} or embedded in a larger mux).
func (s *Server) Handler() http.Handler {
	return s.router()
}

// Router returns the *http.ServeMux (same as Handler but typed).
func (s *Server) Router() *http.ServeMux { return s.router() }

// storeAuditSink records AuditEvents into the store's audit_log table.
type storeAuditSink struct{ cs *store.CentralStore }

func (s *storeAuditSink) Record(ev store.AuditEvent) {
	_ = s.cs.RecordAudit(ev)
}

// authenticate resolves the request's bearer token to a user. It returns the
// token-level record (with UserID/Level) and true, or (nil,false) for an
// unauthenticated request. An open instance (no registered users/tokens) allows
// anonymous access (token=nil, ok=true via instanceIsOpen).
func (s *Server) authenticate(r *http.Request) (*store.Token, bool) {
	header := r.Header.Get("Authorization")
	raw := ""
	if strings.HasPrefix(header, "Bearer ") {
		raw = strings.TrimPrefix(header, "Bearer ")
	}
	if raw == "" {
		// No credentials: allowed only if the instance is open.
		return nil, s.cs.IsOpenInstance()
	}
	tk, err := s.cs.LookupToken(raw)
	if err != nil {
		return nil, false
	}
	return tk, true
}

// userID returns a *int64 for a token, or nil for anonymous.
func userID(tk *store.Token) *int64 {
	if tk == nil {
		return nil
	}
	return &tk.UserID
}

// requireRepoAccess guards a handler with repository read/write ACL. write=false
// for advertise/fetch; write=true for push. It records an audit event on both
// allow and deny.
func (s *Server) requireRepoAccess(write bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ns, name := r.PathValue("ns"), r.PathValue("name")
		tk, ok := s.authenticate(r)
		if !ok {
			s.audit.Record(store.AuditEvent{Timestamp: time.Now().UTC().UnixMilli(), Action: actionName(r), Namespace: ns, Repo: name, UserID: nil, IP: remoteIP(r), Outcome: "denied", Detail: "bad token"})
			writeErr(w, http.StatusUnauthorized, fmt.Errorf("unauthorized: missing or invalid token"))
			return
		}
		rref := store.RepoRef{Namespace: ns, Name: name}
		if write {
			if !s.cs.UserCanWriteRepo(rref, userID(tk)) {
				s.audit.Record(store.AuditEvent{Timestamp: time.Now().UTC().UnixMilli(), Action: actionName(r), Namespace: ns, Repo: name, UserID: userID(tk), IP: remoteIP(r), Outcome: "denied", Detail: "no write access"})
				writeErr(w, http.StatusForbidden, fmt.Errorf("forbidden: no write access to %s", rref))
				return
			}
			// Branch-level allowlist is enforced in handlePush once the bundle is
			// decoded (CanPushBranch per ref).
		} else if !s.cs.UserCanReadRepo(rref, userID(tk)) {
			s.audit.Record(store.AuditEvent{Timestamp: time.Now().UTC().UnixMilli(), Action: actionName(r), Namespace: ns, Repo: name, UserID: userID(tk), IP: remoteIP(r), Outcome: "denied", Detail: "no read access"})
			writeErr(w, http.StatusForbidden, fmt.Errorf("forbidden: no read access to %s", rref))
			return
		}
		// Record the allowed read access (push success is recorded in handlePush).
		if !write {
			s.audit.Record(store.AuditEvent{Timestamp: time.Now().UTC().UnixMilli(), Action: actionName(r), Namespace: ns, Repo: name, UserID: userID(tk), IP: remoteIP(r), Outcome: "ok"})
		}
		next(w, r)
	}
}


// actionName returns the protocol action for audit purposes.
func actionName(r *http.Request) string {
	p := r.URL.Path
	switch {
	case strings.HasSuffix(p, "/advertise"):
		return "advertise"
	case strings.HasSuffix(p, "/fetch"):
		return "fetch"
	case strings.HasSuffix(p, "/push"):
		return "push"
	default:
		return r.Method + " " + p
	}
}

// remoteIP returns the caller's IP (best-effort).
func remoteIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	return r.RemoteAddr
}

func (s *Server) router() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /repo/{ns}/{name}/advertise", s.requireRepoAccess(false, s.handleAdvertise))
	mux.HandleFunc("POST /repo/{ns}/{name}/fetch", s.requireRepoAccess(false, s.handleFetch))
	mux.HandleFunc("POST /repo/{ns}/{name}/push", s.requireRepoAccess(true, s.handlePush))
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
	ns, name := r.PathValue("ns"), r.PathValue("name")
	repo, ok := s.repo(w, r)
	if !ok {
		return
	}
	b, err := decodeBundleBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// Branch-level allowlist: for each branch being pushed, the user must be
	// allowed to push it. A user with no branch-ACL rows falls back to the repo
	// write role (already granted by requireRepoAccess).
	if tk, authOK := s.authenticate(r); authOK && tk != nil {
		for _, rf := range b.Refs {
			if rf.Kind != store.RefBranch {
				continue
			}
			allowed, aerr := repo.CanPushBranch(tk.UserID, rf.Name)
			if aerr != nil {
				writeErr(w, http.StatusBadRequest, aerr)
				return
			}
			if !allowed {
				s.audit.Record(store.AuditEvent{Timestamp: time.Now().UTC().UnixMilli(), Action: "push", Namespace: ns, Repo: name, UserID: &tk.UserID, IP: remoteIP(r), Outcome: "denied", Detail: "branch " + rf.Name + " not in allowlist"})
				writeErr(w, http.StatusForbidden, fmt.Errorf("forbidden: branch %s not allowed for this user", rf.Name))
				return
			}
		}
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
	// Audit the successful write.
	if tk, authOK := s.authenticate(r); authOK {
		s.audit.Record(store.AuditEvent{Timestamp: time.Now().UTC().UnixMilli(), Action: "push", Namespace: ns, Repo: name, UserID: userID(tk), IP: remoteIP(r), Outcome: "ok", Detail: fmt.Sprintf("applied=%d", n)})
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

