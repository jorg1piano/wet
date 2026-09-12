package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

var worktreeMountCmd = &cobra.Command{
	Use:   "mount <branch-name>",
	Short: "Mount an existing branch in a new worktree",
	Long: `Mount an existing branch in a new git worktree at ../worktree-<branch>.

Unlike ` + "`wet worktree create`" + `, mount does NOT create a new branch — the
branch must already exist locally or on ` + "`origin`" + `. If only the remote
branch exists, mount fetches it and creates a local tracking branch from
` + "`origin/<branch>`" + ` before checking it out in the worktree.

After the worktree exists on disk, mount runs the same post-create steps as
create: copies ` + "`.env`" + ` from the main repo root (if present) and opens a
tmux window when inside tmux. Run ` + "`wet worktree setup`" + ` from inside the new
worktree to perform the ` + "`.wet.yaml`" + `-driven configuration step (port
allocation, file copy list, hooks).

Pass ` + "`--no-multiplexer`" + ` to skip opening a configured terminal
multiplexer window when another tool manages sessions.`,
	Args: cobra.ExactArgs(1),
	RunE: runWorktreeMount,
}

var worktreeMountNoMultiplexer bool

func init() {
	worktreeMountCmd.Flags().BoolVar(&worktreeMountNoMultiplexer, "no-multiplexer", false, "don't open a new terminal multiplexer window after mounting the worktree")
	worktreeCmd.AddCommand(worktreeMountCmd)
}

func runWorktreeMount(cmd *cobra.Command, args []string) error {
	branchName := strings.TrimSpace(args[0])
	if branchName == "" {
		return fmt.Errorf("branch name cannot be empty")
	}
	return mountWorktree(branchName, worktreeMountNoMultiplexer)
}

// mountWorktree is the worktree-mount path for a branch that already exists
// somewhere (locally or on origin). It:
//  1. Resolves the main repo root.
//  2. Resolves where the branch lives: prefer a local branch; otherwise fetch
//     origin/<branch> and create a local tracking branch from it. Fails
//     clearly if neither exists.
//  3. Runs `git worktree add <path> <branch>` (no `-b` — the branch already
//     exists, we're checking it out, not creating it).
//  4. Runs the shared post-create steps (tmux window, next-step hint) via
//     postWorktreeAdd.
//
// No .wet.yaml load, no port allocation, no state file write, no hooks. Run
// `wet worktree setup` from inside the new worktree to configure the rest.
func mountWorktree(branchName string, noMultiplexer bool) error {
	repoRoot, err := getMainRepoRoot()
	if err != nil {
		return err
	}

	if err := ensureLocalBranch(repoRoot, branchName); err != nil {
		return err
	}

	worktreePath := worktreePathFor(repoRoot, branchName)

	gitCmd := exec.Command("git", "-C", repoRoot, "worktree", "add", worktreePath, branchName)
	gitCmd.Stdout = os.Stdout
	gitCmd.Stderr = os.Stderr
	if err := gitCmd.Run(); err != nil {
		return fmt.Errorf("failed to create worktree: %w", err)
	}

	return postWorktreeAdd(repoRoot, worktreePath, branchName, "Mounted", noMultiplexer)
}

// ensureLocalBranch guarantees that a local branch named `branchName` exists
// in the repository at `repoRoot`, creating it from `origin/<branchName>` if
// needed. Returns an error if neither a local branch nor an origin-tracked
// remote branch exists.
func ensureLocalBranch(repoRoot, branchName string) error {
	if localBranchExists(repoRoot, branchName) {
		return nil
	}

	// No local branch — try origin. Fetch so origin/<branch> is up to date
	// (or exists at all if this is a freshly cloned repo that hasn't fetched
	// since the remote branch was pushed).
	fetchCmd := exec.Command("git", "-C", repoRoot, "fetch", "origin", branchName)
	fetchCmd.Stdout = os.Stdout
	fetchCmd.Stderr = os.Stderr
	if err := fetchCmd.Run(); err != nil {
		return fmt.Errorf("branch '%s' does not exist locally and `git fetch origin %s` failed: %w", branchName, branchName, err)
	}

	if !remoteBranchExists(repoRoot, branchName) {
		return fmt.Errorf("branch '%s' not found locally or on origin", branchName)
	}

	// Create a local tracking branch from origin/<branch>. `git branch
	// <branch> origin/<branch>` sets up the upstream automatically when the
	// start-point is a remote-tracking ref.
	createCmd := exec.Command("git", "-C", repoRoot, "branch", branchName, "origin/"+branchName)
	createCmd.Stdout = os.Stdout
	createCmd.Stderr = os.Stderr
	if err := createCmd.Run(); err != nil {
		return fmt.Errorf("failed to create local tracking branch for origin/%s: %w", branchName, err)
	}
	fmt.Printf("  Created local tracking branch '%s' from origin/%s\n", branchName, branchName)
	return nil
}

// localBranchExists reports whether a local ref refs/heads/<branchName>
// exists in the repository at repoRoot.
func localBranchExists(repoRoot, branchName string) bool {
	c := exec.Command("git", "-C", repoRoot, "show-ref", "--verify", "--quiet", "refs/heads/"+branchName)
	return c.Run() == nil
}

// remoteBranchExists reports whether refs/remotes/origin/<branchName> exists
// in the repository at repoRoot. Call after `git fetch origin <branch>` if
// you want the freshest answer.
func remoteBranchExists(repoRoot, branchName string) bool {
	c := exec.Command("git", "-C", repoRoot, "show-ref", "--verify", "--quiet", "refs/remotes/origin/"+branchName)
	return c.Run() == nil
}
