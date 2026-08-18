package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// printPopHookFailure writes a multi-line recovery block to stderr when a
// pre_remove hook returns a nonzero exit. Nothing has been removed — the
// git worktree is still on disk, the branch still exists, and the state
// file still lists the entry. The user fixes the cause and retries.
func printPopHookFailure(branch, path string, slot int, hook, rendered string, hookErr error) {
	fmt.Fprintf(os.Stderr, `
Hook failed while removing worktree '%s'.

  Worktree:  %s
  Branch:    %s
  Slot:      %d
  Hook:      %s
  Command:   %s
  Error:     %v

The worktree has NOT been removed. Its entry in .wet-worktrees.json is unchanged.
Fix the cause and retry `+"`wet worktree pop`"+` from inside the worktree, or edit
.wet.yaml's pre_remove hooks if the hook itself is broken.

`, branch, path, branch, slot, hook, rendered, hookErr)
}

var worktreePopCmd = &cobra.Command{
	Use:   "pop",
	Short: "Remove current worktree and exit tmux pane",
	Long:  "Removes the git worktree for the current directory, deletes the branch, and kills the tmux pane.",
	RunE:  runWorktreePop,
}

func init() {
	worktreeCmd.AddCommand(worktreePopCmd)
}

func runWorktreePop(cmd *cobra.Command, args []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get working directory: %w", err)
	}

	// Resolve the MAIN repo root (not the worktree root). This is where
	// .wet.yaml lives and where we cd after removal.
	repoRoot, err := getMainRepoRoot()
	if err != nil {
		return fmt.Errorf("not in a git repository: %w", err)
	}

	absCwd, _ := filepath.Abs(cwd)
	absMainRoot, _ := filepath.Abs(repoRoot)
	if absCwd == absMainRoot {
		return fmt.Errorf("refusing to pop the main repo; run `wet worktree pop` from inside a worktree")
	}

	// Worktree root is the toplevel of the current working tree.
	worktreeRoot, err := getRepoRoot()
	if err != nil {
		return fmt.Errorf("not in a git worktree: %w", err)
	}
	worktreePath, _ := filepath.Abs(worktreeRoot)

	// Find a matching state entry (tracked worktree). Untracked worktrees
	// (created via `git worktree add` or before wet existed) are allowed
	// too — we just skip hooks/state cleanup for them.
	config, err := loadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	var found *WorktreeState
	foundIndex := -1
	for i, wt := range config.Worktrees {
		absPath, _ := filepath.Abs(wt.Path)
		if absPath == worktreePath {
			found = &config.Worktrees[i]
			foundIndex = i
			break
		}
	}

	var branchName string
	if found != nil {
		branchName = found.Branch
	} else {
		// Untracked worktree: derive branch from HEAD.
		out, err := exec.Command("git", "-C", worktreePath, "rev-parse", "--abbrev-ref", "HEAD").Output()
		if err != nil {
			return fmt.Errorf("failed to resolve branch for %s: %w", worktreePath, err)
		}
		branchName = strings.TrimSpace(string(out))
		if branchName == "" || branchName == "HEAD" {
			return fmt.Errorf("worktree %s is in detached HEAD; resolve manually with `git worktree remove`", worktreePath)
		}
	}

	// Run pre_remove hooks BEFORE changing out of the worktree dir. Skip
	// for untracked and legacy entries (no Ports, Index==0) — they were
	// created before `.wet.yaml` existed or outside wet, so the hook
	// contract is undefined for them.
	//
	// Pool allocations release implicitly: AllocatePools scans existing
	// state entries for claimed items, so once we drop this entry from
	// `state.Worktrees` at the end of this function, its pool picks are
	// automatically available to the next `wet worktree create`. No
	// explicit release step needed.
	// An untracked worktree, or one recorded before ports existed, has no
	// hooks to run — there is no config state to tear down.
	isUnconfigured := found == nil || (found.Index == 0 && len(found.Ports) == 0)
	if !isUnconfigured {
		// Same worktree-first rule as setup: pre_remove tears down what
		// post_create built, so both must read the same config.
		cfg, _, cfgErr := LoadWetConfigForWorktree(worktreePath, repoRoot)
		if cfgErr == nil && len(cfg.Hooks.PreRemove) > 0 {
			ports := make([]ResolvedPort, 0, len(cfg.Ports))
			for _, p := range cfg.Ports {
				if v, ok := found.Ports[p.Name]; ok {
					ports = append(ports, ResolvedPort{Name: p.Name, Value: v})
				}
			}
			data := BuildTemplateData(cfg, found.Branch, found.Index, ports, found.Pools)
			// pre_remove hooks tear down what setup brought up, so they need
			// the same machine overrides setup used — a compose project named
			// after a DEV_HOST from .env.local won't come down without them.
			local, err := LoadEnvLocal(repoRoot)
			if err != nil {
				return err
			}
			hookEnv, err := BuildHookEnv(cfg, data, ports, local)
			if err != nil {
				return fmt.Errorf("build hook env: %w", err)
			}
			for i, body := range cfg.Hooks.PreRemove {
				hookName := fmt.Sprintf("pre_remove[%d]", i)
				rendered, err := RenderHookCommand("hooks."+hookName, body, data)
				if err != nil {
					return fmt.Errorf("hooks.%s: render: %w", hookName, err)
				}
				if err := runWetHook("hooks."+hookName, rendered, worktreePath, hookEnv); err != nil {
					printPopHookFailure(branchName, worktreePath, found.Index, hookName, rendered, err)
					return fmt.Errorf("pre_remove hook failed: %s", hookName)
				}
			}
		} else if cfgErr != nil && cfgErr != ErrConfigMissing {
			return fmt.Errorf("load .wet.yaml: %w", cfgErr)
		}
	}

	if err := os.Chdir(repoRoot); err != nil {
		return fmt.Errorf("failed to change directory: %w", err)
	}

	// Remove the worktree
	gitCmd := exec.Command("git", "worktree", "remove", "--force", worktreePath)
	gitCmd.Stdout = os.Stdout
	gitCmd.Stderr = os.Stderr
	if err := gitCmd.Run(); err != nil {
		return fmt.Errorf("failed to remove worktree: %w", err)
	}

	// Delete the branch
	gitCmd = exec.Command("git", "branch", "-D", branchName)
	gitCmd.Stdout = os.Stdout
	gitCmd.Stderr = os.Stderr
	if err := gitCmd.Run(); err != nil {
		fmt.Printf("  Warning: could not delete branch '%s': %v\n", branchName, err)
	}

	// Remove from config (only if this was a tracked worktree)
	if foundIndex >= 0 {
		config.Worktrees = append(config.Worktrees[:foundIndex], config.Worktrees[foundIndex+1:]...)
		if err := saveConfig(config); err != nil {
			fmt.Printf("  Warning: could not update config: %v\n", err)
		}
	}

	fmt.Printf("  Removed worktree '%s'\n", branchName)

	// Kill tmux pane if inside tmux
	if os.Getenv("TMUX") != "" {
		tmuxCmd := exec.Command("tmux", "kill-pane")
		tmuxCmd.Run() // Best effort, ignore errors
	}

	return nil
}
