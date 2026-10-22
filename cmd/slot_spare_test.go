package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func spareTestConfig() *WetConfig {
	return &WetConfig{
		ProjectName:  "app",
		Env:          map[string]string{"COMPOSE_PROJECT_NAME": "app-slot-{{.Slot}}"},
		Ports:        []PortConfig{{Name: "APP", Base: 47100, Stride: 10}},
		ClusterSlots: 7,
	}
}

func TestPlanSpareSlotsCountsWarmOnesAndPrefersStopped(t *testing.T) {
	cfg := spareTestConfig()
	warmth := func(slot int) string {
		switch slot {
		case 2:
			return "yes"
		case 5:
			return "stopped"
		}
		return "no"
	}
	reusable := func(slot int) bool { return slot != 3 }

	plan := planSpareSlots(cfg, []int{0, 1}, 3, warmth, reusable)
	want := spareSlotPlan{Ready: []int{2}, ToWarm: []int{2, 5, 4}}
	if !reflect.DeepEqual(plan, want) {
		t.Fatalf("plan = %+v, want %+v", plan, want)
	}

	// The warm spare is rewarmed rather than skipped: that repairs one whose
	// last warm stopped halfway.
	if plan := planSpareSlots(cfg, []int{1}, 1, warmth, reusable); !reflect.DeepEqual(plan.ToWarm, []int{2}) || plan.Short != 0 {
		t.Fatalf("one warm spare already there, plan = %+v", plan)
	}

	// Slots 2, 4, 5 and 6 are usable; 3's ports are taken.
	plan = planSpareSlots(cfg, []int{1}, 6, warmth, reusable)
	if !reflect.DeepEqual(plan.ToWarm, []int{2, 5, 4, 6}) || plan.Short != 2 {
		t.Fatalf("plan = %+v, want 2, 5, 4, 6 warmed and 2 short", plan)
	}
}

func TestWarmSpareSlotsRunsTheHookFromMainWithTheSlotsEnv(t *testing.T) {
	isolateMachine(t)
	repo := initCleanupRepo(t)
	config := `project_name: app
slot_pool: true
cluster_slots: 5
ports:
  - { name: APP_PORT, base: 47200 }
env:
  COMPOSE_PROJECT_NAME: 'app-slot-{{.Slot}}'
hooks:
  slot_warm:
    - 'echo "$SLOT $APP_PORT $(grep COMPOSE_PROJECT_NAME "$WET_ENV_FILE")" >> warm.log'
`
	if err := os.WriteFile(filepath.Join(repo, wetConfigFileName), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	wt := addWorktree(t, repo, "feature")
	state := &WorktreeConfig{Worktrees: []WorktreeState{{Branch: "feature", Path: wt, Index: 1}}}
	if err := saveConfigTo(projectStatePath(repo), state); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	prev := listDockerContainerPorts
	t.Cleanup(func() { listDockerContainerPorts = prev })
	listDockerContainerPorts = func() []dockerContainerPorts {
		return []dockerContainerPorts{{Project: "app-slot-2", Holds: true}}
	}

	if err := warmSpareSlots(2); err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(filepath.Join(repo, "warm.log"))
	if err != nil {
		t.Fatalf("the hook must run in the main checkout: %v", err)
	}
	want := "2 47220 COMPOSE_PROJECT_NAME=app-slot-2\n3 47230 COMPOSE_PROJECT_NAME=app-slot-3"
	if got := strings.TrimSpace(string(log)); got != want {
		t.Fatalf("warm.log = %q, want slots 2 and 3, each with its own env", got)
	}
}

func TestWarmSpareSlotsNeedsTheHookAndSlotPool(t *testing.T) {
	isolateMachine(t)
	repo := initCleanupRepo(t)
	t.Chdir(repo)
	for config, want := range map[string]string{
		"project_name: app\nslot_pool: true\n":             "hooks.slot_warm",
		"project_name: app\nhooks:\n  slot_warm: [true]\n": "slot_pool: true",
	} {
		if err := os.WriteFile(filepath.Join(repo, wetConfigFileName), []byte(config), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := warmSpareSlots(1); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want it to name %s", err, want)
		}
	}
}
