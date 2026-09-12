package cmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

var worktreeCleanupDryRun bool

var worktreeCleanupCmd = &cobra.Command{
	Use:     "cleanup",
	Args:    cobra.NoArgs,
	Aliases: []string{"clean"},
	Short:   "Clear all tracked worktree allocations",
	Long: `Clear every entry from .wet-worktrees.json without removing git
worktrees, deleting branches, or running lifecycle hooks.

This is the panic button for stale allocation state: it releases all tracked
slots, ports, pool items, and extra mobile devices so the next wet setup can
allocate from an empty state file. It does not stop anything already listening
on those ports; wet setup will still skip ports that are actually bound.`,
	RunE: runWorktreeCleanup,
}

func init() {
	worktreeCleanupCmd.Flags().BoolVar(&worktreeCleanupDryRun, "dry-run", false, "show what would be cleared without writing .wet-worktrees.json")
	worktreeCmd.AddCommand(worktreeCleanupCmd)
}

func runWorktreeCleanup(cmd *cobra.Command, args []string) error {
	return cleanupWorktreeAllocations(cmd.OutOrStdout(), worktreeCleanupDryRun)
}

func cleanupWorktreeAllocations(out io.Writer, dryRun bool) error {
	statePath := configFilePath()
	removed := 0

	if err := withStateLock(func() error {
		state, err := loadConfig()
		if err != nil {
			return fmt.Errorf("failed to load worktree state: %w", err)
		}
		removed = len(state.Worktrees)
		if removed == 0 || dryRun {
			return nil
		}
		state.Worktrees = []WorktreeState{}
		return saveConfig(state)
	}); err != nil {
		return err
	}

	if removed == 0 {
		fmt.Fprintf(out, "  No tracked worktree allocations in %s\n", statePath)
		return nil
	}
	if dryRun {
		fmt.Fprintf(out, "  Would clear %d tracked worktree allocation(s) from %s\n", removed, statePath)
		return nil
	}

	fmt.Fprintf(out, "  Cleared %d tracked worktree allocation(s) from %s\n", removed, statePath)
	fmt.Fprintln(out, "  Worktree directories and branches were not removed; running stacks were not stopped.")
	fmt.Fprintln(out, "  Re-run `wet setup` in any checkout that should claim fresh ports/pools.")
	return nil
}
