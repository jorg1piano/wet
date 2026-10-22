package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

func slotGlobalCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "global",
		Short: "Slots across every wet project on this machine",
		Long: `Commands that act on every project in ` + "`wet machine list`" + `, wherever you
run them from.`,
	}
	cmd.AddCommand(slotGlobalListCmd())
	return cmd
}

func slotGlobalListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Args:    cobra.NoArgs,
		Short:   "List the slots claimed in every wet project",
		Long: `Print ` + "`wet slot list`" + ` for every project in ` + "`wet machine list`" + `, one
section per project. The checkout you are standing in is marked with *.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			projects, err := loadGlobalProjects()
			if err != nil {
				return err
			}
			current, _ := getRepoRoot()
			writeGlobalSlotList(cmd.OutOrStdout(), projects, listDockerContainerPorts(), current)
			return nil
		},
	}
}

// globalProject is one registered project as the global commands see it. Err
// says why its config or state could not be read; Gone, that its main
// checkout no longer exists.
type globalProject struct {
	Root  string
	Cfg   *WetConfig
	State *WorktreeConfig
	Gone  bool
	Err   error
}

func loadGlobalProjects() ([]globalProject, error) {
	m, err := loadMachineState()
	if err != nil {
		return nil, err
	}
	out := make([]globalProject, 0, len(m.Projects))
	for _, p := range m.Projects {
		out = append(out, loadGlobalProject(p.Root))
	}
	return out, nil
}

func loadGlobalProject(root string) globalProject {
	p := globalProject{Root: root}
	if _, err := os.Stat(root); os.IsNotExist(err) {
		p.Gone = true
		return p
	}
	state, err := loadProjectState(root)
	if err != nil {
		p.Err = fmt.Errorf("load %s: %w", projectStatePath(root), err)
		return p
	}
	p.State = state
	cfg, _, err := LoadWetConfigForWorktree(root, root)
	if err != nil && err != ErrConfigMissing {
		p.Err = err
	}
	p.Cfg = cfg
	return p
}

func writeGlobalSlotList(out io.Writer, projects []globalProject, containers []dockerContainerPorts, current string) {
	if len(projects) == 0 {
		fmt.Fprintln(out, "  No wet projects on this machine (see `wet machine list`).")
		return
	}
	for i, p := range projects {
		if i > 0 {
			fmt.Fprintln(out)
		}
		fmt.Fprintf(out, "==> %s  %s\n", filepath.Base(p.Root), p.Root)
		switch {
		case p.Gone:
			fmt.Fprintln(out, "  The main checkout is gone.")
			continue
		case p.State == nil:
			fmt.Fprintf(out, "  %v\n", p.Err)
			continue
		case p.Err != nil:
			fmt.Fprintf(out, "  Config: %v\n", p.Err)
		}
		writeSlotList(out, p.State, p.Cfg, containers, p.Root, current)
	}
}
