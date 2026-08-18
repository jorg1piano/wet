package cmd

import (
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
		return serial, nil
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
	cmd := exec.Command("emulator", args...)
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("emulator start failed for %s: %w", avdName, err)
	}
	fmt.Printf("  Booting %s (cold boot, port %d)...\n", avdName, port)
	if err := waitForDevice(serial, 120*time.Second); err != nil {
		return "", err
	}
	fmt.Printf("  %s is online (%s).\n", serial, avdName)
	return serial, nil
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

func attachedEmulatorSerials() []string {
	out, err := exec.Command("adb", "devices").Output()
	if err != nil {
		return nil
	}
	var serials []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[1] != "device" {
			continue
		}
		if m := emulatorSerialRE.FindStringSubmatch(fields[0]); m != nil {
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
	out, err := exec.Command("emulator", "-list-avds").Output()
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
	fmt.Printf("  Creating AVD %s...\n", avdName)
	create := exec.Command("avdmanager", "create", "avd",
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
		if !strings.HasPrefix(item, emulatorItemPrefix) {
			handles[pool] = item
			continue
		}
		serial, err := ensureEmulatorRunning(strings.TrimPrefix(item, emulatorItemPrefix), headless)
		if err != nil {
			return nil, fmt.Errorf("pool %q: %w", pool, err)
		}
		handles[pool] = serial
	}
	return handles, nil
}
