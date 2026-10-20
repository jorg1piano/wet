package cmd

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func slotListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Args:    cobra.NoArgs,
		Short:   "List the slots claimed in this repo",
		Long: `List every checkout recorded in .wet-worktrees.json with its slot, branch,
ports, pool items and path. The checkout you are standing in is marked with *.

WARM reads Docker for the slot's COMPOSE_PROJECT_NAME: yes when a container
runs, stopped when containers exist but none runs, no when there are none, and
- when .wet.yaml sets no COMPOSE_PROJECT_NAME.

When .wet.yaml declares ports, a closing line lists the slots in this clone's
band that no checkout claims, with (warm) or (stopped) after a released slot
whose containers remain. Unclaimed means unclaimed in the state file; the host
ports may still be bound by something wet does not track.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			repoRoot, err := getMainRepoRoot()
			if err != nil {
				return err
			}
			current, err := getRepoRoot()
			if err != nil {
				return err
			}
			state, err := loadConfig()
			if err != nil {
				return fmt.Errorf("load wet state: %w", err)
			}
			// The table needs only the state file. The config adds the
			// unclaimed-slot line, so a repo without one still lists.
			cfg, _, err := LoadWetConfigForWorktree(current, repoRoot)
			if err != nil && err != ErrConfigMissing {
				return err
			}
			var containers []dockerContainerPorts
			if cfg.composeProjectNameForSlot(0) != "" {
				containers = listDockerContainerPorts()
			}
			writeSlotList(cmd.OutOrStdout(), state, cfg, containers, repoRoot, current)
			return nil
		},
	}
}

func writeSlotList(out io.Writer, state *WorktreeConfig, cfg *WetConfig, containers []dockerContainerPorts, repoRoot, current string) {
	if len(state.Worktrees) == 0 {
		fmt.Fprintln(out, "  No wet worktrees are claimed.")
		return
	}
	entries := append([]WorktreeState{}, state.Worktrees...)
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Index < entries[j].Index })

	warmth := slotWarmth(cfg, containers)

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  SLOT\tBRANCH\tWARM\tPORTS\tPOOLS\tPATH")
	claimed := map[int]bool{}
	for _, wt := range entries {
		marker := " "
		if samePath(wt.Path, current) {
			marker = "*"
		}
		// Index 0 belongs to the main checkout. A worktree at 0 with no
		// ports never ran setup, and printing 0 would read as main's slot.
		slot := strconv.Itoa(wt.Index)
		warm := "-"
		if wt.Index == 0 && len(wt.Ports) == 0 && !samePath(wt.Path, repoRoot) {
			slot = "-"
		} else {
			claimed[wt.Index] = true
			warm = warmth(wt.Index)
		}
		path := wt.Path
		if _, err := os.Stat(wt.Path); err != nil {
			path += " (missing)"
		}
		branch := wt.Branch
		if wt.PendingReset {
			branch += " (reset pending)"
		}
		fmt.Fprintf(tw, "%s %s\t%s\t%s\t%s\t%s\t%s\n", marker, slot, branch, warm, formatSlotPorts(wt.Ports), formatSlotPools(wt), path)
	}
	_ = tw.Flush()

	if cfg == nil || len(cfg.Ports) == 0 {
		return
	}
	var free []string
	for slot := 1; slot < cfg.ClusterSlotCount(); slot++ {
		if claimed[slot] {
			continue
		}
		label := strconv.Itoa(slot)
		switch warmth(slot) {
		case "yes":
			label += " (warm)"
		case "stopped":
			label += " (stopped)"
		}
		free = append(free, label)
	}
	if len(free) == 0 {
		fmt.Fprintf(out, "\n  Unclaimed: none (cluster_slots: %d)\n", cfg.ClusterSlotCount())
		return
	}
	fmt.Fprintf(out, "\n  Unclaimed: %s\n", strings.Join(free, ", "))
}

// slotWarmth reports whether a slot's Compose project has containers: yes when
// one holds its ports, stopped when they all sit exited, no when there are
// none. Without a COMPOSE_PROJECT_NAME wet cannot tell which containers are
// the slot's, so every slot reads -.
func slotWarmth(cfg *WetConfig, containers []dockerContainerPorts) func(slot int) string {
	running := map[string]bool{}
	present := map[string]bool{}
	for _, ct := range containers {
		if ct.Project == "" {
			continue
		}
		present[ct.Project] = true
		if ct.Holds {
			running[ct.Project] = true
		}
	}
	return func(slot int) string {
		project := cfg.composeProjectNameForSlot(slot)
		switch {
		case project == "":
			return "-"
		case running[project]:
			return "yes"
		case present[project]:
			return "stopped"
		}
		return "no"
	}
}

// formatSlotPorts orders ports by number, which is the order a reader scans
// for a collision in.
func formatSlotPorts(ports map[string]int) string {
	if len(ports) == 0 {
		return "-"
	}
	names := make([]string, 0, len(ports))
	for n := range ports {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if ports[names[i]] != ports[names[j]] {
			return ports[names[i]] < ports[names[j]]
		}
		return names[i] < names[j]
	})
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s=%d", n, ports[n]))
	}
	return strings.Join(parts, " ")
}

func formatSlotPools(wt WorktreeState) string {
	seen := map[string]bool{}
	var names []string
	for n := range wt.Pools {
		seen[n] = true
		names = append(names, n)
	}
	for n := range wt.PoolExtras {
		if !seen[n] {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return "-"
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		if claims := wt.poolClaims(n); len(claims) > 0 {
			parts = append(parts, n+"="+strings.Join(claims, ","))
		}
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, " ")
}
