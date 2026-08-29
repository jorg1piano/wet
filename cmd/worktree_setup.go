package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var worktreeSetupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Configure the current worktree (copy files, allocate ports, run hooks)",
	Long: `Run from inside an existing worktree. Loads .wet.yaml from the main
repo root, allocates a slot + pool items + ports for this worktree, copies
files declared in .wet.yaml's copy: list, writes .env.worktree, runs
post_create hooks, and persists the entry to .wet-worktrees.json.

If the current worktree already has an entry in the state file, it keeps its
slot, its pool picks and the extra devices ` + "`wet mobile add`" + ` claimed,
its ports are re-resolved, and the existing entry is updated in place (no
duplicate entries). This makes setup idempotent.`,
	RunE: runWorktreeSetup,
}

func init() {
	worktreeSetupCmd.Flags().Bool("headless", false, "Run emulators without a visible window (feeds into pool grow strategies)")
	worktreeCmd.AddCommand(worktreeSetupCmd)
}

func runWorktreeSetup(cmd *cobra.Command, args []string) error {
	// Resolve where we are and where the main repo lives. getRepoRoot returns
	// the current worktree path; getMainRepoRoot returns the main working
	// tree regardless of cwd.
	worktreePath, err := getRepoRoot()
	if err != nil {
		return err
	}
	repoRoot, err := getMainRepoRoot()
	if err != nil {
		return err
	}

	// Guard: `wet worktree setup` is worktree-only. The main checkout is set
	// up by `wet setup`, which pins slot 0 and skips the copy step.
	absWt, _ := filepath.Abs(worktreePath)
	absMain, _ := filepath.Abs(repoRoot)
	if absWt == absMain {
		return fmt.Errorf("this is the main repo (%s) — run `wet setup` to configure it", absMain)
	}

	// Derive the branch name from HEAD inside this worktree.
	branchOut, err := exec.Command("git", "-C", worktreePath, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return fmt.Errorf("determine current branch: %w", err)
	}
	branchName := strings.TrimSpace(string(branchOut))
	if branchName == "" || branchName == "HEAD" {
		return fmt.Errorf("could not determine branch name for %s (detached HEAD?)", worktreePath)
	}

	headless, _ := cmd.Flags().GetBool("headless")
	return runWorktreeSetupCore(repoRoot, worktreePath, branchName, headless)
}

// runWorktreeSetupCore performs the full `.wet.yaml`-driven configuration
// step for an existing worktree on disk:
//
//  1. Loads `.wet.yaml`, preferring the checkout's own copy.
//  2. Loads the wet state file and either updates the existing entry for
//     this worktree or appends a new one at the next free slot.
//  3. Allocates pool items.
//  4. Copies files from cfg.Copy (worktrees only).
//  5. Writes `.env.worktree` into the worktree.
//  6. Runs post_create hooks.
//  7. Persists the state file.
//
// Errors before persistence leave the worktree on disk but the state file
// unchanged (or reverted, for updates). Hook failures after partial work
// print a recovery block via printSetupHookFailure.
func runWorktreeSetupCore(repoRoot, worktreePath, branchName string, headless bool) error {
	return runSetupCore(repoRoot, worktreePath, branchName, headless, false)
}

// runSetupCore is runWorktreeSetupCore with the main-repo case folded in.
//
// The main checkout wants everything a worktree wants — rendered env files, a
// claimed device, a recorded slot — because it is a working checkout too. An
// emulator that got deleted has to be re-claimable there, not hand-fixed. So
// main runs the same pipeline, differing only where a difference is forced:
//
//   - Slot is pinned to 0. NextFreeSlot already reserves it, so main's ports
//     are the declared `base:` values and no worktree can be handed them.
//   - copy: is skipped — source and destination are the same directory.
//   - Hook-failure guidance never suggests removing the checkout.
//
// Pools are allocated exactly as for a worktree: main claims an item and
// holds it, which is the honest accounting — that device really is in use.
func runSetupCore(repoRoot, worktreePath, branchName string, headless, isMain bool) error {
	cfg, cfgPath, cfgErr := LoadWetConfigForWorktree(worktreePath, repoRoot)
	if cfgErr == nil && filepath.Dir(cfgPath) != repoRoot {
		fmt.Printf("  Config: %s (worktree's own)\n", cfgPath)
	}
	if cfgErr == nil && len(cfg.LocalKeys) > 0 {
		fmt.Printf("  Config: %s overrides %s\n", wetLocalConfigFileName, strings.Join(cfg.LocalKeys, ", "))
	}
	if cfgErr == ErrConfigMissing {
		// No fallback copy list. A hardcoded one would mean project knowledge
		// baked into a CLI that claims to have none, and it silently produces
		// a worktree with no ports allocated — which fails later, somewhere
		// else, as a port collision nobody traces back to here.
		//
		// Writing a starter config is the one safe exception: it is the file
		// the user was told to go and write, it declares nothing about the
		// project beyond a name derived from the directory, and setup still
		// stops so they can fill it in before anything is allocated.
		searched := worktreePath
		if repoRoot != worktreePath {
			searched = fmt.Sprintf("%s or %s", worktreePath, repoRoot)
		}
		// Announce the search before scaffolding, not after: the scaffolder
		// prints its own lines (the .gitignore entry it added), and a summary
		// that arrives after the details it summarizes reads as a non-sequitur.
		fmt.Printf("  No %s found in %s.\n", wetConfigFileName, searched)
		path, writeErr := scaffoldStarterConfig(repoRoot)
		if writeErr != nil {
			return fmt.Errorf("no %s found in %s, and writing a starter one failed: %w",
				wetConfigFileName, searched, writeErr)
		}
		fmt.Printf("  Wrote a starter one: %s\n", path)

		// Offering the catalog here rather than leaving it to a second
		// command is the difference between a config that boots and one that
		// boots without the keys the app reads. Only on a fresh scaffold: a
		// re-run is not the moment to ask again, and a non-interactive caller
		// gets a pointer instead of a picker it cannot answer.
		if err := offerSecretGroups(path); err != nil {
			return err
		}
		return fmt.Errorf("edit %s (check project_name and ports:), then re-run setup", path)
	} else if cfgErr != nil {
		return cfgErr
	}

	// Everything machine-local — the .wet.local.yaml a grown pool item is
	// appended to, the .env.local overrides, the gitignored files a new
	// worktree is seeded with — lives beside the trunk checkout, not
	// necessarily in git's "main working tree". Shared allocation state
	// (.wet-worktrees.json) stays at repoRoot: it is per-repo, not
	// per-checkout, and git's main tree is the one place every worktree
	// agrees on.
	localRoot := LocalConfigDir(worktreePath, repoRoot)

	// Claim slot, ports and pool items under the state lock, and record the
	// claim BEFORE the hooks run.
	//
	// Hooks take minutes (image builds), so the lock cannot span them — and
	// it must not: two worktrees booting their stacks concurrently is the
	// feature. What must not overlap is the read-modify-write that decides
	// who owns which slot. Writing the claim only after hooks succeeded left
	// a minutes-long window in which a second `wet setup` saw a free slot
	// that wasn't, and handed out the same ports and the same phone.
	//
	// A claim that outlives a failed setup is the right trade: the worktree
	// still exists on disk, re-running setup finds its own entry and keeps
	// the same slot, and `wet worktree pop` releases it.
	var slot int
	var pools map[string]string
	var extras map[string][]string
	var copiedFiles []string
	var copySkipped string
	if err := withStateLock(func() error {
		state, err := loadConfig()
		if err != nil {
			return fmt.Errorf("failed to load worktree state: %w", err)
		}

		// Find existing entry for this worktree (by absolute path). If we
		// find one, update it in place; otherwise append at the end.
		absWorktree, _ := filepath.Abs(worktreePath)
		existingIdx := -1
		for i, wt := range state.Worktrees {
			absPath, _ := filepath.Abs(wt.Path)
			if absPath == absWorktree {
				existingIdx = i
				break
			}
		}

		// The view of "other worktrees" for allocation. If this worktree
		// already has an entry, exclude it so allocation doesn't see its own
		// previous claims as conflicts.
		others := make([]WorktreeState, 0, len(state.Worktrees))
		used := make([]int, 0, len(state.Worktrees))
		for i, wt := range state.Worktrees {
			if i == existingIdx {
				continue
			}
			others = append(others, wt)
			used = append(used, wt.Index)
		}

		switch {
		case isMain:
			// Slot 0 is the main checkout by definition; never probe for a
			// free one, or a running main stack would push itself to slot 1
			// and collide with a worktree.
			slot = 0
		case existingIdx >= 0 && state.Worktrees[existingIdx].Index > 0:
			// Idempotent re-setup: keep this worktree's existing slot. Don't
			// port-probe it — its own stack may be running, and a probe would
			// see those ports busy and reassign it to a different slot.
			slot = state.Worktrees[existingIdx].Index
		default:
			// New worktree: normally require both an unclaimed state slot and
			// free host ports. A slot pool intentionally leaves warm resources
			// (usually the DB container) bound after release, so in that mode
			// the state file is the authority and the project's slot_reset hook
			// is responsible for proving the reused resources are sane.
			if cfg.SlotPool {
				slot = NextFreeSlot(used)
			} else {
				slot = cfg.NextFreeSlotWithFreePorts(used)
			}
		}

		// A re-run keeps what this worktree already holds: its primary picks
		// (so the emulator it has been running on stays its emulator) and the
		// extras `wet mobile add` recorded. The extras are offered to the
		// allocator as a claimant of their own, because `others` excludes this
		// tree's entry and a primary re-pick could otherwise land on one of
		// them. Extras from a pool the config no longer declares are dropped
		// — there is nothing left for them to be an extra of.
		var keep map[string]string
		claimants := others
		if existingIdx >= 0 {
			prev := state.Worktrees[existingIdx]
			keep = prev.Pools
			for name, items := range prev.PoolExtras {
				if _, declared := cfg.Pools[name]; declared && len(items) > 0 {
					if extras == nil {
						extras = map[string][]string{}
					}
					extras[name] = append([]string{}, items...)
				}
			}
			if len(extras) > 0 {
				claimants = append(append([]WorktreeState{}, others...),
					WorktreeState{Branch: branchName, PoolExtras: extras})
			}
		}
		allocated, err := AllocatePoolsKeeping(cfg, claimants, keep, localRoot, headless)
		if err != nil {
			return err
		}
		pools = allocated

		// In the main repo there is nowhere to copy from — source and
		// destination are the same tree. This is a fast path, not the guard:
		// copyFromConfig refuses a self-copy on its own, because localRoot can
		// equal worktreePath in a worktree too (see samePath).
		if !isMain {
			copiedFiles, copySkipped = copyFromConfig(cfg, localRoot, worktreePath)
		}

		// CreatedAt is preserved on update so `wet worktree list`'s "age"
		// column reflects the original creation time, not the latest setup.
		entry := WorktreeState{
			Branch:    branchName,
			Path:      worktreePath,
			CreatedAt: time.Now(),
			Index:     slot,
		}
		if existingIdx >= 0 {
			entry.CreatedAt = state.Worktrees[existingIdx].CreatedAt
		}
		entry.Ports = make(map[string]int, len(cfg.Ports))
		for _, p := range cfg.ResolvePorts(slot) {
			entry.Ports[p.Name] = p.Value
		}
		if len(pools) > 0 {
			entry.Pools = make(map[string]string, len(pools))
			for k, v := range pools {
				entry.Pools[k] = v
			}
		}
		if len(extras) > 0 {
			entry.PoolExtras = extras
		}
		if existingIdx >= 0 {
			state.Worktrees[existingIdx] = entry
		} else {
			state.Worktrees = append(state.Worktrees, entry)
		}
		return saveConfig(state)
	}); err != nil {
		return err
	}

	if len(copiedFiles) > 0 {
		fmt.Printf("  Copied: %s\n", strings.Join(copiedFiles, ", "))
	}
	// Say so when copy: was declared but did nothing. Silence here is what
	// made the self-copy bug hard to spot: the step appeared not to exist.
	if copySkipped != "" && (len(cfg.Copy.Files) > 0 || len(cfg.Copy.Dirs) > 0) {
		fmt.Printf("  Skipped copy: %s\n", copySkipped)
	}

	// Bring claimed emulators up before anything renders: FLUTTER_DEVICE_ID
	// and friends want a serial, and the serial only exists once the AVD is
	// running. Physical devices pass straight through.
	handles, err := resolvePoolHandles(pools, headless)
	if err != nil {
		return err
	}
	// The extras come up too: a re-run is how a machine reboot gets every
	// device of a two-phone worktree back, not only the first.
	extraHandles, err := resolveExtraHandles(extras, headless)
	if err != nil {
		return err
	}

	ports := cfg.ResolvePorts(slot)
	data := WithPoolHandles(BuildTemplateData(cfg, branchName, slot, ports, pools), handles)
	WithPoolExtras(data, extras, extraHandles)
	if len(pools) > 0 {
		names := make([]string, 0, len(pools))
		for n := range pools {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Printf("  Allocated: pool %s = %s\n", n, pools[n])
			if more := extras[n]; len(more) > 0 {
				fmt.Printf("  Kept:      pool %s extras %s (from `wet mobile add`)\n", n, strings.Join(more, ", "))
			}
		}
	}

	local, err := LoadEnvLocalForTree(worktreePath, repoRoot)
	if err != nil {
		return err
	}
	if n := len(local); n > 0 {
		plural := "keys"
		if n == 1 {
			plural = "key"
		}
		fmt.Printf("  Overrides: %s (%d %s)\n", envLocalFileName, n, plural)
	}

	if err := writeEnvWorktree(cfg, worktreePath, data, ports, local); err != nil {
		return err
	}
	fmt.Printf("  Wrote: .env.worktree (slot %d)\n", slot)

	hookEnv, err := BuildHookEnv(cfg, data, ports, local)
	if err != nil {
		return fmt.Errorf("build hook env: %w", err)
	}
	// Generate the declared outputs before hooks run — a hook that boots the
	// stack needs its env files to already exist.
	//
	// `data` already carries every resolved `env:` value at this point:
	// RenderEnvFileContent above resolved them into it in place. That is a
	// real dependency and not an incidental one — reordering these two calls
	// would silently produce outputs missing every derived value, which is
	// why ResolveEnvValues is called explicitly here rather than relied on.
	//
	// This is the same pipeline `wet env pull` runs, called once lifecycle
	// allocation is done and .env.worktree is on disk. Setup owns the
	// lifecycle; it owns no second way to write an env file. The previous
	// design had setup render targets itself, which is how setup and populate
	// came to disagree about whether a target's secrets were fetched at all.
	if len(cfg.Outputs) > 0 {
		if _, err := ResolveEnvValues(cfg, data, local); err != nil {
			return fmt.Errorf("resolve env: %w", err)
		}
		if err := PullOutputs(PullRequest{
			Cfg:      cfg,
			TreePath: worktreePath,
			Data:     data,
		}); err != nil {
			return err
		}
	}

	// A hook that can't report failure defeats the abort below, so say so
	// before running any of them.
	for _, warning := range MaskedFailureHooks(cfg) {
		fmt.Fprintf(os.Stderr, "  warning: %s\n", warning)
	}

	for i, body := range cfg.Hooks.PostCreate {
		hookName := fmt.Sprintf("post_create[%d]", i)
		rendered, err := RenderHookCommand("hooks."+hookName, body, data)
		if err != nil {
			return fmt.Errorf("hooks.%s: render: %w", hookName, err)
		}
		if err := runWetHook("hooks."+hookName, rendered, worktreePath, hookEnv); err != nil {
			printSetupHookFailure(branchName, worktreePath, slot, hookName, rendered, err, isMain)
			return fmt.Errorf("post_create hook failed: %s", hookName)
		}
	}
	// Final summary.
	label := "Worktree configured"
	if isMain {
		label = "Main checkout configured"
	}
	fmt.Printf("\n  %s: %s\n", label, branchName)
	fmt.Printf("  Path:   %s\n", worktreePath)
	fmt.Printf("  Slot:   %d\n", slot)
	if len(ports) > 0 {
		names := make([]string, 0, len(ports))
		for _, p := range ports {
			names = append(names, fmt.Sprintf("%s=%d", p.Name, p.Value))
		}
		fmt.Printf("  Ports:  %s\n", strings.Join(names, ", "))
	}
	if len(pools) > 0 {
		names := make([]string, 0, len(pools))
		for n := range pools {
			names = append(names, fmt.Sprintf("%s=%s", n, strings.Join(append([]string{pools[n]}, extras[n]...), "+")))
		}
		sort.Strings(names)
		fmt.Printf("  Pools:  %s\n", strings.Join(names, ", "))
	}
	if n := len(cfg.Hooks.PostCreate); n > 0 {
		fmt.Printf("  Hooks:  %d post_create ran\n", n)
	}

	return nil
}

// ---------------------------------------------------------------------------
// wet setup — one bootstrap for both contexts
// ---------------------------------------------------------------------------

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Configure this checkout (main repo or worktree): ports, pools, env files, hooks",
	Long: `Bootstrap the checkout you are standing in.

In a worktree this is identical to ` + "`wet worktree setup`" + `: allocate a slot,
claim pool items, copy files, write .env.worktree, run post_create hooks.

In the main repo it does the same at slot 0 — the declared ` + "`base:`" + ` ports,
its own pool claim, its own rendered env files — minus the copy step, which
has no source to copy from. That is the point: a fresh clone and a fresh
worktree bootstrap with the same command, and a main checkout whose emulator
was deleted re-claims one by re-running setup instead of being hand-fixed.

Destructive lifecycle stays worktree-only: pre_remove hooks and ` + "`wet worktree pop`" + `
never run against the main checkout.`,
	RunE: runSetup,
}

func init() {
	setupCmd.Flags().Bool("headless", false, "Run emulators without a visible window (feeds into pool grow strategies)")
	rootCmd.AddCommand(setupCmd)
}

func runSetup(cmd *cobra.Command, args []string) error {
	treePath, err := getRepoRoot()
	if err != nil {
		return err
	}
	repoRoot, err := getMainRepoRoot()
	if err != nil {
		return err
	}
	absTree, _ := filepath.Abs(treePath)
	absMain, _ := filepath.Abs(repoRoot)
	isMain := absTree == absMain

	branchOut, err := exec.Command("git", "-C", treePath, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return fmt.Errorf("determine current branch: %w", err)
	}
	branchName := strings.TrimSpace(string(branchOut))
	if branchName == "" || branchName == "HEAD" {
		return fmt.Errorf("could not determine branch name for %s (detached HEAD?)", treePath)
	}

	headless, _ := cmd.Flags().GetBool("headless")
	return runSetupCore(repoRoot, treePath, branchName, headless, isMain)
}

// envWorktreeFileName is the dotenv `wet setup` renders into the tree root —
// every `env:` value, resolved for this checkout.
const envWorktreeFileName = ".env.worktree"

// writeEnvWorktree renders and writes that file.
//
// It exists as its own function because setup is no longer the only thing
// that invalidates it. `wet mobile add` changes what {{.AllHandles.devices}}
// resolves to, and a file left holding one device while the worktree drives
// two is worse than no file: it is a file that looks current. The render is
// the same one setup does — same data, same order, same `.env.local` layer —
// so there is one definition of the file's contents and no second way to
// write it.
func writeEnvWorktree(cfg *WetConfig, treePath string, data map[string]any, ports []ResolvedPort, local map[string]string) error {
	content, err := RenderEnvFileContent(cfg, data, ports, local)
	if err != nil {
		return fmt.Errorf("render %s: %w", envWorktreeFileName, err)
	}
	path := filepath.Join(treePath, envWorktreeFileName)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", envWorktreeFileName, err)
	}
	return nil
}
