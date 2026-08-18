package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Which trunk a new worktree forks from decides which .wet.yaml it runs, so
// both staleness directions have to be covered: a local checkout left behind,
// and committed-but-unpushed work that the next worktree must not miss.
func TestBaseRefForNewBranch(t *testing.T) {
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	// baseRefForNewBranch shells out in the process's cwd, so each case runs
	// with the repo as the working directory.
	inRepo := func(dir string, fn func()) {
		t.Helper()
		prev, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chdir(dir); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chdir(prev) }()
		fn()
	}

	newRepo := func(t *testing.T) (repo string) {
		t.Helper()
		base := t.TempDir()
		upstream := filepath.Join(base, "upstream.git")
		repo = filepath.Join(base, "repo")
		run(base, "init", "-q", "--bare", upstream)
		run(base, "clone", "-q", upstream, repo)
		run(repo, "config", "user.email", "t@t")
		run(repo, "config", "user.name", "t")
		if err := os.WriteFile(filepath.Join(repo, "f"), []byte("one\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		run(repo, "add", "f")
		run(repo, "commit", "-qm", "one")
		run(repo, "branch", "-M", "development")
		run(repo, "push", "-q", "-u", "origin", "development")
		return repo
	}

	t.Run("local ahead of origin wins — unpushed config fixes must reach the worktree", func(t *testing.T) {
		repo := newRepo(t)
		if err := os.WriteFile(filepath.Join(repo, "f"), []byte("two\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		run(repo, "commit", "-qam", "unpushed")
		inRepo(repo, func() {
			if got := baseRefForNewBranch("development"); got != "development" {
				t.Errorf("got %q, want the local branch", got)
			}
		})
	})

	t.Run("local behind origin loses — a stale checkout must not leak in", func(t *testing.T) {
		repo := newRepo(t)
		// Advance origin without moving the local branch, the way a stale
		// clone sits after someone else pushes.
		run(repo, "commit", "-q", "--allow-empty", "-m", "remote work")
		run(repo, "push", "-q")
		run(repo, "reset", "-q", "--hard", "HEAD~1")
		inRepo(repo, func() {
			if got := baseRefForNewBranch("development"); got != "origin/development" {
				t.Errorf("got %q, want origin/development", got)
			}
		})
	})

	t.Run("no remote falls back to the local branch", func(t *testing.T) {
		dir := t.TempDir()
		run(dir, "init", "-q", "-b", "development")
		run(dir, "config", "user.email", "t@t")
		run(dir, "config", "user.name", "t")
		run(dir, "commit", "-q", "--allow-empty", "-m", "one")
		inRepo(dir, func() {
			if got := baseRefForNewBranch("development"); got != "development" {
				t.Errorf("got %q, want development", got)
			}
		})
	})

	t.Run("unknown trunk falls back to HEAD", func(t *testing.T) {
		repo := newRepo(t)
		inRepo(repo, func() {
			if got := baseRefForNewBranch("no-such-branch"); got != "HEAD" {
				t.Errorf("got %q, want HEAD", got)
			}
		})
	})
}
