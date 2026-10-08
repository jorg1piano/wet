package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeBootingADB puts an adb on PATH whose getprop sys.boot_completed answers
// "0" for the first answersBeforeBoot calls and "1" after.
func fakeBootingADB(t *testing.T, answersBeforeBoot int) {
	t.Helper()
	dir := t.TempDir()
	counter := filepath.Join(dir, "calls")
	script := `#!/bin/sh
n=$(cat "` + counter + `" 2>/dev/null || echo 0)
echo $((n + 1)) > "` + counter + `"
if [ "$n" -ge ` + fmt.Sprint(answersBeforeBoot) + ` ]; then echo 1; else echo 0; fi
`
	if err := os.WriteFile(filepath.Join(dir, "adb"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestWaitForBootCompletedWaitsForAndroidNotJustAdb(t *testing.T) {
	fakeBootingADB(t, 1)
	start := time.Now()
	if err := waitForBootCompleted("emulator-5556", "wet-avd-1", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < time.Second {
		t.Fatal("returned on the first poll, while the device still reported boot_completed=0")
	}
}

func TestWaitForBootCompletedTimesOut(t *testing.T) {
	fakeBootingADB(t, 9)
	err := waitForBootCompleted("emulator-5556", "wet-avd-1", time.Second)
	if err == nil || !strings.Contains(err.Error(), "did not finish booting") {
		t.Fatalf("err = %v, want a timeout naming the AVD", err)
	}
}

// fakeADBWithEmulator puts an adb on PATH that lists one emulator, serial,
// running avd.
func fakeADBWithEmulator(t *testing.T, serial, avd string) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
case "$*" in
  devices) printf 'List of devices attached\n` + serial + `\tdevice\n\n' ;;
  *"emu avd name"*) printf '` + avd + `\nOK\n' ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "adb"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// The cycle-3 race: two setups started emulators on the same port. The second
// one's process died, but another AVD answered on that serial, and wet took
// it for the one it started.
func TestWaitForStartedEmulatorRequiresItsOwnAVDOnTheSerial(t *testing.T) {
	fakeADBWithEmulator(t, "emulator-5558", "wet-avd-2")
	sleeper := func() *exec.Cmd {
		cmd := exec.Command("sleep", "30")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return cmd
	}
	var out strings.Builder
	if err := waitForStartedEmulator(sleeper(), "emulator-5558", "wet-avd-3", 4*time.Second, &out); err == nil {
		t.Fatal("wet-avd-3 was reported online on a serial running wet-avd-2")
	}
	if err := waitForStartedEmulator(sleeper(), "emulator-5558", "wet-avd-2", 4*time.Second, &out); err != nil {
		t.Fatalf("the AVD actually on the serial was not recognised: %v", err)
	}
}
