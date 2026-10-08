package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

var machineCmd = &cobra.Command{
	Use:   "machine",
	Short: "Projects and emulators shared by every wet project on this machine",
	Long: `Every wet project on this machine allocates under one lock and checks the
others' ports before claiming its own. The list of projects and the shared
emulator pool live in ` + "`~/.local/state/wet/machine.json`" + ` (or $WET_STATE_DIR).
What each project claims stays in its own .wet-worktrees.json.

` + "`wet setup`" + ` adds a project the first time it runs there. ` + "`wet machine add`" + `
lists a project without running anything in it, so its ports are protected
before its next setup.`,
}

func init() {
	machineCmd.AddCommand(&cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Args:    cobra.NoArgs,
		Short:   "List every wet project and shared emulator on this machine",
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := loadMachineState()
			if err != nil {
				return err
			}
			writeMachineList(cmd.OutOrStdout(), m)
			return nil
		},
	})
	machineCmd.AddCommand(&cobra.Command{
		Use:   "add PATH...",
		Args:  cobra.MinimumNArgs(1),
		Short: "List a project's main checkout so other projects keep off its ports",
		RunE: func(cmd *cobra.Command, args []string) error {
			var failed []string
			for _, arg := range args {
				root, err := filepath.Abs(arg)
				if err != nil {
					return err
				}
				added, err := registerMachineProject(root)
				switch {
				case err != nil:
					fmt.Fprintf(os.Stderr, "  %v\n", err)
					failed = append(failed, root)
				case added:
					fmt.Printf("  Added %s\n", root)
				default:
					fmt.Printf("  Already listed: %s\n", root)
				}
			}
			if len(failed) > 0 {
				return fmt.Errorf("not added: %s", strings.Join(failed, ", "))
			}
			return nil
		},
	})
	rootCmd.AddCommand(machineCmd)
}

func writeMachineList(out io.Writer, m *MachineState) {
	if len(m.Projects) == 0 {
		fmt.Fprintln(out, "  No wet projects are listed on this machine yet.")
		return
	}
	holders := map[string]string{}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  PROJECT\tCLUSTER\tPORTS\tSLOTS")
	for _, p := range m.Projects {
		state, _ := loadProjectState(p.Root)
		cluster, ports := "-", "-"
		if cfg, err := LoadWetConfig(p.Root); err == nil && len(cfg.Ports) > 0 {
			cluster = fmt.Sprint(cfg.PortCluster)
			first := cfg.ResolvePorts(0)[0]
			last := cfg.ResolvePorts(cfg.ClusterSlotCount() - 1)[0]
			ports = fmt.Sprintf("%s %d..%d", first.Name, first.Value, last.Value)
		} else if err != nil {
			ports = "config: " + err.Error()
		}
		var slots []string
		if state != nil {
			entries := append([]WorktreeState{}, state.Worktrees...)
			sort.Slice(entries, func(i, j int) bool { return entries[i].Index < entries[j].Index })
			for _, wt := range entries {
				slots = append(slots, fmt.Sprintf("%d:%s", wt.Index, wt.Branch))
				for _, items := range wt.Pools {
					holders[items] = fmt.Sprintf("%s slot %d", filepath.Base(p.Root), wt.Index)
				}
				for _, extra := range wt.PoolExtras {
					for _, item := range extra {
						holders[item] = fmt.Sprintf("%s slot %d", filepath.Base(p.Root), wt.Index)
					}
				}
			}
		}
		if _, err := os.Stat(p.Root); err != nil {
			ports = "(missing)"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", p.Root, cluster, ports, strings.Join(slots, " "))
	}
	tw.Flush()
	if len(m.Devices) == 0 {
		return
	}
	fmt.Fprintln(out)
	tw = tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  SHARED DEVICE\tHELD BY\tLAST USED BY")
	for _, d := range m.Devices {
		held := holders[d.Item]
		if held == "" {
			held = "-"
		}
		last := "-"
		if d.LastProject != "" {
			last = fmt.Sprintf("%s slot %d", filepath.Base(d.LastProject), d.LastSlot)
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", d.Item, held, last)
	}
	tw.Flush()
}
