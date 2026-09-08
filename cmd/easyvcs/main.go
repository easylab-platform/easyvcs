// Command easyvcs is the EasyVCS command-line interface.
//
// EasyVCS uses a central SQLite database (default ~/.easyvcs/easyvcs.db,
// overridable via EASYVCS_HOME) holding all repositories. A working directory
// is bound to a repository via a .easyvcs-workspace pointer file. Commands are
// minimal and mirror the revision-native model: committing creates a snapshot
// under a stable revision id; rebase repoints a revision's parents changing only
// the snapshot's sha.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/easylab-platform/easyvcs/ignore"
	"github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/revision"
	"github.com/easylab-platform/easyvcs/store"
)

// HasIgnores reports whether the current directory has any ignore files.
func HasIgnores() bool {
	_, err := os.Stat(".gitignore")
	if err == nil {
		return true
	}
	_, err = os.Stat(".vcsignore")
	return err == nil
}

const usage = `easyvcs - a revision-native version control system (central-store)

Repository / workspace:
  init <ns/name> [DIR]      create a repository and a workspace pointing at it
  clone <url> [DIR]         clone a remote (http(s):// or a local path) into a new repo + workspace
  workspace attach [DIR]    bind current dir to a repo (auto-forks on mismatch)
  workspace list            list registered workspaces
  repositories              list all repositories in the central store

Revisions:
  commit [--new] [DIR]      create a snapshot (default: amend current revision)
  amend [DIR]               update the workspace's current revision snapshot
  log [DIR]                 list revisions and their current snapshot
  show <sha> [DIR]          show a snapshot's metadata
  checkout <sha> [DIR]      set current_revision + materialize tree into dir
  rebase <revision> [--onto <parent>]...  repoint a revision onto one or more parents
  squash <revision> [DIR]     absorb a revision into its parent (parent id stable)
  resolve <revision> <path> [--side N]  resolve a conflict to a chosen side
  message <revision> <text>    rewrite a revision's commit message (id stable)

Maintenance:
  gc [--dry-run]            prune unreferenced objects (whole store)
  verify                   check object/snapshot consistency

References:
  branch <name> <revision> [DIR]  derive a mutable branch -> independent revision
  tag <name> <revision> [DIR]       set an immutable tag -> revision
  refs [DIR]                list all branchs and tags

  log [--count] <path>       list revisions that modified a file (or count)

Content:
  diff <shaA> <shaB> [DIR]  list file differences between two snapshot trees

Portable:
  export <ns/name> -o FILE  export a repository to a single .evcs file
  import FILE [-n ns]       import a repository from a .evcs file

Network (own smart protocol):
  remote add <name> <url>   register a remote server URL
  remote                    list remotes
  fetch <remote> [ref...]   fetch all (or given) remote branchs into origin/*
  pull <remote> [branch]  fetch + merge a remote branch into local (default branch)
  push <remote>             push changes to an EasyVCS server

Git interop (no smart protocol, full + minimal-diff commit):
  git-pull <git-url>        fetch a git repo as one local revision
  git-push <git-url>        push current revision as one git commit

Flags:
  -help, -h                 show this help
`

type ctx struct {
	cs *store.CentralStore
	// onErr, when set, replaces the process-exit behavior of command failures
	// (tests install a panic-based hook so failures are observable).
	onErr func()
}

// fatal reports a command failure and terminates the command. In production it
// exits the process with status 1; tests may override via onErr.
func (c *ctx) fatal(args ...any) {
	fmt.Fprintln(os.Stderr, args...)
	if c.onErr != nil {
		c.onErr()
		return
	}
	os.Exit(1)
}

// fatalf is fatal with formatting.
func (c *ctx) fatalf(format string, args ...any) {
	c.fatal(fmt.Sprintf(format, args...))
}

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(0)
	}
	cmd := os.Args[1]
	switch cmd {
	case "-h", "-help", "--help", "help":
		fmt.Print(usage)
		os.Exit(0)
	}

	cs, err := store.OpenDefault()
	if err != nil {
		fmt.Fprintln(os.Stderr, "open store:", err)
		os.Exit(1)
	}
	if err := cs.SetWAL(); err != nil {
		fmt.Fprintln(os.Stderr, "enable WAL:", err)
		os.Exit(1)
	}
	c := &ctx{cs: cs}

	switch cmd {
	case "init":
		cmdInit(c)
	case "clone":
		cmdClone(c)
	case "workspace":
		cmdWorkspace(c)
	case "repositories":
		cmdRepositories(c)
	case "commit":
		cmdCommit(c)
	case "amend":
		cmdAmend(c)
	case "log":
		// log [--count] <path>: report revisions that modified a file.
		if len(os.Args) >= 3 {
			count := false
			pathArg := ""
			for _, a := range os.Args[2:] {
				if a == "--count" || a == "-count" || a == "-n" {
					count = true
				} else if pathArg == "" {
					pathArg = a
				}
			}
			if pathArg != "" {
				cmdLogPath(c, pathArg, count)
				return
			}
		}
		cmdLog(c)
	case "show":
		cmdShow(c)
	case "checkout":
		cmdCheckout(c)
	case "rebase":
		cmdRebase(c)
	case "squash":
		cmdSquash(c)
	case "resolve":
		cmdResolve(c)
	case "message":
		cmdMessage(c)
	case "branch":
		cmdBranch(c)
	case "tag":
		cmdTag(c)
	case "refs":
		cmdRefs(c)
	case "gc":
		cmdGC(c)
	case "verify":
		cmdVerify(c)
	case "diff":
		cmdDiff(c)
	case "export":
		cmdExport(c)
	case "import":
		cmdImport(c)
	case "remote":
		cmdRemote(c)
	case "fetch":
		cmdFetch(c)
	case "pull":
		cmdPull(c)
	case "push":
		cmdPush(c)
	case "git-pull":
		cmdGitPull(c)
	case "git-push":
		cmdGitPush(c)
	default:
		c.fatalf("unknown command %q\n\n%s", cmd, usage)
	}
}

// resolveWorkingDir returns the directory a command should operate on.
func resolveWorkingDir() string {
	return "."
}

// loadRepo resolves the workspace for the current directory and returns the
// repo-scoped handle plus the workspace marker. The store the workspace belongs
// to is opened from the marker's recorded location (Home/DSN), falling back to
// the process EASYVCS_HOME when the marker does not record one.
func (c *ctx) loadRepo() (*store.Repo, *store.WorkspaceMarker, error) {
	marker, err := store.LookupMarker(".")
	if err != nil {
		return nil, nil, err
	}
	cs, err := store.OpenStoreForMarker(marker)
	if err != nil {
		return nil, nil, err
	}
	c.cs = cs
	repo, err := c.cs.OpenRepo(marker.Repo)
	if err != nil {
		return nil, nil, err
	}
	return repo, marker, nil
}

func cmdInit(c *ctx) {
	args := os.Args[2:]
	if len(args) == 0 {
		c.fatal("usage: init <ns/name> [DIR]")
	}
	ns, name, err := splitRepo(args[0])
	if err != nil {
		c.fatal("init:", err)
	}
	dir := "."
	if len(args) > 1 {
		dir = args[1]
	}
	repo, err := c.cs.Create(store.RepoRef{Namespace: ns, Name: name})
	if err != nil {
		c.fatal("init:", err)
	}
	if err := store.WriteMarker(dir, &store.WorkspaceMarker{Repo: repo.RepoRef()}); err != nil {
		c.fatal("init:", err)
	}
	fmt.Printf("initialized repository %s/%s, workspace bound at %s\n", ns, name, dir)
}

// cmdClone clones a repository from a remote URL (http(s):// or a local path)
// into a fresh repository + workspace in the current directory. It registers the
// remote as "origin", performs the first fetch to record remote refs, and pulls
// the remote default branch (or an explicit source ref) into the local branch.
// For a local remote the source repo is identified by the target workspace
// marker's Home + Repo; for an http(s) URL it talks to the EasyVCS server.
func cmdClone(c *ctx) {
	args := os.Args[2:]
	url := ""
	var branch string
	var dir string
	var namespace string
	var repoName string
	destDir := "."
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-b", "--branch":
			if i+1 < len(args) {
				branch = args[i+1]
				i++
			}
		case "-d", "--dir":
			if i+1 < len(args) {
				destDir = args[i+1]
				i++
			}
		case "-n", "--namespace":
			if i+1 < len(args) {
				namespace = args[i+1]
				i++
			}
		default:
			if url == "" {
				url = args[i]
			} else if dir == "" {
				dir = args[i]
			}
		}
	}
	if url == "" {
		c.fatal("usage: clone <url> [DIR] [-b branch] [-n namespace]")
	}
	if dir != "" {
		destDir = dir
	}
	if destDir == "." {
		// Default destination directory = last path segment of the URL.
		base := filepath.Base(strings.TrimRight(expandLocalPath(url), "/"))
		if base != "" && base != "/" && base != "." {
			destDir = base
		}
	}

	// Determine the source repo ref: for a local path use the target marker's
	// repo; for an http(s) URL derive it from the path.
	srcRef := store.RepoRef{}
	if isLocalURL(url) {
		m, err := store.LookupMarker(expandLocalPath(url))
		if err != nil {
			c.fatal("clone:", err)
		}
		srcRef = m.Repo
	} else {
		ns, name := repoRefFromURLPath(url)
		if ns == "" {
			ns = "default"
		}
		srcRef = store.RepoRef{Namespace: ns, Name: name}
	}
	if namespace != "" {
		srcRef.Namespace = namespace
	}
	if repoName != "" {
		srcRef.Name = repoName
	}

	// Create the destination repo (fresh central store for the clone).
	home := store.HomeDir()
	cs, err := store.OpenDefault()
	if err != nil {
		c.fatal("clone:", err)
	}
	dstRepo, err := cs.Create(srcRef)
	if err != nil {
		c.fatal("clone:", err)
	}
	if err := dstRepo.PutRemote("origin", url, ""); err != nil {
		c.fatal("clone:", err)
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		c.fatal("clone:", err)
	}
	if err := store.WriteMarker(destDir, &store.WorkspaceMarker{Repo: dstRepo.RepoRef(), Home: home}); err != nil {
		c.fatal("clone:", err)
	}
	if err := cs.PutWorkspace(&store.WorkspaceRow{Path: destDir, Repo: dstRepo.RepoRef(), Branch: branch}); err != nil {
		c.fatal("clone:", err)
	}

	// Fetch/pull the remote default branch (or explicit branch).
	var pullErr error
	if isLocalURL(url) {
		if err := doFetch(dstRepo, []string{"origin"}); err != nil {
			fmt.Fprintln(os.Stderr, "clone (fetch):", err)
		}
		want := branch
		if want == "" {
			if def, _ := dstRepo.GetRemoteDefaultBranch("origin"); def != "" {
				want = def
			} else if meta, _ := dstRepo.RepoMeta(); meta.DefaultBranch != "" {
				want = meta.DefaultBranch
			}
		}
		if want == "" {
			want = "main"
		}
		if err := doPull(dstRepo, []string{"origin", want}); err != nil {
			pullErr = err
		}
	} else {
		want := branch
		if want == "" {
			want = "main"
		}
		pullErr = doPull(dstRepo, []string{"origin", want})
	}
	if pullErr != nil {
		c.fatal("clone (pull):", pullErr)
	}
	fmt.Printf("cloned %s -> %s\n", url, destDir)
}

// repoRefFromURLPath derives a RepoRef{Namespace,Name} from a url path of the
// form .../namespace/name or .../name. It is used to name a clone of a (network)
// remote when no explicit namespace/name is given.
func repoRefFromURLPath(url string) (string, string) {
	p := url
	if i := strings.Index(p, "://"); i >= 0 {
		p = p[i+3:]
		// Drop the host (up to the first '/'), leaving the path segments.
		if j := strings.Index(p, "/"); j >= 0 {
			p = p[j+1:]
		} else {
			p = ""
		}
	}
	p = strings.Trim(p, "/")
	parts := strings.Split(p, "/")
	for len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	if len(parts) >= 2 {
		return parts[len(parts)-2], parts[len(parts)-1]
	}
	if len(parts) == 1 {
		return "default", parts[0]
	}
	return "default", ""
}

func cmdWorkspace(c *ctx) {
	args := os.Args[2:]
	if len(args) == 0 {
		c.fatal("usage: workspace attach|open|list")
	}
	switch args[0] {
	case "attach", "open":
		cmdWorkspaceAttach(c)
	case "list":
		cmdWorkspaceList(c)
	default:
		c.fatalf("unknown workspace subcommand %q\n", args[0])
	}
}

// cmdWorkspaceAttach binds the current directory to a repository. It reads (or
// creates) a workspace marker, resolves the repo, and if the recorded current
// revision no longer exists it auto-forks a new tip revision (plan A). It only
// runs on explicit attach, never implicitly during other commands.
func cmdWorkspaceAttach(c *ctx) {
	marker, err := store.LookupMarker(".")
	if err != nil {
		// No marker yet: create from a repo arg if provided.
		args := os.Args[2:]
		if len(args) < 2 {
			c.fatal("usage: workspace attach <ns/name> [DIR]")
		}
		ns, name, err := splitRepo(args[1])
		if err != nil {
			c.fatal("workspace attach:", err)
		}
		repo, err := c.cs.OpenRepo(store.RepoRef{Namespace: ns, Name: name})
		if err != nil {
			c.fatal("workspace attach:", err)
		}
		marker = &store.WorkspaceMarker{Repo: repo.RepoRef()}
		if err := store.WriteMarker(".", marker); err != nil {
			c.fatal("workspace attach:", err)
		}
		fmt.Printf("workspace attached to %s\n", repo)
		return
	}
	cs, err := store.OpenStoreForMarker(marker)
	if err != nil {
		c.fatal("workspace attach:", err)
	}
	repo, err := cs.OpenRepo(marker.Repo)
	if err != nil {
		c.fatal("workspace attach: repository not found:", marker.Repo, "(import it first)")
	}
	// If current_revision is set but missing, auto-fork a new tip.
	if marker.CurrentRevision != "" {
		_, err := repo.GetRevision(marker.CurrentRevision)
		if err != nil {
			fmt.Printf("detected current revision %s missing; forking new tip from workspace\n", short(marker.CurrentRevision))
			ws := revision.NewWorkspace(repo)
			treeID, err := ws.BuildTreeFromFS(".")
			if err != nil {
				c.fatal("workspace attach:", err)
			}
			// Filter the new tree with the ignore matcher before committing.
			m, err := ignore.New(".")
			if err != nil {
				c.fatal("workspace attach: load ignore rules:", err)
			}
			if HasIgnores() {
				treeID, err = ws.RebuildTreeFiltered(".", m, treeID)
				if err != nil {
					c.fatal("workspace attach:", err)
				}
			}
			// New revision on top of the repo tip (first branch or root).
			parents := repoTip(repo)
			_, ch, err := ws.Commit(revision.CommitParams{
				Parents:     parents,
				TreeID:      treeID,
				Description: "fork",
				Author:      store.Author{Name: "easyvcs", Email: "easyvcs@example.com"},
			})
			if err != nil {
				c.fatal("workspace attach:", err)
			}
			marker.CurrentRevision = ch.ID
			if err := store.WriteMarker(".", marker); err != nil {
				c.fatal("workspace attach:", err)
			}
			fmt.Printf("forked new revision %s\n", short(ch.ID))
		}
	}
	if err := c.cs.PutWorkspace(&store.WorkspaceRow{
		Path: ".", Repo: marker.Repo, CurrentRevision: marker.CurrentRevision, Branch: marker.Branch,
	}); err != nil {
		c.fatal("workspace attach:", err)
	}
	fmt.Printf("workspace bound to %s (current revision %s)\n", repo, short(marker.CurrentRevision))
}

func cmdWorkspaceList(c *ctx) {
	ws, err := c.cs.ListWorkspaces()
	if err != nil {
		c.fatal("workspace list:", err)
	}
	if len(ws) == 0 {
		fmt.Println("no workspaces")
		return
	}
	for _, w := range ws {
		fmt.Printf("%s  ->  %s  (%s)\n", w.Path, w.Repo, short(w.CurrentRevision))
	}
}

func cmdRepositories(c *ctx) {
	repos, err := c.cs.List()
	if err != nil {
		c.fatal("repositories:", err)
	}
	if len(repos) == 0 {
		fmt.Println("no repositories")
		return
	}
	for _, r := range repos {
		fmt.Println(r)
	}
}

func cmdCommit(c *ctx) {
	repo, marker, err := c.loadRepo()
	if err != nil {
		c.fatal("commit:", err)
	}
	ws := revision.NewWorkspace(repo)
	dir := resolveWorkingDir()

	// --new creates a fresh revision; otherwise commit AMENDS onto the current
	// revision (the workspace's existing revision), keeping revision_id stable.
	// This matches the jj revision model: edit repeatedly, "new" to split.
	isNew := false
	for _, a := range os.Args[2:] {
		if a == "--new" || a == "-new" {
			isNew = true
		}
	}

	// Resolve the parent snapshot hash (current revision). Empty for a root
	// commit.
	var parentHash object.ID
	if marker.CurrentRevision != "" {
		cur, err := ws.GetRevision(marker.CurrentRevision)
		if err == nil {
			parentHash = cur.Hash
		}
	}

	// Read-only scan of the working directory: compute the diff against the
	// parent tree WITHOUT persisting the whole tree. Only the changed paths are
	// produced as file changes.
	changes, err := ws.ComputeChangesFromDir(parentHash, dir)
	if err != nil {
		c.fatal("commit:", err)
	}

	// Atomic commit from the computed changes (same code path as the server
	// /files API). Default amends onto the current revision (revision_id stable);
	// --new creates a fresh revision chain from the current tip.
	revisionID := ""
	if !isNew && marker.CurrentRevision != "" {
		revisionID = marker.CurrentRevision
	}
	snap, ch, err := ws.CommitFromChanges(
		parentHash, changes, "commit",
		store.Author{Name: "easyvcs", Email: "easyvcs@example.com"}, revisionID,
	)
	if err != nil {
		c.fatal("commit:", err)
	}
	if !isNew && marker.CurrentRevision != "" {
		fmt.Printf("amended revision %s -> snapshot %s\n", short(ch.ID), short(snap.RevisionHash.String()))
	} else {
		fmt.Printf("revision %s -> snapshot %s\n", short(ch.ID), short(snap.RevisionHash.String()))
	}
	// Detect a change in ignore files vs the previous snapshot. If the matcher
	// now ignores paths that were previously tracked in any ancestor, rewrite the
	// history to exclude them and auto-rebase descendants.
	if HasIgnores() {
		if err := applyIgnoreRewrite(c, repo, ws, ch.ID, marker); err != nil {
			c.fatal("commit (ignore rewrite):", err)
		}
	}
	// Update the workspace pointer to the current revision.
	marker.CurrentRevision = ch.ID
	if err := store.WriteMarker(".", marker); err != nil {
		c.fatal("commit:", err)
	}
	if err := c.cs.PutWorkspace(&store.WorkspaceRow{Path: ".", Repo: marker.Repo, CurrentRevision: ch.ID, Branch: marker.Branch}); err != nil {
		c.fatal("commit: record workspace:", err)
	}
}

// applyIgnoreRewrite checks whether ignore rules changed; if so it removes
// now-ignored paths from all ancestor snapshots and rebases descendants to the
// rewritten parents. It uses the workspace's current snapshot (just committed)
// as the tip whose ancestry is rewritten.
func applyIgnoreRewrite(c *ctx, repo *store.Repo, ws *revision.Workspace, revisionID string, marker *store.WorkspaceMarker) error {
	// Compute the matcher and a hash of ignore files at the working dir.
	m, err := ignore.New(".")
	if err != nil {
		return err
	}
	curHash, err := revision.IgnoreHash(".")
	if err != nil {
		return err
	}
	// Find the previously recorded ignore hash, if any (stored in the workspaces
	// row / marker). If none or changed, run the rewrite.
	prevHash := marker.IgnoreHash
	if prevHash == curHash {
		return nil // no ignore rules changed
	}
	// (A missing baseline is handled below: the rewrite pass targets the
	// current commit's ancestors, which is a no-op for a first commit.)

	rewrote, err := ws.RewriteHistoryWithIgnores(".", m, revisionID)
	if err != nil {
		return err
	}
	if rewrote > 0 {
		fmt.Printf("ignore rules changed; excluded now-ignored paths from %d snapshot(s) and rebased descendants\n", rewrote)
	}
	// Persist the new ignore hash in the workspace marker.
	marker.IgnoreHash = curHash
	return nil
}

func cmdAmend(c *ctx) {
	repo, marker, err := c.loadRepo()
	if err != nil {
		c.fatal("amend:", err)
	}
	ws := revision.NewWorkspace(repo)
	dir := resolveWorkingDir()
	revisionID := marker.CurrentRevision
	cur, err := repo.GetRevision(revisionID)
	if err != nil {
		c.fatal("amend: no current revision:", err)
	}
	curSnap, err := repo.GetSnapshot(cur.Hash)
	if err != nil {
		c.fatal("amend:", err)
	}
	// Compute the diff against the current snapshot's tree using a read-only
	// scan (no full-tree persistence). The amend rewrites the current revision.
	changes, err := ws.ComputeChangesFromDir(cur.Hash, dir)
	if err != nil {
		c.fatal("amend:", err)
	}
	ns, ch, err := ws.CommitFromChanges(
		cur.Hash, changes,
		curSnap.Description, curSnap.Author, revisionID,
	)
	if err != nil {
		c.fatal("amend:", err)
	}
	// Amend keeps the same revision_id; repoint it to the new hash.
	if err := repo.UpdateRevisionHash(ch.ID, ns.RevisionHash); err != nil {
		c.fatal("amend:", err)
	}
	fmt.Printf("amended revision %s -> snapshot %s\n", short(ch.ID), short(ns.RevisionHash.String()))
}

func cmdLog(c *ctx) {
	repo, _, err := c.loadRepo()
	if err != nil {
		c.fatal("log:", err)
	}
	ws := revision.NewWorkspace(repo)
	revs, err := ws.Log()
	if err != nil {
		c.fatal("log:", err)
	}
	for _, ch := range revs {
		snap, err := ws.GetSnapshot(ch.Hash)
		if err != nil {
			fmt.Fprintf(os.Stderr, "log: %v\n", err)
			continue
		}
		fmt.Printf("%s  %s\n", short(ch.ID), short(snap.RevisionHash.String()))
		fmt.Printf("    %s\n", snap.Description)
	}
}

// cmdLogPath reports the revisions that modified a given path, along with the
// count. It walks the DAG from the current revision upward and filters using
// each revision's ChangedPaths (cheap), then compares blob hashes to confirm
// an actual content change.
func cmdLogPath(c *ctx, path string, countOnly bool) {
	repo, _, err := c.loadRepo()
	if err != nil {
		c.fatal("log:", err)
	}
	ws := revision.NewWorkspace(repo)
	edits, err := ws.FileHistory("", path)
	if err != nil {
		c.fatal("log:", err)
	}
	if countOnly {
		fmt.Println(len(edits))
		return
	}
	for _, e := range edits {
		fmt.Printf("%s  %s\n", short(e.RevisionID), short(e.RevisionHash.String()))
		fmt.Printf("    %s\n", e.Timestamp.Format("2006-01-02 15:04:05"))
	}
}

func cmdShow(c *ctx) {
	repo, _, err := c.loadRepo()
	if err != nil {
		c.fatal("show:", err)
	}
	if len(os.Args) < 3 {
		c.fatal("usage: show <sha>")
	}
	id, err := object.HexToID(os.Args[2])
	if err != nil {
		c.fatal("show:", err)
	}
	snap, err := repo.GetSnapshot(id)
	if err != nil {
		c.fatal("show:", err)
	}
	fmt.Printf("snapshot  %s\n", snap.RevisionHash)
	fmt.Printf("revision  %s\n", snap.RevisionID)
	fmt.Printf("tree      %s\n", snap.TreeID)
	for _, p := range snap.Parents {
		fmt.Printf("parent    %s\n", p)
	}
	fmt.Printf("author    %s\n", snap.Author)
	fmt.Printf("description %s\n", snap.Description)
}

func cmdCheckout(c *ctx) {
	repo, marker, err := c.loadRepo()
	if err != nil {
		c.fatal("checkout:", err)
	}
	if len(os.Args) < 3 {
		c.fatal("usage: checkout <sha> [DEST]")
	}
	id, err := object.HexToID(os.Args[2])
	if err != nil {
		c.fatal("checkout:", err)
	}
	snap, err := repo.GetSnapshot(id)
	if err != nil {
		c.fatal("checkout: unknown snapshot:", err)
	}
	ws := revision.NewWorkspace(repo)
	dest := ""
	if len(os.Args) > 3 {
		dest = os.Args[3]
		if !filepath.IsAbs(dest) {
			wd, _ := os.Getwd()
			dest = filepath.Join(wd, dest)
		}
	} else {
		dest = "."
	}
	if err := ws.Materialize(snap.TreeID, dest); err != nil {
		c.fatal("checkout:", err)
	}
	// Checkout repoints the workspace's current_change only (does not touch db).
	marker.CurrentRevision = snap.RevisionID
	if err := store.WriteMarker(".", marker); err != nil {
		c.fatal("checkout:", err)
	}
	if err := c.cs.PutWorkspace(&store.WorkspaceRow{Path: ".", Repo: marker.Repo, CurrentRevision: snap.RevisionID, Branch: marker.Branch}); err != nil {
		c.fatal("checkout:", err)
	}
	fmt.Printf("checked out snapshot %s (revision %s)\n", short(id.String()), short(snap.RevisionID))
}

func cmdRebase(c *ctx) {
	repo, _, err := c.loadRepo()
	if err != nil {
		c.fatal("rebase:", err)
	}
	args := os.Args[2:]
	var positional []string
	var ontoIDs []object.ID
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--onto", "-onto":
			if i+1 < len(args) {
				id, err := object.HexToID(args[i+1])
				if err != nil {
					c.fatal("rebase:", err)
				}
				ontoIDs = append(ontoIDs, id)
				i++
			}
		default:
			positional = append(positional, args[i])
		}
	}
	if len(positional) < 1 || len(ontoIDs) == 0 {
		c.fatal("usage: rebase <revision> [--onto <parent-sha>]...")
	}
	ws := revision.NewWorkspace(repo)
	ns, ch, err := ws.Rebase(positional[0], ontoIDs)
	if err != nil {
		c.fatal("rebase:", err)
	}
	fmt.Printf("rebased revision %s (id unchanged) -> snapshot %s\n", short(ch.ID), short(ns.RevisionHash.String()))
	// Report conflicts embedded in the new tree, if any.
	atoms, err := ws.ConflictsInTree(ns.TreeID)
	if err == nil && len(atoms) > 0 {
		for _, a := range atoms {
			fmt.Fprintf(os.Stderr, "conflict at %s (resolve <revision> %s)\n", a.Path, a.Path)
		}
	}
}

func cmdResolve(c *ctx) {
	repo, _, err := c.loadRepo()
	if err != nil {
		c.fatal("resolve:", err)
	}
	if len(os.Args) < 4 {
		c.fatal("usage: resolve <revision> <path> [--side N]")
	}
	revisionID := os.Args[2]
	path := os.Args[3]
	side := 0
	for i := 4; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--side", "-side":
			if i+1 < len(os.Args) {
				if _, serr := fmt.Sscanf(os.Args[i+1], "%d", &side); serr != nil {
					c.fatalf("invalid --side %q (expected a number)", os.Args[i+1])
				}
				i++
			}
		}
	}
	ws := revision.NewWorkspace(repo)
	ns, ch, err := ws.Resolve(revisionID, path, side)
	if err != nil {
		c.fatal("resolve:", err)
	}
	fmt.Printf("resolved conflict %s in revision %s -> snapshot %s\n", path, short(ch.ID), short(ns.RevisionHash.String()))
}

func cmdSquash(c *ctx) {
	repo, _, err := c.loadRepo()
	if err != nil {
		c.fatal("squash:", err)
	}
	if len(os.Args) < 3 {
		c.fatal("usage: squash <revision>")
	}
	ws := revision.NewWorkspace(repo)
	parentSnap, parentCh, err := ws.Squash(os.Args[2])
	if err != nil {
		c.fatal("squash:", err)
	}
	fmt.Printf("squashed into revision %s -> snapshot %s\n", short(parentCh.ID), short(parentSnap.RevisionHash.String()))
}

func cmdBranch(c *ctx) {
	repo, _, err := c.loadRepo()
	if err != nil {
		c.fatal("branch:", err)
	}
	args := os.Args[2:]
	if len(args) < 2 {
		c.fatal("usage: branch <name> <revision> [--message <text>] [--auto-commit]")
	}
	name := args[0]
	revisionArg := args[1]
	var message string
	autoCommit := false
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--message", "-m", "--message=":
			if strings.HasPrefix(args[i], "--message=") {
				message = strings.TrimPrefix(args[i], "--message=")
			} else if i+1 < len(args) {
				message = args[i+1]
				i++
			}
		case "--auto-commit", "-auto-commit":
			autoCommit = true
		}
	}
	// Resolve the revision argument to a revision id (supports @ / id prefix /
	// existing branch/tag name).
	ws := revision.NewWorkspace(repo)
	revisionID, err := resolveRevisionArg(ws, repo, revisionArg)
	if err != nil {
		c.fatal("branch:", err)
	}

	// A branch always owns a distinct revision id so an in-place amend on one
	// branch never mutates a revision shared by another branch. Creating a FRESH
	// branch derives a content clone with its own id and a ForkFrom link; the
	// source revision is left untouched. Re-running `branch` for an existing
	// name is idempotent and keeps its already-private target. message comes
	// from --message (or is inherited from the source); auto_commit finalizes an
	// uncommitted fork.
	var target string
	if cur, cerr := ws.GetRef(name); cerr == nil && cur != nil && cur.Kind == store.RefBranch {
		// Branch already exists and owns its derived target: no-op.
		target = cur.Target
	} else {
		_, ch, err2 := ws.Derive(revisionID, message, autoCommit)
		if err2 != nil {
			c.fatal("branch:", err2)
		}
		target = ch.ID
	}
	r, err := ws.SetRef(name, store.RefBranch, target)
	if err != nil {
		c.fatal("branch:", err)
	}
	if target != revisionID {
		fmt.Printf("branch %s -> revision %s (forked from %s)\n", r.Name, short(r.Target), short(revisionID))
	} else {
		fmt.Printf("branch %s -> revision %s\n", r.Name, short(r.Target))
	}
}

func cmdMessage(c *ctx) {
	repo, _, err := c.loadRepo()
	if err != nil {
		c.fatal("message:", err)
	}
	args := os.Args[2:]
	if len(args) < 2 {
		c.fatal("usage: message <revision> <text>")
	}
	ws := revision.NewWorkspace(repo)
	revisionID, err := resolveRevisionArg(ws, repo, args[0])
	if err != nil {
		c.fatal("message:", err)
	}
	ns, ch, err := ws.SetDescription(revisionID, args[1])
	if err != nil {
		c.fatal("message:", err)
	}
	fmt.Printf("updated message on revision %s (id unchanged) -> snapshot %s\n", short(ch.ID), short(ns.RevisionHash.String()))
}

// resolveRevisionArg resolves a user-supplied revision expression (exact id, id
// prefix, branch/tag name, or "@") to a revision id.
func resolveRevisionArg(ws *revision.Workspace, repo *store.Repo, arg string) (string, error) {
	if arg == "" || arg == "@" {
		revs, err := ws.Log()
		if err != nil {
			return "", err
		}
		if len(revs) == 0 {
			return "", fmt.Errorf("no revisions")
		}
		return revs[0].ID, nil
	}
	if rf, err := ws.GetRef(arg); err == nil && rf != nil {
		return rf.Target, nil
	}
	revs, err := ws.Log()
	if err != nil {
		return "", err
	}
	for _, rv := range revs {
		if rv.ID == arg {
			return rv.ID, nil
		}
	}
	for _, rv := range revs {
		if strings.HasPrefix(rv.ID, arg) {
			return rv.ID, nil
		}
	}
	return "", fmt.Errorf("unknown revision %q", arg)
}

func cmdTag(c *ctx) {
	repo, _, err := c.loadRepo()
	if err != nil {
		c.fatal("tag:", err)
	}
	if len(os.Args) < 4 {
		c.fatal("usage: tag <name> <revision>")
	}
	ws := revision.NewWorkspace(repo)
	// A tag is immutable: re-tagging an existing name is rejected by SetRef.
	r, err := ws.SetRef(os.Args[2], store.RefTag, os.Args[3])
	if err != nil {
		c.fatal("tag:", err)
	}
	fmt.Printf("tag %s -> revision %s\n", r.Name, short(r.Target))
}

func cmdRefs(c *ctx) {
	repo, _, err := c.loadRepo()
	if err != nil {
		c.fatal("refs:", err)
	}
	refs, err := revision.NewWorkspace(repo).ListRefs()
	if err != nil {
		c.fatal("refs:", err)
	}
	for _, r := range refs {
		fmt.Printf("%s  %s -> revision %s\n", r.Kind, r.Name, short(r.Target))
	}
}

func cmdGC(c *ctx) {
	dry := false
	for _, a := range os.Args[2:] {
		if a == "--dry-run" || a == "-dry-run" || a == "-n" {
			dry = true
		}
	}
	res, err := revision.GCRun(c.cs, revision.GCOptions{DryRun: dry})
	if err != nil {
		c.fatal("gc:", err)
	}
	if dry {
		fmt.Printf("gc (dry-run): %d object(s) present, %d would be pruned\n", res.Total, res.Swept)
	} else {
		fmt.Printf("gc: %d object(s) scanned, pruned %d\n", res.Total, res.Swept)
	}
}

func cmdVerify(c *ctx) {
	res, err := revision.Verify(c.cs)
	if err != nil {
		c.fatal("verify:", err)
	}
	fmt.Printf("verify: repos=%d revisions=%d snapshots=%d objects=%d missing_objects=%d broken_snapshots=%d\n",
		res.Repos, res.Revisions, res.Snapshots, res.Objects, res.MissingObjects, res.BrokenSnapshots)
	if res.MissingObjects > 0 || res.BrokenSnapshots > 0 {
		c.fatal("verify: FAILED — dangling objects or broken snapshots found")
	}
}

func cmdDiff(c *ctx) {
	repo, _, err := c.loadRepo()
	if err != nil {
		c.fatal("diff:", err)
	}
	if len(os.Args) < 4 {
		c.fatal("usage: diff <shaA> <shaB>")
	}
	a, err := object.HexToID(os.Args[2])
	if err != nil {
		c.fatal("diff:", err)
	}
	b, err := object.HexToID(os.Args[3])
	if err != nil {
		c.fatal("diff:", err)
	}
	ws := revision.NewWorkspace(repo)
	aTree := snapshotOrTree(ws, a)
	bTree := snapshotOrTree(ws, b)
	fileDiffs, err := ws.DiffContent(aTree, bTree)
	if err != nil {
		c.fatal("diff:", err)
	}
	for _, fd := range fileDiffs {
		prefix := "M"
		switch fd.Status {
		case revision.StatusAdded:
			prefix = "A"
		case revision.StatusRemoved:
			prefix = "D"
		}
		fmt.Printf("%s %s\n", prefix, fd.Path)
	}
	// Print full unified diffs if content is available (non-empty).
	for _, fd := range fileDiffs {
		if fd.Content != "" {
			fmt.Println("--- " + fd.Path)
			fmt.Println("+++ " + fd.Path)
			fmt.Println(fd.Content)
		}
	}
}

func cmdMerge(c *ctx) {
	repo, _, err := c.loadRepo()
	if err != nil {
		c.fatal("merge:", err)
	}
	fs := flag.NewFlagSet("merge", flag.ExitOnError)
	var base, ours, theirs string
	fs.StringVar(&base, "base", "", "base tree sha")
	fs.StringVar(&ours, "ours", "", "ours tree sha")
	fs.StringVar(&theirs, "theirs", "", "theirs tree sha")
	if err := fs.Parse(os.Args[2:]); err != nil {
		c.fatal("merge:", err)
	}
	if base == "" || ours == "" || theirs == "" {
		c.fatal("usage: merge --base <sha> --ours <sha> --theirs <sha>")
	}
	b, _ := object.HexToID(base)
	o, _ := object.HexToID(ours)
	t, _ := object.HexToID(theirs)
	ws := revision.NewWorkspace(repo)
	mergedID, conflicts, err := ws.Merge(b, o, t)
	if err != nil {
		c.fatal("merge:", err)
	}
	fmt.Printf("merged tree %s\n", mergedID.String())
	for _, cf := range conflicts {
		fmt.Fprintf(os.Stderr, "conflict at %s\n", cf.Path)
	}
}

// repoTip returns the parent snapshot ids for a fresh fork: the tip of the
// first branch's revision if any, otherwise no parents.
func repoTip(repo *store.Repo) []object.ID {
	refs, err := repo.ListRefs()
	if err == nil && len(refs) > 0 {
		for _, r := range refs {
			if r.Kind == store.RefBranch {
				if ch, err := repo.GetRevision(r.Target); err == nil {
					return []object.ID{ch.Hash}
				}
			}
		}
	}
	return nil
}

func snapshotOrTree(ws *revision.Workspace, id object.ID) object.ID {
	if snap, err := ws.GetSnapshot(id); err == nil {
		return snap.TreeID
	}
	return id
}

func splitRepo(s string) (string, string, error) {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return s[:i], s[i+1:], nil
		}
	}
	return "", "", fmt.Errorf("expected namespace/name, got %q", s)
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
