package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/revision"
	"github.com/easylab-platform/easyvcs/store"
	"github.com/easylab-platform/easyvcs/transfer"
)

// advertiseReq/Resp mirrors the server's smart protocol request/response.
type advertiseReq struct {
	Have        []string `json:"have"`
	HaveObjects []string `json:"have_objects"`
	WantChanges []string `json:"want_changes,omitempty"`
}

type advertiseResp struct {
	Repo    string       `json:"repo"`
	Changes []string     `json:"changes"`
	Refs    []*store.Ref `json:"refs"`
}

// --- remote configuration ---

func cmdRemote(c *ctx) {
	repo, _, err := c.loadRepo()
	if err != nil {
		fmt.Fprintln(os.Stderr, "remote:", err)
		os.Exit(1)
	}
	args := os.Args[2:]
	if len(args) == 0 {
		remotes, err := repo.ListRemotes()
		if err != nil {
			fmt.Fprintln(os.Stderr, "remote:", err)
			os.Exit(1)
		}
		for _, r := range remotes {
			fmt.Printf("%s\t%s\n", r.Name, r.URL)
		}
		return
	}
	switch args[0] {
	case "add":
		token := ""
		url := ""
		var name string
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--token", "-token":
				if i+1 < len(args) {
					token = args[i+1]
					i++
				}
			case "--url", "-url":
				if i+1 < len(args) {
					url = args[i+1]
					i++
				}
			default:
				if name == "" {
					name = args[i]
				} else if url == "" {
					url = args[i]
				}
			}
		}
		if name == "" || url == "" {
			fmt.Fprintln(os.Stderr, "usage: remote add <name> <url> [--token <token>]")
			os.Exit(1)
		}
		if err := repo.PutRemote(name, url, token); err != nil {
			fmt.Fprintln(os.Stderr, "remote add:", err)
			os.Exit(1)
		}
		if token != "" {
			fmt.Printf("added remote %s -> %s (with token)\n", name, url)
		} else {
			fmt.Printf("added remote %s -> %s\n", name, url)
		}
	case "remove", "rm":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: remote remove <name>")
			os.Exit(1)
		}
		if err := repo.DeleteRemote(args[1]); err != nil {
			fmt.Fprintln(os.Stderr, "remote remove:", err)
			os.Exit(1)
		}
		fmt.Printf("removed remote %s\n", args[1])
	default:
		rem, err := repo.GetRemote(args[0])
		if err != nil {
			fmt.Fprintln(os.Stderr, "remote:", err)
			os.Exit(1)
		}
		fmt.Printf("%s\t%s\n", rem.Name, rem.URL)
	}
}

// --- smart protocol: pull / push ---

// remoteClient returns a client that can talk to a remote easylab gateway over
// both HTTP/1.1 and HTTP/2. For https:// targets it uses TLS + ALPN so it
// negotiates h2 (or falls back to h1) based on the server; for http:// targets
// it uses cleartext h2 (h2c prior knowledge) and falls back to h1 if the server
// only speaks HTTP/1.1. This keeps the client compatible with a broad range of
// gateways without requiring QUIC/HTTP3.
func remoteClient() *http.Client {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	return &http.Client{Transport: &http.Transport{Protocols: protocols}}
}

// doPost performs an HTTP POST with an optional gzip-compressed body and token,
// returning the raw response body bytes.
func doPost(url string, body []byte, token string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if len(body) > 1024 {
		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		if _, err := gw.Write(body); err == nil {
			if err := gw.Close(); err == nil {
				req.Header.Set("Content-Encoding", "gzip")
				req.Body = io.NopCloser(bytes.NewReader(buf.Bytes()))
				req.ContentLength = int64(buf.Len())
			}
		}
	}
	resp, err := remoteClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		rb, _ := io.ReadAll(resp.Body)
		return nil, nonFastForwardOr(resp.StatusCode, rb, fmt.Errorf("server error %d: %s", resp.StatusCode, string(rb)))
	}
	return io.ReadAll(resp.Body)
}

// postJSON is a thin wrapper around doPost for small JSON payloads with a JSON
// response.
func postJSON(url string, body any, out any, token string) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if len(payload) > 1024 {
		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		if _, err := gw.Write(payload); err == nil {
			if err := gw.Close(); err == nil {
				req.Header.Set("Content-Encoding", "gzip")
				req.Body = io.NopCloser(bytes.NewReader(buf.Bytes()))
				req.ContentLength = int64(buf.Len())
			}
		}
	}
	resp, err := remoteClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		rb, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("server error %d: %s", resp.StatusCode, string(rb))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	// Drain the response body so the connection can be reused; a drain error is
	// non-fatal to the request's outcome and is intentionally not surfaced.
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// postBundle sends a fully-encoded bundle frame to the server and returns the
// raw response bytes.
//
// CompressBundle already produces a gzip-compressed binary frame, so the bytes
// are sent verbatim with Content-Encoding: gzip — doPost's size-based gzip must
// NOT be applied on top, otherwise the server sees a second gzip layer and
// cannot decode the frame.
func postBundle(url string, bundle *transfer.Bundle, token string) ([]byte, error) {
	if encoded, err := transfer.CompressBundle(bundle); err == nil {
		return doPostCompressed(url, encoded, token)
	}
	// Fallback to JSON if binary encoding fails.
	payload, _ := json.Marshal(bundle)
	return doPost(url, payload, token)
}

// doPostCompressed sends a body that is already gzip-compressed by the caller,
// marking it with Content-Encoding: gzip so the server decompresses exactly
// once. It never re-compresses the body.
func doPostCompressed(url string, body []byte, token string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "gzip")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := remoteClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		rb, _ := io.ReadAll(resp.Body)
		return nil, nonFastForwardOr(resp.StatusCode, rb, fmt.Errorf("server error %d: %s", resp.StatusCode, string(rb)))
	}
	return io.ReadAll(resp.Body)
}

// nonFastForwardOr inspects a non-200 response body. If the server reported a
// non-fast-forward conflict (409 with {"error":"non-fast-forward", ...}), it
// returns a *transfer.NonFastForwardError so callers can present a friendly
// message; otherwise it returns the given fallback error.
func nonFastForwardOr(code int, body []byte, fallback error) error {
	if code == http.StatusConflict {
		var v struct {
			Error            string         `json:"error"`
			ConflictingRefs  []*store.Ref   `json:"conflicting_refs"`
		}
		if err := json.Unmarshal(body, &v); err == nil && v.Error == "non-fast-forward" {
			return &transfer.NonFastForwardError{ConflictingRefs: v.ConflictingRefs}
		}
	}
	return fallback
}

// getBundle decodes either a binary bundle or a JSON bundle from raw bytes.
func getBundle(data []byte) (*transfer.Bundle, error) {
	if len(data) > 0 && data[0] != '{' {
		return transfer.UnmarshalBinary(data)
	}
	var b transfer.Bundle
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

func resolveRemote(repo *store.Repo, nameOrURL string) (*store.Remote, error) {
	if rem, err := repo.GetRemote(nameOrURL); err == nil {
		return rem, nil
	}
	return &store.Remote{Name: nameOrURL, URL: nameOrURL}, nil
}

// isLocalURL reports whether a remote URL denotes a local filesystem path
// (absolute, ./ or ../, ~-expanded, or a bare path) rather than a network
// endpoint. Network URLs carry an explicit "://" scheme or a host:port look.
func isLocalURL(u string) bool {
	trimmed := strings.TrimSpace(u)
	if trimmed == "" {
		return false
	}
	if strings.Contains(trimmed, "://") {
		return false
	}
	if strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, "./") ||
		strings.HasPrefix(trimmed, "../") || strings.HasPrefix(trimmed, "~") ||
		strings.HasPrefix(trimmed, ".") {
		return true
	}
	// A ":" denotes host:port (a network target), even when it carries a path.
	if strings.Contains(trimmed, ":") {
		return false
	}
	// A bare word with a '/' is a relative path; a bare word without one is a
	// host name and is treated as remote.
	return strings.Contains(trimmed, "/")
}

// expandLocalPath expands a leading "~" to the user home directory for a local
// remote path and resolves relative paths against the current working dir. It is
// only meaningful for local URLs (callers branch on isLocalURL first); a URL is
// returned unchanged.
func expandLocalPath(u string) string {
	p := strings.TrimSpace(u)
	if strings.Contains(p, "://") {
		return p
	}
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	if !filepath.IsAbs(p) {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
	}
	return p
}

// openLocalRepo opens the repository identified by a local remote path. The path
// must be a workspace directory (it carries a .easyvcs-workspace marker) whose
// marker records the store location (Home/DSN) and the repo ref. This keeps
// local remotes self-contained and independent of the current process home.
func openLocalRepo(path string) (*store.Repo, error) {
	marker, err := store.LookupMarker(expandLocalPath(path))
	if err != nil {
		return nil, fmt.Errorf("open local remote: %w", err)
	}
	cs, err := store.OpenStoreForMarker(marker)
	if err != nil {
		return nil, err
	}
	return cs.OpenRepo(marker.Repo)
}

// localRepoWithMarker is the local counterpart of a remote: it resolves the
// target repo and exposes the same advertise/fetch info the HTTP server would.
type localRepoWithMarker struct {
	repo   *store.Repo
	marker *store.WorkspaceMarker
}

// openLocalRepoWithMarker opens a local remote and keeps its marker so advance
// bookkeeping (e.g. storing the remote sync tip) can be done.
func openLocalRepoWithMarker(path string) (*localRepoWithMarker, error) {
	marker, err := store.LookupMarker(expandLocalPath(path))
	if err != nil {
		return nil, fmt.Errorf("open local remote: %w", err)
	}
	cs, err := store.OpenStoreForMarker(marker)
	if err != nil {
		return nil, err
	}
	repo, err := cs.OpenRepo(marker.Repo)
	if err != nil {
		return nil, err
	}
	return &localRepoWithMarker{repo: repo, marker: marker}, nil
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func currentRevisionIDs(repo *store.Repo) []string {
	ch, err := repo.ListRevisions()
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(ch))
	for _, c := range ch {
		ids = append(ids, c.ID)
	}
	return ids
}

// baseForRepo joins a remote base URL with the repo's namespace/name path used
// by the smart protocol endpoints (/repo/{ns}/{name}/...).
func baseForRepo(base string, repoRef store.RepoRef) string {
	base = trimTrailingSlash(base)
	return base + "/repo/" + repoRef.Namespace + "/" + repoRef.Name
}

// cmdFetch fetches remote branchs (all, or those named in args) and records
// them into the local remote_refs namespace (git's refs/remotes).
func cmdFetch(c *ctx) {
	repo, _, err := c.loadRepo()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fetch:", err)
		os.Exit(1)
	}
	if err := doFetch(repo, os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "fetch:", err)
		os.Exit(1)
	}
}

// doFetch performs the fetch against repo directly (no workspace dependency).
func doFetch(repo *store.Repo, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: fetch <remote> [ref...]")
	}
	rem, err := resolveRemote(repo, args[0])
	if err != nil {
		return err
	}
	only := args[1:]

	// Local remote: a marker-based workspace directory. Advertise by reading
	// the target repo's refs/revisions directly; record the chosen refs.
	if isLocalURL(rem.URL) {
		target, err := openLocalRepo(rem.URL)
		if err != nil {
			return err
		}
		refs, err := target.ListRefs()
		if err != nil {
			return err
		}
		return recordFetchedRefs(repo, rem.Name, refs, rem.URL, only)
	}

	full := baseForRepo(rem.URL, repo.RepoRef())

	var adv advertiseResp
	if err := postJSON(full+"/advertise", advertiseReq{Have: currentRevisionIDs(repo)}, &adv, rem.Token); err != nil {
		return err
	}
	return recordFetchedRefs(repo, rem.Name, adv.Refs, rem.URL, only)
}

// recordFetchedRefs records the chosen branch refs into the local remote_refs
// namespace and sets the remote default branch from the first branch if none is
// recorded yet. list may be []*store.Ref (remote) or a reference source.
func recordFetchedRefs(repo *store.Repo, remoteName string, list []*store.Ref, displayURL string, only []string) error {
	if len(only) > 0 {
		var sel []*store.Ref
		for _, r := range list {
			for _, o := range only {
				if r.Name == o {
					sel = append(sel, r)
				}
			}
		}
		list = sel
	}
	count := 0
	for _, r := range list {
		if r.Kind != store.RefBranch {
			continue
		}
		if err := repo.SetRemoteRef(remoteName, &store.RemoteRef{RemoteName: remoteName, Kind: store.RefBranch, Name: r.Name, Target: r.Target}); err != nil {
			return err
		}
		count++
	}
	if def, _ := repo.GetRemoteDefaultBranch(remoteName); def == "" && len(list) > 0 {
		if err := repo.SetRemoteDefaultBranch(remoteName, list[0].Name); err != nil {
			return err
		}
	}
	fmt.Printf("fetched %d ref(s) from %s\n", count, displayURL)
	return nil
}

func cmdPull(c *ctx) {
	repo, _, err := c.loadRepo()
	if err != nil {
		fmt.Fprintln(os.Stderr, "pull:", err)
		os.Exit(1)
	}
	args := os.Args[2:]
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: pull <remote> [branch]")
		os.Exit(1)
	}
	rem, err := resolveRemote(repo, args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "pull:", err)
		os.Exit(1)
	}
	// Determine the branch to merge: explicit arg, else the remote's default
	// branch, else the repo's default branch.
	want := ""
	if len(args) > 1 {
		want = args[1]
	} else if def, _ := repo.GetRemoteDefaultBranch(rem.Name); def != "" {
		want = def
	} else if meta, _ := repo.RepoMeta(); meta.DefaultBranch != "" {
		want = meta.DefaultBranch
	}
	if want == "" {
		fmt.Fprintln(os.Stderr, "pull: no branch specified and no default; pass a branch name")
		os.Exit(1)
	}

	if isLocalURL(rem.URL) {
		if err := doLocalPull(c, repo, rem, want, rem.URL); err != nil {
			fmt.Fprintln(os.Stderr, "pull:", err)
			os.Exit(1)
		}
		return
	}

	full := baseForRepo(rem.URL, repo.RepoRef())

	var adv advertiseResp
	if err := postJSON(full+"/advertise", advertiseReq{Have: currentRevisionIDs(repo)}, &adv, rem.Token); err != nil {
		fmt.Fprintln(os.Stderr, "pull:", err)
		os.Exit(1)
	}

	// Restrict the fetch to just that chain, and capture the remote tip.
	var fetchReq advertiseReq = advertiseReq{
		Have:        currentRevisionIDs(repo),
		HaveObjects: objectIDsAsStrings(ownObjects(repo)),
	}
	remoteTip := ""
	found := false
	for _, r := range adv.Refs {
		if r.Name == want && r.Kind == store.RefBranch {
			found = true
			fetchReq.WantChanges = []string{r.Target}
			remoteTip = r.Target
			break
		}
	}
	if !found {
		fmt.Fprintln(os.Stderr, "pull: branch not found on server:", want)
		os.Exit(1)
	}
	// Collaborative pull: fetch objects, then rebase the remote tip onto the
	// local branch tip so the two merge into one line.
	if err := collaborativePull(c, repo, rem, full, fetchReq, want, remoteTip); err != nil {
		fmt.Fprintln(os.Stderr, "pull:", err)
		os.Exit(1)
	}
}

// doPull performs pull directly against repo (no workspace dependency).
func doPull(repo *store.Repo, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: pull <remote> [branch]")
	}
	rem, err := resolveRemote(repo, args[0])
	if err != nil {
		return err
	}
	want := ""
	if len(args) > 1 {
		want = args[1]
	} else if def, _ := repo.GetRemoteDefaultBranch(rem.Name); def != "" {
		want = def
	} else if meta, _ := repo.RepoMeta(); meta.DefaultBranch != "" {
		want = meta.DefaultBranch
	}
	if want == "" {
		return fmt.Errorf("no branch specified and no default; pass a branch name")
	}

	if isLocalURL(rem.URL) {
		return doLocalPull(&ctx{}, repo, rem, want, rem.URL)
	}

	full := baseForRepo(rem.URL, repo.RepoRef())

	var adv advertiseResp
	if err := postJSON(full+"/advertise", advertiseReq{Have: currentRevisionIDs(repo)}, &adv, rem.Token); err != nil {
		return err
	}
	var fetchReq advertiseReq = advertiseReq{
		Have:        currentRevisionIDs(repo),
		HaveObjects: objectIDsAsStrings(ownObjects(repo)),
	}
	remoteTip := ""
	found := false
	for _, r := range adv.Refs {
		if r.Name == want && r.Kind == store.RefBranch {
			found = true
			fetchReq.WantChanges = []string{r.Target}
			remoteTip = r.Target
			break
		}
	}
	if !found {
		return fmt.Errorf("branch not found on server: %s", want)
	}
	return collaborativePull(&ctx{}, repo, rem, full, fetchReq, want, remoteTip)
}

// collaborativePull fetches a remote branch's objects and then rebases the
// remote tip revision onto the local branch tip, so the two edits merge into
// one linear change (conflicts become first-class objects). It re-points the
// local branch to the merged result and records the remote tip for the next
// incremental pull.
func collaborativePull(c *ctx, repo *store.Repo, rem *store.Remote, full string, fetchReq advertiseReq, branch, remoteTip string) error {
	// Ensure the local branch exists (create it if missing).
	ws := revision.NewWorkspace(repo)
	localRef, err := ws.GetRef(branch)
	if err != nil || localRef == nil {
		localRef = &store.Ref{Name: branch, Kind: store.RefBranch}
	}

	// Fetch the remote chain once; the bundle carries the objects needed to
	// rebase the remote tip locally.
	data, err := fetchRaw(full, fetchReq, rem.Token)
	if err != nil {
		return err
	}
	b, err := getBundle(data)
	if err != nil {
		return err
	}
	if _, err := transfer.Apply(repo, b); err != nil {
		return err
	}

	// Sanity-check the remote tip actually landed in this store before we
	// rebase onto it (Apply above should have written it).
	if _, err := repo.GetRevision(remoteTip); err != nil {
		return fmt.Errorf("remote tip %s missing after fetch: %w", short(remoteTip), err)
	}

	// Determine the local tip to rebase onto.
	localTip := localRef.Target
	if localTip == "" {
		// No local tip yet: place the remote revision directly as the local tip.
		if _, err := ws.SetRef(branch, store.RefBranch, remoteTip); err != nil {
			return err
		}
		if err := repo.UpdateRemoteSyncTip(rem.Name, remoteTip); err != nil {
			return err
		}
		fmt.Printf("pulled %s -> %s (no local tip; placed remote tip)\n", rem.URL, short(remoteTip))
		return nil
	}

	// The local tip's snapshot hash; the remote tip is rebased ONTO it so the
	// remote edit merges into the local line (conflicts -> first-class objects).
	localRev, err := repo.GetRevision(localTip)
	if err != nil {
		return err
	}
	localHash := localRev.Hash

	// Rebase the remote tip onto the local tip's snapshot (3-way).
	if _, _, err := ws.Rebase(remoteTip, []object.ID{localHash}); err != nil {
		return err
	}

	// Re-point the local branch to the merged (rebased) remote revision id.
	mergedRev, err := repo.GetRevision(remoteTip)
	if err != nil {
		return err
	}
	if _, err := ws.SetRef(branch, store.RefBranch, mergedRev.ID); err != nil {
		return err
	}
	if err := repo.UpdateRemoteSyncTip(rem.Name, mergedRev.ID); err != nil {
		return err
	}
	snap, _ := repo.GetSnapshot(mergedRev.Hash)
	if snap != nil {
		fmt.Printf("pulled %s -> %s merged onto %s (revision %s)\n", rem.URL, short(remoteTip), short(localTip), short(mergedRev.ID))
	}
	return nil
}

// fetchRaw performs a fetch request and returns raw bytes (binary bundle).
func fetchRaw(full string, req advertiseReq, token string) ([]byte, error) {
	payload, _ := json.Marshal(req)
	return doPost(full+"/fetch", payload, token)
}

// doLocalPull performs a collaborative pull against a local (marker-based)
// workspace directory used as a remote. It mirrors the HTTP branch: read the
// target refs, apply the remote tip, then rebase the remote tip onto the local
// branch tip so the two edits merge into one linear line (conflicts become
// first-class objects). No network transport is used.
func doLocalPull(c *ctx, repo *store.Repo, rem *store.Remote, branch, url string) error {
	ws := revision.NewWorkspace(repo)
	localRef, err := ws.GetRef(branch)
	if err != nil || localRef == nil {
		localRef = &store.Ref{Name: branch, Kind: store.RefBranch}
	}

	target, err := openLocalRepoWithMarker(url)
	if err != nil {
		return err
	}
	remoteRef, err := target.repo.GetRef(branch)
	if err != nil || remoteRef == nil || remoteRef.Kind != store.RefBranch {
		return fmt.Errorf("branch not found on local remote: %s", branch)
	}
	remoteTip := remoteRef.Target

	// Bring the remote tip (and its objects) into the local repo.
	b, err := transfer.Collect(target.repo, nil, nil)
	if err != nil {
		return err
	}
	if _, err := transfer.Apply(repo, b); err != nil {
		return err
	}

	// No local tip yet: place the remote revision directly as the local tip.
	localTip := localRef.Target
	if localTip == "" {
		if _, err := ws.SetRef(branch, store.RefBranch, remoteTip); err != nil {
			return err
		}
		if err := repo.UpdateRemoteSyncTip(rem.Name, remoteTip); err != nil {
			return err
		}
		fmt.Printf("pulled %s -> %s (no local tip; placed remote tip)\n", url, short(remoteTip))
		return nil
	}

	// The local tip's snapshot hash; the remote tip is rebased ONTO it so the
	// remote edit merges into the local line (conflicts -> first-class objects).
	localRev, err := repo.GetRevision(localTip)
	if err != nil {
		return err
	}
	if _, _, err := ws.Rebase(remoteTip, []object.ID{localRev.Hash}); err != nil {
		return err
	}
	mergedRev, err := repo.GetRevision(remoteTip)
	if err != nil {
		return err
	}
	if _, err := ws.SetRef(branch, store.RefBranch, mergedRev.ID); err != nil {
		return err
	}
	if err := repo.UpdateRemoteSyncTip(rem.Name, mergedRev.ID); err != nil {
		return err
	}
	fmt.Printf("pulled %s -> %s merged onto %s (revision %s)\n", url, short(remoteTip), short(localTip), short(mergedRev.ID))
	return nil
}

func cmdPush(c *ctx) {
	repo, _, err := c.loadRepo()
	if err != nil {
		fmt.Fprintln(os.Stderr, "push:", err)
		os.Exit(1)
	}
	args := os.Args[2:]
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: push <remote>")
		os.Exit(1)
	}
	rem, err := resolveRemote(repo, args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "push:", err)
		os.Exit(1)
	}
	full := baseForRepo(rem.URL, repo.RepoRef())

	if isLocalURL(rem.URL) {
		if err := doPushLocal(repo, rem); err != nil {
			fmt.Fprintln(os.Stderr, "push:", err)
			os.Exit(1)
		}
		return
	}

	// Incremental push: advertise what the server currently holds (refs +
	// revisions + objects), then send only a delta the server is missing.
	var adv advertiseResp
	if err := postJSON(full+"/advertise", advertiseReq{Have: currentRevisionIDs(repo)}, &adv, rem.Token); err != nil {
		// Advertise failure is fatal: falling back to a "full" push built from
		// local state would skip revisions the server lacks and leave dangling
		// refs. The server is unreachable or rejecting us; surface that.
		fmt.Fprintln(os.Stderr, "push: advertise failed:", err)
		os.Exit(1)
	}
	// Incremental: skip revisions/snapshots the server already advertises. An
	// empty advertise means the server holds nothing yet: send the full bundle
	// (have=nil) so every revision lands before the refs point at them.
	have := adv.Changes
	b, err := transfer.Collect(repo, have, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "push:", err)
		os.Exit(1)
	}

	data, err := postBundle(full+"/push", b, rem.Token)
	if err != nil {
		var nff *transfer.NonFastForwardError
		if errors.As(err, &nff) {
			fmt.Fprintf(os.Stderr, "push: %v\n(pull the branch and merge before pushing again)\n", nff)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "push:", err)
		os.Exit(1)
	}
	var resp map[string]any
	if err := json.Unmarshal(data, &resp); err != nil {
		fmt.Fprintln(os.Stderr, "push:", err)
		os.Exit(1)
	}
	fmt.Printf("pushed to %s: %v\n", rem.URL, resp)
}

// doPushLocal pushes the full bundle into a local remote, applying it via
// transfer.Apply (idempotent) and checking non-fast-forward locally first.
func doPushLocal(repo *store.Repo, rem *store.Remote) error {
	b, err := transfer.CollectAll(repo)
	if err != nil {
		return err
	}
	targetRepo, err := openLocalRepo(rem.URL)
	if err != nil {
		return err
	}
	// Server-side refs (authoritative) for a local remote.
	serverRefs, err := targetRepo.ListRefs()
	if err != nil {
		return err
	}
	incoming := b.Refs
	conflicts := transfer.CheckNonFastForward(targetRepo, serverRefs, incoming, b)
	if len(conflicts) > 0 {
		return &transfer.NonFastForwardError{ConflictingRefs: conflicts, LocalRefs: incoming}
	}
	if _, err := transfer.Apply(targetRepo, b); err != nil {
		return err
	}
	fmt.Printf("pushed to %s: applied %d revision(s)\n", rem.URL, len(b.Revisions))
	return nil
}

// localRemoteRefs returns the branch refs the local repo believes the remote
// holds, from the recorded remote_refs for `remoteName`. It is the client-side
// expectation used to let the server reject a non-fast-forward push.
func localRemoteRefs(repo *store.Repo, remoteName string) ([]*store.Ref, error) {
	rrefs, err := repo.ListRemoteRefs(remoteName)
	if err != nil {
		return nil, err
	}
	out := make([]*store.Ref, 0, len(rrefs))
	for _, rr := range rrefs {
		if rr.Kind != store.RefBranch {
			continue
		}
		out = append(out, &store.Ref{Name: rr.Name, Kind: store.RefBranch, Target: rr.Target})
	}
	return out, nil
}

// ownObjects returns all object ids the repo already holds.
func ownObjects(repo *store.Repo) []object.ID {
	ids, _ := transfer.EnumerateObjectIDs(repo)
	return ids
}

// objectIDsAsStrings converts object ids to hex strings.
func objectIDsAsStrings(ids []object.ID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	return out
}

// --- git interop (no smart protocol, full transfer + minimal diff commit) ---

func gitArg() string {
	args := os.Args[2:]
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func copyDirTree(src, dst string, skipGit bool) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if skipGit && (e.Name() == ".git" || e.Name() == ".easyvcs") {
			continue
		}
		sPath := filepath.Join(src, e.Name())
		dPath := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := os.MkdirAll(dPath, 0o755); err != nil {
				return err
			}
			if err := copyDirTree(sPath, dPath, skipGit); err != nil {
				return err
			}
			continue
		}
		if err := copyFile(sPath, dPath); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}
