package cmd

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// `wet mobile open` puts the device a worktree claims on screen.
//
// Everything else here addresses an emulator by its serial, which is what
// `flutter run -d` and `adb -s` need and is enough for them. Looking at the
// thing is a different question: the window exists only if the emulator was
// booted with one, and a stopped emulator has none at all. So the command
// covers both halves, booting the claimed AVD with a window when it is down
// and raising the window that is already there when it is up.

var mobileOpenCmd = &cobra.Command{
	Use:     "open [ITEM|SERIAL]",
	Aliases: []string{"show"},
	Short:   "Show this worktree's emulator on screen",
	Long: `Bring the emulator this worktree claims to the front, booting it with a
window if it is not running.

  wet mobile open                     the primary device of the devices pool
  wet mobile open emulator:pixel-b    one claim, named by item or by serial
  wet mobile open --all               every emulator this worktree claims
  wet mobile open --restart           reboot a headless emulator so it gets a window

A boot from here is always windowed, the opposite of ` + "`--headless`" + `, and this
worktree's ports are reversed onto what it brought up, so the app on that
emulator reaches this worktree's stack. An emulator that is already running is
only raised: its reverses and its state are left alone.

A window cannot be attached to a live emulator, so one booted headless has to
be restarted to get one. That is what --restart does, and it is opt-in because
it costs whatever the emulator was holding.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runMobileOpen,
}

var (
	mobileOpenPool    string
	mobileOpenAll     bool
	mobileOpenRestart bool
)

func init() {
	mobileOpenCmd.Flags().StringVar(&mobileOpenPool, "pool", "",
		"pool whose primary to open (default: `devices`, or the only pool declared)")
	mobileOpenCmd.Flags().BoolVar(&mobileOpenAll, "all", false,
		"open every emulator this worktree claims, not only the primary")
	mobileOpenCmd.Flags().BoolVar(&mobileOpenRestart, "restart", false,
		"reboot an emulator that is running headless, so it comes back with a window")
	mobileCmd.AddCommand(mobileOpenCmd)
}

func runMobileOpen(cmd *cobra.Command, args []string) error {
	cfg, treePath, _, err := loadConfigForDevices("wet mobile open")
	if err != nil {
		return err
	}
	pool, err := devicePoolName(cfg, mobileOpenPool)
	if err != nil {
		return err
	}
	if err := requireADB(); err != nil {
		return err
	}
	state, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load wet state: %w", err)
	}
	own := worktreeStateForPath(state, treePath)
	if own == nil {
		return fmt.Errorf("no wet state for %s: run `wet setup` first, it claims the device this opens", treePath)
	}
	attached, err := attachedSet()
	if err != nil {
		return err
	}
	query := ""
	if len(args) == 1 {
		query = strings.TrimSpace(args[0])
		if query == "" {
			return fmt.Errorf("ITEM must not be empty")
		}
	}
	targets, err := openTargets(*own, attached, pool, query, mobileOpenAll)
	if err != nil {
		return err
	}

	// Under --all one dead emulator must not hide the ones that did come up,
	// so every target is attempted and the failures are reported together.
	var failures []error
	for _, claim := range targets {
		if err := openClaim(cfg, treePath, claim, mobileOpenRestart); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// openTargets picks the claims one run acts on: the device named on the
// command line, every emulator under --all, or the pool's primary.
//
// A named device is matched against this worktree's claims alone. Opening an
// AVD nothing here claims would put a window on screen that belongs to another
// worktree's stack, and the serial it prints would be one this worktree must
// not run against.
func openTargets(ws WorktreeState, attached map[string]bool, pool, query string, all bool) ([]deviceClaim, error) {
	rows := claimsOf(ws, attached)
	if len(rows) == 0 {
		return nil, errors.New("this worktree claims no device: `wet setup` claims the first, `wet mobile add` the next")
	}
	if query != "" {
		for _, claim := range rows {
			if claim.Item == query || claim.Handle == query {
				return []deviceClaim{claim}, nil
			}
		}
		return nil, fmt.Errorf("%s is not a device this worktree claims (claims: %s), and `wet mobile list` shows them",
			query, strings.Join(claimItems(rows), ", "))
	}
	if all {
		emulators := emulatorClaims(rows)
		if len(emulators) == 0 {
			return nil, fmt.Errorf("this worktree claims no emulator (claims: %s), and a phone on your desk has no window to open",
				strings.Join(claimItems(rows), ", "))
		}
		return emulators, nil
	}
	for _, claim := range rows {
		if claim.Primary && claim.Pool == pool {
			return []deviceClaim{claim}, nil
		}
	}
	return nil, fmt.Errorf("no primary device in pool %q (claims: %s): name one of them, or run `wet setup` to claim the pool's primary",
		pool, strings.Join(claimItems(rows), ", "))
}

func claimItems(rows []deviceClaim) []string {
	items := make([]string, 0, len(rows))
	for _, claim := range rows {
		items = append(items, claim.Item)
	}
	return items
}

func emulatorClaims(rows []deviceClaim) []deviceClaim {
	out := make([]deviceClaim, 0, len(rows))
	for _, claim := range rows {
		if strings.HasPrefix(claim.Item, emulatorItemPrefix) {
			out = append(out, claim)
		}
	}
	return out
}

// openClaim brings one claimed device on screen.
func openClaim(cfg *WetConfig, treePath string, claim deviceClaim, restart bool) error {
	avd, ok := strings.CutPrefix(claim.Item, emulatorItemPrefix)
	if !ok {
		return fmt.Errorf("%s (pool %s) is a physical device, and a phone on your desk has no window to open",
			claim.Item, claim.Pool)
	}
	proc, found, err := emulatorProcessFor(avd)
	if err != nil {
		return err
	}
	if found && !proc.Windowed {
		if !restart {
			return fmt.Errorf("%s is running headless, and a window cannot be attached to a live emulator: "+
				"`wet mobile open --restart %s` reboots it with one, at the cost of what it is holding",
				avd, claim.Item)
		}
		if err := restartWindowed(proc, claim, avd); err != nil {
			return err
		}
		// claim is a copy, so this records for the boot below what the restart
		// just made true: the console that answered a moment ago is gone.
		found, claim.Running = false, false
	}
	if !found && !claim.Running {
		serial, err := bootPoolItem(claim.Item, false)
		if err != nil {
			return err
		}
		if err := reverseOntoBooted(cfg, treePath, serial); err != nil {
			return fmt.Errorf("%w\n  %s is up; re-run `wet mobile reverse-tcp` once adb answers", err, serial)
		}
		if proc, found, err = emulatorProcessFor(avd); err != nil {
			return err
		}
	}
	if !found {
		// An emulator this machine's process table doesn't hold is one wet did
		// not start here: a network adb, or a console answering for something
		// running elsewhere. It is a device either way, just not a window.
		fmt.Printf("  %s answers adb, but no emulator process here names it, so its window cannot be raised\n", avd)
		return nil
	}
	if err := raiseWindow(proc.PID); err != nil {
		return fmt.Errorf("%s is up (pid %d) but its window could not be raised: %w", avd, proc.PID, err)
	}
	fmt.Printf("  %s is on screen\n", avd)
	return nil
}

// restartWindowed stops a headless emulator and waits for it to let go of its
// AVD. The wait is the point: the lock file outlives the console by a moment,
// and a boot that starts inside that moment fails with "AVD is already in
// use".
func restartWindowed(proc emulatorProcess, claim deviceClaim, avd string) error {
	serial, ok := proc.Serial()
	if !ok {
		serial = claim.Handle
	}
	if strings.HasPrefix(serial, emulatorItemPrefix) {
		return fmt.Errorf("%s is running headless, but wet cannot tell which console it answers to, so it cannot be stopped", avd)
	}
	fmt.Printf("  Stopping headless %s (%s)...\n", avd, serial)
	if err := killEmulator(serial); err != nil {
		return err
	}
	return waitForEmulatorGone(avd, 60*time.Second)
}

func waitForEmulatorGone(avd string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		_, running, err := emulatorProcessFor(avd)
		if err != nil {
			return err
		}
		if !running {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s is still running %s after %s: stop it yourself and re-run", avd, timeout, avd)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// reverseOntoBooted repeats what `wet mobile add` does after a boot: an
// emulator loses its reverses when it stops, so one this command started would
// otherwise come back up unable to reach the stack it is claimed for.
func reverseOntoBooted(cfg *WetConfig, treePath, serial string) error {
	alloc, err := readWorktreeAllocation(cfg, treePath)
	if err != nil {
		return err
	}
	if len(alloc.Ports) == 0 {
		fmt.Println("  No allocated ports to reverse.")
		return nil
	}
	fmt.Printf("  Reversing this worktree's ports onto %s:\n", serial)
	return reversePorts(serial, alloc.Ports, false)
}

// emulatorProcess is one running emulator as the process table shows it. The
// console cannot answer either question this command asks, whether the
// emulator has a window and which process owns it, so the argv it was started
// with is the only source for both.
type emulatorProcess struct {
	PID      int
	AVD      string
	Port     int
	Windowed bool
}

// Serial is the adb serial an emulator answers to, which is its console port.
// One started without -port took the first free console, and that number is
// not in its argv.
func (p emulatorProcess) Serial() (string, bool) {
	if p.Port == 0 {
		return "", false
	}
	return fmt.Sprintf("emulator-%d", p.Port), true
}

// listEmulatorProcesses is a seam: tests replace it so the unit suite never
// depends on what this machine happens to be running.
var listEmulatorProcesses = readEmulatorProcesses

func readEmulatorProcesses() ([]emulatorProcess, error) {
	// -ww keeps the full argv, which is where both answers live; the default
	// width truncates it at the terminal's.
	out, err := exec.Command("ps", "-A", "-ww", "-o", "pid=", "-o", "args=").Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	return parseEmulatorProcesses(string(out)), nil
}

// parseEmulatorProcesses keeps the process lines that are an emulator running
// an AVD. The binary is a qemu-system-<arch> under the SDK's emulator
// directory, so its path is what tells an emulator apart from anything else
// that happens to carry an -avd argument.
func parseEmulatorProcesses(psOutput string) []emulatorProcess {
	var procs []emulatorProcess
	for _, line := range strings.Split(psOutput, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		argv := fields[1:]
		if !strings.Contains(argv[0], "emulator") {
			continue
		}
		avd, ok := argValue(argv, "-avd")
		if !ok {
			continue
		}
		port, _ := strconv.Atoi(argValueOr(argv, "-port"))
		procs = append(procs, emulatorProcess{
			PID:      pid,
			AVD:      avd,
			Port:     port,
			Windowed: !slices.Contains(argv, "-no-window"),
		})
	}
	return procs
}

func argValue(argv []string, name string) (string, bool) {
	for i, arg := range argv {
		if arg == name && i+1 < len(argv) {
			return argv[i+1], true
		}
	}
	return "", false
}

func argValueOr(argv []string, name string) string {
	value, _ := argValue(argv, name)
	return value
}

func emulatorProcessFor(avd string) (emulatorProcess, bool, error) {
	procs, err := listEmulatorProcesses()
	if err != nil {
		return emulatorProcess{}, false, err
	}
	for _, proc := range procs {
		if proc.AVD == avd {
			return proc, true, nil
		}
	}
	return emulatorProcess{}, false, nil
}

// raiseWindow is the other seam, so no test puts a window on the screen of
// whoever is running the suite.
var raiseWindow = raiseProcessWindow

// raiseProcessWindow brings a process's windows to the front.
//
// The emulator is addressed by pid rather than by name because a machine
// running four of them has four identical qemu-system processes, and the pid
// is the only thing that picks out the right one.
func raiseProcessWindow(pid int) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("wet cannot raise a window on %s: the emulator has one, switch to it yourself", runtime.GOOS)
	}
	script := fmt.Sprintf("tell application \"System Events\" to set frontmost of "+
		"(first process whose unix id is %d) to true", pid)
	out, err := exec.Command("osascript", "-e", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("osascript: %w: %s\n  macOS gates this behind Accessibility: allow the terminal "+
			"running wet under System Settings > Privacy & Security > Accessibility",
			err, strings.TrimSpace(string(out)))
	}
	return nil
}
