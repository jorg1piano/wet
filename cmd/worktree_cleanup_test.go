package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCleanupWorktreeAllocationsClearsStateOnly(t *testing.T) {
	repo := initCleanupRepo(t)
	inDir(t, repo)

	state := WorktreeConfig{Worktrees: []WorktreeState{
		{
			Branch:    "feature/a",
			Path:      filepath.Join(filepath.Dir(repo), "worktree-feature-a"),
			CreatedAt: time.Now(),
			Index:     1,
			Ports:     map[string]int{"APP_PORT": 4010},
			Pools:     map[string]string{"devices": "emulator:a"},
		},
		{
			Branch:    "feature/b",
			Path:      filepath.Join(filepath.Dir(repo), "worktree-feature-b"),
			CreatedAt: time.Now(),
			Index:     2,
			Ports:     map[string]int{"APP_PORT": 4020},
		},
	}}
	if err := saveConfig(&state); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := cleanupWorktreeAllocations(&out, false); err != nil {
		t.Fatalf("cleanupWorktreeAllocations: %v", err)
	}
	if !strings.Contains(out.String(), "Cleared 2 tracked worktree allocation(s)") {
		t.Errorf("cleanup output did not report removals:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Worktree directories and branches were not removed") {
		t.Errorf("cleanup output should explain its scope:\n%s", out.String())
	}

	got, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Worktrees) != 0 {
		t.Fatalf("state still has allocations: %+v", got.Worktrees)
	}
}

func TestCleanupWorktreeAllocationsDryRunLeavesState(t *testing.T) {
	repo := initCleanupRepo(t)
	inDir(t, repo)

	state := WorktreeConfig{Worktrees: []WorktreeState{{
		Branch:    "feature/a",
		Path:      filepath.Join(filepath.Dir(repo), "worktree-feature-a"),
		CreatedAt: time.Now(),
		Index:     1,
		Ports:     map[string]int{"APP_PORT": 4010},
	}}}
	if err := saveConfig(&state); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := cleanupWorktreeAllocations(&out, true); err != nil {
		t.Fatalf("cleanupWorktreeAllocations dry-run: %v", err)
	}
	if !strings.Contains(out.String(), "Would clear 1 tracked worktree allocation(s)") {
		t.Errorf("dry-run output did not report preview:\n%s", out.String())
	}

	data, err := os.ReadFile(filepath.Join(repo, ".wet-worktrees.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got WorktreeConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Worktrees) != 1 || got.Worktrees[0].Branch != "feature/a" {
		t.Fatalf("dry-run changed state: %+v", got.Worktrees)
	}
}

func initCleanupRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	run("commit", "-q", "--allow-empty", "-m", "initial")
	return repo
}

func inDir(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
}
