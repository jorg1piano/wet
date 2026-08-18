package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type WorktreeState struct {
	Branch    string            `json:"branch"`
	Path      string            `json:"path"`
	CreatedAt time.Time         `json:"created_at"`
	Index     int               `json:"index,omitempty"`
	Ports     map[string]int    `json:"ports,omitempty"`
	Pools     map[string]string `json:"pools,omitempty"`
}

type WorktreeConfig struct {
	Worktrees []WorktreeState `json:"worktrees"`
}

func configFilePath() string {
	// Always resolve to the MAIN repo root so worktrees and the main repo
	// share the same state file.
	repoRoot, err := getMainRepoRoot()
	if err != nil {
		return ".wet-worktrees.json"
	}
	return filepath.Join(repoRoot, ".wet-worktrees.json")
}

func loadConfig() (*WorktreeConfig, error) {
	data, err := os.ReadFile(configFilePath())
	if err != nil {
		if os.IsNotExist(err) {
			return &WorktreeConfig{Worktrees: []WorktreeState{}}, nil
		}
		return nil, err
	}

	var config WorktreeConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	return &config, nil
}

func saveConfig(config *WorktreeConfig) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configFilePath(), data, 0644)
}

// getRepoRoot returns the top-level directory of the current git repository.
// Inside a worktree it returns the WORKTREE root (use getMainRepoRoot for the
// main working tree).
func getRepoRoot() (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("not a git repository: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// getMainRepoRoot returns the root of the main working tree, even when called
// from inside a worktree. This is where .wet-worktrees.json and .wet.yaml live.
func getMainRepoRoot() (string, error) {
	cmd := exec.Command("git", "rev-parse", "--path-format=absolute", "--git-common-dir")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("not a git repository: %w", err)
	}
	// --git-common-dir returns the .git directory; the repo root is its parent
	gitDir := strings.TrimSpace(string(output))
	return filepath.Dir(gitDir), nil
}

// checkoutForBranch returns the working tree that has branch checked out.
//
// "The main repo" is not reliably git's main working tree. A machine can carry
// several checkouts of one repo, and the one git calls "main" is simply the
// one that was cloned first — it can be parked on an unrelated branch, or be a
// stale clone nobody works in. What wet means by the main checkout is the one
// holding the trunk (`main_branch:`), because that is where the machine-local
// .wet.local.yaml sits and what a worktree's copied files should come from.
// dir scopes the query to the repo that owns the config being loaded; running
// it in the process's cwd would answer about whatever repo wet was invoked
// from, which need not be the same one.
func checkoutForBranch(dir, branch string) (string, bool) {
	if branch == "" || dir == "" {
		return "", false
	}
	out, err := exec.Command("git", "-C", dir, "worktree", "list", "--porcelain").Output()
	if err != nil {
		return "", false
	}
	want := "branch refs/heads/" + branch
	path := ""
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			path = strings.TrimPrefix(line, "worktree ")
		case line == want && path != "":
			return path, true
		}
	}
	return "", false
}

var worktreeCmd = &cobra.Command{
	Use:     "worktree",
	Short:   "Manage git worktrees",
	Aliases: []string{"wt"},
}

func init() {
	rootCmd.AddCommand(worktreeCmd)
}
