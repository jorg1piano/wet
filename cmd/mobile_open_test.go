package cmd

import (
	"errors"
	"strings"
	"testing"
)

// These cover `wet mobile open`: which claim a run acts on, and what it does
// with the emulator process it finds. Nothing here starts an emulator or puts
// a window on the screen of whoever runs the suite.

func stubEmulatorProcesses(t *testing.T, procs ...emulatorProcess) {
	t.Helper()
	listEmulatorProcesses = func() ([]emulatorProcess, error) { return procs, nil }
	t.Cleanup(func() { listEmulatorProcesses = readEmulatorProcesses })
}

func stubRaiseWindow(t *testing.T, raised *[]int) {
	t.Helper()
	raiseWindow = func(pid int) error {
		*raised = append(*raised, pid)
		return nil
	}
	t.Cleanup(func() { raiseWindow = raiseProcessWindow })
}

// --- reading the process table ---

// The console answers neither question this command asks, so the argv is the
// whole source: which AVD a process runs, which console it answers to, and
// whether it was given a window.
func TestParseEmulatorProcessesReadsTheArgv(t *testing.T) {
	const psOutput = `  4221 /Users/j/Library/Android/sdk/emulator/qemu/darwin-aarch64/qemu-system-aarch64 -avd pixel-a -port 5554 -no-snapshot-load
  7441 /Users/j/Library/Android/sdk/emulator/qemu/darwin-aarch64/qemu-system-aarch64 -avd pixel-b -port 5556 -no-snapshot-load -no-window
  4224 /Users/j/Library/Android/sdk/emulator/crashpad_handler --database=/tmp/emu-crash.db
  9001 /usr/bin/vim notes-avd.txt -avd pixel-c
   PID COMMAND`
	got := parseEmulatorProcesses(psOutput)
	if len(got) != 2 {
		t.Fatalf("got %d processes, want the 2 emulators: %+v", len(got), got)
	}
	if got[0] != (emulatorProcess{PID: 4221, AVD: "pixel-a", Port: 5554, Windowed: true}) {
		t.Errorf("windowed emulator = %+v", got[0])
	}
	if got[1] != (emulatorProcess{PID: 7441, AVD: "pixel-b", Port: 5556, Windowed: false}) {
		t.Errorf("headless emulator = %+v", got[1])
	}
	if serial, ok := got[0].Serial(); !ok || serial != "emulator-5554" {
		t.Errorf("Serial() = %q, %v; want emulator-5554", serial, ok)
	}
}

// An emulator started without -port took whichever console was free, and that
// number is nowhere in its argv. Saying so lets the caller fall back to the
// serial the claim was peeked as, instead of killing emulator-0.
func TestEmulatorProcessWithoutAPortHasNoSerial(t *testing.T) {
	got := parseEmulatorProcesses("  10 /sdk/emulator/qemu/linux-x86_64/qemu-system-x86_64 -avd pixel-a")
	if len(got) != 1 {
		t.Fatalf("got %+v, want one process", got)
	}
	if serial, ok := got[0].Serial(); ok {
		t.Errorf("Serial() = %q, want no answer", serial)
	}
}

// --- picking the target ---

func openTestState() WorktreeState {
	return WorktreeState{
		Branch:     "chat",
		Pools:      map[string]string{"devices": "emulator:pixel-a", "fixtures": "snap-alpha"},
		PoolExtras: map[string][]string{"devices": {"emulator:pixel-b", "RFCR921L4DP"}},
	}
}

func TestOpenTargetsDefaultsToThePoolPrimary(t *testing.T) {
	stubADB(t, nil, map[string]string{"pixel-a": "emulator-5554"})
	got, err := openTargets(openTestState(), nil, "devices", "", false)
	if err != nil {
		t.Fatalf("openTargets: %v", err)
	}
	if len(got) != 1 || got[0].Item != "emulator:pixel-a" {
		t.Errorf("got %+v, want the devices primary alone", got)
	}
}

// `wet mobile list` prints serials, so a serial is what gets pasted back in.
func TestOpenTargetsMatchesARunningSerial(t *testing.T) {
	stubADB(t, nil, map[string]string{"pixel-b": "emulator-5556"})
	got, err := openTargets(openTestState(), nil, "devices", "emulator-5556", false)
	if err != nil {
		t.Fatalf("openTargets: %v", err)
	}
	if len(got) != 1 || got[0].Item != "emulator:pixel-b" {
		t.Errorf("got %+v, want emulator:pixel-b", got)
	}
}

// --all means every emulator, not every claim: a phone and a fixture snapshot
// have no window, and reporting a failure for each of them would bury the
// emulators that did come up.
func TestOpenTargetsAllKeepsOnlyEmulators(t *testing.T) {
	stubADB(t, nil, nil)
	got, err := openTargets(openTestState(), map[string]bool{"RFCR921L4DP": true}, "devices", "", true)
	if err != nil {
		t.Fatalf("openTargets: %v", err)
	}
	if len(got) != 2 || got[0].Item != "emulator:pixel-a" || got[1].Item != "emulator:pixel-b" {
		t.Errorf("got %+v, want both emulator claims", got)
	}
}

func TestOpenTargetsAllWithNoEmulatorSaysSo(t *testing.T) {
	stubADB(t, nil, nil)
	ws := WorktreeState{Branch: "chat", Pools: map[string]string{"devices": "RFCR921L4DP"}}
	_, err := openTargets(ws, map[string]bool{"RFCR921L4DP": true}, "devices", "", true)
	if err == nil || !strings.Contains(err.Error(), "no emulator") {
		t.Fatalf("expected the no-emulator error, got %v", err)
	}
}

// Opening an AVD this worktree does not claim would put another worktree's
// device on screen and print a serial this one must not run against.
func TestOpenTargetsRejectsAnUnclaimedDevice(t *testing.T) {
	stubADB(t, nil, nil)
	_, err := openTargets(openTestState(), nil, "devices", "emulator:pixel-z", false)
	if err == nil || !strings.Contains(err.Error(), "emulator:pixel-a") {
		t.Fatalf("the refusal should list what is claimed, got %v", err)
	}
}

func TestOpenTargetsWithNoClaimsPointsAtSetup(t *testing.T) {
	stubADB(t, nil, nil)
	_, err := openTargets(WorktreeState{Branch: "chat"}, nil, "devices", "", false)
	if err == nil || !strings.Contains(err.Error(), "wet setup") {
		t.Fatalf("expected the no-claims error, got %v", err)
	}
}

// --- opening ---

// A running emulator is raised and nothing else: stubADB fails the test if the
// boot path is reached, which is the assertion that its state survives.
func TestOpenClaimRaisesARunningEmulator(t *testing.T) {
	stubADB(t, nil, map[string]string{"pixel-a": "emulator-5554"})
	stubEmulatorProcesses(t, emulatorProcess{PID: 4221, AVD: "pixel-a", Port: 5554, Windowed: true})
	var raised []int
	stubRaiseWindow(t, &raised)

	claim := deviceClaim{Pool: "devices", Item: "emulator:pixel-a", Handle: "emulator-5554", Primary: true, Running: true}
	if err := openClaim(nil, "", claim, false); err != nil {
		t.Fatalf("openClaim: %v", err)
	}
	if len(raised) != 1 || raised[0] != 4221 {
		t.Errorf("raised %v, want the emulator's pid alone", raised)
	}
}

// A window cannot be attached to a live emulator, so the refusal has to name
// the flag that does something about it.
func TestOpenClaimRefusesAHeadlessEmulatorWithoutRestart(t *testing.T) {
	stubADB(t, nil, map[string]string{"pixel-a": "emulator-5554"})
	stubEmulatorProcesses(t, emulatorProcess{PID: 4221, AVD: "pixel-a", Port: 5554, Windowed: false})
	var raised []int
	stubRaiseWindow(t, &raised)

	claim := deviceClaim{Pool: "devices", Item: "emulator:pixel-a", Handle: "emulator-5554", Running: true}
	err := openClaim(nil, "", claim, false)
	if err == nil || !strings.Contains(err.Error(), "--restart") {
		t.Fatalf("expected the headless refusal to name --restart, got %v", err)
	}
	if len(raised) != 0 {
		t.Errorf("raised %v, want nothing raised", raised)
	}
}

func TestOpenClaimRejectsAPhysicalDevice(t *testing.T) {
	stubADB(t, nil, nil)
	claim := deviceClaim{Pool: "devices", Item: "RFCR921L4DP", Handle: "RFCR921L4DP", Running: true}
	err := openClaim(nil, "", claim, false)
	if err == nil || !strings.Contains(err.Error(), "no window") {
		t.Fatalf("expected the physical-device refusal, got %v", err)
	}
}

// An emulator adb answers for but no local process owns was not started here.
// It is still a device, so this is a note rather than a failure, and there is
// nothing to boot: stubADB fails the test if the boot path is reached.
func TestOpenClaimReportsAnEmulatorItCannotSee(t *testing.T) {
	stubADB(t, nil, map[string]string{"pixel-a": "emulator-5554"})
	stubEmulatorProcesses(t)
	var raised []int
	stubRaiseWindow(t, &raised)

	claim := deviceClaim{Pool: "devices", Item: "emulator:pixel-a", Handle: "emulator-5554", Running: true}
	if err := openClaim(nil, "", claim, false); err != nil {
		t.Fatalf("openClaim: %v", err)
	}
	if len(raised) != 0 {
		t.Errorf("raised %v, want nothing raised", raised)
	}
}

// --- restarting a headless emulator ---

// The kill goes to the console the process is actually on, and the wait is for
// the AVD lock: a boot inside that window fails with "AVD is already in use".
func TestRestartWindowedStopsTheEmulatorAndWaitsForIt(t *testing.T) {
	proc := emulatorProcess{PID: 4221, AVD: "pixel-a", Port: 5556, Windowed: false}
	var killed []string
	killEmulator = func(serial string) error {
		killed = append(killed, serial)
		return nil
	}
	t.Cleanup(func() {
		killEmulator = func(serial string) error { return errors.New("unstubbed") }
	})
	calls := 0
	listEmulatorProcesses = func() ([]emulatorProcess, error) {
		calls++
		if calls == 1 {
			return []emulatorProcess{proc}, nil
		}
		return nil, nil
	}
	t.Cleanup(func() { listEmulatorProcesses = readEmulatorProcesses })

	claim := deviceClaim{Pool: "devices", Item: "emulator:pixel-a", Handle: "emulator:pixel-a"}
	if err := restartWindowed(proc, claim, "pixel-a"); err != nil {
		t.Fatalf("restartWindowed: %v", err)
	}
	if len(killed) != 1 || killed[0] != "emulator-5556" {
		t.Errorf("killed %v, want the console from the process argv", killed)
	}
	if calls < 2 {
		t.Errorf("the process table was read %d times, want a wait for the AVD to be let go", calls)
	}
}

// With no -port in the argv and a claim that was never peeked to a serial,
// there is no console to send the kill to, and guessing one would stop an
// emulator belonging to somebody else.
func TestRestartWindowedRefusesWithoutAConsole(t *testing.T) {
	stubEmulatorProcesses(t, emulatorProcess{PID: 4221, AVD: "pixel-a"})
	claim := deviceClaim{Pool: "devices", Item: "emulator:pixel-a", Handle: "emulator:pixel-a"}
	err := restartWindowed(emulatorProcess{PID: 4221, AVD: "pixel-a"}, claim, "pixel-a")
	if err == nil || !strings.Contains(err.Error(), "which console") {
		t.Fatalf("expected the no-console refusal, got %v", err)
	}
}

func TestWaitForEmulatorGoneTimesOut(t *testing.T) {
	stubEmulatorProcesses(t, emulatorProcess{PID: 4221, AVD: "pixel-a", Port: 5554})
	if err := waitForEmulatorGone("pixel-a", 0); err == nil {
		t.Fatal("expected a timeout for an emulator that never goes away")
	}
}
