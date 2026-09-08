package main

import (
	"fmt"
	"os"

	"github.com/easylab-platform/easyvcs/gitbridge"
	"github.com/easylab-platform/easyvcs/revision"
	"github.com/easylab-platform/easyvcs/store"
)

func cmdGitPull(c *ctx) {
	gitURLOrDir, branch, token, sshKey, pass, _ := parseGitArgs("git-pull", true)
	if gitURLOrDir == "" {
		c.fatal("usage: git-pull <git-repo-or-url> [BRANCH] [--token <t>] [--ssh-key <file>] [--passphrase <p>]")
	}
	repo, marker, err := c.loadRepo()
	if err != nil {
		c.fatal("git-pull:", err)
	}
	ws := revision.NewWorkspace(repo)

	// Import each git commit as an easyvcs revision (reuse the revision header
	// when it matches a known revision, otherwise generate a new id — jj-style
	// weak traceability).
	revIDs, err := gitbridge.ImportBranch(ws, repo, gitbridge.ImportOptions{
		Source: gitURLOrDir, Branch: branch, Token: token, SSHKey: sshKey, SSHKeyPassphrase: pass,
	})
	if err != nil {
		c.fatal("git-pull:", err)
	}
	if len(revIDs) == 0 {
		c.fatalf("git-pull: no revisions imported from %s\n", gitURLOrDir)
	}

	// Move the workspace to the newest imported revision and point the branch.
	lastID := revIDs[len(revIDs)-1]
	lastRev, _ := ws.GetRevision(lastID)
	marker.CurrentRevision = lastID
	marker.Branch = branch
	if err := store.WriteMarker(".", marker); err != nil {
		c.fatal("git-pull:", err)
	}
	if err := c.cs.PutWorkspace(&store.WorkspaceRow{Path: ".", Repo: marker.Repo, CurrentRevision: lastID, Branch: branch}); err != nil {
		c.fatal("git-pull:", err)
	}
	if _, err := ws.SetRef(branch, store.RefBranch, lastID); err != nil {
		c.fatal("git-pull:", err)
	}
	snap, _ := ws.GetSnapshot(lastRev.Hash)
	if snap != nil {
		fmt.Printf("pulled from git %d revision(s); tip %s -> snapshot %s\n",
			len(revIDs), short(lastID), short(snap.RevisionHash.String()))
	}
}

func cmdGitPush(c *ctx) {
	gitDest, branch, token, sshKey, pass, squash := parseGitArgs("git-push", true)
	if gitDest == "" {
		c.fatal("usage: git-push <git-repo-or-url> [BRANCH] [--token <t>] [--ssh-key <file>] [--passphrase <p>]")
	}
	repo, marker, err := c.loadRepo()
	if err != nil {
		c.fatal("git-push:", err)
	}
	ws := revision.NewWorkspace(repo)

	// Resolve the branch tip revision id.
	target := marker.CurrentRevision
	if b, berr := ws.GetRef(branch); berr == nil && b != nil {
		target = b.Target
	}
	if target == "" {
		c.fatal("git-push: no current revision to push")
	}
	// Collect the operand chain (newest -> oldest) then reverse to oldest-first.
	var chain []string
	cur := target
	seen := map[string]bool{}
	for cur != "" && !seen[cur] {
		seen[cur] = true
		chain = append(chain, cur)
		// Walk the first-parent chain via the snapshot.
		rev, err := ws.GetRevision(cur)
		if err != nil || rev == nil {
			break
		}
		snap, err := ws.GetSnapshot(rev.Hash)
		if err != nil || len(snap.Parents) == 0 {
			break
		}
		// Find the revision that owns the parent snapshot.
		_, byHash, err := ws.AllSnapshots()
		if err != nil {
			break
		}
		cur = byHash[snap.Parents[0]]
	}
	// Reverse to oldest-first.
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	if len(chain) == 0 {
		c.fatal("git-push: empty revision chain")
	}

	shas, err := gitbridge.ExportRevisions(ws, repo, gitbridge.PushOptions{
		Dest: gitDest, Branch: branch, Revisions: chain,
		Author: store.Author{Name: "easyvcs", Email: "easyvcs@example.com"},
		Token:  token, SSHKey: sshKey, SSHKeyPassphrase: pass,
		Tags: collectTags(ws, repo), Squash: squash,
	})
	if err != nil {
		c.fatal("git-push:", err)
	}
	fmt.Printf("pushed %d revision(s) to git %s (branch %s)\n", len(shas), gitDest, branch)
}

// collectTags returns the repo's immutable tags (name -> revision id) so the
// git-push exports them as light git tags alongside the branch commits.
func collectTags(ws *revision.Workspace, repo *store.Repo) []gitbridge.TagRef {
	refs, err := ws.ListRefs()
	if err != nil {
		return nil
	}
	var out []gitbridge.TagRef
	for _, r := range refs {
		if r.Kind == store.RefTag {
			out = append(out, gitbridge.TagRef{Name: r.Name, Rev: r.Target})
		}
	}
	return out
}

// parseGitArgs walks args after the URL and returns (url, branch, token,
// sshKey, passphrase, squash). It supports "<url> [branch]" plus
// --token/--ssh-key/--passphrase/--squash. When allowBranch is true and no
// branch is given the default is "main".
func parseGitArgs(cmd string, allowBranch bool) (url, branch, token, sshKey, pass string, squash bool) {
	args := os.Args[2:]
	branch = "main"
	for i := 0; i < len(args); i++ {
		a := args[i]
		// flags consume a following value; skip both. Honor --squash here.
		if a == "--squash" || a == "-squash" {
			squash = true
			continue
		}
		if isGitFlag(a) {
			i++
			continue
		}
		if url == "" {
			url = a
		} else if allowBranch && branch == "main" {
			branch = a
		}
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--token", "-token":
			if i+1 < len(args) {
				token = args[i+1]
				i++
			}
		case "--ssh-key", "-ssh-key":
			if i+1 < len(args) {
				sshKey = args[i+1]
				i++
			}
		case "--passphrase", "-passphrase":
			if i+1 < len(args) {
				pass = args[i+1]
				i++
			}
		}
	}
	return
}

// isGitFlag reports whether an arg is one of the gitbridge auth flags.
func isGitFlag(a string) bool {
	switch a {
	case "--token", "-token", "--ssh-key", "-ssh-key", "--passphrase", "-passphrase":
		return true
	}
	return false
}
