package main

import (
	"os/exec"
	"testing"

	"github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/revision"
	"github.com/easylab-platform/easyvcs/store"
)

// runGit runs `git ...` in the given directory (or no dir for global commands)
// and returns the trimmed stdout. It skips the test if git is missing.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
	return string(out)
}

// objectKindBlob returns object.KindBlob for entry-kind comparisons.
func objectKindBlob() object.Kind { return object.KindBlob }

// revisionNewWorkspace wraps a repo in a revision.Workspace for tests.
func revisionNewWorkspace(repo *store.Repo) *revision.Workspace {
	return revision.NewWorkspace(repo)
}
