package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// warmEnvFileEnv names the env file a slot_warm hook reads in place of
// .env.worktree: the hook runs from the main checkout, whose own
// .env.worktree is slot 0's.
const warmEnvFileEnv = "WET_ENV_FILE"

// spareSlotPlan is which unclaimed slots a warm run takes as the spares, and
// which of those already hold warm resources.
type spareSlotPlan struct {
	Ready  []int
	ToWarm []int
	// Short is how many of the asked-for spares no slot could take: the band
	// ran out of unclaimed slots whose ports are free.
	Short int
}

// planSpareSlots picks the spares from the slots below cluster_slots that no
// checkout claims. warmth is slotWarmth's verdict per slot and reusable says
// whether a slot's ports are free or held by its own Compose project. Warm
// slots come first, then stopped ones, whose volumes are already there, then
// cold ones; within each group the lowest slot goes first, which is the order
// setup claims them in. Every pick is warmed, warm ones included: the hook is
// idempotent, and rerunning it is what repairs a slot whose last warm stopped
// halfway with its DB up.
func planSpareSlots(cfg *WetConfig, used []int, want int, warmth func(int) string, reusable func(int) bool) spareSlotPlan {
	taken := make(map[int]bool, len(used))
	for _, slot := range used {
		taken[slot] = true
	}
	var plan spareSlotPlan
	var warm, stopped, cold []int
	for slot := 1; slot < cfg.ClusterSlotCount(); slot++ {
		if taken[slot] || cfg.validateSlotPorts(slot) != nil {
			continue
		}
		switch warmth(slot) {
		case "yes":
			warm = append(warm, slot)
			continue
		case "stopped":
			if reusable(slot) {
				stopped = append(stopped, slot)
			}
			continue
		}
		if reusable(slot) {
			cold = append(cold, slot)
		}
	}
	need := want
	for _, slot := range append(append(warm, stopped...), cold...) {
		if need <= 0 {
			break
		}
		plan.ToWarm = append(plan.ToWarm, slot)
		if slices.Contains(warm, slot) {
			plan.Ready = append(plan.Ready, slot)
		}
		need--
	}
	if need > 0 {
		plan.Short = need
	}
	return plan
}

// warmSpareSlots makes sure want unclaimed slots of this project hold warm
// resources, so the next `wet setup` claims a slot whose DB is already up.
// Setup prefers a slot whose Compose project has containers, which is what
// makes a warmed spare the one it takes.
//
// An unclaimed slot has no checkout, so hooks.slot_warm runs from the main
// checkout with the slot's hook env and $WET_ENV_FILE naming a rendered copy
// of what that slot's .env.worktree would hold. It must start only what is
// slot-stable: anything that bind-mounts a checkout would mount main's.
func warmSpareSlots(want int) error {
	repoRoot, err := getMainRepoRoot()
	if err != nil {
		return err
	}
	cfg, _, err := LoadWetConfigForWorktree(repoRoot, repoRoot)
	if err != nil {
		return err
	}
	if len(cfg.Hooks.SlotWarm) == 0 {
		return fmt.Errorf("--spare needs hooks.slot_warm in %s: the commands that boot an unclaimed slot's DB and other slot-stable services", wetConfigFileName)
	}
	if !cfg.SlotPool {
		return fmt.Errorf("--spare needs slot_pool: true in %s; without it setup skips a slot whose ports a warm container holds", wetConfigFileName)
	}
	if cfg.composeProjectNameForSlot(1) == "" {
		return fmt.Errorf("--spare needs env.COMPOSE_PROJECT_NAME built from {{.Slot}} in %s, so wet can tell a warm slot from a cold one", wetConfigFileName)
	}
	state, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load wet state: %w", err)
	}
	used := make([]int, 0, len(state.Worktrees))
	for _, wt := range state.Worktrees {
		used = append(used, wt.Index)
	}
	containers := listDockerContainerPorts()
	holders := portHolders(containers)
	plan := planSpareSlots(cfg, used, want, slotWarmth(cfg, containers),
		func(slot int) bool { return cfg.slotPortsReusable(slot, holders) })

	if len(plan.ToWarm) > 0 {
		fmt.Printf("  Spare slots: %s", joinSlots(plan.ToWarm))
		if len(plan.Ready) > 0 {
			fmt.Printf(" (already warm: %s)", joinSlots(plan.Ready))
		}
		fmt.Println()
	}
	local, err := LoadEnvLocalForTree(repoRoot, repoRoot)
	if err != nil {
		return err
	}
	var failed []string
	for _, slot := range plan.ToWarm {
		fmt.Printf("\n==> warming spare slot %d\n", slot)
		ctx, err := spareSlotContext(cfg, repoRoot, slot, local)
		if err == nil {
			err = runSlotHooks(ctx, "slot_warm", cfg.Hooks.SlotWarm)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "  slot %d: %v\n", slot, err)
			failed = append(failed, fmt.Sprint(slot))
		}
	}
	if plan.Short > 0 {
		fmt.Fprintf(os.Stderr, "\n  %d spare slot(s) short: no other unclaimed slot below cluster_slots (%d) has free ports\n", plan.Short, cfg.ClusterSlotCount())
	}
	if len(failed) > 0 {
		return fmt.Errorf("warm failed for slot(s): %s", strings.Join(failed, ", "))
	}
	if plan.Short > 0 {
		return fmt.Errorf("warmed %d of the %d spare slot(s) asked for", len(plan.ToWarm), want)
	}
	return nil
}

// spareSlotContext is the hook context of a slot no checkout holds: the
// slot's ports, no pool items, main's .env.local, and the env file written
// to the git dir for $WET_ENV_FILE.
func spareSlotContext(cfg *WetConfig, repoRoot string, slot int, local map[string]string) (*slotContext, error) {
	ports := cfg.ResolvePorts(slot)
	data := BuildTemplateData(cfg, "", slot, ports, nil)
	if _, err := ResolveEnvValues(cfg, data, local); err != nil {
		return nil, fmt.Errorf("resolve env: %w", err)
	}
	content, err := RenderEnvFileContent(cfg, data, ports, local)
	if err != nil {
		return nil, fmt.Errorf("render %s: %w", envWorktreeFileName, err)
	}
	path := spareEnvFilePath(repoRoot, slot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return nil, err
	}
	hookEnv, err := BuildHookEnv(cfg, data, ports, local)
	if err != nil {
		return nil, fmt.Errorf("build hook env: %w", err)
	}
	hookEnv = append(hookEnv, warmEnvFileEnv+"="+path)
	return &slotContext{cfg: cfg, worktreePath: repoRoot, branch: "(unclaimed)", slot: slot, ports: ports, data: data, hookEnv: hookEnv}, nil
}

// spareEnvFilePath sits in the main checkout's .git: per project, never
// committed, and gone with the clone.
func spareEnvFilePath(repoRoot string, slot int) string {
	return filepath.Join(repoRoot, ".git", "wet", fmt.Sprintf("slot-%d.env", slot))
}

func joinSlots(slots []int) string {
	parts := make([]string, len(slots))
	for i, s := range slots {
		parts[i] = fmt.Sprint(s)
	}
	return strings.Join(parts, ", ")
}
