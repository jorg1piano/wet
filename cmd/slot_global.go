package cmd

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

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
	cmd.AddCommand(slotGlobalDownCmd())
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

func slotGlobalDownCmd() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "down",
		Args:  cobra.NoArgs,
		Short: "Free every worktree's slot and stop its containers and emulator, in every project",
		Long: `For every project in ` + "`wet machine list`" + `, release every claim except the
main checkout's, as ` + "`wet slot release --all`" + ` does, and then stop what the
project's other slots run:

  - containers of every slot's COMPOSE_PROJECT_NAME except slot 0's, stopped
    with docker stop. Containers and volumes stay, so the next ` + "`wet setup`" + `
    on a slot finds its database where it left it.
  - emulators the released claims held, the ones in the project's pools and
    the ones in the machine pool. The main checkouts' emulators keep running.

Worktrees, branches and files are not touched, and no hooks run. Run
` + "`wet setup`" + ` in a worktree to claim a slot for it again.

--dry-run prints what would be released and stopped.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return globalDown(cmd.OutOrStdout(), dryRun)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be released and stopped")
	return cmd
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

// projectDownPlan is what `down` does to one project.
type projectDownPlan struct {
	Root     string
	Released []WorktreeState
	// Compose projects whose running containers get stopped.
	ComposeProjects []string
	// Emulator pool items to stop, and the ones main holds and must keep.
	Emulators []string
	Keep      []string
}

// planProjectDown releases every claim but main's and names the compose
// projects and emulators that belong to the other slots. Main's slot 0 is
// left alone, claim and resources both.
func planProjectDown(p globalProject, containers []dockerComposeContainer) projectDownPlan {
	plan := projectDownPlan{Root: p.Root}
	var main *WorktreeState
	for i := range p.State.Worktrees {
		wt := p.State.Worktrees[i]
		if samePath(wt.Path, p.Root) {
			main = &p.State.Worktrees[i]
			continue
		}
		plan.Released = append(plan.Released, wt)
	}

	keepCompose := map[string]bool{}
	if name := p.Cfg.composeProjectNameForSlot(0); name != "" {
		keepCompose[name] = true
	}
	stop := map[string]bool{}
	if p.Cfg != nil {
		if main != nil {
			if name, err := composeProjectNameForWorktreeState(p.Cfg, p.Root, *main); err == nil && name != "" {
				keepCompose[name] = true
			}
		}
		for slot := 1; slot <= maxContainerSlotScan(p.State, containers); slot++ {
			if name := p.Cfg.composeProjectNameForSlot(slot); name != "" {
				stop[name] = true
			}
		}
		for _, wt := range plan.Released {
			if name, err := composeProjectNameForWorktreeState(p.Cfg, p.Root, wt); err == nil && name != "" {
				stop[name] = true
			}
		}
	}
	for name := range stop {
		if !keepCompose[name] {
			plan.ComposeProjects = append(plan.ComposeProjects, name)
		}
	}
	sort.Strings(plan.ComposeProjects)

	if main != nil {
		plan.Keep = main.poolClaimsAll()
	}
	items := map[string]bool{}
	for _, wt := range plan.Released {
		for _, item := range wt.poolClaimsAll() {
			items[item] = true
		}
	}
	if p.Cfg != nil {
		for _, pool := range p.Cfg.Pools {
			for _, item := range pool {
				items[item] = true
			}
		}
	}
	for item := range items {
		if strings.HasPrefix(item, emulatorItemPrefix) {
			plan.Emulators = append(plan.Emulators, item)
		}
	}
	sort.Strings(plan.Emulators)
	return plan
}

// containersToStop picks the containers of the given compose projects that
// are up. A restarting one counts: its restart policy would bring it back.
func containersToStop(containers []dockerComposeContainer, projects []string) []dockerComposeContainer {
	want := map[string]bool{}
	for _, p := range projects {
		want[p] = true
	}
	var out []dockerComposeContainer
	for _, c := range containers {
		if want[c.Project] && dockerContainerHolds(c.State, "") {
			out = append(out, c)
		}
	}
	return out
}

// emulatorsToStop maps the running emulators among items to their serials,
// skipping any item in keep.
func emulatorsToStop(items, keep []string, running map[string]string) map[string]string {
	kept := map[string]bool{}
	for _, item := range keep {
		kept[item] = true
	}
	out := map[string]string{}
	for _, item := range items {
		if kept[item] {
			continue
		}
		avd := strings.TrimPrefix(item, emulatorItemPrefix)
		if serial, ok := running[avd]; ok {
			out[item] = serial
		}
	}
	return out
}

// runningEmulatorAVDs maps each running emulator's AVD name to its serial. It
// is a seam so the unit suite never reaches a device.
var runningEmulatorAVDs = func() map[string]string {
	out := map[string]string{}
	for _, serial := range attachedEmulatorSerials() {
		body, err := exec.Command("adb", "-s", serial, "emu", "avd", "name").Output()
		if err != nil {
			continue
		}
		if name := strings.TrimSpace(strings.SplitN(string(body), "\n", 2)[0]); name != "" {
			out[name] = serial
		}
	}
	return out
}

// dockerStopContainers is a seam for the same reason.
var dockerStopContainers = func(containers []dockerComposeContainer) error {
	if len(containers) == 0 {
		return nil
	}
	args := []string{"stop"}
	for _, c := range containers {
		args = append(args, c.ID)
	}
	cmd := exec.Command("docker", args...)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func globalDown(out io.Writer, dryRun bool) error {
	projects, err := loadGlobalProjects()
	if err != nil {
		return err
	}
	if len(projects) == 0 {
		fmt.Fprintln(out, "  No wet projects on this machine (see `wet machine list`).")
		return nil
	}
	containers, err := listDockerComposeContainers()
	if err != nil {
		fmt.Fprintf(out, "  Docker: %v; no containers will be stopped\n", err)
		containers = nil
	}

	var plans []projectDownPlan
	var keepItems, emulatorItems []string
	for _, p := range projects {
		if p.Gone || p.State == nil {
			continue
		}
		plan := planProjectDown(p, containers)
		plans = append(plans, plan)
		keepItems = append(keepItems, plan.Keep...)
		emulatorItems = append(emulatorItems, plan.Emulators...)
	}
	if m, err := loadMachineState(); err == nil {
		for _, d := range m.Devices {
			emulatorItems = append(emulatorItems, d.Item)
		}
	}

	if !dryRun {
		if err := releaseGlobalClaims(plans); err != nil {
			return err
		}
	}

	var failed []string
	for _, plan := range plans {
		fmt.Fprintf(out, "==> %s  %s\n", filepath.Base(plan.Root), plan.Root)
		verb := "Released"
		if dryRun {
			verb = "Would release"
		}
		for _, wt := range plan.Released {
			fmt.Fprintf(out, "  %s slot %d (%s)\n", verb, wt.Index, wt.Branch)
		}
		stop := containersToStop(containers, plan.ComposeProjects)
		for _, name := range composeProjectsOf(stop) {
			if dryRun {
				fmt.Fprintf(out, "  Would stop %s\n", name)
			} else {
				fmt.Fprintf(out, "  Stopping %s\n", name)
			}
		}
		if len(plan.Released) == 0 && len(stop) == 0 {
			fmt.Fprintln(out, "  Nothing to release or stop besides the main checkout.")
		}
		if !dryRun {
			if err := dockerStopContainers(stop); err != nil {
				fmt.Fprintf(out, "  docker stop failed: %v\n", err)
				failed = append(failed, filepath.Base(plan.Root))
			}
		}
	}

	emulators := emulatorsToStop(uniqueSorted(emulatorItems), keepItems, runningEmulatorAVDs())
	if len(emulators) > 0 {
		fmt.Fprintln(out, "==> emulators")
		for _, item := range sortedKeys(emulators) {
			serial := emulators[item]
			if dryRun {
				fmt.Fprintf(out, "  Would stop %s (%s)\n", item, serial)
				continue
			}
			fmt.Fprintf(out, "  Stopping %s (%s)\n", item, serial)
			if err := killEmulator(serial); err != nil {
				fmt.Fprintf(out, "    %v\n", err)
				failed = append(failed, item)
			}
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("could not stop everything: %s", strings.Join(failed, ", "))
	}
	return nil
}

// releaseGlobalClaims drops every planned claim under the machine lock,
// rereading each state file so a setup that ran since planning keeps its
// claim.
func releaseGlobalClaims(plans []projectDownPlan) error {
	return withStateLock(func() error {
		for _, plan := range plans {
			if len(plan.Released) == 0 {
				continue
			}
			state, err := loadProjectState(plan.Root)
			if err != nil {
				return fmt.Errorf("load %s: %w", projectStatePath(plan.Root), err)
			}
			release := map[string]bool{}
			for _, wt := range plan.Released {
				release[resolvePath(wt.Path)] = true
			}
			kept := []WorktreeState{}
			for _, wt := range state.Worktrees {
				if !release[resolvePath(wt.Path)] {
					kept = append(kept, wt)
				}
			}
			state.Worktrees = kept
			if err := saveConfigTo(projectStatePath(plan.Root), state); err != nil {
				return err
			}
		}
		return nil
	})
}

func composeProjectsOf(containers []dockerComposeContainer) []string {
	seen := map[string]bool{}
	for _, c := range containers {
		seen[c.Project] = true
	}
	return sortedKeys(seen)
}

func uniqueSorted(items []string) []string {
	seen := map[string]bool{}
	for _, item := range items {
		seen[item] = true
	}
	return sortedKeys(seen)
}
