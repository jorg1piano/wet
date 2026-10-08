package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ensureEmulatorRunning returns the adb serial for an AVD, booting it first if
// it isn't already up.
//
// A pool item names an AVD ("emulator:pixel-a") because that is the durable
// identity — an `emulator-5554` serial is assigned at boot and means nothing
// while the emulator is stopped. But everything downstream (`flutter run -d`,
// `adb -s`, a screenshot) addresses the *serial*. Something has to bridge the
// two, and it must be this: resolving by AVD name is the only way that works
// with several emulators attached. The obvious shell version — `adb
// wait-for-device` after `flutter emulators --launch` — fails outright the
// moment a second device is present ("error: more than one device/emulator"),
// which is precisely the situation parallel worktrees create.
func ensureEmulatorRunning(avdName string, headless bool) (string, error) {
	if serial, ok := runningEmulatorSerial(avdName); ok {
		return serial, waitForBootCompleted(serial, avdName, bootCompletedTimeout)
	}
	if err := ensureAVDExists(avdName); err != nil {
		return "", err
	}
	port, err := nextEmulatorPort()
	if err != nil {
		return "", err
	}
	serial := fmt.Sprintf("emulator-%d", port)

	args := []string{"-avd", avdName, "-port", strconv.Itoa(port), "-no-snapshot-load"}
	if headless {
		args = append(args, "-no-window")
	}
	emulator, err := androidTool("emulator")
	if err != nil {
		return "", err
	}
	cmd := exec.Command(emulator, args...)
	var emulatorOutput bytes.Buffer
	cmd.Stdout = &emulatorOutput
	cmd.Stderr = &emulatorOutput
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("emulator start failed for %s: %w", avdName, err)
	}
	fmt.Printf("  Booting %s (cold boot, port %d)...\n", avdName, port)
	if err := waitForStartedEmulator(cmd, serial, 120*time.Second, &emulatorOutput); err != nil {
		return "", err
	}
	if err := waitForBootCompleted(serial, avdName, bootCompletedTimeout); err != nil {
		return "", err
	}
	fmt.Printf("  %s is online (%s).\n", serial, avdName)
	return serial, nil
}

const bootCompletedTimeout = 180 * time.Second

// waitForBootCompleted blocks until Android reports sys.boot_completed=1.
//
// adb lists an emulator as `device` long before Android has booted, and adbd
// restarts on the way up, dropping every `adb reverse` rule set before it.
// A slot_reset run in that window set its forwards and uninstalled the app
// against a device that then forgot both.
var waitForBootCompleted = func(serial, avdName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	announced := false
	for time.Now().Before(deadline) {
		out, err := exec.Command("adb", "-s", serial, "shell", "getprop", "sys.boot_completed").Output()
		if err == nil && strings.TrimSpace(string(out)) == "1" {
			return nil
		}
		if !announced {
			fmt.Printf("  Waiting for %s to finish booting...\n", avdName)
			announced = true
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("%s (%s) did not finish booting within %s", avdName, serial, timeout)
}

// runningEmulatorSerial finds a booted emulator by the AVD it is running.
//
// `adb devices` lists serials, not AVD names, so each console is asked what it
// is running. Devices that don't answer are skipped rather than failing the
// lookup: a physical phone has no `emu` console, and an emulator still booting
// answers nothing useful yet.
func runningEmulatorSerial(avdName string) (string, bool) {
	for _, serial := range attachedEmulatorSerials() {
		out, err := exec.Command("adb", "-s", serial, "emu", "avd", "name").Output()
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(out), "\n") {
			if strings.TrimSpace(line) == avdName {
				return serial, true
			}
		}
	}
	return "", false
}

var emulatorSerialRE = regexp.MustCompile(`^(emulator-\d+)\b`)

// requireADB fails with the fix rather than "executable file not found".
func requireADB() error {
	if _, err := exec.LookPath("adb"); err != nil {
		return errors.New("adb is not installed or not on PATH: install the Android " +
			"platform-tools (brew install --cask android-platform-tools) and re-run")
	}
	return nil
}

// attachedSerials lists every adb serial currently in the `device` state,
// physical phones included. Callers that want only emulators filter what it
// returns rather than parsing `adb devices` again.
//
// One other reader of `adb devices` survives, deliberately: nextEmulatorPort
// counts consoles in ANY state, because a booting or offline emulator still
// holds its port, and filtering to `device` the way this does would hand out a
// port that is already taken.
//
// The error is returned rather than folded into an empty list because the two
// mean opposite things to a caller: "adb cannot run here" is a machine-setup
// problem to report, while "no devices" is a fact about what is plugged in.
func attachedSerials() ([]string, error) {
	if err := requireADB(); err != nil {
		return nil, err
	}
	out, err := exec.Command("adb", "devices").Output()
	if err != nil {
		return nil, fmt.Errorf("adb devices: %w", err)
	}
	var serials []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[1] != "device" {
			continue
		}
		serials = append(serials, fields[0])
	}
	return serials, nil
}

// attachedEmulatorSerials narrows attachedSerials to booted emulators. A
// machine without adb has none, which is the honest answer for every caller
// here: they are all asking "which emulator can I address right now?".
func attachedEmulatorSerials() []string {
	all, err := attachedSerials()
	if err != nil {
		return nil
	}
	var serials []string
	for _, serial := range all {
		if m := emulatorSerialRE.FindStringSubmatch(serial); m != nil {
			serials = append(serials, m[1])
		}
	}
	return serials
}

// ensureAVDExists creates the AVD if this machine doesn't have it, cloning the
// system image and device profile of one it does (see pickAVDTemplate).
//
// A pool item can name an AVD that only ever existed on the machine that grew
// it — .wet.local.yaml is per-machine, but it gets copied, and a teammate's
// item is otherwise a dead end.
func ensureAVDExists(avdName string) error {
	emulator, err := androidTool("emulator")
	if err != nil {
		return err
	}
	out, err := exec.Command(emulator, "-list-avds").Output()
	if err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.TrimSpace(line) == avdName {
				return nil
			}
		}
	}
	sysImage, device, err := pickAVDTemplate()
	if err != nil {
		return fmt.Errorf("cannot create AVD %s: %w", avdName, err)
	}
	avdmanager, err := androidTool("avdmanager")
	if err != nil {
		return err
	}
	fmt.Printf("  Creating AVD %s...\n", avdName)
	create := exec.Command(avdmanager, "create", "avd",
		"-n", avdName, "-k", sysImage, "-d", device, "--force")
	create.Stdout, create.Stderr = os.Stdout, os.Stderr
	create.Stdin = strings.NewReader("no\n")
	if err := create.Run(); err != nil {
		return fmt.Errorf("avdmanager create avd %s: %w", avdName, err)
	}
	return nil
}

// resolvePoolHandles turns claimed pool items into the address each resource
// answers to right now: an emulator item becomes a live adb serial, everything
// else is already its own handle.
//
// Both forms are kept, because they answer different questions. The item is
// what the pool owns and what teardown reasons about ("is this mine to kill?").
// The handle is what a command targets. Collapsing them loses one or the other.
func resolvePoolHandles(items map[string]string, headless bool) (map[string]string, error) {
	handles := make(map[string]string, len(items))
	for pool, item := range items {
		serial, err := resolveItemHandle(item, headless)
		if err != nil {
			return nil, fmt.Errorf("pool %q: %w", pool, err)
		}
		handles[pool] = serial
	}
	return handles, nil
}

// resolveExtraHandles is resolvePoolHandles for the extra claims: every
// emulator among them is booted too, so a setup re-run brings the whole
// set back up and `wet mobile reverse-tcp` in a hook reaches all of them.
func resolveExtraHandles(extras map[string][]string, headless bool) (map[string][]string, error) {
	handles := make(map[string][]string, len(extras))
	for pool, items := range extras {
		for _, item := range items {
			serial, err := resolveItemHandle(item, headless)
			if err != nil {
				return nil, fmt.Errorf("pool %q (extra %s): %w", pool, item, err)
			}
			handles[pool] = append(handles[pool], serial)
		}
	}
	return handles, nil
}

// resolveItemHandle turns one pool item into the address it answers to,
// booting an emulator item that isn't running. Anything else is its own
// handle.
func resolveItemHandle(item string, headless bool) (string, error) {
	avd, ok := strings.CutPrefix(item, emulatorItemPrefix)
	if !ok {
		return item, nil
	}
	return ensureEmulatorRunning(avd, headless)
}

// peekItemHandle resolves what it can without starting anything: a running
// emulator answers with its serial, a stopped one keeps its item name, and
// everything else is its own handle. running reports whether the address is
// one adb can be pointed at right now.
//
// This is what every render outside `wet setup` uses. A pull must not boot
// an emulator to write a dotenv, but it must not write `emulator:pixel-a`
// where the last setup wrote `emulator-5554` either — the file it
// regenerates is the one `flutter run -d` reads.
func peekItemHandle(item string) (handle string, running bool) {
	avd, ok := strings.CutPrefix(item, emulatorItemPrefix)
	if !ok {
		return item, true
	}
	if serial, up := lookupRunningEmulator(avd); up {
		return serial, true
	}
	return item, false
}

// peekPoolHandles and peekExtraHandles are the no-boot counterparts of
// resolvePoolHandles and resolveExtraHandles.
func peekPrimaryHandles(items map[string]string) map[string]string {
	handles := make(map[string]string, len(items))
	for pool, item := range items {
		handles[pool], _ = peekItemHandle(item)
	}
	return handles
}

func peekExtraHandles(extras map[string][]string) map[string][]string {
	handles := make(map[string][]string, len(extras))
	for pool, items := range extras {
		for _, item := range items {
			h, _ := peekItemHandle(item)
			handles[pool] = append(handles[pool], h)
		}
	}
	return handles
}

// killEmulator stops a running emulator through its console. It is a seam so
// the unit suite never reaches a device.
var killEmulator = func(serial string) error {
	out, err := exec.Command("adb", "-s", serial, "emu", "kill").CombinedOutput()
	if err != nil {
		return fmt.Errorf("adb -s %s emu kill: %w: %s", serial, err, strings.TrimSpace(string(out)))
	}
	return nil
}
