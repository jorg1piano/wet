package cmd

import (
	"errors"
	"strings"
	"testing"
)

func testAllocation() worktreeAllocation {
	return worktreeAllocation{
		Branch: "feature/x",
		Slot:   2,
		Ports: []ResolvedPort{
			{Name: "APP_PORT", Value: 4020},
			{Name: "GRPC_PORT", Value: 50071},
		},
		Pools: map[string]string{"devices": "emulator:pixel-a", "fixtures": "snap-alpha"},
	}
}

// No names means every allocated port, in declared order — the reverse set has
// to match what the worktree actually got, not the config's base values.
func TestSelectAllocatedPortsDefaultsToAll(t *testing.T) {
	got, err := selectAllocatedPorts(testAllocation(), nil)
	if err != nil {
		t.Fatalf("selectAllocatedPorts: %v", err)
	}
	if len(got) != 2 || got[0].Name != "APP_PORT" || got[0].Value != 4020 || got[1].Value != 50071 {
		t.Fatalf("got %+v, want both allocated ports in order", got)
	}
}

// A name repeated on the command line is one port, for the same reason a
// repeated serial is one device.
func TestSelectAllocatedPortsDedupesRepeatedNames(t *testing.T) {
	got, err := selectAllocatedPorts(testAllocation(), []string{"APP_PORT", "APP_PORT"})
	if err != nil {
		t.Fatalf("selectAllocatedPorts: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %+v, want APP_PORT once", got)
	}
}

func TestSelectAllocatedPortsFiltersByName(t *testing.T) {
	got, err := selectAllocatedPorts(testAllocation(), []string{"GRPC_PORT"})
	if err != nil {
		t.Fatalf("selectAllocatedPorts: %v", err)
	}
	if len(got) != 1 || got[0].Value != 50071 {
		t.Fatalf("got %+v, want only GRPC_PORT=50071", got)
	}
}

// A typo'd port name must fail loudly and say what is available: silently
// reversing nothing looks identical to success and costs an hour to debug.
func TestSelectAllocatedPortsRejectsUnknownName(t *testing.T) {
	_, err := selectAllocatedPorts(testAllocation(), []string{"WEB_PORT"})
	if err == nil {
		t.Fatal("expected an error for an unallocated port name")
	}
	if !strings.Contains(err.Error(), "WEB_PORT") || !strings.Contains(err.Error(), "APP_PORT") {
		t.Fatalf("error should name the bad port and the available ones, got: %v", err)
	}
}

func TestSelectAllocatedPortsWithNoAllocationExplainsSetup(t *testing.T) {
	_, err := selectAllocatedPorts(worktreeAllocation{}, nil)
	if err == nil || !strings.Contains(err.Error(), "wet setup") {
		t.Fatalf("expected an error pointing at `wet setup`, got: %v", err)
	}
}

// stubADB replaces the two adb seams so the unit suite never starts an adb
// server and never depends on what is plugged into the machine.
func stubADB(t *testing.T, attached []string, running map[string]string) {
	t.Helper()
	listAttachedSerials = func() ([]string, error) { return attached, nil }
	// Booting is stubbed to a hard failure by default: a unit test must never
	// start an AVD, and a test that means to exercise the boot path replaces
	// this itself.
	bootPoolHandles = func(items map[string]string, headless bool) (map[string]string, error) {
		t.Errorf("resolvePoolHandles must not be reached: it boots emulators (items %v)", items)
		return nil, errors.New("boot not stubbed")
	}
	lookupRunningEmulator = func(avd string) (string, bool) {
		serial, ok := running[avd]
		return serial, ok
	}
	t.Cleanup(func() {
		listAttachedSerials = attachedSerials
		lookupRunningEmulator = runningEmulatorSerial
		bootPoolHandles = resolvePoolHandles
	})
}

func addresses(targets []reverseTarget) []string {
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		out = append(out, t.Address)
	}
	return out
}

// --device is the escape hatch for a device wet knows nothing about, so it
// must bypass pool resolution entirely (which would otherwise boot an AVD).
func TestReverseTargetsPrefersExplicitDevices(t *testing.T) {
	stubADB(t, nil, nil)
	got, err := reverseTargets(reverseRequest{
		Pools:   testAllocation().Pools,
		Devices: []string{"RFCR921L4DP"},
	})
	if err != nil {
		t.Fatalf("reverseTargets: %v", err)
	}
	if len(got) != 1 || got[0].Address != "RFCR921L4DP" || !got[0].Booted {
		t.Fatalf("got %+v, want the --device serial", got)
	}
}

// A worktree whose only pool holds a fixture name has no device: passing
// "snap-alpha" to `adb -s` would fail with an adb error that says nothing
// about pools.
func TestReverseTargetsIgnoresNonDevicePoolItems(t *testing.T) {
	stubADB(t, []string{"emulator-5554"}, nil)
	got, err := reverseTargets(reverseRequest{Pools: map[string]string{"fixtures": "snap-alpha"}, DryRun: true})
	if err != nil {
		t.Fatalf("reverseTargets: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %+v, want no targets", got)
	}
}

// A pool holding a serial that is actually attached is a device, even though
// it carries no `emulator:` prefix.
func TestReverseTargetsAcceptsAnAttachedPhysicalSerial(t *testing.T) {
	stubADB(t, []string{"RFCR921L4DP"}, nil)
	got, err := reverseTargets(reverseRequest{
		Pools:  map[string]string{"devices": "RFCR921L4DP"},
		DryRun: true,
	})
	if err != nil {
		t.Fatalf("reverseTargets: %v", err)
	}
	if len(got) != 1 || got[0].Address != "RFCR921L4DP" {
		t.Fatalf("got %+v, want the attached serial", got)
	}
}

// One pool naming the AVD and another naming the serial it booted as is the
// same device twice.
func TestReverseTargetsDedupesAcrossPools(t *testing.T) {
	stubADB(t, []string{"emulator-5554"}, map[string]string{"pixel-a": "emulator-5554"})
	got, err := reverseTargets(reverseRequest{
		Pools:  map[string]string{"devices": "emulator:pixel-a", "debug": "emulator-5554"},
		DryRun: true,
	})
	if err != nil {
		t.Fatalf("reverseTargets: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %v, want one device", addresses(got))
	}
}

func TestReverseTargetsDedupesAndDropsEmptyDevices(t *testing.T) {
	stubADB(t, nil, nil)
	got, err := reverseTargets(reverseRequest{
		Devices: []string{"emulator-5554", " ", "emulator-5554", "RFCR921L4DP"},
	})
	if err != nil {
		t.Fatalf("reverseTargets: %v", err)
	}
	want := []string{"emulator-5554", "RFCR921L4DP"}
	if got := addresses(got); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v, want %v — a repeated serial reverses twice and, under --remove, "+
			"reports the port it just removed as already absent", got, want)
	}
}

// A dry run must not boot: a stopped emulator keeps its item name and is
// marked unbooted, which is what makes the printed command honest about not
// being copy-pasteable.
func TestPeekPoolHandlesKeepsStoppedEmulatorItems(t *testing.T) {
	stubADB(t, nil, map[string]string{"pixel-b": "emulator-5556"})
	got := peekPoolHandles(map[string]string{
		"stopped": "emulator:pixel-a",
		"running": "emulator:pixel-b",
		"phone":   "RFCR921L4DP",
	})
	if got["stopped"].Address != "emulator:pixel-a" || got["stopped"].Booted {
		t.Errorf("stopped emulator = %+v, want the unresolved item, unbooted", got["stopped"])
	}
	if got["running"].Address != "emulator-5556" || !got["running"].Booted {
		t.Errorf("running emulator = %+v, want its live serial", got["running"])
	}
	if got["phone"].Address != "RFCR921L4DP" || !got["phone"].Booted {
		t.Errorf("physical device = %+v, want it addressable as-is", got["phone"])
	}
}

func TestDescribePoolsIsStableAndNamesItems(t *testing.T) {
	if got := describePools(nil); got != "none claimed" {
		t.Errorf("describePools(nil) = %q", got)
	}
	got := describePools(map[string]string{"devices": "emulator:pixel-a", "fixtures": "snap-alpha"})
	if got != "devices=emulator:pixel-a, fixtures=snap-alpha" {
		t.Errorf("describePools = %q", got)
	}
}

func TestAdbReverseArgsMapsThePortToItself(t *testing.T) {
	got := strings.Join(adbReverseArgs("emulator-5554", 4020, false), " ")
	if got != "-s emulator-5554 reverse tcp:4020 tcp:4020" {
		t.Fatalf("add args = %q", got)
	}
	got = strings.Join(adbReverseArgs("emulator-5554", 4020, true), " ")
	if got != "-s emulator-5554 reverse --remove tcp:4020" {
		t.Fatalf("remove args = %q", got)
	}
}

// The whole point of the classifier: a missing listener is success under
// --remove, a missing device is not, and both adb messages say "not found".
func TestIsMissingListenerRejectsAMissingDevice(t *testing.T) {
	if !isMissingListener("error: listener 'tcp:4020' not found", 4020) {
		t.Error("an absent listener must count as already removed")
	}
	if isMissingListener("error: device 'RFCR921L4DP' not found", 4020) {
		t.Error("a missing device must not be reported as a successful removal")
	}
	if isMissingListener("adb: no devices/emulators found", 4020) {
		t.Error("no devices at all must not be reported as a successful removal")
	}
	if isMissingListener("error: listener 'tcp:9999' not found", 4020) {
		t.Error("a message about a different listener must not pass")
	}
}

// The boot path is what a real run takes: every claimed item resolves to a
// live serial, ordered by pool name so two devices reverse the same way twice.
func TestReverseTargetsBootsClaimedEmulatorsInPoolOrder(t *testing.T) {
	stubADB(t, nil, nil)
	bootPoolHandles = func(items map[string]string, headless bool) (map[string]string, error) {
		if !headless {
			t.Error("headless must reach the boot path, or --headless is a no-op")
		}
		// The booter must never see a fixture: it would try to start an AVD
		// named after a database snapshot.
		if _, leaked := items["fixtures"]; leaked {
			t.Errorf("non-device pool item reached the booter: %v", items)
		}
		return map[string]string{"alpha": "emulator-5554", "beta": "emulator-5556"}, nil
	}
	got, err := reverseTargets(reverseRequest{
		Pools: map[string]string{
			"alpha":    "emulator:pixel-a",
			"beta":     "emulator:pixel-b",
			"fixtures": "snap-alpha",
		},
		Headless: true,
	})
	if err != nil {
		t.Fatalf("reverseTargets: %v", err)
	}
	want := []string{"emulator-5554", "emulator-5556"}
	if addr := addresses(got); len(addr) != 2 || addr[0] != want[0] || addr[1] != want[1] {
		t.Fatalf("got %v, want %v in pool order", addr, want)
	}
	for _, target := range got {
		if !target.Booted {
			t.Errorf("%s came off the boot path and must be marked booted", target.Address)
		}
	}
}

// A working adb that cannot answer must not be reported as "this worktree
// claims no device": the pool claim is fine, the daemon is not.
func TestReverseTargetsSurfacesAnAdbFailureOnARealRun(t *testing.T) {
	stubADB(t, nil, nil)
	listAttachedSerials = func() ([]string, error) { return nil, errors.New("adb devices: exit status 1") }
	_, err := reverseTargets(reverseRequest{Pools: map[string]string{"devices": "RFCR921L4DP"}})
	if err == nil || !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("expected the adb failure to surface, got %v", err)
	}
}

// A dry run changes nothing, so it stays usable on a machine with no adb.
func TestReverseTargetsToleratesAnAdbFailureOnADryRun(t *testing.T) {
	stubADB(t, nil, nil)
	listAttachedSerials = func() ([]string, error) { return nil, errors.New("adb: not found") }
	got, err := reverseTargets(reverseRequest{
		Pools:  map[string]string{"devices": "emulator:pixel-a"},
		DryRun: true,
	})
	if err != nil {
		t.Fatalf("reverseTargets: %v", err)
	}
	if len(got) != 1 || got[0].Address != "emulator:pixel-a" || got[0].Booted {
		t.Fatalf("got %+v, want the unbooted item", got)
	}
}

// Naming a device and having every name be blank is a mistake to report, not a
// reason to talk about pools.
func TestReverseTargetsRejectsAllEmptyDevices(t *testing.T) {
	stubADB(t, nil, nil)
	_, err := reverseTargets(reverseRequest{Devices: []string{" ", ""}})
	if err == nil || !strings.Contains(err.Error(), "--device") {
		t.Fatalf("expected an error naming --device, got %v", err)
	}
}

// stubReverse captures every adb invocation reversePorts would run, and
// answers each with the queued (output, error) pair — so the reverse path is
// exercised end to end without an adb on the machine.
type fakeReverse struct {
	calls  [][]string
	fail   error
	output string
}

func (f *fakeReverse) install(t *testing.T) {
	t.Helper()
	runADBReverse = func(args []string) (string, error) {
		f.calls = append(f.calls, args)
		return f.output, f.fail
	}
	t.Cleanup(func() { runADBReverse = execADBReverse })
}

func TestReversePortsIssuesOneAdbCallPerPort(t *testing.T) {
	fake := &fakeReverse{}
	fake.install(t)
	if err := reversePorts("emulator-5554", testAllocation().Ports, false); err != nil {
		t.Fatalf("reversePorts: %v", err)
	}
	want := []string{
		"-s emulator-5554 reverse tcp:4020 tcp:4020",
		"-s emulator-5554 reverse tcp:50071 tcp:50071",
	}
	if len(fake.calls) != len(want) {
		t.Fatalf("ran %v, want %v", fake.calls, want)
	}
	for i, w := range want {
		if got := strings.Join(fake.calls[i], " "); got != w {
			t.Errorf("call %d = %q, want %q", i, got, w)
		}
	}
}

// The tolerance that keeps --remove re-runnable, and the line that keeps it
// honest: an absent listener is the desired end state, any other adb failure
// is not.
func TestReversePortsTreatsAnAbsentListenerAsRemoved(t *testing.T) {
	fake := &fakeReverse{output: "error: listener 'tcp:4020' not found", fail: errors.New("exit status 1")}
	fake.install(t)
	ports := []ResolvedPort{{Name: "APP_PORT", Value: 4020}}
	if err := reversePorts("emulator-5554", ports, true); err != nil {
		t.Errorf("removing an absent listener must succeed, got %v", err)
	}
	// The same failure while ADDING is a real failure.
	if err := reversePorts("emulator-5554", ports, false); err == nil {
		t.Error("an adb failure on add must surface")
	}
	fake.output = "error: device 'RFCR921L4DP' not found"
	if err := reversePorts("emulator-5554", ports, true); err == nil {
		t.Error("a missing device must not be reported as a successful removal")
	}
}

// A failure stops the run rather than reversing the rest quietly: a half-open
// device is the state that costs an hour to debug.
func TestReversePortsStopsOnTheFirstFailure(t *testing.T) {
	fake := &fakeReverse{output: "error: cannot bind listener", fail: errors.New("exit status 1")}
	fake.install(t)
	err := reversePorts("emulator-5554", testAllocation().Ports, false)
	if err == nil || !strings.Contains(err.Error(), "cannot bind listener") {
		t.Fatalf("err = %v, want adb's own message", err)
	}
	if len(fake.calls) != 1 {
		t.Errorf("ran %d calls, want 1 — the run must stop at the first failure", len(fake.calls))
	}
}

// --dry-run executes nothing. The seam proves it: a dry run that reached adb
// would show up here as a call.
func TestPrintReversePlanRunsNothing(t *testing.T) {
	fake := &fakeReverse{}
	fake.install(t)
	printReversePlan(reverseTarget{Address: "emulator:pixel-a"}, testAllocation().Ports, false)
	if len(fake.calls) != 0 {
		t.Errorf("a dry run executed %v", fake.calls)
	}
}
