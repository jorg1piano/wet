package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func globalTestConfig() *WetConfig {
	return &WetConfig{
		ProjectName:  "app",
		Env:          map[string]string{"COMPOSE_PROJECT_NAME": "app-slot-{{.Slot}}"},
		Ports:        []PortConfig{{Name: "APP", Base: 8080}},
		ClusterSlots: 4,
		Pools:        map[string][]string{"devices": {"emulator:app-1", "emulator:app-2", "phone-serial"}},
	}
}

func TestPlanProjectDownKeepsMainAndStopsTheOtherSlots(t *testing.T) {
	root := t.TempDir()
	wt1 := t.TempDir()
	wt2 := t.TempDir()
	p := globalProject{Root: root, Cfg: globalTestConfig(), State: &WorktreeConfig{Worktrees: []WorktreeState{
		{Branch: "main", Path: root, Index: 0, Ports: map[string]int{"APP": 8080}, Pools: map[string]string{"devices": "emulator:app-main"}},
		{Branch: "feat/a", Path: wt1, Index: 1, Ports: map[string]int{"APP": 8090}, Pools: map[string]string{"devices": "emulator:app-1"}},
		{Branch: "feat/b", Path: wt2, Index: 3, Ports: map[string]int{"APP": 8110}},
	}}}

	plan := planProjectDown(p, nil)

	var released []string
	for _, wt := range plan.Released {
		released = append(released, wt.Branch)
	}
	if !reflect.DeepEqual(released, []string{"feat/a", "feat/b"}) {
		t.Fatalf("released %v, want feat/a and feat/b", released)
	}
	for _, name := range plan.ComposeProjects {
		if name == "app-slot-0" {
			t.Fatalf("main's compose project must not be stopped: %v", plan.ComposeProjects)
		}
	}
	for _, want := range []string{"app-slot-1", "app-slot-3", "app-slot-20"} {
		if !contains(plan.ComposeProjects, want) {
			t.Fatalf("want %s stopped, got %v", want, plan.ComposeProjects)
		}
	}
	if !reflect.DeepEqual(plan.Emulators, []string{"emulator:app-1", "emulator:app-2"}) {
		t.Fatalf("emulators %v: want the pool's emulators, not the phone", plan.Emulators)
	}
	if !reflect.DeepEqual(plan.Keep, []string{"emulator:app-main"}) {
		t.Fatalf("keep %v, want main's emulator", plan.Keep)
	}
}

func TestContainersToStopTakesRunningAndRestartingOnly(t *testing.T) {
	containers := []dockerComposeContainer{
		{ID: "a", Project: "app-slot-1", State: "running"},
		{ID: "b", Project: "app-slot-1", State: "exited"},
		{ID: "c", Project: "app-slot-2", State: "restarting"},
		{ID: "d", Project: "app-slot-0", State: "running"},
		{ID: "e", Project: "other-slot-1", State: "running"},
	}
	got := containersToStop(containers, []string{"app-slot-1", "app-slot-2"})
	var ids []string
	for _, c := range got {
		ids = append(ids, c.ID)
	}
	if !reflect.DeepEqual(ids, []string{"a", "c"}) {
		t.Fatalf("stop %v, want a and c", ids)
	}
}

func TestEmulatorsToStopSkipsKeptAndStoppedOnes(t *testing.T) {
	running := map[string]string{"app-1": "emulator-5554", "app-main": "emulator-5556", "mine": "emulator-5558"}
	got := emulatorsToStop([]string{"emulator:app-1", "emulator:app-2", "emulator:app-main"}, []string{"emulator:app-main"}, running)
	want := map[string]string{"emulator:app-1": "emulator-5554"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestReleaseGlobalClaimsKeepsMainAndTheCheckouts(t *testing.T) {
	isolateMachine(t)
	root := t.TempDir()
	wt := t.TempDir()
	fresh := t.TempDir()
	state := &WorktreeConfig{Worktrees: []WorktreeState{
		{Branch: "main", Path: root},
		{Branch: "feat/a", Path: wt, Index: 1},
	}}
	if err := saveConfigTo(projectStatePath(root), state); err != nil {
		t.Fatal(err)
	}
	plan := planProjectDown(globalProject{Root: root, State: state}, nil)

	// A setup that claims a slot after the plan was made keeps its claim.
	state.Worktrees = append(state.Worktrees, WorktreeState{Branch: "feat/new", Path: fresh, Index: 2})
	if err := saveConfigTo(projectStatePath(root), state); err != nil {
		t.Fatal(err)
	}

	if err := releaseGlobalClaims([]projectDownPlan{plan}); err != nil {
		t.Fatal(err)
	}
	after, err := loadProjectState(root)
	if err != nil {
		t.Fatal(err)
	}
	var branches []string
	for _, w := range after.Worktrees {
		branches = append(branches, w.Branch)
	}
	if !reflect.DeepEqual(branches, []string{"main", "feat/new"}) {
		t.Fatalf("left %v, want main and feat/new", branches)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("the released checkout must stay on disk: %v", err)
	}
}

func TestWriteGlobalSlotListOneSectionPerProject(t *testing.T) {
	root := t.TempDir()
	gone := filepath.Join(t.TempDir(), "gone")
	projects := []globalProject{
		{Root: root, Cfg: globalTestConfig(), State: &WorktreeConfig{Worktrees: []WorktreeState{
			{Branch: "main", Path: root, Index: 0, Ports: map[string]int{"APP": 8080}},
		}}},
		{Root: gone, Gone: true},
	}
	var out bytes.Buffer
	writeGlobalSlotList(&out, projects, nil, "")
	got := out.String()
	for _, want := range []string{"==> " + filepath.Base(root), "main", "Unclaimed: 1, 2, 3", "==> gone", "main checkout is gone"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
}
