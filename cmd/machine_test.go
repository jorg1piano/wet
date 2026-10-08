package cmd

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateMachine gives a test its own empty machine registry.
func isolateMachine(t *testing.T) {
	t.Helper()
	t.Setenv(machineStateDirEnv, t.TempDir())
}

// stubProjectPorts answers loadProjectPorts from a fixed table, so a test
// can list "other projects" without building their repos.
func stubProjectPorts(t *testing.T, ports map[string][]int) {
	t.Helper()
	prev := loadProjectPorts
	t.Cleanup(func() { loadProjectPorts = prev })
	loadProjectPorts = func(root string) map[int]bool {
		out := map[int]bool{}
		for _, p := range ports[root] {
			out[p] = true
		}
		return out
	}
}

// A high base keeps the probe in findFreePortCluster off ports a developer's
// machine is likely to have bound.
func machineTestConfig() *WetConfig {
	return &WetConfig{
		ProjectName:  "app",
		PortStride:   10,
		ClusterSlots: DefaultClusterSlots,
		Ports:        []PortConfig{{Name: "APP_PORT", Base: 47000, Stride: 10}},
	}
}

func TestMachineStateRoundTripsAndListsEachProjectOnce(t *testing.T) {
	isolateMachine(t)
	m, err := loadMachineState()
	if err != nil || len(m.Projects) != 0 {
		t.Fatalf("fresh machine state = %+v, %v", m, err)
	}
	root := t.TempDir()
	if !m.addProject(root) || m.addProject(root) {
		t.Fatal("addProject must report new once and then already listed")
	}
	if err := saveMachineState(m); err != nil {
		t.Fatal(err)
	}
	back, err := loadMachineState()
	if err != nil || len(back.Projects) != 1 || !back.hasProject(root) {
		t.Fatalf("reloaded = %+v, %v", back, err)
	}
}

// A project that never claimed anything is moved off a band another project
// owns, and the move lands in its .wet.local.yaml where the project shows it.
func TestClaimMachineBandMovesAFreshProjectOffAnOwnedBand(t *testing.T) {
	isolateMachine(t)
	stubProjectPorts(t, map[string][]int{"/other": {47000, 47050}})
	m := &MachineState{Projects: []MachineProject{{Root: "/other"}}}
	cfg := machineTestConfig()
	root, local := t.TempDir(), t.TempDir()

	if err := claimMachineBand(m, cfg, root, local, &WorktreeConfig{}); err != nil {
		t.Fatal(err)
	}
	if cfg.PortCluster != 1 {
		t.Fatalf("PortCluster = %d, want 1 (cluster 0 overlaps /other)", cfg.PortCluster)
	}
	cluster, recorded, err := overlayPortCluster(local)
	if err != nil || !recorded || cluster != 1 {
		t.Fatalf("overlay port_cluster = %d recorded=%v err=%v, want 1", cluster, recorded, err)
	}
	if !m.hasProject(root) {
		t.Fatal("the project must be listed once its band is clear")
	}
}

// A project that already holds slots has stacks on its current ports, so it is
// never moved behind its back; setup stops and names the other project.
func TestClaimMachineBandRefusesToMoveAProjectWithClaims(t *testing.T) {
	isolateMachine(t)
	stubProjectPorts(t, map[string][]int{"/other": {47020}})
	m := &MachineState{Projects: []MachineProject{{Root: "/other"}}}
	cfg := machineTestConfig()
	root, local := t.TempDir(), t.TempDir()
	state := &WorktreeConfig{Worktrees: []WorktreeState{{Branch: "main", Path: root}}}

	err := claimMachineBand(m, cfg, root, local, state)
	if err == nil || !strings.Contains(err.Error(), "/other") || !strings.Contains(err.Error(), "wet port-cluster next") {
		t.Fatalf("err = %v, want one naming /other and the fix", err)
	}
	if cfg.PortCluster != 0 || m.hasProject(root) {
		t.Fatal("a refused project must be neither moved nor listed")
	}
	if _, err := os.Stat(filepath.Join(local, wetLocalConfigFileName)); !os.IsNotExist(err) {
		t.Fatal("a refused project's .wet.local.yaml must not be written")
	}
}

// A recorded port_cluster is a decision someone made, so it is not overridden
// either, even when the project has no worktrees yet.
func TestClaimMachineBandKeepsARecordedCluster(t *testing.T) {
	isolateMachine(t)
	stubProjectPorts(t, map[string][]int{"/other": {47000}})
	m := &MachineState{Projects: []MachineProject{{Root: "/other"}}}
	local := t.TempDir()
	if err := setLocalPortCluster(local, 0); err != nil {
		t.Fatal(err)
	}
	if err := claimMachineBand(m, machineTestConfig(), t.TempDir(), local, &WorktreeConfig{}); err == nil {
		t.Fatal("a recorded cluster that overlaps must be reported, not moved")
	}
}

func TestClaimMachineBandIgnoresTheProjectsOwnEntry(t *testing.T) {
	isolateMachine(t)
	root := t.TempDir()
	stubProjectPorts(t, map[string][]int{root: {47000}})
	m := &MachineState{Projects: []MachineProject{{Root: root}}}
	state := &WorktreeConfig{Worktrees: []WorktreeState{{Branch: "main", Path: root}}}
	if err := claimMachineBand(m, machineTestConfig(), root, t.TempDir(), state); err != nil {
		t.Fatalf("a project must not clash with itself: %v", err)
	}
}

// Ports a state entry records count even when they sit outside the band,
// which is what entries written before clusters existed look like.
func TestProjectPortsIncludesRecordedPortsOutsideTheBand(t *testing.T) {
	cfg := machineTestConfig()
	state := &WorktreeConfig{Worktrees: []WorktreeState{{Ports: map[string]int{"APP_PORT": 47230}}}}
	ports := projectPorts(cfg, state)
	if !ports[47000] || !ports[47090] || ports[47100] {
		t.Fatalf("band ports wrong: %v", ports)
	}
	if !ports[47230] {
		t.Fatal("a recorded port outside the band must still count")
	}
}

func stubDockerContainers(t *testing.T, containers ...dockerContainerPorts) {
	t.Helper()
	prev := listDockerContainerPorts
	t.Cleanup(func() { listDockerContainerPorts = prev })
	listDockerContainerPorts = func() []dockerContainerPorts { return containers }
}

func slotPoolTestConfig() *WetConfig {
	return &WetConfig{
		ProjectName:  "app",
		PortStride:   10,
		ClusterSlots: DefaultClusterSlots,
		SlotPool:     true,
		Ports:        []PortConfig{{Name: "APP_PORT", Base: 47000, Stride: 10}},
		Env:          map[string]string{"COMPOSE_PROJECT_NAME": "{{.ProjectName}}-slot-{{.Slot}}"},
	}
}

func TestParseDockerContainerPortsReadsDeclaredBindings(t *testing.T) {
	body := `swelfi-slot-4|restarting|unless-stopped|{"4000/tcp":[{"HostIp":"","HostPort":"4040"}],"9001/tcp":[{"HostIp":"","HostPort":"9072"}]}
app-slot-7|exited|unless-stopped|{"4000/tcp":[{"HostIp":"","HostPort":"4070"}]}
|exited|always|{"80/tcp":[{"HostIp":"","HostPort":"8080"}]}
other|running|no|{"5432/tcp":[{"HostIp":"","HostPort":""}]}`
	got := parseDockerContainerPorts(body)
	if len(got) != 4 {
		t.Fatalf("parsed %d containers, want 4: %+v", len(got), got)
	}
	want := []bool{true, false, true, true}
	for i, ct := range got {
		if ct.Holds != want[i] {
			t.Errorf("container %d (%s) Holds = %v, want %v", i, ct.Project, ct.Holds, want[i])
		}
	}
	if len(got[0].Ports) != 2 || len(got[3].Ports) != 0 {
		t.Errorf("ports: %v and %v, want two bound and none for an unpublished binding", got[0].Ports, got[3].Ports)
	}
	holders := portHolders(got)
	if holders[8080][0] != "-" || len(holders[4070]) != 0 {
		t.Errorf("holders = %v: a non-compose container is foreign, an exited one holds nothing", holders)
	}
}

// The cycle-2 failure: another project's Phoenix crash-loops on slot 1's
// port. A probe in the gap between restarts finds the port free; the declared
// binding must still keep wet off the slot.
func TestNextFreeReusableSlotSkipsAPortARestartingContainerDeclares(t *testing.T) {
	stubDockerContainers(t, dockerContainerPorts{Project: "swelfi-slot-1", Ports: []int{47010}, Holds: true})
	cfg := slotPoolTestConfig()
	if got, err := cfg.NextFreeReusableSlot(nil); err != nil || got != 2 {
		t.Fatalf("NextFreeReusableSlot = %d, %v, want 2", got, err)
	}
	if got, err := cfg.NextFreeSlotWithFreePorts(nil); err != nil || got != 2 {
		t.Fatalf("NextFreeSlotWithFreePorts = %d, %v, want 2", got, err)
	}
}

// The cycle-2 waste: slot 3's DB was warm, and the allocator built a new one
// on slot 1 because 1 sorts first.
func TestNextFreeReusableSlotPrefersAWarmSlot(t *testing.T) {
	stubDockerContainers(t, dockerContainerPorts{Project: "app-slot-3", Ports: []int{47033}, Holds: false})
	cfg := slotPoolTestConfig()
	if got, err := cfg.NextFreeReusableSlot(nil); err != nil || got != 3 {
		t.Fatalf("NextFreeReusableSlot = %d, %v, want the warm slot 3", got, err)
	}
	if got, err := cfg.NextFreeReusableSlot([]int{3}); err != nil || got != 1 {
		t.Fatalf("with slot 3 claimed: %d, %v, want 1", got, err)
	}
}

// A slot whose own DB container is up has its port bound; that is the warm
// resource the pool exists to reuse, not a collision.
func TestNextFreeReusableSlotReusesPortsItsOwnProjectHolds(t *testing.T) {
	ln, err := net.Listen("tcp", ":47013")
	if err != nil {
		t.Skipf("cannot bind 47013: %v", err)
	}
	defer ln.Close()
	stubDockerContainers(t, dockerContainerPorts{Project: "app-slot-1", Ports: []int{47013}, Holds: true})
	cfg := slotPoolTestConfig()
	cfg.Ports = append(cfg.Ports, PortConfig{Name: "DB_PORT", Base: 47003, Stride: 10})
	if got, err := cfg.NextFreeReusableSlot(nil); err != nil || got != 1 {
		t.Fatalf("NextFreeReusableSlot = %d, %v, want 1 (its own DB holds 47013)", got, err)
	}
}
