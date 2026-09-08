package main

import (
	"fmt"
	"os"

	"github.com/easylab-platform/easyvcs/gitbridge"
	"github.com/easylab-platform/easyvcs/revision"
	"github.com/easylab-platform/easyvcs/store"
)

func cmdGitPull(c *ctx) {
	gitURLOrDir := gitArg()
	if gitURLOrDir == "" {
		fmt.Fprintln(os.Stderr, "usage: git-pull <git-repo-or-url> [BRANCH]")
		os.Exit(1)
	}
	branch := "main"
	args := os.Args[2:]
	if len(args) > 1 {
		branch = args[1]
	}
	repo, marker, err := c.loadRepo()
	if err != nil {
		fmt.Fprintln(os.Stderr, "git-pull:", err)
		os.Exit(1)
	}
	ws := revision.NewWorkspace(repo)

	// Import each git commit as an easyvcs revision (reuse the revision header
	// when it matches a known revision, otherwise generate a new id — jj-style
	// weak traceability).
	revIDs, err := gitbridge.ImportBranch(ws, repo, gitbridge.ImportOptions{
		Source: gitURLOrDir, Branch: branch,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "git-pull:", err)
		os.Exit(1)
	}
	if len(revIDs) == 0 {
		fmt.Fprintf(os.Stderr, "git-pull: no revisions imported from %s\n", gitURLOrDir)
		os.Exit(1)
	}

	// Move the workspace to the newest imported revision and point the branch.
	lastID := revIDs[len(revIDs)-1]
	lastRev, _ := ws.GetRevision(lastID)
	marker.CurrentRevision = lastID
	marker.Branch = branch
	if err := store.WriteMarker(".", marker); err != nil {
		fmt.Fprintln(os.Stderr, "git-pull:", err)
		os.Exit(1)
	}
	if err := c.cs.PutWorkspace(&store.WorkspaceRow{Path: ".", Repo: marker.Repo, CurrentRevision: lastID, Branch: branch}); err != nil {
		fmt.Fprintln(os.Stderr, "git-pull:", err)
		os.Exit(1)
	}
	if _, err := ws.SetRef(branch, store.RefBranch, lastID); err != nil {
		fmt.Fprintln(os.Stderr, "git-pull:", err)
		os.Exit(1)
	}
	snap, _ := ws.GetSnapshot(lastRev.Hash)
	if snap != nil {
		fmt.Printf("pulled from git %d revision(s); tip %s -> snapshot %s\n",
			len(revIDs), short(lastID), short(snap.RevisionHash.String()))
	}
}

func cmdGitPush(c *ctx) {
	gitDest := gitArg()
	if gitDest == "" {
		fmt.Fprintln(os.Stderr, "usage: git-push <git-repo-or-url> [BRANCH]")
		os.Exit(1)
	}
	branch := "main"
	args := os.Args[2:]
	if len(args) > 1 {
		branch = args[1]
	}
	repo, marker, err := c.loadRepo()
	if err != nil {
		fmt.Fprintln(os.Stderr, "git-push:", err)
		os.Exit(1)
	}
	ws := revision.NewWorkspace(repo)

	// Resolve the branch tip revision id.
	target := marker.CurrentRevision
	if b, berr := ws.GetRef(branch); berr == nil && b != nil {
		target = b.Target
	}
	if target == "" {
		fmt.Fprintln(os.Stderr, "git-push: no current revision to push")
		os.Exit(1)
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
		fmt.Fprintln(os.Stderr, "git-push: empty revision chain")
		os.Exit(1)
	}

	shas, err := gitbridge.ExportRevisions(ws, repo, gitbridge.PushOptions{
		Dest: gitDest, Branch: branch, Revisions: chain,
		Author: store.Author{Name: "easyvcs", Email: "easyvcs@example.com"},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "git-push:", err)
		os.Exit(1)
	}
	fmt.Printf("pushed %d revision(s) to git %s (branch %s)\n", len(shas), gitDest, branch)
}
