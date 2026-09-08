package main

import (
	"os"
	"testing"
)

func TestParseGitArgs(t *testing.T) {
	setArg := func(args ...string) { os.Args = append([]string{"easyvcs", "git-push"}, args...) }
	setArg("git@github.com:o/r.git", "feature", "--token", "abc", "--ssh-key", "/tmp/k")
	u, b, tk, sk, p := parseGitArgs("git-push", true)
	if u != "git@github.com:o/r.git" || b != "feature" || tk != "abc" || sk != "/tmp/k" || p != "" {
		t.Fatalf("got (%q,%q,%q,%q,%q)", u, b, tk, sk, p)
	}

	// URL only (default branch).
	setArg("https://github.com/o/r.git")
	u, b, tk, sk, p = parseGitArgs("git-push", true)
	if u != "https://github.com/o/r.git" || b != "main" || tk != "" {
		t.Fatalf("default: got (%q,%q,%q)", u, b, tk)
	}
}
