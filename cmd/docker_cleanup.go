package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

var dockerCleanupDryRun bool
var dockerCleanupVerbose bool

var dockerCleanupCmd = &cobra.Command{
	Use:   "cleanup",
	Args:  cobra.NoArgs,
	Short: "Remove stale Docker Compose containers not owned by active panes or the slot pool",
	Long: `Remove stale Docker Compose containers for this wet repo.

wet keeps Compose projects that belong to a checkout currently referenced by a
Herdr pane's current working directory, falling back to tmux panes when Herdr is
not available. When slot_pool is enabled, wet also keeps Compose projects that
match the repo's reusable slot project names, because those containers are the
warm pool. Everything else with a Compose project label is stale: cleanup runs
docker compose down for that project and then docker rm -f for any listed
containers still present.

Always run --dry-run first if you want to inspect the decision.`,
	RunE: runDockerCleanup,
}

func init() {
	dockerCleanupCmd.Flags().BoolVar(&dockerCleanupDryRun, "dry-run", false, "show stale Compose projects without removing containers")
	dockerCleanupCmd.Flags().BoolVarP(&dockerCleanupVerbose, "verbose", "v", false, "show kept Compose projects too")
	dockerCmd.AddCommand(dockerCleanupCmd)
}

type dockerComposeContainer struct {
	ID         string
	Name       string
	Project    string
	WorkingDir string
	// State is Docker's: running, restarting, exited, ...
	State string
}

type dockerCleanupProject struct {
	Project    string
	Containers []dockerComposeContainer
}

func runDockerCleanup(cmd *cobra.Command, args []string) error {
	return cleanupDockerContainers(cmd.OutOrStdout(), dockerCleanupDryRun)
}

func cleanupDockerContainers(out io.Writer, dryRun bool) error {
	repoRoot, err := getMainRepoRoot()
	if err != nil {
		return fmt.Errorf("not in a git repository: %w", err)
	}
	cfg, err := LoadWetConfig(repoRoot)
	if err != nil {
		return err
	}
	state, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load wet state: %w", err)
	}
	panes, err := paneCurrentPaths()
	if err != nil {
		return err
	}
	containers, err := listDockerComposeContainers()
	if err != nil {
		return err
	}
	keep, err := dockerCleanupKeepProjects(cfg, repoRoot, state, panes, containers)
	if err != nil {
		return err
	}
	stale := planDockerCleanup(cfg, containers, keep)
	kept := sortedKeepProjects(keep, containers)
	if len(stale) == 0 {
		fmt.Fprintln(out, "Docker cleanup plan")
		fmt.Fprintln(out, "  Stale: none")
		fmt.Fprintf(out, "  Kept:  %d Compose project(s) referenced by panes or slot_pool\n", len(kept))
		if dockerCleanupVerbose {
			printDockerProjectList(out, "\nKept projects", kept)
		}
		return nil
	}
	if dryRun {
		fmt.Fprintln(out, "Docker cleanup plan")
		fmt.Fprintf(out, "  Stale: %d Compose project(s)\n", len(stale))
		fmt.Fprintf(out, "  Kept:  %d Compose project(s) referenced by panes or slot_pool\n", len(kept))
		printDockerCleanupTable(out, "\nWould remove", stale)
		if dockerCleanupVerbose {
			printDockerProjectList(out, "\nKept projects", kept)
		} else {
			fmt.Fprintln(out, "\nPass --verbose to list kept projects.")
		}
		return nil
	}
	for _, p := range stale {
		fmt.Fprintf(out, "  Removing Compose project %s (%d container(s))\n", p.Project, len(p.Containers))
		if err := dockerComposeDown(p.Project); err != nil {
			fmt.Fprintf(out, "    docker compose down failed: %v\n", err)
		}
		if err := dockerRemoveContainers(p.Containers); err != nil {
			return err
		}
	}
	return nil
}

func paneCurrentPaths() ([]string, error) {
	paths, err := herdrPaneCurrentPaths()
	if err == nil {
		return paths, nil
	}
	herdrErr := err
	paths, err = tmuxPaneCurrentPaths()
	if err == nil {
		return paths, nil
	}
	return nil, fmt.Errorf("list panes: Herdr failed (%v); tmux failed (%w)", herdrErr, err)
}

type herdrPaneList struct {
	Result struct {
		Panes []struct {
			CWD           string `json:"cwd"`
			ForegroundCWD string `json:"foreground_cwd"`
		} `json:"panes"`
	} `json:"result"`
}

func herdrPaneCurrentPaths() ([]string, error) {
	body, err := exec.Command("herdr", "pane", "list").Output()
	if err != nil {
		return nil, fmt.Errorf("herdr pane list: %w", err)
	}
	var parsed herdrPaneList
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parse herdr pane list: %w", err)
	}
	seen := map[string]bool{}
	var paths []string
	for _, pane := range parsed.Result.Panes {
		for _, p := range []string{pane.ForegroundCWD, pane.CWD} {
			p = strings.TrimSpace(p)
			if p == "" || seen[p] {
				continue
			}
			seen[p] = true
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("herdr pane list returned no pane paths")
	}
	return paths, nil
}

func tmuxPaneCurrentPaths() ([]string, error) {
	body, err := exec.Command("tmux", "list-panes", "-a", "-F", "#{pane_current_path}").Output()
	if err != nil {
		return nil, fmt.Errorf("tmux list-panes: %w", err)
	}
	paths := uniqueNonEmptyLines(string(body))
	if len(paths) == 0 {
		return nil, fmt.Errorf("tmux list-panes returned no pane paths")
	}
	return paths, nil
}

func uniqueNonEmptyLines(body string) []string {
	var paths []string
	seen := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		p := strings.TrimSpace(line)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		paths = append(paths, p)
	}
	return paths
}

func listDockerComposeContainers() ([]dockerComposeContainer, error) {
	body, err := exec.Command("docker", "ps", "-a", "--filter", "label=com.docker.compose.project", "--format", `{{.ID}}\t{{.Names}}\t{{.Label "com.docker.compose.project"}}\t{{.Label "com.docker.compose.project.working_dir"}}\t{{.State}}`).Output()
	if err != nil {
		return nil, fmt.Errorf("list docker containers: %w", err)
	}
	var out []dockerComposeContainer
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 5 || fields[0] == "" || fields[2] == "" {
			continue
		}
		out = append(out, dockerComposeContainer{ID: fields[0], Name: fields[1], Project: fields[2], WorkingDir: fields[3], State: fields[4]})
	}
	return out, nil
}

func dockerCleanupKeepProjects(cfg *WetConfig, repoRoot string, state *WorktreeConfig, panePaths []string, containers []dockerComposeContainer) (map[string]bool, error) {
	keep := map[string]bool{}
	gitRoots := map[string]string{}
	for _, pane := range panePaths {
		for _, c := range containers {
			if composeProjectBelongsToConfig(cfg, c.Project) && paneReferencesComposeWorkingDir(pane, c.WorkingDir, gitRoots) {
				keep[c.Project] = true
			}
		}
		for i := range state.Worktrees {
			wt := state.Worktrees[i]
			if pathInside(pane, wt.Path) {
				project, err := composeProjectNameForWorktreeState(cfg, repoRoot, wt)
				if err != nil {
					return nil, err
				}
				if project != "" {
					keep[project] = true
				}
			}
		}
	}
	if cfg.SlotPool {
		maxSlot := maxContainerSlotScan(state, containers)
		for slot := 1; slot <= maxSlot; slot++ {
			if project := cfg.composeProjectNameForSlot(slot); project != "" {
				keep[project] = true
			}
		}
	}
	return keep, nil
}

func composeProjectNameForWorktreeState(cfg *WetConfig, repoRoot string, wt WorktreeState) (string, error) {
	ports := make([]ResolvedPort, 0, len(wt.Ports))
	for _, p := range cfg.Ports {
		if v, ok := wt.Ports[p.Name]; ok {
			ports = append(ports, ResolvedPort{Name: p.Name, Value: v})
		}
	}
	data := allocationTemplateData(cfg, worktreeAllocation{Branch: wt.Branch, Slot: wt.Index, Ports: ports, Pools: wt.Pools, Extras: wt.PoolExtras})
	local, err := LoadEnvLocalForTree(wt.Path, repoRoot)
	if err != nil {
		return "", err
	}
	if _, err := ResolveEnvValues(cfg, data, local); err != nil {
		return "", err
	}
	project, _ := data["COMPOSE_PROJECT_NAME"].(string)
	return project, nil
}

func pathInside(path, dir string) bool {
	if path == "" || dir == "" {
		return false
	}
	if samePath(path, dir) {
		return true
	}
	return containsPath(dir, path)
}

func paneReferencesComposeWorkingDir(pane, workingDir string, gitRoots map[string]string) bool {
	if pane == "" || workingDir == "" {
		return false
	}
	// A shell inside the Compose working dir is plainly active.
	if pathInside(pane, workingDir) {
		return true
	}
	// A shell at the checkout root should also keep a compose file in a subdir
	// such as web_elixir/. A shell at a broad parent like ~/github/devda should
	// not keep every worktree below it, so require both paths to resolve to the
	// same git toplevel.
	return containsPath(pane, workingDir) && sameGitRoot(pane, workingDir, gitRoots)
}

func sameGitRoot(a, b string, cache map[string]string) bool {
	ra, okA := gitRootForPath(a, cache)
	rb, okB := gitRootForPath(b, cache)
	return okA && okB && samePath(ra, rb)
}

func gitRootForPath(path string, cache map[string]string) (string, bool) {
	if v, ok := cache[path]; ok {
		return v, v != ""
	}
	out, err := exec.Command("git", "-C", path, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		cache[path] = ""
		return "", false
	}
	root := strings.TrimSpace(string(out))
	cache[path] = root
	return root, root != ""
}

func composeProjectBelongsToConfig(cfg *WetConfig, project string) bool {
	if cfg == nil || cfg.ProjectName == "" || project == "" {
		return false
	}
	return project == cfg.ProjectName || strings.HasPrefix(project, cfg.ProjectName+"-")
}

func maxContainerSlotScan(state *WorktreeConfig, containers []dockerComposeContainer) int {
	maxSlot := 20
	for _, wt := range state.Worktrees {
		if wt.Index > maxSlot {
			maxSlot = wt.Index
		}
	}
	if len(containers) > maxSlot {
		maxSlot = len(containers)
	}
	return maxSlot
}

func planDockerCleanup(cfg *WetConfig, containers []dockerComposeContainer, keep map[string]bool) []dockerCleanupProject {
	byProject := map[string][]dockerComposeContainer{}
	for _, c := range containers {
		if c.Project == "" || keep[c.Project] || !composeProjectBelongsToConfig(cfg, c.Project) {
			continue
		}
		byProject[c.Project] = append(byProject[c.Project], c)
	}
	projects := make([]string, 0, len(byProject))
	for p := range byProject {
		projects = append(projects, p)
	}
	sort.Strings(projects)
	out := make([]dockerCleanupProject, 0, len(projects))
	for _, p := range projects {
		out = append(out, dockerCleanupProject{Project: p, Containers: byProject[p]})
	}
	return out
}

func printDockerCleanupTable(out io.Writer, title string, stale []dockerCleanupProject) {
	fmt.Fprintln(out, title)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  PROJECT\tCONTAINERS\tACTION")
	for _, p := range stale {
		fmt.Fprintf(tw, "  %s\t%d\tcompose down + rm -f\n", p.Project, len(p.Containers))
	}
	_ = tw.Flush()
	for _, p := range stale {
		fmt.Fprintf(out, "\n  %s containers:\n", p.Project)
		for _, name := range containerNames(p.Containers) {
			fmt.Fprintf(out, "    - %s\n", name)
		}
	}
}

func printDockerProjectList(out io.Writer, title string, projects []string) {
	fmt.Fprintln(out, title)
	for _, project := range projects {
		fmt.Fprintf(out, "  - %s\n", project)
	}
}

func dockerComposeDown(project string) error {
	cmd := exec.Command("docker", "compose", "-p", project, "down", "--remove-orphans")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func dockerRemoveContainers(containers []dockerComposeContainer) error {
	ids := make([]string, 0, len(containers))
	for _, c := range containers {
		ids = append(ids, c.ID)
	}
	if len(ids) == 0 {
		return nil
	}
	args := append([]string{"rm", "-f"}, ids...)
	cmd := exec.Command("docker", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func containerNames(containers []dockerComposeContainer) []string {
	names := make([]string, 0, len(containers))
	for _, c := range containers {
		name := c.Name
		if name == "" {
			name = c.ID
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedKeepProjects(keep map[string]bool, containers []dockerComposeContainer) []string {
	present := map[string]bool{}
	for _, c := range containers {
		if keep[c.Project] {
			present[c.Project] = true
		}
	}
	projects := make([]string, 0, len(present))
	for p := range present {
		projects = append(projects, p)
	}
	sort.Strings(projects)
	if len(projects) == 0 {
		return []string{"none"}
	}
	return projects
}
