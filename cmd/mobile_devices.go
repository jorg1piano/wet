package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// `wet mobile add` / `remove` / `list` — a worktree's devices beyond the one
// `wet setup` claimed.
//
// Setup claims one item per pool, and one device per worktree is the right
// default: it is what makes `{{.Handles.devices}}` a single value that
// `flutter run -d` can take. But some work needs two phones on one stack — a
// chat between two accounts, a share flow, anything where the second user
// is the point. That is not a second worktree (same branch, same backend,
// same database) and it is not a second pool (the devices are
// interchangeable). It is the same worktree holding one more claim.
//
// An extra is exactly that: a claim recorded beside the primary, honoured by
// the allocator so no other worktree gets the device, reversed onto by
// `wet mobile reverse-tcp`, exposed to hooks as `$POOL_DEVICES_ALL` /
// `$POOL_HANDLE_DEVICES_ALL`, and released with the worktree. What it never
// does is change what the primary renders.

var mobileAddCmd = &cobra.Command{
	Use:   "add [ITEM]",
	Short: "Claim another device for this worktree",
	Long: `Claim one more device for this worktree, boot it if it is an emulator,
and reverse this worktree's ports onto it — so a second phone reaches the same
stack the first one does.

  wet mobile add                      the next free item of the devices pool
  wet mobile add emulator:pixel-b     a specific pool item (or any AVD)
  wet mobile add RFCR921L4DP          an attached adb serial
  wet mobile add --headless           boot the emulator without a window

With no ITEM the pool is drawn from like ` + "`wet setup`" + ` does: the first item no
worktree holds, or a grown one when pool_grow: has a strategy. A named ITEM
must be a declared pool item, an ` + "`emulator:<AVD>`" + `, or a device adb can see,
and must not be claimed by any worktree.

The device joins this worktree's claims: ` + "`wet mobile reverse-tcp`" + ` reverses
onto it, ` + "`wet setup`" + ` boots it again on a re-run, and ` + "`wet worktree pop`" + ` releases
it. Hooks and templates see every claim, primary first, as
$POOL_DEVICES_ALL / $POOL_HANDLE_DEVICES_ALL and {{.AllPools.devices}} /
{{.AllHandles.devices}}. {{.Handles.devices}} keeps meaning the primary, so
nothing that renders a single device changes.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runMobileAdd,
}

var mobileEnsureCmd = &cobra.Command{
	Use:   "ensure --count N",
	Short: "Ensure this worktree claims N devices",
	Long: `Ensure this worktree has a fixed number of devices claimed, booted, and
reversed onto its ports.

  wet mobile ensure --count 4
  wet mobile ensure -n 4 --headless
  wet mobile ensure --count 4 --no-reverse

The primary device from ` + "`wet setup`" + ` counts as one. Missing devices are added
with the same allocator as ` + "`wet mobile add`" + `, so predeclared free pool items are
reused before pool_grow creates new AVDs. Existing extra devices are kept; this
command never releases devices just because the count is lower. Use
` + "`wet mobile remove`" + ` for that explicit teardown.`,
	Args: cobra.NoArgs,
	RunE: runMobileEnsure,
}

var mobileRemoveCmd = &cobra.Command{
	Use:   "remove ITEM|SERIAL",
	Short: "Release a device this worktree added",
	Long: `Release a device ` + "`wet mobile add`" + ` claimed, named by its pool item or by
the serial it is running as. An emulator is stopped as well, unless
--keep-running; a physical device has this worktree's reverses removed.

The primary device — the one ` + "`wet setup`" + ` claimed and {{.Handles.devices}}
renders — is not removable here: it is released with the worktree by
` + "`wet worktree pop`" + `.`,
	Args: cobra.ExactArgs(1),
	RunE: runMobileRemove,
}

var mobileListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls", "devices"},
	Short:   "Show the devices this worktree claims",
	Long: `List this worktree's claimed devices: pool, item, the address each answers
to right now, whether it is the primary, and whether it is up. Nothing is
booted to answer.

  wet mobile list            this worktree
  wet mobile list --all      every worktree wet tracks
  wet mobile list --json     for scripts: [{pool,item,handle,primary,running}]`,
	Args: cobra.NoArgs,
	RunE: runMobileList,
}

var (
	mobileAddPool         string
	mobileAddHeadless     bool
	mobileAddNoReverse    bool
	mobileEnsureCount     int
	mobileEnsurePool      string
	mobileEnsureHeadless  bool
	mobileEnsureNoReverse bool
	mobileRemovePool      string
	mobileRemoveKeepRun   bool
	mobileListAll         bool
	mobileListJSON        bool
)

func init() {
	mobileAddCmd.Flags().StringVar(&mobileAddPool, "pool", "",
		"pool to claim from (default: `devices`, or the only pool declared)")
	mobileAddCmd.Flags().BoolVar(&mobileAddHeadless, "headless", false,
		"boot an emulator without a visible window")
	mobileAddCmd.Flags().BoolVar(&mobileAddNoReverse, "no-reverse", false,
		"claim and boot only; skip `adb reverse` for this worktree's ports")
	mobileEnsureCmd.Flags().IntVarP(&mobileEnsureCount, "count", "n", 0,
		"total number of devices this worktree should claim, including the primary")
	mobileEnsureCmd.Flags().StringVar(&mobileEnsurePool, "pool", "",
		"pool to claim from (default: `devices`, or the only pool declared)")
	mobileEnsureCmd.Flags().BoolVar(&mobileEnsureHeadless, "headless", false,
		"boot any added emulators without visible windows")
	mobileEnsureCmd.Flags().BoolVar(&mobileEnsureNoReverse, "no-reverse", false,
		"claim and boot only; skip `adb reverse` for this worktree's ports")
	mobileRemoveCmd.Flags().StringVar(&mobileRemovePool, "pool", "",
		"pool the item belongs to (default: search every pool)")
	mobileRemoveCmd.Flags().BoolVar(&mobileRemoveKeepRun, "keep-running", false,
		"release the claim but leave an emulator running")
	mobileListCmd.Flags().BoolVar(&mobileListAll, "all", false,
		"list every worktree's devices, not only this one's")
	mobileListCmd.Flags().BoolVar(&mobileListJSON, "json", false,
		"write the list as JSON")
	mobileCmd.AddCommand(mobileAddCmd, mobileEnsureCmd, mobileRemoveCmd, mobileListCmd)
}

// loadConfigForDevices is loadConfigForTree with the error naming the
// command that needs the config.
func loadConfigForDevices(command string) (*WetConfig, string, string, error) {
	treePath, err := getRepoRoot()
	if err != nil {
		return nil, "", "", err
	}
	repoRoot, err := getMainRepoRoot()
	if err != nil {
		return nil, "", "", err
	}
	cfg, _, err := LoadWetConfigForWorktree(treePath, repoRoot)
	if err == ErrConfigMissing {
		return nil, "", "", fmt.Errorf("`%s` requires a .wet.yaml (none at %s or %s)", command, treePath, repoRoot)
	}
	if err != nil {
		return nil, "", "", err
	}
	return cfg, treePath, repoRoot, nil
}

// devicePoolName picks the pool a device claim comes from. `devices` is the
// convention every example and the skill use, so it wins when declared; a
// config with exactly one pool has no other candidate; anything else has to
// be named, because guessing between `phones` and `fixtures` would claim a
// database snapshot as a phone.
func devicePoolName(cfg *WetConfig, flag string) (string, error) {
	names := make([]string, 0, len(cfg.Pools))
	for name := range cfg.Pools {
		names = append(names, name)
	}
	sort.Strings(names)
	if flag != "" {
		if _, ok := cfg.Pools[flag]; !ok {
			return "", fmt.Errorf("no pool named %q is declared (have: %s)", flag, strings.Join(names, ", "))
		}
		return flag, nil
	}
	if _, ok := cfg.Pools["devices"]; ok {
		return "devices", nil
	}
	switch len(names) {
	case 0:
		return "", fmt.Errorf("no pools: declared — a device claim needs a pool to come from. Declare one:\n" +
			"  pools:\n" +
			"    devices: []\n" +
			"  pool_grow:\n" +
			"    devices: android-emulator\n" +
			"(the pool itself belongs in .wet.local.yaml when it lists this machine's devices)")
	case 1:
		return names[0], nil
	}
	return "", fmt.Errorf("several pools are declared (%s) — pass --pool to say which holds devices",
		strings.Join(names, ", "))
}

// deviceClaim is one row of `wet mobile list`.
type deviceClaim struct {
	Branch  string `json:"branch,omitempty"`
	Pool    string `json:"pool"`
	Item    string `json:"item"`
	Handle  string `json:"handle"`
	Primary bool   `json:"primary"`
	// Running is whether Handle is an address adb can reach now: a booted
	// emulator, or a serial in the attached list. attached may be nil when
	// adb could not be asked, in which case a non-emulator item is reported
	// as-is and Running is false.
	Running bool `json:"running"`
}

// claimsOf lists a worktree's device claims in the order orderedPoolClaims
// gives them, with handles peeked (never booted).
func claimsOf(ws WorktreeState, attached map[string]bool) []deviceClaim {
	var out []deviceClaim
	for _, c := range orderedPoolClaims(ws.Pools, ws.PoolExtras) {
		handle, running := peekItemHandle(c.Item)
		if !strings.HasPrefix(c.Item, emulatorItemPrefix) {
			running = attached[c.Item]
		}
		out = append(out, deviceClaim{
			Branch:  ws.Branch,
			Pool:    c.Pool,
			Item:    c.Item,
			Handle:  handle,
			Primary: ws.Pools[c.Pool] == c.Item,
			Running: running,
		})
	}
	return out
}

// attachedSet asks adb once for the attached serials. A missing or failing
// adb is not fatal to a listing — the answer is then "unknown", reported by
// the caller — but it is to a claim, which needs the answer.
func attachedSet() (map[string]bool, error) {
	serials, err := listAttachedSerials()
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(serials))
	for _, s := range serials {
		set[s] = true
	}
	return set, nil
}

// --- add ---

func runMobileAdd(cmd *cobra.Command, args []string) error {
	cfg, treePath, repoRoot, err := loadConfigForDevices("wet mobile add")
	if err != nil {
		return err
	}
	pool, err := devicePoolName(cfg, mobileAddPool)
	if err != nil {
		return err
	}
	if err := requireADB(); err != nil {
		return err
	}
	attached, err := attachedSet()
	if err != nil {
		return err
	}
	// Grown pool items are appended to the machine's .wet.local.yaml — the
	// same file setup grows into, found the same way.
	localRoot := LocalConfigDir(treePath, repoRoot)

	requested := ""
	if len(args) == 1 {
		requested = strings.TrimSpace(args[0])
		if requested == "" {
			return fmt.Errorf("ITEM must not be empty")
		}
	}

	var item string
	var branch string
	if err := withStateLock(func() error {
		state, err := loadConfig()
		if err != nil {
			return fmt.Errorf("load wet state: %w", err)
		}
		own := worktreeStateForPath(state, treePath)
		if own == nil {
			return fmt.Errorf("no wet state for %s — run `wet setup` first; it claims the first device, "+
				"`wet mobile add` claims the next", treePath)
		}
		branch = own.Branch
		if requested != "" {
			if err := checkClaimable(cfg, pool, requested, state.Worktrees, own, attached); err != nil {
				return err
			}
			item = requested
		} else {
			// Every entry counts, this worktree's included: its primary and
			// its extras are claimed, and the next free item is what we want.
			picked, err := allocatePoolItem(cfg, pool, state.Worktrees, localRoot, mobileAddHeadless)
			if err != nil {
				return err
			}
			item = picked
		}
		if own.PoolExtras == nil {
			own.PoolExtras = map[string][]string{}
		}
		own.PoolExtras[pool] = append(own.PoolExtras[pool], item)
		return saveConfig(state)
	}); err != nil {
		return err
	}
	fmt.Printf("  Claimed: pool %s += %s (for %s)\n", pool, item, branch)

	// The claim is recorded before the boot, as setup records before hooks:
	// a boot that fails leaves a claim `wet mobile remove` can release, not
	// a device two worktrees can race for.
	handle, err := resolveItemHandle(item, mobileAddHeadless)
	if err != nil {
		return fmt.Errorf("%w\n  The claim is recorded. Fix the cause and re-run `wet setup`, or "+
			"release it with `wet mobile remove %s`", err, item)
	}
	reachable := strings.HasPrefix(item, emulatorItemPrefix) || attached[item]
	switch {
	case !reachable:
		fmt.Printf("  %s is not attached; reverses skipped until it is (then `wet mobile reverse-tcp`)\n", item)
	case mobileAddNoReverse:
		fmt.Printf("  Reverses skipped (--no-reverse); `wet mobile reverse-tcp` adds them\n")
	default:
		alloc, err := readWorktreeAllocation(cfg, treePath)
		if err != nil {
			return err
		}
		if len(alloc.Ports) == 0 {
			fmt.Println("  No allocated ports to reverse — declare ports: in .wet.yaml and run `wet setup`.")
		} else {
			fmt.Printf("  Reversing this worktree's ports onto %s:\n", handle)
			if err := reversePorts(handle, alloc.Ports, false); err != nil {
				return fmt.Errorf("%w\n  The device is claimed and up; re-run `wet mobile reverse-tcp` once adb answers", err)
			}
		}
	}

	regenerateAfterClaimChange(cfg, treePath, repoRoot)
	fmt.Printf("\n  Added %s as %s\n", item, handle)
	printDeviceSummary(treePath, attached)
	fmt.Printf("  Run on it:  flutter run -d %s   (hooks: $POOL_HANDLE_%s_ALL)\n", handle, strings.ToUpper(pool))
	return nil
}

// --- ensure ---

func runMobileEnsure(cmd *cobra.Command, args []string) error {
	if mobileEnsureCount < 1 {
		return fmt.Errorf("--count must be at least 1 (the primary device from `wet setup` counts as one)")
	}
	cfg, treePath, repoRoot, err := loadConfigForDevices("wet mobile ensure")
	if err != nil {
		return err
	}
	pool, err := devicePoolName(cfg, mobileEnsurePool)
	if err != nil {
		return err
	}
	if err := requireADB(); err != nil {
		return err
	}
	attached, err := attachedSet()
	if err != nil {
		return err
	}
	localRoot := LocalConfigDir(treePath, repoRoot)

	var branch string
	var added []string
	var claims []string
	if err := withStateLock(func() error {
		state, err := loadConfig()
		if err != nil {
			return fmt.Errorf("load wet state: %w", err)
		}
		own := worktreeStateForPath(state, treePath)
		if own == nil {
			return fmt.Errorf("no wet state for %s — run `wet setup` first; it claims the primary device", treePath)
		}
		branch = own.Branch
		current := len(own.poolClaims(pool))
		if own.PoolExtras == nil {
			own.PoolExtras = map[string][]string{}
		}
		for current < mobileEnsureCount {
			item, err := allocatePoolItem(cfg, pool, state.Worktrees, localRoot, mobileEnsureHeadless)
			if err != nil {
				return err
			}
			own.PoolExtras[pool] = append(own.PoolExtras[pool], item)
			added = append(added, item)
			current++
		}
		claims = append([]string{}, own.poolClaims(pool)...)
		if len(added) == 0 {
			return nil
		}
		return saveConfig(state)
	}); err != nil {
		return err
	}

	if len(added) == 0 {
		fmt.Printf("  Already claimed %d devices for %s; checking boot/reverse on all of them\n", len(claims), branch)
	} else {
		fmt.Printf("  Ensuring %d devices for %s: added %s\n", mobileEnsureCount, branch, strings.Join(added, ", "))
	}

	alloc, err := readWorktreeAllocation(cfg, treePath)
	if err != nil {
		return err
	}
	for _, item := range claims {
		handle, err := resolveItemHandle(item, mobileEnsureHeadless)
		if err != nil {
			return fmt.Errorf("%w\n  The claim is recorded. Fix the cause and re-run `wet mobile ensure --count %d`, or release extras with `wet mobile remove ITEM`", err, mobileEnsureCount)
		}
		if mobileEnsureNoReverse {
			fmt.Printf("  %s is %s; reverses skipped (--no-reverse)\n", item, handle)
			continue
		}
		if len(alloc.Ports) == 0 {
			fmt.Println("  No allocated ports to reverse — declare ports: in .wet.yaml and run `wet setup`.")
			continue
		}
		fmt.Printf("  Reversing this worktree's ports onto %s:\n", handle)
		if err := reversePorts(handle, alloc.Ports, false); err != nil {
			return fmt.Errorf("%w\n  The device is claimed and up; re-run `wet mobile ensure --count %d` once adb answers", err, mobileEnsureCount)
		}
	}

	regenerateAfterClaimChange(cfg, treePath, repoRoot)
	attached, _ = attachedSet()
	printDeviceSummary(treePath, attached)
	return nil
}

// checkClaimable decides whether a named item may become this worktree's
// extra: it must be something wet can make sense of — a declared pool item,
// an emulator item, or a device adb sees — and nobody may hold it already.
// An item this worktree itself holds is refused too, because the ask was for
// *another* device.
func checkClaimable(cfg *WetConfig, pool, item string, all []WorktreeState, own *WorktreeState, attached map[string]bool) error {
	for i := range all {
		ws := all[i]
		for _, claimed := range ws.poolClaims(pool) {
			if claimed != item {
				continue
			}
			if resolvePath(ws.Path) == resolvePath(own.Path) {
				role := "an extra"
				if ws.Pools[pool] == item {
					role = "the primary"
				}
				return fmt.Errorf("%s is already this worktree's device (%s) — `wet mobile add` claims another; "+
					"see `wet mobile list`", item, role)
			}
			return fmt.Errorf("%s is claimed by worktree '%s' (%s) — pop it, or `wet mobile remove` it there first",
				item, ws.Branch, ws.Path)
		}
	}
	if slices.Contains(cfg.Pools[pool], item) || strings.HasPrefix(item, emulatorItemPrefix) || attached[item] {
		return nil
	}
	serials := make([]string, 0, len(attached))
	for s := range attached {
		serials = append(serials, s)
	}
	sort.Strings(serials)
	return fmt.Errorf("%s is not an item of pool %q, not an emulator:<AVD>, and not an attached adb device "+
		"(attached: %s) — wet cannot tell what it is, so it cannot boot or reverse onto it",
		item, pool, strings.Join(serials, ", "))
}

// --- remove ---

func runMobileRemove(cmd *cobra.Command, args []string) error {
	cfg, treePath, repoRoot, err := loadConfigForDevices("wet mobile remove")
	if err != nil {
		return err
	}
	query := strings.TrimSpace(args[0])
	if query == "" {
		return fmt.Errorf("ITEM must not be empty")
	}
	if mobileRemovePool != "" {
		if _, ok := cfg.Pools[mobileRemovePool]; !ok {
			return fmt.Errorf("no pool named %q is declared", mobileRemovePool)
		}
	}

	var released deviceClaim
	if err := withStateLock(func() error {
		state, err := loadConfig()
		if err != nil {
			return fmt.Errorf("load wet state: %w", err)
		}
		own := worktreeStateForPath(state, treePath)
		if own == nil {
			return fmt.Errorf("no wet state for %s — nothing is claimed here", treePath)
		}
		match, err := matchExtraClaim(*own, mobileRemovePool, query)
		if err != nil {
			return err
		}
		released = match
		own.PoolExtras[match.Pool] = slices.DeleteFunc(own.PoolExtras[match.Pool], func(it string) bool {
			return it == match.Item
		})
		if len(own.PoolExtras[match.Pool]) == 0 {
			delete(own.PoolExtras, match.Pool)
		}
		if len(own.PoolExtras) == 0 {
			own.PoolExtras = nil
		}
		return saveConfig(state)
	}); err != nil {
		return err
	}
	fmt.Printf("  Released: pool %s -= %s\n", released.Pool, released.Item)

	// What `add` started, `remove` stops — the emulator, or the reverses on
	// a phone. Neither failure un-releases the claim: the state is what the
	// allocator reads, and it is already right.
	switch {
	case strings.HasPrefix(released.Item, emulatorItemPrefix) && !released.Running:
		fmt.Printf("  %s is not running; nothing to stop\n", released.Item)
	case strings.HasPrefix(released.Item, emulatorItemPrefix) && mobileRemoveKeepRun:
		fmt.Printf("  %s left running (--keep-running)\n", released.Handle)
	case strings.HasPrefix(released.Item, emulatorItemPrefix):
		if err := killEmulator(released.Handle); err != nil {
			fmt.Printf("  Could not stop %s (%v); it is released either way\n", released.Handle, err)
		} else {
			fmt.Printf("  Stopped %s\n", released.Handle)
		}
	case released.Running:
		alloc, err := readWorktreeAllocation(cfg, treePath)
		if err == nil && len(alloc.Ports) > 0 {
			fmt.Printf("  Removing this worktree's reverses from %s:\n", released.Handle)
			if err := reversePorts(released.Handle, alloc.Ports, true); err != nil {
				fmt.Printf("  Reverse removal incomplete (%v); the device is released either way\n", err)
			}
		}
	}

	regenerateAfterClaimChange(cfg, treePath, repoRoot)
	attached, _ := attachedSet()
	printDeviceSummary(treePath, attached)
	return nil
}

// matchExtraClaim finds the extra a `remove` names: by item, or by the
// serial a running emulator answers to. The primary matches too, on purpose,
// so the refusal can say what it is instead of "no such extra".
func matchExtraClaim(ws WorktreeState, pool, query string) (deviceClaim, error) {
	attached, _ := attachedSet()
	var extras []string
	for _, c := range claimsOf(ws, attached) {
		if pool != "" && c.Pool != pool {
			continue
		}
		if c.Item != query && c.Handle != query {
			if !c.Primary {
				extras = append(extras, c.Item)
			}
			continue
		}
		if c.Primary {
			return deviceClaim{}, fmt.Errorf("%s is this worktree's primary device (pool %s) — it is what "+
				"{{.Handles.%s}} renders, and `wet worktree pop` releases it. Only devices `wet mobile add` "+
				"claimed can be removed", c.Item, c.Pool, c.Pool)
		}
		return c, nil
	}
	if len(extras) == 0 {
		return deviceClaim{}, fmt.Errorf("%s is not an extra device of this worktree — none is claimed beyond the primary; "+
			"`wet mobile list` shows what is", query)
	}
	return deviceClaim{}, fmt.Errorf("%s is not an extra device of this worktree (extras: %s) — name the item or its serial",
		query, strings.Join(extras, ", "))
}

// --- list ---

func runMobileList(cmd *cobra.Command, args []string) error {
	cfg, treePath, _, err := loadConfigForDevices("wet mobile list")
	if err != nil {
		return err
	}
	state, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load wet state: %w", err)
	}
	attached, adbErr := attachedSet()

	var rows []deviceClaim
	if mobileListAll {
		for _, ws := range state.Worktrees {
			rows = append(rows, claimsOf(ws, attached)...)
		}
	} else {
		own := worktreeStateForPath(state, treePath)
		if own == nil {
			return fmt.Errorf("no wet state for %s — run `wet setup` first", treePath)
		}
		rows = claimsOf(*own, attached)
	}

	if mobileListJSON {
		if rows == nil {
			rows = []deviceClaim{}
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	if adbErr != nil {
		fmt.Printf("  adb is unavailable (%v); attached devices could not be checked\n", adbErr)
	}
	if len(rows) == 0 {
		if len(cfg.Pools) == 0 {
			fmt.Println("  No devices: this config declares no pools.")
		} else {
			fmt.Println("  No devices claimed. `wet setup` claims the first, `wet mobile add` the next.")
		}
		return nil
	}
	printClaimTable(rows, mobileListAll)
	return nil
}

func printClaimTable(rows []deviceClaim, withBranch bool) {
	for _, r := range rows {
		role := "extra"
		if r.Primary {
			role = "primary"
		}
		status := "stopped"
		if r.Running {
			status = "running"
		}
		if !strings.HasPrefix(r.Item, emulatorItemPrefix) {
			status = "not attached"
			if r.Running {
				status = "attached"
			}
		}
		if withBranch {
			fmt.Printf("  %-28s %-9s %-26s %-16s %-8s %s\n", r.Branch, r.Pool, r.Item, r.Handle, role, status)
			continue
		}
		fmt.Printf("  %-9s %-26s %-16s %-8s %s\n", r.Pool, r.Item, r.Handle, role, status)
	}
}

// printDeviceSummary shows where this worktree stands after a claim change.
func printDeviceSummary(treePath string, attached map[string]bool) {
	state, err := loadConfig()
	if err != nil {
		return
	}
	own := worktreeStateForPath(state, treePath)
	if own == nil {
		return
	}
	rows := claimsOf(*own, attached)
	if len(rows) == 0 {
		return
	}
	fmt.Println("  Devices:")
	printClaimTable(rows, false)
}

// regenerateAfterClaimChange re-renders .env.worktree and the declared
// outputs, so a file carrying {{.AllHandles.devices}} names the device that
// was just added or released instead of the set from the last setup.
//
// It cannot fail the command: the claim is recorded and the device is in the
// state the user asked for, so a signed-out secret store is a reason to run
// `wet env pull` later, not to report the add as failed. Nothing is booted to
// render — the handles are peeked, exactly as a pull peeks them.
func regenerateAfterClaimChange(cfg *WetConfig, treePath, repoRoot string) {
	err := func() error {
		alloc, err := readWorktreeAllocation(cfg, treePath)
		if err != nil {
			return err
		}
		local, err := LoadEnvLocalForTree(treePath, repoRoot)
		if err != nil {
			return err
		}
		data := allocationTemplateData(cfg, alloc)
		if _, err := ResolveEnvValues(cfg, data, local); err != nil {
			return err
		}
		if err := writeEnvWorktree(cfg, treePath, data, alloc.Ports, local); err != nil {
			return err
		}
		if !cfg.hasAutomaticOutputs() {
			return nil
		}
		return PullOutputs(PullRequest{Cfg: cfg, TreePath: treePath, Data: data})
	}()
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Env files not regenerated (%v); `wet setup` will.\n", err)
		return
	}
	fmt.Printf("  Regenerated: %s%s\n", envWorktreeFileName, outputCountSuffix(cfg))
}

func outputCountSuffix(cfg *WetConfig) string {
	outputs, err := cfg.SelectOutputs(nil)
	if err != nil {
		return ""
	}
	outputs, err = cfg.withFileDependencies(outputs)
	if err != nil {
		return ""
	}
	switch n := len(outputs); n {
	case 0:
		return ""
	case 1:
		return " and 1 output"
	default:
		return fmt.Sprintf(" and %d outputs", n)
	}
}
