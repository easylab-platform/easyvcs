package gitbridge

import (
	"os"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	gitobject "github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport/http"

	evobject "github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/revision"
	"github.com/easylab-platform/easyvcs/store"
)

// revisionHeaderPrefix is the message line marking an easyvcs revision id for
// weak traceability (jj-style): when importing from git, if the header matches
// a known local revision it is reused; otherwise a new revision is generated.
const revisionHeaderPrefix = "revision: "

// Branch ref prefixes.
const (
	RefHeadsPrefix = "refs/heads/"
	RefTagsPrefix  = "refs/tags/"
)

// PushOptions configures an export.
type PushOptions struct {
	Dest      string
	Branch    string
	Revisions []string
	Author    store.Author
	Token     string
	Squash    bool
}

// ImportOptions configures a pull.
type ImportOptions struct {
	Source string
	Branch string
	Token  string
}

// ExportRevisions writes the revisions to dest as git commits on Branch and
// returns the commit shas written. It creates a fresh repo per run and
// force-pushes the branch, so it is idempotent for the exported tip. EasyVCS's
// own object store is never written as git objects — git only exists here in a
// temporary worktree.
func ExportRevisions(ws *revision.Workspace, repo *store.Repo, opts PushOptions) ([]string, error) {
	dir, err := os.MkdirTemp("", "easyvcs-gitbridge-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	g, err := git.PlainInit(dir, false)
	if err != nil {
		return nil, err
	}
	if _, err := g.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{opts.Dest}}); err != nil {
		return nil, err
	}

	var shas []string
	for _, rid := range opts.Revisions {
		sha, err := exportOneRevision(ws, g, dir, opts, rid)
		if err != nil {
			return nil, err
		}
		if err := repo.PutGitLink(rid, sha.String(), RefHeadsPrefix+opts.Branch, "export"); err != nil {
			return nil, err
		}
		shas = append(shas, sha.String())
	}

	// Rename the default branch to opts.Branch so the commits are under
	// refs/heads/<branch>, then force-push that branch.
	if opts.Branch != "" {
		if err := renameBranchTo(g, opts.Branch); err != nil {
			return nil, err
		}
	}
	if err := pushBranch(g, opts); err != nil {
		return nil, err
	}
	// If the destination is a local path, point its HEAD at the pushed branch so
	// a subsequent clone/fetch sees a valid default branch (a bare repo pushed
	// with go-git keeps its original unborn HEAD otherwise).
	if isLocalPath(opts.Dest) {
		_ = setDestHEAD(opts.Dest, opts.Branch)
	}
	return shas, nil
}

// isLocalPath reports whether a dest/source string is a local filesystem path.
func isLocalPath(p string) bool {
	return p != "" && !strings.Contains(p, "://") && !strings.HasPrefix(p, "git@")
}

// setDestHEAD points a local git repo's HEAD at refs/heads/<branch> (best-effort).
func setDestHEAD(path, branch string) error {
	dg, err := git.PlainOpen(path)
	if err != nil {
		return err
	}
	return dg.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName(branch)))
}

// renameBranchTo renames the current branch to the given name so the commits are
// available under refs/heads/<branch>.
func renameBranchTo(g *git.Repository, branch string) error {
	head, err := g.Head()
	if err != nil {
		return err
	}
	newRef := plumbing.NewBranchReferenceName(branch)
	if err := g.Storer.SetReference(plumbing.NewHashReference(newRef, head.Hash())); err != nil {
		return err
	}
	if err := g.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, newRef)); err != nil {
		return err
	}
	_ = g.Storer.RemoveReference(plumbing.NewBranchReferenceName(head.Name().Short()))
	return nil
}

// ImportBranch pulls a git branch into easyvcs. Each git commit becomes a
// revision (reusing the `revision:` header id when known, else generating a new
// one) and returns the local revision ids created, oldest first.
func ImportBranch(ws *revision.Workspace, repo *store.Repo, opts ImportOptions) ([]string, error) {
	dir, err := os.MkdirTemp("", "easyvcs-gitbridge-pull-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	co := &git.CloneOptions{URL: opts.Source}
	if opts.Token != "" {
		co.Auth = &http.BasicAuth{Username: "token", Password: opts.Token}
	}
	g, err := git.PlainClone(dir, false, co)
	if err != nil {
		return nil, err
	}

	ref, err := g.Reference(plumbing.NewBranchReferenceName(opts.Branch), true)
	if err != nil {
		// Fall back: the cloned repo's remote-tracking ref (refs/remotes/origin/<b>)
		// or the repository HEAD.
		if r, rerr := g.Reference(plumbing.NewRemoteReferenceName("origin", opts.Branch), true); rerr == nil {
			ref = r
		} else if hr, herr := g.Head(); herr == nil {
			ref = hr
		} else {
			return nil, err
		}
	}
	iter, err := g.Log(&git.LogOptions{From: ref.Hash()})
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	var commits []*gitobject.Commit
	_ = iter.ForEach(func(c *gitobject.Commit) error {
		commits = append(commits, c)
		return nil
	})
	reverse(commits)

	var out []string
	var parentSnapID evobject.ID
	for _, c := range commits {
		revID, err := importOneCommit(ws, repo, opts, c, parentSnapID)
		if err != nil {
			return out, err
		}
		if revID == "" {
			continue
		}
		out = append(out, revID)
		rev, err := ws.GetRevision(revID)
		if err != nil {
			continue
		}
		parentSnapID = rev.Hash
	}
	return out, nil
}


func exportOneRevision(ws *revision.Workspace, g *git.Repository, dir string, opts PushOptions, revID string) (plumbing.Hash, error) {
	rev, err := ws.GetRevision(revID)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	snap, err := ws.GetSnapshot(rev.Hash)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	wt, err := g.Worktree()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if err := clearDir(dir); err != nil {
		return plumbing.ZeroHash, err
	}
	if err := ws.Materialize(snap.TreeID, dir); err != nil {
		return plumbing.ZeroHash, err
	}
	if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		return plumbing.ZeroHash, err
	}
	msg := revisionHeaderPrefix + revID + "\n\n" + snap.Description
	commit, err := wt.Commit(msg, &git.CommitOptions{
		Author: &gitobject.Signature{Name: opts.Author.Name, Email: opts.Author.Email},
	})
	if err != nil {
		return plumbing.ZeroHash, err
	}
	return commit, nil
}

func importOneCommit(ws *revision.Workspace, repo *store.Repo, opts ImportOptions, c *gitobject.Commit, parentSnapID evobject.ID) (string, error) {
	dir, err := os.MkdirTemp("", "easyvcs-gitbridge-tree-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)

	tree, err := c.Tree()
	if err != nil {
		return "", err
	}
	if err := copyGitTree(tree, dir, ""); err != nil {
		return "", err
	}
	treeID, err := ws.BuildTreeFromFS(dir)
	if err != nil {
		return "", err
	}

	revID := headerRevision(c.Message)
	if revID != "" {
		if _, err := repo.GetRevision(revID); err != nil {
			revID = ""
		}
	}

	snap, ch, err := ws.Commit(revision.CommitParams{
		RevisionID:  revID,
		Parents:     parentSet(parentSnapID),
		TreeID:      treeID,
		Description: headerDescription(c.Message),
		Author:      authorFromSig(&c.Author),
	})
	if err != nil {
		return "", err
	}
	_ = snap
	if err := repo.PutGitLink(ch.ID, c.Hash.String(), "", "import"); err != nil {
		return "", err
	}
	return ch.ID, nil
}

func pushBranch(g *git.Repository, opts PushOptions) error {
	refName := plumbing.NewBranchReferenceName(opts.Branch)
	if err := g.Push(&git.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{config.RefSpec(refName.String() + ":" + refName.String())},
		Force:      true,
	}); err != nil {
		return err
	}
	return nil
}

func clearDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if name == ".git" {
			continue
		}
		if err := os.RemoveAll(dir + "/" + name); err != nil {
			return err
		}
	}
	return nil
}

// copyGitTree writes a git tree's entries into dir (recursively).
func copyGitTree(t *gitobject.Tree, dir, prefix string) error {
	for _, e := range t.Entries {
		full := dir + "/" + e.Name
		switch {
		case e.Mode == 040000:
			sub, err := t.Tree(e.Name)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(full, 0o755); err != nil {
				return err
			}
			if err := copyGitTree(sub, full, e.Name); err != nil {
				return err
			}
		default:
			f, err := t.File(e.Name)
			if err != nil {
				return err
			}
			data, err := f.Contents()
			if err != nil {
				return err
			}
			if err := os.WriteFile(full, []byte(data), 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

func parentSet(h evobject.ID) []evobject.ID {
	if h.IsZero() {
		return nil
	}
	return []evobject.ID{h}
}

// headerRevision extracts the revision id from a commit message, or "".
func headerRevision(msg string) string {
	prefix := revisionHeaderPrefix
	if len(msg) >= len(prefix) && msg[:len(prefix)] == prefix {
		rest := msg[len(prefix):]
		end := 0
		for end < len(rest) && rest[end] != '\n' {
			end++
		}
		id := rest[:end]
		if id != "" {
			return id
		}
	}
	return ""
}

// headerDescription strips the revision header, returning the human message.
func headerDescription(msg string) string {
	if headerRevision(msg) != "" {
		rest := msg[len(revisionHeaderPrefix):]
		if i := indexByte(rest, '\n'); i >= 0 {
			rest = rest[i+1:]
		} else {
			rest = ""
		}
		for len(rest) > 0 && rest[0] == '\n' {
			rest = rest[1:]
		}
		return rest
	}
	return msg
}

func authorFromSig(sig *gitobject.Signature) store.Author {
	if sig == nil {
		return store.Author{}
	}
	return store.Author{Name: sig.Name, Email: sig.Email}
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func reverse(cs []*gitobject.Commit) {
	for i, j := 0, len(cs)-1; i < j; i, j = i+1, j-1 {
		cs[i], cs[j] = cs[j], cs[i]
	}
}

