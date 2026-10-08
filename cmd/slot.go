package cmd

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

var slotCmd = &cobra.Command{
	Use:   "slot",
	Short: "Run lifecycle hooks for the current wet slot",
	Long: `Run slot lifecycle hooks for the checkout you are standing in.

A slot is the reusable allocation wet recorded for this checkout: slot number,
ports, and pool items. The commands here do not allocate anything; run ` + "`wet setup`" + ` first.

Projects define the actual work in .wet.yaml:

  hooks:
    slot_reset:   # clean reusable state: reset DB, uninstall app, adb reverse
    slot_start:   # boot runtime for this checkout
    slot_stop:    # stop runtime, keep reusable resources warm
    slot_rebuild: # nuke and recreate dirty slot resources
    slot_doctor:  # verify the slot is healthy
`,
}

var slotHeadless bool

func init() {
	slotCmd.PersistentFlags().BoolVar(&slotHeadless, "headless", false, "Run emulators without a visible window while resolving pool handles")
	slotCmd.AddCommand(slotLifecycleCmd("reset", "Clean the current slot back to its project baseline", func(h HooksConfig) []string { return h.SlotReset }))
	slotCmd.AddCommand(slotLifecycleCmd("start", "Start the current slot's runtime", func(h HooksConfig) []string { return h.SlotStart }))
	slotCmd.AddCommand(slotLifecycleCmd("stop", "Stop the current slot's runtime while keeping reusable resources", func(h HooksConfig) []string { return h.SlotStop }))
	slotCmd.AddCommand(slotLifecycleCmd("rebuild", "Nuke and recreate dirty resources for the current slot", func(h HooksConfig) []string { return h.SlotRebuild }))
	slotCmd.AddCommand(slotLifecycleCmd("doctor", "Check the current slot's health", func(h HooksConfig) []string { return h.SlotDoctor }))
	slotCmd.AddCommand(slotWarmCmd())
	slotCmd.AddCommand(slotListCmd())
	slotCmd.AddCommand(slotReleaseCmd())
	rootCmd.AddCommand(slotCmd)
}

func slotLifecycleCmd(name, short string, selectHooks func(HooksConfig) []string) *cobra.Command {
	return &cobra.Command{
		Use:   name,
		Args:  cobra.NoArgs,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, err := currentSlotContext(slotHeadless)
			if err != nil {
				return err
			}
			hooks := selectHooks(ctx.cfg.Hooks)
			if len(hooks) == 0 {
				fmt.Printf("  No hooks.%s declared in %s\n", "slot_"+name, wetConfigFileName)
				return nil
			}
			if err := runSlotHooks(ctx, "slot_"+name, hooks); err != nil {
				return err
			}
			if name == "reset" {
				return clearPendingReset(ctx.worktreePath)
			}
			return nil
		},
	}
}

type slotContext struct {
	cfg          *WetConfig
	worktreePath string
	branch       string
	slot         int
	ports        []ResolvedPort
	data         map[string]any
	hookEnv      []string
}

func currentSlotContext(headless bool) (*slotContext, error) {
	worktreePath, err := getRepoRoot()
	if err != nil {
		return nil, err
	}
	repoRoot, err := getMainRepoRoot()
	if err != nil {
		return nil, err
	}
	state, err := loadConfig()
	if err != nil {
		return nil, fmt.Errorf("load wet state: %w", err)
	}
	wt := worktreeStateForPath(state, worktreePath)
	if wt == nil {
		return nil, fmt.Errorf("no wet state for %s — run `wet setup` first", worktreePath)
	}
	return slotContextForWorktree(repoRoot, worktreePath, *wt, headless)
}

func slotContextForWorktree(repoRoot, worktreePath string, wt WorktreeState, headless bool) (*slotContext, error) {
	cfg, _, err := LoadWetConfigForWorktree(worktreePath, repoRoot)
	if err != nil {
		return nil, err
	}

	ports := make([]ResolvedPort, 0, len(cfg.Ports))
	for _, p := range cfg.Ports {
		if v, ok := wt.Ports[p.Name]; ok {
			ports = append(ports, ResolvedPort{Name: p.Name, Value: v})
		}
	}

	handles, err := resolvePoolHandles(wt.Pools, headless)
	if err != nil {
		return nil, err
	}
	extraHandles, err := resolveExtraHandles(wt.PoolExtras, headless)
	if err != nil {
		return nil, err
	}
	data := WithPoolHandles(BuildTemplateData(cfg, wt.Branch, wt.Index, ports, wt.Pools), handles)
	WithPoolExtras(data, wt.PoolExtras, extraHandles)

	local, err := LoadEnvLocalForTree(worktreePath, repoRoot)
	if err != nil {
		return nil, err
	}
	if _, err := ResolveEnvValues(cfg, data, local); err != nil {
		return nil, fmt.Errorf("resolve env: %w", err)
	}
	hookEnv, err := BuildHookEnv(cfg, data, ports, local)
	if err != nil {
		return nil, fmt.Errorf("build hook env: %w", err)
	}
	return &slotContext{cfg: cfg, worktreePath: worktreePath, branch: wt.Branch, slot: wt.Index, ports: ports, data: data, hookEnv: hookEnv}, nil
}

func slotWarmCmd() *cobra.Command {
	var all bool
	var reset bool
	cmd := &cobra.Command{
		Use:   "warm",
		Args:  cobra.NoArgs,
		Short: "Start warm slot resources",
		Long: `Start slot runtimes for claimed worktrees.

With --all, wet walks .wet-worktrees.json, boots any claimed emulator handles,
and runs hooks.slot_start in each worktree. Add --reset to run hooks.slot_reset
before hooks.slot_start.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !all {
				ctx, err := currentSlotContext(slotHeadless)
				if err != nil {
					return err
				}
				if reset && len(ctx.cfg.Hooks.SlotReset) > 0 {
					if err := runSlotHooks(ctx, "slot_reset", ctx.cfg.Hooks.SlotReset); err != nil {
						return err
					}
					if err := clearPendingReset(ctx.worktreePath); err != nil {
						return err
					}
				}
				return runSlotHooks(ctx, "slot_start", ctx.cfg.Hooks.SlotStart)
			}
			return warmAllSlots(slotHeadless, reset)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "warm every claimed worktree in this repo")
	cmd.Flags().BoolVar(&reset, "reset", false, "run slot_reset before slot_start")
	return cmd
}

func warmAllSlots(headless, reset bool) error {
	repoRoot, err := getMainRepoRoot()
	if err != nil {
		return err
	}
	state, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load wet state: %w", err)
	}
	if len(state.Worktrees) == 0 {
		fmt.Println("  No wet worktrees are claimed.")
		return nil
	}
	var failed []string
	for _, wt := range state.Worktrees {
		if wt.Path == "" {
			continue
		}
		if _, err := os.Stat(wt.Path); err != nil {
			fmt.Fprintf(os.Stderr, "\n  Skipping %s: %s is not on disk\n", wt.Branch, wt.Path)
			failed = append(failed, wt.Branch)
			continue
		}
		ctx, err := slotContextForWorktree(repoRoot, wt.Path, wt, headless)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\n  Skipping %s: %v\n", wt.Branch, err)
			failed = append(failed, wt.Branch)
			continue
		}
		fmt.Printf("\n==> warming slot %d: %s\n", ctx.slot, ctx.branch)
		if reset && len(ctx.cfg.Hooks.SlotReset) > 0 {
			if err := runSlotHooks(ctx, "slot_reset", ctx.cfg.Hooks.SlotReset); err != nil {
				failed = append(failed, ctx.branch)
				continue
			}
			if err := clearPendingReset(ctx.worktreePath); err != nil {
				failed = append(failed, ctx.branch)
				continue
			}
		}
		if len(ctx.cfg.Hooks.SlotStart) == 0 {
			fmt.Printf("  No hooks.slot_start declared in %s\n", wetConfigFileName)
			continue
		}
		if err := runSlotHooks(ctx, "slot_start", ctx.cfg.Hooks.SlotStart); err != nil {
			failed = append(failed, ctx.branch)
			continue
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("warm failed for: %s", strings.Join(failed, ", "))
	}
	return nil
}

func runSlotHooks(ctx *slotContext, hookBase string, hooks []string) error {
	for i, body := range hooks {
		hookName := fmt.Sprintf("%s[%d]", hookBase, i)
		rendered, err := RenderHookCommand("hooks."+hookName, body, ctx.data)
		if err != nil {
			return fmt.Errorf("hooks.%s: render: %w", hookName, err)
		}
		if err := runWetHook("hooks."+hookName, rendered, ctx.worktreePath, ctx.hookEnv); err != nil {
			fmt.Fprintf(os.Stderr, "\nHook failed while running slot lifecycle.\n\n  Worktree:  %s\n  Branch:    %s\n  Slot:      %d\n  Hook:      %s\n  Command:   %s\n  Error:     %v\n\n", ctx.worktreePath, ctx.branch, ctx.slot, hookName, rendered, err)
			return fmt.Errorf("slot hook failed: %s", hookName)
		}
	}
	fmt.Printf("\n  Slot %d %s complete: %s\n", ctx.slot, strings.TrimPrefix(hookBase, "slot_"), ctx.branch)
	if len(ctx.ports) > 0 {
		parts := make([]string, 0, len(ctx.ports))
		for _, p := range ctx.ports {
			parts = append(parts, fmt.Sprintf("%s=%d", p.Name, p.Value))
		}
		fmt.Printf("  Ports: %s\n", strings.Join(parts, ", "))
	}
	if pools, ok := ctx.data["Pools"].(map[string]string); ok && len(pools) > 0 {
		names := make([]string, 0, len(pools))
		for n := range pools {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Printf("  Pool %s: %s\n", n, pools[n])
		}
	}
	return nil
}

// clearPendingReset records that path's slot has been reset.
func clearPendingReset(path string) error {
	return withStateLock(func() error {
		state, err := loadConfig()
		if err != nil {
			return fmt.Errorf("load wet state: %w", err)
		}
		own := worktreeStateForPath(state, path)
		if own == nil || !own.PendingReset {
			return nil
		}
		own.PendingReset = false
		return saveConfig(state)
	})
}
