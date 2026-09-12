package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPopMainSubdirectoryDoesNotRunTeardown(t *testing.T) {
	repo := initCleanupRepo(t)
	t.Chdir(repo)
	t.Setenv("TMUX", "")
	if err := os.WriteFile(filepath.Join(repo, wetConfigFileName), []byte("project_name: app\nhooks:\n  pre_remove: ['touch teardown-ran']\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	state := &WorktreeConfig{Worktrees: []WorktreeState{{Branch: "main", Path: repo, Ports: map[string]int{"APP_PORT": 4000}}}}
	if err := saveConfig(state); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(repo, "backend")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(subdir)
	if err := runWorktreePop(worktreePopCmd, nil); err == nil || !strings.Contains(err.Error(), "refusing to pop the main repo") {
		t.Fatalf("expected refusal before teardown, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "teardown-ran")); !os.IsNotExist(err) {
		t.Fatalf("main teardown hook ran: %v", err)
	}
}

func TestRemovalPreservesStateWrittenDuringHook(t *testing.T) {
	repo := initCleanupRepo(t)
	t.Chdir(repo)
	wt := filepath.Join(filepath.Dir(repo), "worktree-feature")
	if out, err := exec.Command("git", "worktree", "add", "-b", "feature", wt).CombinedOutput(); err != nil {
		t.Fatalf("create worktree: %v\n%s", err, out)
	}
	state := &WorktreeConfig{Worktrees: []WorktreeState{{Branch: "feature", Path: wt, Index: 1}}}
	if err := saveConfig(state); err != nil {
		t.Fatal(err)
	}
	// Reproduce a setup finishing after removal's initial read, while its
	// teardown hook is running. The external CLI suite also runs two wet
	// processes with a barrier at this point.
	state.Worktrees = append(state.Worktrees, WorktreeState{Branch: "other", Path: filepath.Join(filepath.Dir(repo), "other"), Index: 2})
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "next-state.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf("project_name: app\nhooks:\n  pre_remove:\n    - 'cp next-state.json \"%s\"'\n", filepath.Join(repo, ".wet-worktrees.json"))
	if err := os.WriteFile(filepath.Join(wt, wetConfigFileName), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := removeWorktreeByPath("feature", wt); err != nil {
		t.Fatal(err)
	}
	got, err := loadConfig()
	if err != nil || len(got.Worktrees) != 1 || got.Worktrees[0].Branch != "other" {
		t.Fatalf("lost allocation recorded during teardown: %+v, %v", got, err)
	}
	if err := exec.Command("git", "show-ref", "--verify", "refs/heads/feature").Run(); err != nil {
		t.Fatal("list removal deleted the branch")
	}
}

func TestListRemovalAbortsOnHookFailure(t *testing.T) {
	repo := initCleanupRepo(t)
	t.Chdir(repo)
	wt := filepath.Join(filepath.Dir(repo), "worktree-feature")
	if out, err := exec.Command("git", "worktree", "add", "-b", "feature", wt).CombinedOutput(); err != nil {
		t.Fatalf("create worktree: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(wt, wetConfigFileName), []byte("project_name: app\nhooks:\n  pre_remove: ['exit 7']\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveConfig(&WorktreeConfig{Worktrees: []WorktreeState{{Branch: "feature", Path: wt, Index: 1}}}); err != nil {
		t.Fatal(err)
	}
	if err := removeWorktreeByPath("feature", wt); err == nil || !strings.Contains(err.Error(), "pre_remove") {
		t.Fatalf("expected teardown failure, got %v", err)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree removed after failed teardown: %v", err)
	}
}

func TestSetupWithOnlyOnDemandOutputsRunsHooks(t *testing.T) {
	repo := initCleanupRepo(t)
	t.Chdir(repo)
	config := `project_name: app
outputs:
  - name: deploy
    out: deploy.env
    format: dotenv
    on_demand: true
    values:
      HOST: {literal: deploy.example}
hooks:
  post_create: ['touch setup-ran']
`
	if err := os.WriteFile(filepath.Join(repo, wetConfigFileName), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runSetupCore(repo, repo, "main", false, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, "setup-ran")); err != nil {
		t.Fatalf("setup did not finish its hooks: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "deploy.env")); !os.IsNotExist(err) {
		t.Fatalf("setup generated an on-demand file: %v", err)
	}
}
