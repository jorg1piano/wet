package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// claimIsDead reports whether a state entry belongs to a checkout that no
// longer exists: its directory is gone, or what is left there is no longer a
// git checkout of its own (a half-removed worktree). The main checkout is
// never dead; it is the project.
func claimIsDead(root string, wt WorktreeState) bool {
	if wt.Path == "" || resolvePath(wt.Path) == resolvePath(root) {
		return false
	}
	info, err := os.Stat(wt.Path)
	if os.IsNotExist(err) {
		return true
	}
	if err != nil || !info.IsDir() {
		return false
	}
	out, err := exec.Command("git", "-C", wt.Path, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return true
	}
	return resolvePath(strings.TrimSpace(string(out))) != resolvePath(wt.Path)
}

// sweepState drops every dead entry from state and returns what it dropped.
func sweepState(root string, state *WorktreeConfig) []WorktreeState {
	var kept, dropped []WorktreeState
	for _, wt := range state.Worktrees {
		if claimIsDead(root, wt) {
			dropped = append(dropped, wt)
		} else {
			kept = append(kept, wt)
		}
	}
	if len(dropped) > 0 {
		if kept == nil {
			kept = []WorktreeState{}
		}
		state.Worktrees = kept
	}
	return dropped
}

// sweepDeadClaims releases the claims of checkouts that were removed without
// `wet worktree pop`, in this project (ownState, saved by the caller) and in
// every other listed project (saved here). It also drops listed projects
// whose main checkout is gone. Runs under the machine lock.
//
// Nothing is torn down: pre_remove hooks need the checkout, which is gone.
// Warm resources such as a slot's DB stay for the next checkout on that slot.
func sweepDeadClaims(m *MachineState, ownRoot string, ownState *WorktreeConfig) []string {
	var released []string
	report := func(root string, dropped []WorktreeState) {
		for _, wt := range dropped {
			released = append(released, fmt.Sprintf("%s slot %d (%s): %s is gone",
				filepath.Base(root), wt.Index, wt.Branch, wt.Path))
		}
	}
	report(ownRoot, sweepState(ownRoot, ownState))

	var projects []MachineProject
	for _, p := range m.Projects {
		if resolvePath(p.Root) == resolvePath(ownRoot) {
			projects = append(projects, p)
			continue
		}
		if _, err := os.Stat(p.Root); os.IsNotExist(err) {
			released = append(released, fmt.Sprintf("project %s: its checkout is gone", p.Root))
			continue
		}
		projects = append(projects, p)
		state, err := loadProjectState(p.Root)
		if err != nil {
			continue
		}
		if dropped := sweepState(p.Root, state); len(dropped) > 0 {
			if err := saveConfigTo(projectStatePath(p.Root), state); err == nil {
				report(p.Root, dropped)
			}
		}
	}
	m.Projects = projects
	return released
}

func printReleased(released []string) {
	for _, line := range released {
		fmt.Printf("  Released stale claim: %s\n", line)
	}
}

func slotReleaseCmd() *cobra.Command {
	var force bool
	var all bool
	cmd := &cobra.Command{
		Use:   "release [PATH]",
		Args:  cobra.MaximumNArgs(1),
		Short: "Free slot claims without removing the checkouts",
		Long: `Drop PATH's entry from .wet-worktrees.json, freeing its slot, ports and
devices for the next ` + "`wet setup`" + `. No hooks run and nothing is stopped:
warm resources such as the slot's DB stay for the next checkout on that slot.

` + "`wet setup`" + ` already releases claims whose checkout is gone. Use this when you
want one released now. A checkout that still exists is refused, since
` + "`wet worktree pop`" + ` runs its pre_remove hooks; --force releases it anyway.

With --all, wet releases every claim in this project except the main
checkout's slot 0, whether or not the checkout still exists. Worktrees and
branches stay as they are; run ` + "`wet setup`" + ` in one to claim a slot again.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			repoRoot, err := getMainRepoRoot()
			if err != nil {
				return err
			}
			if all {
				if len(args) > 0 {
					return fmt.Errorf("pass PATH or --all, not both")
				}
				return releaseAllClaims(repoRoot)
			}
			if len(args) == 0 {
				return fmt.Errorf("pass the PATH of a checkout to release, or --all")
			}
			target, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			return releaseClaim(repoRoot, target, force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "release the claim even though the checkout still exists")
	cmd.Flags().BoolVar(&all, "all", false, "release every claim except the main checkout's")
	return cmd
}

// releaseAllClaims drops every entry but the main checkout's from this
// project's state. Like releaseClaim with --force it runs no hooks and leaves
// the checkouts, their branches and the slots' warm resources alone.
func releaseAllClaims(repoRoot string) error {
	var released []WorktreeState
	err := withStateLock(func() error {
		state, err := loadConfig()
		if err != nil {
			return fmt.Errorf("load wet state: %w", err)
		}
		kept := []WorktreeState{}
		for _, wt := range state.Worktrees {
			if resolvePath(wt.Path) == resolvePath(repoRoot) {
				kept = append(kept, wt)
			} else {
				released = append(released, wt)
			}
		}
		if len(released) == 0 {
			return nil
		}
		state.Worktrees = kept
		return saveConfig(state)
	})
	if err != nil {
		return err
	}
	if len(released) == 0 {
		fmt.Println("  No claims to release besides the main checkout's.")
		return nil
	}
	for _, wt := range released {
		printReleasedClaim(wt)
	}
	return nil
}

func releaseClaim(repoRoot, target string, force bool) error {
	if resolvePath(target) == resolvePath(repoRoot) {
		return fmt.Errorf("%s is the main checkout; its slot 0 is not released", target)
	}
	var released WorktreeState
	err := withStateLock(func() error {
		state, err := loadConfig()
		if err != nil {
			return fmt.Errorf("load wet state: %w", err)
		}
		own := worktreeStateForPath(state, target)
		if own == nil {
			return fmt.Errorf("no claim recorded for %s (see `wet slot list`)", target)
		}
		if !force && !claimIsDead(repoRoot, *own) {
			return fmt.Errorf("%s is still a checkout; run `wet worktree pop` inside it, or pass --force", target)
		}
		released = *own
		kept := []WorktreeState{}
		for _, wt := range state.Worktrees {
			if resolvePath(wt.Path) != resolvePath(target) {
				kept = append(kept, wt)
			}
		}
		state.Worktrees = kept
		return saveConfig(state)
	})
	if err != nil {
		return err
	}
	printReleasedClaim(released)
	return nil
}

func printReleasedClaim(wt WorktreeState) {
	fmt.Printf("  Released slot %d (%s)\n", wt.Index, wt.Branch)
	for _, items := range wt.poolClaimsAll() {
		fmt.Printf("  Released device %s\n", items)
	}
}

// poolClaimsAll lists every pool item this entry holds, primaries and extras.
func (ws WorktreeState) poolClaimsAll() []string {
	var out []string
	for pool := range ws.Pools {
		out = append(out, ws.poolClaims(pool)...)
	}
	for pool, extras := range ws.PoolExtras {
		if _, primary := ws.Pools[pool]; !primary {
			out = append(out, extras...)
		}
	}
	return out
}
