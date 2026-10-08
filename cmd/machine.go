package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// machineStateDirEnv points the machine registry somewhere else. Tests set it
// so they never touch the real one; a user can set it to keep two sets of
// projects apart.
const machineStateDirEnv = "WET_STATE_DIR"

const machineStateFileName = "machine.json"

// MachineState is what every wet project on this machine shares.
//
// It holds facts no single project owns: which project folders exist, and the
// emulators in the machine-wide pool. Everything a project claims (slots,
// ports, devices) stays in that project's own .wet-worktrees.json and
// .wet.local.yaml, so it is visible from inside the project. Allocation reads
// every listed project's files while holding the machine lock, which is what
// keeps two projects off the same ports and the same emulator.
type MachineState struct {
	Projects []MachineProject `json:"projects"`
	Devices  []MachineDevice  `json:"devices,omitempty"`
}

// MachineProject is one clone wet has set up: its main checkout's root.
type MachineProject struct {
	Root string `json:"root"`
}

// MachineDevice is one item of the machine-wide emulator pool. LastProject
// and LastSlot remember who used it last, so a slot gets its own emulator
// back instead of whichever one sorts first.
type MachineDevice struct {
	Item        string `json:"item"`
	LastProject string `json:"last_project,omitempty"`
	LastSlot    int    `json:"last_slot,omitempty"`
}

func machineStateDir() string {
	if dir := os.Getenv(machineStateDirEnv); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "wet-state")
	}
	return filepath.Join(home, ".local", "state", "wet")
}

func machineStatePath() string {
	return filepath.Join(machineStateDir(), machineStateFileName)
}

func loadMachineState() (*MachineState, error) {
	data, err := os.ReadFile(machineStatePath())
	if os.IsNotExist(err) {
		return &MachineState{}, nil
	}
	if err != nil {
		return nil, err
	}
	var m MachineState
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", machineStatePath(), err)
	}
	return &m, nil
}

func saveMachineState(m *MachineState) error {
	if err := os.MkdirAll(machineStateDir(), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(machineStatePath(), data)
}

// hasProject reports whether root is listed.
func (m *MachineState) hasProject(root string) bool {
	want := resolvePath(root)
	for _, p := range m.Projects {
		if resolvePath(p.Root) == want {
			return true
		}
	}
	return false
}

// addProject lists root, reporting whether it was new.
func (m *MachineState) addProject(root string) bool {
	if m.hasProject(root) {
		return false
	}
	m.Projects = append(m.Projects, MachineProject{Root: resolvePath(root)})
	sort.Slice(m.Projects, func(i, j int) bool { return m.Projects[i].Root < m.Projects[j].Root })
	return true
}

// otherProjects lists every project root except self.
func (m *MachineState) otherProjects(self string) []string {
	own := resolvePath(self)
	var out []string
	for _, p := range m.Projects {
		if resolvePath(p.Root) != own {
			out = append(out, p.Root)
		}
	}
	return out
}

// projectStatePath is a project's .wet-worktrees.json, given its main root.
func projectStatePath(root string) string {
	return filepath.Join(root, ".wet-worktrees.json")
}

func loadProjectState(root string) (*WorktreeConfig, error) {
	return loadConfigFrom(projectStatePath(root))
}

// projectPorts is every host port a project may bind: its whole cluster band
// (all slots, not only the claimed ones) plus any port its state file records.
// The recorded ports matter for entries written before the band existed, which
// can sit outside it.
func projectPorts(cfg *WetConfig, state *WorktreeConfig) map[int]bool {
	ports := map[int]bool{}
	if cfg != nil {
		for slot := 0; slot < cfg.ClusterSlotCount(); slot++ {
			for _, p := range cfg.ResolvePorts(slot) {
				ports[p.Value] = true
			}
		}
	}
	if state != nil {
		for _, wt := range state.Worktrees {
			for _, v := range wt.Ports {
				ports[v] = true
			}
		}
	}
	return ports
}

// bandPorts is projectPorts for a config alone, at a given cluster.
func bandPorts(cfg *WetConfig, cluster int) map[int]bool {
	ports := map[int]bool{}
	for slot := 0; slot < cfg.ClusterSlotCount(); slot++ {
		for _, p := range cfg.ResolvePortsIn(cluster, slot) {
			ports[p.Value] = true
		}
	}
	return ports
}

// bandClash names a project whose ports overlap the given band.
type bandClash struct {
	Root  string
	Ports []int
}

func (c bandClash) String() string {
	shown := c.Ports
	more := ""
	if len(shown) > 4 {
		more = fmt.Sprintf(" and %d more", len(shown)-4)
		shown = shown[:4]
	}
	parts := make([]string, len(shown))
	for i, p := range shown {
		parts[i] = fmt.Sprint(p)
	}
	return fmt.Sprintf("%s (ports %s%s)", c.Root, strings.Join(parts, ", "), more)
}

// loadProjectPorts reads another project's config and state and returns the
// ports it may bind. A project whose config no longer loads still counts with
// whatever its state file records.
var loadProjectPorts = func(root string) map[int]bool {
	state, _ := loadProjectState(root)
	cfg, err := LoadWetConfig(root)
	if err != nil {
		cfg = nil
	}
	return projectPorts(cfg, state)
}

// bandClashes lists the registered projects other than self whose ports
// overlap band.
func bandClashes(m *MachineState, self string, band map[int]bool) []bandClash {
	var out []bandClash
	for _, root := range m.otherProjects(self) {
		var hit []int
		for port := range loadProjectPorts(root) {
			if band[port] {
				hit = append(hit, port)
			}
		}
		if len(hit) > 0 {
			sort.Ints(hit)
			out = append(out, bandClash{Root: root, Ports: hit})
		}
	}
	return out
}

// claimMachineBand makes sure this project's port band overlaps no other
// listed project, then lists it. Runs under the machine lock.
//
// A project that has never claimed anything and has no recorded
// `port_cluster:` is moved to the lowest free cluster and the move is written
// to its .wet.local.yaml. One that already holds slots is never moved, since
// its stacks run on the old ports; it gets an error naming the other project
// and the command that moves it.
func claimMachineBand(m *MachineState, cfg *WetConfig, root, localRoot string, state *WorktreeConfig) error {
	if len(cfg.Ports) == 0 {
		m.addProject(root)
		return nil
	}
	clashes := bandClashes(m, root, bandPorts(cfg, cfg.PortCluster))
	if len(clashes) == 0 {
		m.addProject(root)
		return nil
	}
	_, recorded, err := overlayPortCluster(localRoot)
	if err != nil {
		return err
	}
	if recorded || len(state.Worktrees) > 0 {
		lines := make([]string, len(clashes))
		for i, c := range clashes {
			lines[i] = "    " + c.String()
		}
		return fmt.Errorf("port cluster %d of %s overlaps another wet project on this machine:\n%s\n"+
			"  Run `wet port-cluster next` here to move this project, then re-run `wet setup`",
			cfg.PortCluster, root, strings.Join(lines, "\n"))
	}
	cluster, err := findFreePortClusterOnMachine(cfg, m, root)
	if err != nil {
		return err
	}
	if err := setLocalPortCluster(localRoot, cluster); err != nil {
		return err
	}
	fmt.Printf("  Port cluster: %d (cluster %d overlaps %s); recorded in %s\n",
		cluster, cfg.PortCluster, clashes[0].Root, filepath.Join(localRoot, wetLocalConfigFileName))
	cfg.PortCluster = cluster
	m.addProject(root)
	return nil
}

// findFreePortClusterOnMachine is findFreePortCluster that also skips every
// band another listed project owns, whether or not its stacks are up.
func findFreePortClusterOnMachine(cfg *WetConfig, m *MachineState, root string) (int, error) {
	taken := map[int]bool{}
	for _, other := range m.otherProjects(root) {
		for port := range loadProjectPorts(other) {
			taken[port] = true
		}
	}
	return findFreePortClusterExcluding(cfg, func(cluster int) bool {
		for port := range bandPorts(cfg, cluster) {
			if taken[port] {
				return false
			}
		}
		return true
	})
}

// registerMachineProject lists root without moving it, refusing when its band
// overlaps a project already listed. It is `wet machine add`'s core.
func registerMachineProject(root string) (bool, error) {
	cfg, err := LoadWetConfig(root)
	if err != nil {
		return false, fmt.Errorf("%s: %w", root, err)
	}
	added := false
	err = withStateLock(func() error {
		m, err := loadMachineState()
		if err != nil {
			return err
		}
		if m.hasProject(root) {
			return nil
		}
		if clashes := bandClashes(m, root, bandPorts(cfg, cfg.PortCluster)); len(clashes) > 0 {
			return fmt.Errorf("not added: port cluster %d of %s overlaps %s; run `wet port-cluster next` in it first",
				cfg.PortCluster, root, clashes[0])
		}
		m.addProject(root)
		added = true
		return saveMachineState(m)
	})
	return added, err
}

// Pool scopes, see WetConfig.PoolScope.
const (
	PoolScopeProject = "project"
	PoolScopeMachine = "machine"
)

// machineAVDPrefix names the shared pool's emulators. They belong to no
// project, so the name carries none.
const machineAVDPrefix = "avd"

func (c *WetConfig) poolIsMachine(name string) bool {
	return c != nil && c.PoolScope[name] == PoolScopeMachine
}

// applyMachinePools replaces each machine-scoped pool's items with the shared
// pool's, so everything that reads cfg.Pools sees the emulators the machine
// actually has. A project's own pools: list is ignored for such a pool.
func applyMachinePools(cfg *WetConfig) error {
	var m *MachineState
	for name := range cfg.Pools {
		if !cfg.poolIsMachine(name) {
			continue
		}
		if m == nil {
			var err error
			if m, err = loadMachineState(); err != nil {
				return err
			}
		}
		items := make([]string, len(m.Devices))
		for i, d := range m.Devices {
			items[i] = d.Item
		}
		cfg.Pools[name] = items
	}
	return nil
}

// machineClaims is every pool item held by any project other than self, read
// from their own state files.
func machineClaims(m *MachineState, self string) map[string]bool {
	claimed := map[string]bool{}
	for _, root := range m.otherProjects(self) {
		state, err := loadProjectState(root)
		if err != nil {
			continue
		}
		for _, wt := range state.Worktrees {
			for _, item := range wt.poolClaimsAll() {
				claimed[item] = true
			}
		}
	}
	return claimed
}

// allocateMachineItem claims an emulator from the machine-wide pool. Runs
// under the machine lock.
//
// The order is: the emulator this project's slot used last, then any free
// one, then a new one. Free means no project holds it, this one included
// (existing carries this project's other claims). The pick is remembered
// against the slot so its next checkout gets the same emulator back.
func allocateMachineItem(cfg *WetConfig, name string, existing []WorktreeState, target poolTarget) (string, error) {
	m, err := loadMachineState()
	if err != nil {
		return "", err
	}
	claimed := machineClaims(m, target.Project)
	for _, ws := range existing {
		for _, item := range ws.poolClaimsAll() {
			claimed[item] = true
		}
	}
	project := resolvePath(target.Project)
	pick := -1
	for i, d := range m.Devices {
		if !claimed[d.Item] && d.LastProject == project && d.LastSlot == target.Slot {
			pick = i
			break
		}
	}
	if pick < 0 {
		for i, d := range m.Devices {
			if !claimed[d.Item] {
				pick = i
				break
			}
		}
	}
	if pick < 0 {
		items := make([]string, len(m.Devices))
		for i, d := range m.Devices {
			items[i] = d.Item
		}
		avd := nextAVDName(machineAVDPrefix, items)
		fmt.Printf("  Growing the shared pool with %s\n", avd)
		m.Devices = append(m.Devices, MachineDevice{Item: emulatorItemPrefix + avd})
		pick = len(m.Devices) - 1
	}
	m.Devices[pick].LastProject = project
	m.Devices[pick].LastSlot = target.Slot
	if err := saveMachineState(m); err != nil {
		return "", err
	}
	item := m.Devices[pick].Item
	if !slices.Contains(cfg.Pools[name], item) {
		cfg.Pools[name] = append(cfg.Pools[name], item)
	}
	return item, nil
}
