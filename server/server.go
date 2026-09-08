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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
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
	// repoLocks serializes pushes per repository so the non-fast-forward check
	// and the transactional apply happen atomically with respect to each other.
	repoLocks sync.Map // int64 (repo id) -> *sync.Mutex
	// maxBody bounds request bodies (http.MaxBytesReader); 0 uses the default.
	maxBody int64
}

// DefaultMaxBody is the default request body limit (512 MiB).
const DefaultMaxBody = 512 << 20

// New builds a Server over the given central store. If sink is nil a default
// store-backed sink is used (writing audit_log). The tokens parameter is gone:
// tokens are resolved via store.LookupToken on each request.
func New(cs *store.CentralStore, sink AuditSink) *Server {
	if sink == nil {
		sink = &storeAuditSink{cs: cs}
	}
	return &Server{cs: cs, audit: sink, maxBody: DefaultMaxBody}
}

// SetMaxBody overrides the request body limit (bytes). Values <= 0 restore the
// default.
func (s *Server) SetMaxBody(n int64) {
	if n <= 0 {
		n = DefaultMaxBody
	}
	s.maxBody = n
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
	if err := s.cs.RecordAudit(ev); err != nil {
		// Audit is append-only diagnostics; a write failure must not break the
		// request, but it must not be invisible either.
		log.Printf("easyvcs-server: audit write failed: %v", err)
	}
}

// auditDenied and auditOK are helpers that build an AuditEvent for the current
// request with the standard fields, so handler code stays one line.
func (s *Server) auditDenied(r *http.Request, tk *store.Token, detail string) {
	ev := auditEvent(r, tk)
	ev.Outcome = outcomeDenied
	ev.Detail = detail
	s.audit.Record(ev)
}

func (s *Server) auditOK(r *http.Request, tk *store.Token, detail string) {
	ev := auditEvent(r, tk)
	ev.Outcome = outcomeOK
	ev.Detail = detail
	s.audit.Record(ev)
}

// outcome constants for audit events.
const (
	outcomeDenied = "denied"
	outcomeOK     = "ok"
)

// auditEvent captures the common fields of an audit record from a request.
func auditEvent(r *http.Request, tk *store.Token) store.AuditEvent {
	return store.AuditEvent{
		Timestamp: time.Now().UTC().UnixMilli(),
		Action:    actionName(r),
		Namespace: r.PathValue("ns"),
		Repo:      r.PathValue("name"),
		UserID:    userID(tk),
		IP:        remoteIP(r),
	}
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
		// Bound the request body to prevent unbounded memory use (413 on
		// overrun; reads beyond the limit error out).
		r.Body = http.MaxBytesReader(w, r.Body, s.maxBody)
		tk, ok := s.authenticate(r)
		if !ok {
			s.auditDenied(r, nil, "bad token")
			writeErr(w, http.StatusUnauthorized, fmt.Errorf("unauthorized: missing or invalid token"))
			return
		}
		if write && tk != nil && tk.Level == "read" {
			s.auditDenied(r, tk, "read-level token")
			writeErr(w, http.StatusForbidden, fmt.Errorf("forbidden: read-level token cannot write"))
			return
		}
		rref := store.RepoRef{Namespace: r.PathValue("ns"), Name: r.PathValue("name")}
		if write {
			if !s.cs.UserCanWriteRepo(rref, userID(tk)) {
				s.auditDenied(r, tk, "no write access")
				writeErr(w, http.StatusForbidden, fmt.Errorf("forbidden: no write access to %s", rref))
				return
			}
			// Branch-level allowlist is enforced in handlePush once the bundle is
			// decoded (CanPushBranch per ref).
		} else if !s.cs.UserCanReadRepo(rref, userID(tk)) {
			s.auditDenied(r, tk, "no read access")
			writeErr(w, http.StatusForbidden, fmt.Errorf("forbidden: no read access to %s", rref))
			return
		}
		// Record the allowed read access (push success is recorded in handlePush).
		if !write {
			s.auditOK(r, tk, "")
		}
		// Stash the resolved token for the handler (avoid re-authenticating).
		next(w, r.WithContext(withToken(r.Context(), tk)))
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

// remoteIP returns the caller's IP. X-Forwarded-For is honored only when
// EASYVCS_TRUSTED_PROXY is set (i.e. the server sits behind a trusted proxy);
// otherwise the socket peer address is authoritative and client-supplied
// headers are ignored (they are trivially spoofable).
func remoteIP(r *http.Request) string {
	if os.Getenv("EASYVCS_TRUSTED_PROXY") != "" {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			return strings.TrimSpace(strings.Split(xff, ",")[0])
		}
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
	// The token resolved by requireRepoAccess, stashed in the request context.
	tk := tokenFrom(r.Context())
	repo, ok := s.repo(w, r)
	if !ok {
		return
	}
	// Read-only mirrors reject all writes.
	if repo.IsMirror() {
		s.auditDenied(r, tk, "mirror repo")
		writeErr(w, http.StatusForbidden, fmt.Errorf("forbidden: %s is a read-only mirror", repo))
		return
	}
	b, err := decodeBundleBody(r)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErr(w, http.StatusRequestEntityTooLarge, fmt.Errorf("request body exceeds limit (%d bytes)", mbe.Limit))
			return
		}
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// Branch-level allowlist: for each branch being pushed, the user must be
	// allowed to push it. A user with no branch-ACL rows falls back to the repo
	// write role (already granted by requireRepoAccess).
	if tk != nil {
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
				s.auditDenied(r, tk, "branch "+rf.Name+" not in allowlist")
				writeErr(w, http.StatusForbidden, fmt.Errorf("forbidden: branch %s not allowed for this user", rf.Name))
				return
			}
		}
	}
	// Serialize pushes per repo and run the authoritative non-fast-forward
	// check + transactional apply atomically, so two concurrent pushes cannot
	// interleave (no TOCTOU) and a failed apply leaves no partial state.
	var conflicts []*store.Ref
	var n int
	err = s.withRepoLock(repo, func() error {
		serverRefs, err := repo.ListRefs()
		if err != nil {
			return err
		}
		conflicts = transfer.CheckNonFastForward(repo, serverRefs, b.Refs, b)
		if len(conflicts) > 0 {
			return nil
		}
		n, err = transfer.Apply(repo, b)
		return err
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if len(conflicts) > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":            "non-fast-forward",
			"conflicting_refs": conflicts,
			"message":          "non-fast-forward update rejected; pull and merge before pushing",
		})
		return
	}
	// Audit the successful write.
	s.auditOK(r, tk, fmt.Sprintf("applied=%d", n))
	writeJSON(w, http.StatusOK, map[string]any{"applied": n, "repo": repo.String()})
}

// withToken stashes the resolved token in the request context for handlers.
type ctxKey int

const ctxToken ctxKey = iota

func withToken(ctx context.Context, tk *store.Token) context.Context {
	return context.WithValue(ctx, ctxToken, tk)
}

// tokenFrom retrieves the token stashed by requireRepoAccess (nil when
// anonymous).
func tokenFrom(ctx context.Context) *store.Token {
	tk, _ := ctx.Value(ctxToken).(*store.Token)
	return tk
}

// withRepoLock runs fn while holding the per-repo push mutex.
func (s *Server) withRepoLock(repo *store.Repo, fn func() error) error {
	v, _ := s.repoLocks.LoadOrStore(repo.RepoID(), &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	return fn()
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
	// Wrap the raw body first (not the gzip stream): MaxBytesReader counts the
	// compressed bytes actually read off the wire, which is the DoS-relevant
	// size. (Wrapping the gzip reader would allow a zip bomb to inflate.)
	raw := r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		gr, err := gzip.NewReader(io.LimitReader(raw, maxGzipStream))
		if err != nil {
			return nil, err
		}
		defer func() { _ = gr.Close() }()
		// Bound decompression too: a small gzip payload must not inflate to
		// unbounded memory.
		data, err := io.ReadAll(io.LimitReader(gr, maxInflatedBody))
		if err != nil {
			return nil, err
		}
		return unmarshalBundle(data)
	}
	data, err := io.ReadAll(raw)
	if err != nil {
		return nil, err
	}
	return unmarshalBundle(data)
}

// maxGzipStream bounds the gzip header read (tiny; the body is streamed via
// r.Body which is already MaxBytesReader-bounded).
const maxGzipStream = 1 << 20

// maxInflatedBody bounds the decompressed bundle size (16 GiB): compression
// ratios beyond that are abusive.
const maxInflatedBody = 16 << 30

func unmarshalBundle(data []byte) (*transfer.Bundle, error) {
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

// writeErr writes an error response. Client errors (4xx) carry the semantic
// message; server errors (5xx) are collapsed to a generic message (the raw
// error may leak SQL or filesystem details) and logged server-side instead.
func writeErr(w http.ResponseWriter, code int, err error) {
	if code >= http.StatusInternalServerError {
		log.Printf("easyvcs-server: %d: %v", code, err)
		writeJSON(w, code, map[string]any{"error": "internal server error"})
		return
	}
	writeJSON(w, code, map[string]any{"error": err.Error()})
}
