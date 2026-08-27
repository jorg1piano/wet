package cmd

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

var mobileCmd = &cobra.Command{
	Use:   "mobile",
	Short: "Work with the devices a worktree drives",
}

var mobileReverseTCPCmd = &cobra.Command{
	Use:   "reverse-tcp [PORT_NAME...]",
	Short: "Point a device's localhost at this worktree's ports",
	Long: `Run ` + "`adb reverse`" + ` for the ports this worktree was allocated.

An Android app talks to ` + "`localhost`" + `, but on a device that is the device.
` + "`adb reverse tcp:P tcp:P`" + ` forwards it back to the host — and which P is
exactly what a wet worktree already knows: every declared port resolved for
this checkout's slot, read from .wet-worktrees.json (or from the hook
environment when a post_create hook invokes it).

  wet mobile reverse-tcp                  every allocated port
  wet mobile reverse-tcp APP_PORT         only the named ones
  wet mobile reverse-tcp --dry-run        print the adb commands, change nothing
  wet mobile reverse-tcp --remove         undo the same set

The target is this worktree's own claimed devices: each pool item that names
an emulator (booted if it isn't running) plus each item that is an attached
adb serial. --device overrides that and targets exactly what you name.`,
	Args: cobra.ArbitraryArgs,
	RunE: runMobileReverseTCP,
}

var (
	mobileReverseDevices  []string
	mobileReverseDryRun   bool
	mobileReverseRemove   bool
	mobileReverseHeadless bool
)

func init() {
	mobileReverseTCPCmd.Flags().StringSliceVar(&mobileReverseDevices, "device", nil,
		"adb serial to target (repeatable); overrides this worktree's claimed devices")
	mobileReverseTCPCmd.Flags().BoolVar(&mobileReverseDryRun, "dry-run", false,
		"print the adb commands without changing anything")
	mobileReverseTCPCmd.Flags().BoolVar(&mobileReverseRemove, "remove", false,
		"remove the reverses instead of adding them")
	mobileReverseTCPCmd.Flags().BoolVar(&mobileReverseHeadless, "headless", false,
		"boot a claimed emulator without a visible window (ignored with --device or --dry-run, which boot nothing)")
	mobileCmd.AddCommand(mobileReverseTCPCmd)
	rootCmd.AddCommand(mobileCmd)
}

func runMobileReverseTCP(cmd *cobra.Command, args []string) error {
	cfg, treePath, err := loadConfigForTree()
	if err != nil {
		return err
	}
	alloc, err := readWorktreeAllocation(cfg, treePath)
	if err != nil {
		return err
	}
	ports, err := selectAllocatedPorts(alloc, args)
	if err != nil {
		return err
	}

	// The adb guard comes before any resolution that will need adb: without
	// it a machine missing the toolchain reaches the "no device claimed"
	// error, which blames the pool for a missing binary.
	if !mobileReverseDryRun {
		if err := requireADB(); err != nil {
			return err
		}
	}

	// A dry run resolves what it can and boots nothing: the command exists to
	// answer "what would this do", and starting an emulator to answer it is
	// the one thing the flag promises not to do. An unbooted AVD then has no
	// serial, so it is shown as its pool item, labelled as such.
	targets, err := reverseTargets(reverseRequest{
		Pools:    alloc.Pools,
		Extras:   alloc.Extras,
		Devices:  mobileReverseDevices,
		DryRun:   mobileReverseDryRun,
		Headless: mobileReverseHeadless,
	})
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		if mobileReverseDryRun {
			fmt.Println("  no device claimed by this worktree; a real run would need --device <serial>")
			return nil
		}
		return fmt.Errorf("no device to reverse onto: this worktree claims no emulator or attached "+
			"adb device (pools: %s) — pass --device <serial>, or claim one with `wet mobile add`",
			describeClaims(alloc.Pools, alloc.Extras))
	}

	for _, target := range targets {
		if mobileReverseDryRun {
			printReversePlan(target, ports, mobileReverseRemove)
			continue
		}
		if err := reversePorts(target.Address, ports, mobileReverseRemove); err != nil {
			return err
		}
	}
	return nil
}

// runADBReverse is a seam over the one command this file actually executes:
// tests replace it so the unit suite never talks to a device.
var runADBReverse = execADBReverse

func execADBReverse(args []string) (string, error) {
	out, err := exec.Command("adb", args...).CombinedOutput()
	return string(out), err
}

// reversePorts issues one `adb reverse` per port on serial and prints what it
// did. It is the single place a reverse is applied, so `wet mobile setup`
// finishing the job for the device it just picked cannot drift from what
// `wet mobile reverse-tcp` does with the same ports.
func reversePorts(serial string, ports []ResolvedPort, remove bool) error {
	for _, p := range ports {
		adbArgs := adbReverseArgs(serial, p.Value, remove)
		out, err := runADBReverse(adbArgs)
		if err != nil {
			// Removing a reverse that isn't there is the desired end state,
			// not a failure: the command has to stay re-runnable after a
			// device reboot dropped the forwards on its own. A missing
			// *device* is not that, so the match is on the listener error
			// specifically.
			if remove && isMissingListener(out, p.Value) {
				fmt.Printf("  %s %s (%d): already absent\n", serial, p.Name, p.Value)
				continue
			}
			return fmt.Errorf("adb %s: %w: %s", strings.Join(adbArgs, " "), err,
				strings.TrimSpace(out))
		}
		verb := "→"
		if remove {
			verb = "✗"
		}
		fmt.Printf("  %s %s %s tcp:%d\n", serial, verb, p.Name, p.Value)
	}
	return nil
}

// printReversePlan is the --dry-run half: it prints the adb commands and
// executes nothing.
func printReversePlan(target reverseTarget, ports []ResolvedPort, remove bool) {
	for _, p := range ports {
		note := p.Name
		if !target.Booted {
			// Say it plainly: this line is not copy-pasteable, because the AVD
			// has no serial until something boots it.
			note += fmt.Sprintf("; %s is not booted, its serial is assigned at boot", target.Address)
		}
		fmt.Printf("  adb %s   # %s\n", strings.Join(adbReverseArgs(target.Address, p.Value, remove), " "), note)
	}
}

// adbReverseArgs builds one `adb` invocation. A reverse maps the same number
// on both sides — the app is compiled against a localhost port and the point
// is to make that exact port answer — so the port appears twice.
func adbReverseArgs(serial string, port int, remove bool) []string {
	args := []string{"-s", serial, "reverse"}
	if remove {
		return append(args, "--remove", fmt.Sprintf("tcp:%d", port))
	}
	return append(args, fmt.Sprintf("tcp:%d", port), fmt.Sprintf("tcp:%d", port))
}

// isMissingListener reports whether adb's failure was "there was no such
// reverse", as opposed to any other error.
//
// adb answers `error: listener 'tcp:4020' not found` for an absent forward and
// `error: device 'X' not found` for an absent device — both contain "not
// found", and treating the second as success would report a removal on a
// device that was never reachable. The port is required to match too, so a
// stale message about a different listener cannot pass either.
func isMissingListener(output string, port int) bool {
	return strings.Contains(output, fmt.Sprintf("listener 'tcp:%d' not found", port))
}

// selectAllocatedPorts narrows the worktree's ports to the names given on the
// command line, keeping the declared order. No names means every allocated
// port: the common case is "make this whole worktree reachable from the app".
func selectAllocatedPorts(alloc worktreeAllocation, names []string) ([]ResolvedPort, error) {
	if len(alloc.Ports) == 0 {
		return nil, fmt.Errorf("this worktree has no allocated ports — declare ports: in .wet.yaml and run `wet setup`")
	}
	if len(names) == 0 {
		return alloc.Ports, nil
	}
	out := make([]ResolvedPort, 0, len(names))
	// A name repeated on the command line is one port: reversing it twice
	// prints a duplicate line, and under --remove the second pass reports the
	// port it just removed as already absent.
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		value, ok := alloc.Port(name)
		if !ok {
			return nil, fmt.Errorf("port %q is not allocated to this worktree (have: %s) — "+
				"a port added to .wet.yaml since this worktree was set up needs `wet setup` to allocate it",
				name, strings.Join(allocatedPortNames(alloc), ", "))
		}
		out = append(out, ResolvedPort{Name: name, Value: value})
	}
	return out, nil
}

func allocatedPortNames(alloc worktreeAllocation) []string {
	names := make([]string, 0, len(alloc.Ports))
	for _, p := range alloc.Ports {
		names = append(names, p.Name)
	}
	return names
}

// reverseRequest is everything reverseTargets needs, so the resolution is a
// function of its arguments rather than of the flag globals — the flags are
// read once, in RunE, as every other command in this package does it.
type reverseRequest struct {
	Pools   map[string]string
	Extras  map[string][]string
	Devices []string
	// DryRun resolves without starting anything and tolerates an adb that
	// cannot answer. It is named for the flag rather than for "don't boot",
	// because the boot is only one of the things it must not do.
	DryRun   bool
	Headless bool
}

// reverseTarget is one device to reverse onto. Booted distinguishes a live adb
// serial from an emulator item that has no serial yet, which only a dry run
// can produce and only the dry-run output has to explain.
type reverseTarget struct {
	Address string
	Booted  bool
}

// reverseTargets decides which devices to reverse onto.
//
// Devices is taken literally, because naming a serial is the escape hatch for
// a device wet knows nothing about. Otherwise only this worktree's own claims
// count — the primary pick per pool and every extra `wet mobile add` recorded:
// unless DryRun is set, an emulator item is booted and resolved to its serial
// (a dry run peeks instead, see peekItemHandle), and any other item is used
// only when it is actually an attached adb serial — a `fixtures` pool holding
// "snap-alpha" must not become an adb argument.
func reverseTargets(req reverseRequest) ([]reverseTarget, error) {
	if len(req.Devices) > 0 {
		targets := dedupeTargets(bootedTargets(req.Devices))
		if len(targets) == 0 {
			// Falling through would report "this worktree claims no device" to
			// someone who just named one.
			return nil, fmt.Errorf("every --device value was empty")
		}
		return targets, nil
	}
	// A real run has already passed requireADB, so a failure here is a working
	// adb that could not answer — a dead daemon, an unauthorized server. That
	// has to be reported: swallowing it drops every physical-serial pool item
	// out of the classification below, and the user is then told the pool
	// claims no device while the error message prints the device it claims.
	// A dry run is allowed to be vaguer, because it changes nothing and must
	// still work on a machine with no toolchain at all.
	attached := map[string]bool{}
	serials, err := listAttachedSerials()
	if err != nil {
		if !req.DryRun {
			return nil, err
		}
		fmt.Printf("  adb is unavailable (%v), so attached devices could not be checked\n", err)
	}
	for _, serial := range serials {
		attached[serial] = true
	}

	// Claims in pool order, primary before extras, so a two-device worktree
	// reverses in a stable order and the output reads the same on every run.
	var ordered []reverseTarget
	for _, claim := range orderedPoolClaims(req.Pools, req.Extras) {
		if !strings.HasPrefix(claim.Item, emulatorItemPrefix) && !attached[claim.Item] {
			continue
		}
		if req.DryRun {
			handle, running := peekItemHandle(claim.Item)
			ordered = append(ordered, reverseTarget{Address: handle, Booted: running})
			continue
		}
		serial, err := bootPoolItem(claim.Item, req.Headless)
		if err != nil {
			return nil, fmt.Errorf("pool %q: %w", claim.Pool, err)
		}
		ordered = append(ordered, reverseTarget{Address: serial, Booted: true})
	}
	// Two pools can hold the same device — or one pool an `emulator:` item and
	// another the serial it boots as. Issuing every reverse twice is harmless
	// but prints a duplicate line, and under --remove the second pass reports
	// "already absent" for a port it just removed, which reads as a bug.
	return dedupeTargets(ordered), nil
}

// poolClaim is one (pool, item) a worktree holds.
type poolClaim struct {
	Pool string
	Item string
}

// orderedPoolClaims flattens a worktree's claims: pools in name order, and
// within a pool the primary first, then the extras in the order they were
// added.
func orderedPoolClaims(pools map[string]string, extras map[string][]string) []poolClaim {
	seen := map[string]bool{}
	for pool := range pools {
		seen[pool] = true
	}
	for pool, items := range extras {
		if len(items) > 0 {
			seen[pool] = true
		}
	}
	names := make([]string, 0, len(seen))
	for pool := range seen {
		names = append(names, pool)
	}
	sort.Strings(names)
	var claims []poolClaim
	for _, pool := range names {
		if item := pools[pool]; item != "" {
			claims = append(claims, poolClaim{Pool: pool, Item: item})
		}
		for _, item := range extras[pool] {
			claims = append(claims, poolClaim{Pool: pool, Item: item})
		}
	}
	return claims
}

// listAttachedSerials is a seam: tests replace it so the unit suite never
// starts an adb server or depends on what is plugged into the machine.
var listAttachedSerials = attachedSerials

// bootPoolItem and lookupRunningEmulator are the same seam for the boot and
// peek paths.
var bootPoolItem = resolveItemHandle

var lookupRunningEmulator = runningEmulatorSerial

func bootedTargets(serials []string) []reverseTarget {
	out := make([]reverseTarget, 0, len(serials))
	for _, serial := range serials {
		out = append(out, reverseTarget{Address: serial, Booted: true})
	}
	return out
}

// dedupeTargets keeps the first occurrence of each address and drops
// whitespace-only ones, so `--device " "` cannot reach `adb -s " "`.
func dedupeTargets(targets []reverseTarget) []reverseTarget {
	seen := make(map[string]bool, len(targets))
	out := make([]reverseTarget, 0, len(targets))
	for _, t := range targets {
		t.Address = strings.TrimSpace(t.Address)
		if t.Address == "" || seen[t.Address] {
			continue
		}
		seen[t.Address] = true
		out = append(out, t)
	}
	return out
}

func describePools(pools map[string]string) string {
	return describeClaims(pools, nil)
}

// describeClaims names every claim, `devices=emulator:a, devices=emulator:b`,
// in the order orderedPoolClaims gives them.
func describeClaims(pools map[string]string, extras map[string][]string) string {
	claims := orderedPoolClaims(pools, extras)
	if len(claims) == 0 {
		return "none claimed"
	}
	parts := make([]string, 0, len(claims))
	for _, c := range claims {
		parts = append(parts, fmt.Sprintf("%s=%s", c.Pool, c.Item))
	}
	return strings.Join(parts, ", ")
}
