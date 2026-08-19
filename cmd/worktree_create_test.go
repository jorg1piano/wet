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

// Copying a checkout onto itself must be a no-op, not a wipe. copyFile
// truncates the destination before reading the source, so an unguarded
// src == dst zeroes every copied file — including ones wet never renders.
//
// Reachable in the wild without the main checkout being involved:
// LocalConfigDir resolves to whichever checkout holds main_branch, so a
// checkout standing on that branch gets srcRoot == dstRoot even under
// `wet worktree setup`, where isMain is false.
func TestCopyFromConfigOntoItselfPreservesFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".vscode"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"settings.json":         `{"a":1}`,
		".vscode/launch.json":   `{"b":2}`,
		".vscode/settings.json": `{"c":3}`,
	}
	for rel, body := range files {
		if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &WetConfig{Copy: CopyConfig{
		Files: []string{"settings.json"},
		Dirs:  []string{".vscode"},
	}}

	copied, skipped := copyFromConfig(cfg, root, root)
	if len(copied) != 0 {
		t.Errorf("copied %v; a self-copy must report nothing", copied)
	}
	if skipped == "" {
		t.Error("a self-copy must report why it was skipped, not stay silent")
	}
	for rel, want := range files {
		got, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q (truncated by a self-copy)", rel, got, want)
		}
	}
}

// The same guard, one level down, for any future caller that forgets.
func TestCopyFileRefusesSelfCopy(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "keep.json")
	if err := os.WriteFile(path, []byte(`{"a":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(path, path); err == nil {
		t.Error("copyFile(src, src) returned nil; want a refusal")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"a":1}` {
		t.Errorf("file = %q; copyFile truncated it", got)
	}
}

// Distinct roots, but a `..` in the declared path lands dst back on src — a
// shape the root guard in copyFromConfig never sees. copyFile is what refuses
// it; this asserts the behaviour, not which layer provides it.
func TestCopyOneFileRefusesWhenRelEscapesOntoSource(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	dst := filepath.Join(base, "dst")
	for _, d := range []string{src, dst} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// rel resolves to base/shared.json from both roots.
	if err := os.WriteFile(filepath.Join(base, "shared.json"), []byte(`{"a":1}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if copyOneFile(src, dst, "../shared.json") {
		t.Error("copyOneFile reported success copying a file onto itself")
	}
	got, err := os.ReadFile(filepath.Join(base, "shared.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"a":1}` {
		t.Errorf("file = %q; it was truncated", got)
	}
}

// A dst nested inside src makes filepath.Walk copy the tree into itself,
// deeper on each pass, because Walk reads a directory's entries as it enters
// it rather than up front. `copy: dirs: ["."]` is legal config
// (validateCopyPath accepts "."), and a worktree inside the repo makes dstRoot
// a subdirectory of srcRoot — which is exactly this shape.
func TestCopyOneDirRefusesDestinationInsideSource(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.json"), []byte(`{"a":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	dstRoot := filepath.Join(src, "worktrees", "feature")

	if copyOneDir(src, dstRoot, ".") {
		t.Error("copyOneDir reported success copying a tree into itself")
	}
	if _, err := os.Stat(filepath.Join(dstRoot, "a.json")); !os.IsNotExist(err) {
		t.Errorf("copied into its own source tree (stat err: %v)", err)
	}
}

// The reverse nesting: copying a parent of the destination.
func TestCopyOneDirRefusesSourceInsideDestination(t *testing.T) {
	dst := t.TempDir()
	srcRoot := filepath.Join(dst, "inner")
	if err := os.MkdirAll(filepath.Join(srcRoot, "cfg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcRoot, "cfg", "a.json"), []byte(`{"a":1}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if copyOneDir(srcRoot, dst, ".") {
		t.Error("copyOneDir reported success copying a tree over its own parent")
	}
}

// samePath must see through symlinks — the reason it does not just compare
// cleaned strings. A symlinked checkout is still the same checkout.
func TestSamePathResolvesSymlinks(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if !samePath(real, link) {
		t.Error("samePath(real, symlink-to-real) = false; want true")
	}
	if samePath(real, filepath.Join(base, "other")) {
		t.Error("samePath reported two distinct paths as the same")
	}
}
