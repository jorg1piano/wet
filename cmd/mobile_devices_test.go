package cmd

import (
	"strings"
	"testing"
)

// These tests cover the second-device feature: the claim beyond the primary
// that `wet mobile add` records, and everything downstream of it — the
// allocator refusing to hand it to another worktree, the ordering the device
// commands walk it in, and the template/hook views that expose it.

func devicesCfg() *WetConfig {
	return &WetConfig{
		ProjectName: "myapp",
		PortStride:  10,
		Pools: map[string][]string{
			"devices":  {"emulator:pixel-a", "emulator:pixel-b", "emulator:pixel-c"},
			"fixtures": {"snap-alpha", "snap-beta"},
		},
	}
}

// An extra is a claim like any other: the next worktree must not be handed a
// device the first one added, or two stacks drive the same phone.
func TestAllocatePoolsSkipsAnotherWorktreesExtra(t *testing.T) {
	existing := []WorktreeState{{
		Branch:     "chat",
		Pools:      map[string]string{"devices": "emulator:pixel-a"},
		PoolExtras: map[string][]string{"devices": {"emulator:pixel-b"}},
	}}
	got, err := AllocatePools(devicesCfg(), existing, "")
	if err != nil {
		t.Fatalf("AllocatePools: %v", err)
	}
	if got["devices"] != "emulator:pixel-c" {
		t.Errorf("devices = %q, want emulator:pixel-c — pixel-b is another worktree's extra", got["devices"])
	}
}

// Exhaustion has to count extras too, and name them: "all 3 claimed" while
// listing two is the kind of message that sends you looking for a bug in the
// allocator instead of at the worktree holding the third.
func TestAllocatePoolsExhaustionNamesExtras(t *testing.T) {
	cfg := &WetConfig{
		ProjectName: "myapp",
		PortStride:  10,
		Pools:       map[string][]string{"devices": {"a", "b"}},
	}
	existing := []WorktreeState{{
		Branch:     "chat",
		Pools:      map[string]string{"devices": "a"},
		PoolExtras: map[string][]string{"devices": {"b"}},
	}}
	_, err := AllocatePools(cfg, existing, "")
	if err == nil {
		t.Fatal("expected an exhaustion error")
	}
	for _, want := range []string{"item=a", "item=b", "branch=chat"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %s, got: %v", want, err)
		}
	}
}

// A setup re-run must not move a worktree onto a different emulator — least
// of all onto one of its own extras, which is what the plain first-free scan
// would pick once its entry is excluded from the claim list.
func TestAllocatePoolsKeepingHoldsThePrimary(t *testing.T) {
	cfg := devicesCfg()
	own := WorktreeState{
		Branch:     "chat",
		Pools:      map[string]string{"devices": "emulator:pixel-b"},
		PoolExtras: map[string][]string{"devices": {"emulator:pixel-c"}},
	}
	got, err := AllocatePoolsKeeping(cfg, []WorktreeState{own}, own.Pools, "", false)
	if err != nil {
		t.Fatalf("AllocatePoolsKeeping: %v", err)
	}
	if got["devices"] != "emulator:pixel-b" {
		t.Errorf("devices = %q, want the kept pixel-b", got["devices"])
	}
}

// A kept item that the config no longer declares is not kept: the pool it
// belonged to has been rewritten, and holding a device nobody lists means
// rendering an AVD this machine may not have.
func TestAllocatePoolsKeepingDropsAnUndeclaredItem(t *testing.T) {
	got, err := AllocatePoolsKeeping(devicesCfg(), nil, map[string]string{"devices": "emulator:gone"}, "", false)
	if err != nil {
		t.Fatalf("AllocatePoolsKeeping: %v", err)
	}
	if got["devices"] != "emulator:pixel-a" {
		t.Errorf("devices = %q, want a fresh pick", got["devices"])
	}
}

// allocatePoolItem is what `wet mobile add` draws from, and it counts the
// asking worktree's own claims — the whole point is the NEXT device.
func TestAllocatePoolItemSkipsTheCallersOwnClaims(t *testing.T) {
	own := WorktreeState{
		Branch:     "chat",
		Pools:      map[string]string{"devices": "emulator:pixel-a"},
		PoolExtras: map[string][]string{"devices": {"emulator:pixel-b"}},
	}
	got, err := allocatePoolItem(devicesCfg(), "devices", []WorktreeState{own}, "", false)
	if err != nil {
		t.Fatalf("allocatePoolItem: %v", err)
	}
	if got != "emulator:pixel-c" {
		t.Errorf("got %q, want emulator:pixel-c", got)
	}
}

// Claims walk pools in name order, primary before extras, so two runs of
// `wet mobile reverse-tcp` print the same thing in the same order.
func TestOrderedPoolClaimsPutsPrimaryFirstPerPool(t *testing.T) {
	claims := orderedPoolClaims(
		map[string]string{"devices": "emulator:pixel-a", "fixtures": "snap-alpha"},
		map[string][]string{"devices": {"emulator:pixel-b", "RFCR921L4DP"}},
	)
	want := []string{"emulator:pixel-a", "emulator:pixel-b", "RFCR921L4DP", "snap-alpha"}
	if len(claims) != len(want) {
		t.Fatalf("got %d claims, want %d: %+v", len(claims), len(want), claims)
	}
	for i, item := range want {
		if claims[i].Item != item {
			t.Errorf("claim %d = %q, want %q", i, claims[i].Item, item)
		}
	}
}

// A pool holding only extras (its primary released by hand) still appears —
// dropping it would hide a claimed device from every command that lists.
func TestOrderedPoolClaimsIncludesAPoolWithOnlyExtras(t *testing.T) {
	claims := orderedPoolClaims(nil, map[string][]string{"devices": {"emulator:pixel-b"}})
	if len(claims) != 1 || claims[0].Pool != "devices" || claims[0].Item != "emulator:pixel-b" {
		t.Fatalf("got %+v, want the lone extra", claims)
	}
}

// The list views carry every claim; the scalar views keep meaning the
// primary. That split is the compatibility promise: a config rendering
// {{.Handles.devices}} into FLUTTER_DEVICE_ID cannot start emitting two.
func TestWithPoolExtrasLeavesTheScalarViewsAlone(t *testing.T) {
	cfg := devicesCfg()
	data := BuildTemplateData(cfg, "feature/x", 1, nil, map[string]string{"devices": "emulator:pixel-a"})
	WithPoolHandles(data, map[string]string{"devices": "emulator-5554"})
	WithPoolExtras(data,
		map[string][]string{"devices": {"emulator:pixel-b"}},
		map[string][]string{"devices": {"emulator-5556"}})

	if got := data["Handles"].(map[string]string)["devices"]; got != "emulator-5554" {
		t.Errorf("Handles.devices = %q, want the primary's serial", got)
	}
	if got := data["Pools"].(map[string]string)["devices"]; got != "emulator:pixel-a" {
		t.Errorf("Pools.devices = %q, want the primary item", got)
	}
	allPools := data["AllPools"].(map[string][]string)["devices"]
	if len(allPools) != 2 || allPools[0] != "emulator:pixel-a" || allPools[1] != "emulator:pixel-b" {
		t.Errorf("AllPools.devices = %v, want both items, primary first", allPools)
	}
	allHandles := data["AllHandles"].(map[string][]string)["devices"]
	if len(allHandles) != 2 || allHandles[0] != "emulator-5554" || allHandles[1] != "emulator-5556" {
		t.Errorf("AllHandles.devices = %v, want both serials, primary first", allHandles)
	}
}

// A worktree with no extras must render the list views too, or a template
// written for two devices breaks on every one-device worktree.
func TestBuildTemplateDataListViewsHoldThePrimaryAlone(t *testing.T) {
	data := BuildTemplateData(devicesCfg(), "feature/x", 1, nil, map[string]string{"devices": "emulator:pixel-a"})
	rendered, err := renderWetTemplate("all", "{{index .AllHandles.devices 0}}", data)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if rendered != "emulator:pixel-a" {
		t.Errorf("rendered = %q, want the primary as the only entry", rendered)
	}
}

// Applying the same extras twice is what a caller reusing a data map does.
// It must not double them — a hook looping over the list would then reverse
// onto, and later kill, the same device twice.
func TestWithPoolExtrasIsIdempotent(t *testing.T) {
	data := BuildTemplateData(devicesCfg(), "feature/x", 1, nil, map[string]string{"devices": "emulator:pixel-a"})
	extras := map[string][]string{"devices": {"emulator:pixel-b"}}
	WithPoolExtras(data, extras, nil)
	WithPoolExtras(data, extras, nil)
	if got := data["AllPools"].(map[string][]string)["devices"]; len(got) != 2 {
		t.Errorf("AllPools.devices = %v, want two entries", got)
	}
}

// An extra with no resolved handle falls back to its item, exactly as the
// primary does before anything boots it.
func TestWithPoolExtrasFallsBackToTheItemAsItsOwnHandle(t *testing.T) {
	data := BuildTemplateData(devicesCfg(), "feature/x", 1, nil, map[string]string{"devices": "emulator:pixel-a"})
	WithPoolExtras(data, map[string][]string{"devices": {"RFCR921L4DP"}}, nil)
	got := data["AllHandles"].(map[string][]string)["devices"]
	if len(got) != 2 || got[1] != "RFCR921L4DP" {
		t.Errorf("AllHandles.devices = %v, want the item as its own handle", got)
	}
}

// The _ALL vars are what makes a teardown hook right for one device and for
// three: `for h in $POOL_HANDLE_DEVICES_ALL` kills everything this worktree
// booted, where `$POOL_HANDLE_DEVICES` kills only the first.
func TestBuildHookEnvExportsEveryClaim(t *testing.T) {
	cfg := devicesCfg()
	data := BuildTemplateData(cfg, "feature/x", 1, nil, map[string]string{"devices": "emulator:pixel-a"})
	WithPoolHandles(data, map[string]string{"devices": "emulator-5554"})
	WithPoolExtras(data,
		map[string][]string{"devices": {"emulator:pixel-b"}},
		map[string][]string{"devices": {"emulator-5556"}})

	env, err := BuildHookEnv(cfg, data, nil, nil)
	if err != nil {
		t.Fatalf("BuildHookEnv: %v", err)
	}
	seen := map[string]bool{}
	for _, kv := range env {
		seen[kv] = true
	}
	for _, want := range []string{
		"POOL_DEVICES=emulator:pixel-a",
		"POOL_HANDLE_DEVICES=emulator-5554",
		"POOL_DEVICES_ALL=emulator:pixel-a emulator:pixel-b",
		"POOL_HANDLE_DEVICES_ALL=emulator-5554 emulator-5556",
	} {
		if !seen[want] {
			t.Errorf("hook env missing %q", want)
		}
	}
}

// A hook that calls back into wet (`wet mobile reverse-tcp` as post_create)
// reads its allocation out of the environment, so the extras have to survive
// that round trip.
func TestAllocationFromEnvReadsExtras(t *testing.T) {
	t.Setenv("BRANCH", "feature/x")
	t.Setenv("APP_PORT", "8100")
	t.Setenv("GRPC_PORT", "50071")
	t.Setenv("POOL_DEVICES", "emulator:pixel-a")
	t.Setenv("POOL_DEVICES_ALL", "emulator:pixel-a emulator:pixel-b")

	alloc, err := allocationFromEnv(testEnvConfig(), 2)
	if err != nil {
		t.Fatalf("allocationFromEnv: %v", err)
	}
	if alloc.Pools["devices"] != "emulator:pixel-a" {
		t.Errorf("primary = %q", alloc.Pools["devices"])
	}
	got := alloc.Extras["devices"]
	if len(got) != 1 || got[0] != "emulator:pixel-b" {
		t.Errorf("extras = %v, want [emulator:pixel-b] — the primary is not an extra of itself", got)
	}
}

// Reverses go onto every claimed device: the second phone is on the same
// stack, so it needs the same forwards.
func TestReverseTargetsIncludesExtras(t *testing.T) {
	stubADB(t, nil, nil)
	booted := map[string]string{"emulator:pixel-a": "emulator-5554", "emulator:pixel-b": "emulator-5556"}
	bootPoolItem = func(item string, headless bool) (string, error) { return booted[item], nil }
	got, err := reverseTargets(reverseRequest{
		Pools:  map[string]string{"devices": "emulator:pixel-a"},
		Extras: map[string][]string{"devices": {"emulator:pixel-b"}},
	})
	if err != nil {
		t.Fatalf("reverseTargets: %v", err)
	}
	want := []string{"emulator-5554", "emulator-5556"}
	if addr := addresses(got); len(addr) != 2 || addr[0] != want[0] || addr[1] != want[1] {
		t.Fatalf("got %v, want %v — primary first, then the extra", addr, want)
	}
}

// A fixtures pool holding a snapshot name stays out of adb's way whether it
// is a primary or an extra.
func TestReverseTargetsIgnoresNonDeviceExtras(t *testing.T) {
	stubADB(t, []string{"emulator-5554"}, map[string]string{"pixel-a": "emulator-5554"})
	got, err := reverseTargets(reverseRequest{
		Pools:  map[string]string{"devices": "emulator:pixel-a"},
		Extras: map[string][]string{"fixtures": {"snap-beta"}},
		DryRun: true,
	})
	if err != nil {
		t.Fatalf("reverseTargets: %v", err)
	}
	if len(got) != 1 || got[0].Address != "emulator-5554" {
		t.Fatalf("got %+v, want only the emulator", got)
	}
}

// The "nothing to reverse onto" message lists what the worktree holds, and
// an extra is part of that.
func TestDescribeClaimsNamesEveryItem(t *testing.T) {
	got := describeClaims(
		map[string]string{"devices": "emulator:pixel-a"},
		map[string][]string{"devices": {"emulator:pixel-b"}},
	)
	if got != "devices=emulator:pixel-a, devices=emulator:pixel-b" {
		t.Errorf("describeClaims = %q", got)
	}
	if got := describeClaims(nil, nil); got != "none claimed" {
		t.Errorf("empty describeClaims = %q", got)
	}
}

// --- pool selection ---

func TestDevicePoolNamePrefersDevices(t *testing.T) {
	got, err := devicePoolName(devicesCfg(), "")
	if err != nil {
		t.Fatalf("devicePoolName: %v", err)
	}
	if got != "devices" {
		t.Errorf("got %q, want devices", got)
	}
}

func TestDevicePoolNameUsesTheOnlyPool(t *testing.T) {
	cfg := &WetConfig{Pools: map[string][]string{"phones": {"a"}}}
	got, err := devicePoolName(cfg, "")
	if err != nil {
		t.Fatalf("devicePoolName: %v", err)
	}
	if got != "phones" {
		t.Errorf("got %q, want phones", got)
	}
}

// Guessing between `phones` and `fixtures` would claim a database snapshot
// as a device and hand it to adb.
func TestDevicePoolNameRefusesToGuessBetweenPools(t *testing.T) {
	cfg := &WetConfig{Pools: map[string][]string{"phones": {"a"}, "fixtures": {"snap"}}}
	_, err := devicePoolName(cfg, "")
	if err == nil || !strings.Contains(err.Error(), "--pool") {
		t.Fatalf("expected an error pointing at --pool, got %v", err)
	}
}

func TestDevicePoolNameRejectsAnUndeclaredFlag(t *testing.T) {
	_, err := devicePoolName(devicesCfg(), "phones")
	if err == nil || !strings.Contains(err.Error(), "devices") {
		t.Fatalf("expected an error listing the declared pools, got %v", err)
	}
}

// A config with no pools cannot claim a device, and the error is the config
// change that would fix it.
func TestDevicePoolNameWithNoPoolsExplainsTheConfig(t *testing.T) {
	_, err := devicePoolName(&WetConfig{}, "")
	if err == nil || !strings.Contains(err.Error(), "pool_grow") {
		t.Fatalf("expected an error showing the pools: block to add, got %v", err)
	}
}

// --- claimability ---

func claimTestState() (WorktreeState, []WorktreeState) {
	own := WorktreeState{
		Branch:     "chat",
		Path:       "/repo/worktree-chat",
		Pools:      map[string]string{"devices": "emulator:pixel-a"},
		PoolExtras: map[string][]string{"devices": {"emulator:pixel-b"}},
	}
	other := WorktreeState{
		Branch: "torget",
		Path:   "/repo/worktree-torget",
		Pools:  map[string]string{"devices": "emulator:pixel-c"},
	}
	return own, []WorktreeState{own, other}
}

// Claiming a device another worktree holds is the failure this whole feature
// exists to prevent, so the error names the holder and how to free it.
func TestCheckClaimableRejectsAnotherWorktreesDevice(t *testing.T) {
	own, all := claimTestState()
	err := checkClaimable(devicesCfg(), "devices", "emulator:pixel-c", all, &own, nil)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	for _, want := range []string{"torget", "worktree-torget"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name the holder (%s), got: %v", want, err)
		}
	}
}

// Asking for a device you already hold is a mistake worth naming precisely:
// "claimed by worktree 'chat'" would read as a conflict with someone else.
func TestCheckClaimableRejectsOwnPrimaryAndExtra(t *testing.T) {
	own, all := claimTestState()
	for item, role := range map[string]string{
		"emulator:pixel-a": "primary",
		"emulator:pixel-b": "extra",
	} {
		err := checkClaimable(devicesCfg(), "devices", item, all, &own, nil)
		if err == nil || !strings.Contains(err.Error(), role) {
			t.Errorf("%s: expected an error naming it as this worktree's %s, got %v", item, role, err)
		}
	}
}

// An emulator: item wet has never seen is claimable — ensureAVDExists creates
// it — but a bare string is not: nothing can boot it or reverse onto it.
func TestCheckClaimableAcceptsAnEmulatorItemAndRejectsAStranger(t *testing.T) {
	own, all := claimTestState()
	if err := checkClaimable(devicesCfg(), "devices", "emulator:brand-new", all, &own, nil); err != nil {
		t.Errorf("an emulator: item must be claimable: %v", err)
	}
	err := checkClaimable(devicesCfg(), "devices", "some-phone", all, &own, map[string]bool{"RFCR921L4DP": true})
	if err == nil || !strings.Contains(err.Error(), "RFCR921L4DP") {
		t.Fatalf("expected a refusal listing the attached devices, got %v", err)
	}
}

// A plugged-in phone is claimable by serial even though no pool lists it —
// that is the escape hatch for the device you just connected.
func TestCheckClaimableAcceptsAnAttachedSerial(t *testing.T) {
	own, all := claimTestState()
	if err := checkClaimable(devicesCfg(), "devices", "RFCR921L4DP", all, &own,
		map[string]bool{"RFCR921L4DP": true}); err != nil {
		t.Errorf("an attached serial must be claimable: %v", err)
	}
}

// --- removal ---

// The primary belongs to the worktree's lifecycle: releasing it would leave
// {{.Handles.devices}} rendering a device nothing claims.
func TestMatchExtraClaimRefusesThePrimary(t *testing.T) {
	stubADB(t, nil, nil)
	own, _ := claimTestState()
	_, err := matchExtraClaim(own, "", "emulator:pixel-a")
	if err == nil || !strings.Contains(err.Error(), "primary") {
		t.Fatalf("expected a refusal naming it as the primary, got %v", err)
	}
	if !strings.Contains(err.Error(), "wet worktree pop") {
		t.Errorf("the refusal should say what does release it, got: %v", err)
	}
}

// `wet mobile list` prints serials, so a serial is what gets pasted back into
// `wet mobile remove`.
func TestMatchExtraClaimMatchesByRunningSerial(t *testing.T) {
	stubADB(t, nil, map[string]string{"pixel-b": "emulator-5556"})
	own, _ := claimTestState()
	got, err := matchExtraClaim(own, "", "emulator-5556")
	if err != nil {
		t.Fatalf("matchExtraClaim: %v", err)
	}
	if got.Item != "emulator:pixel-b" {
		t.Errorf("matched %q, want emulator:pixel-b", got.Item)
	}
}

func TestMatchExtraClaimListsTheExtrasOnAMiss(t *testing.T) {
	stubADB(t, nil, nil)
	own, _ := claimTestState()
	_, err := matchExtraClaim(own, "", "emulator:nope")
	if err == nil || !strings.Contains(err.Error(), "emulator:pixel-b") {
		t.Fatalf("expected an error listing the extras, got %v", err)
	}
}

func TestMatchExtraClaimWithNoExtrasSaysSo(t *testing.T) {
	stubADB(t, nil, nil)
	ws := WorktreeState{Branch: "chat", Pools: map[string]string{"devices": "emulator:pixel-a"}}
	_, err := matchExtraClaim(ws, "", "emulator:pixel-b")
	if err == nil || !strings.Contains(err.Error(), "none is claimed beyond the primary") {
		t.Fatalf("expected the no-extras error, got %v", err)
	}
}

// --- listing ---

func TestClaimsOfMarksThePrimaryAndPeeksHandles(t *testing.T) {
	stubADB(t, nil, map[string]string{"pixel-a": "emulator-5554"})
	own, _ := claimTestState()
	rows := claimsOf(own, nil)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(rows), rows)
	}
	if !rows[0].Primary || rows[0].Handle != "emulator-5554" || !rows[0].Running {
		t.Errorf("primary row = %+v, want the running serial", rows[0])
	}
	if rows[1].Primary || rows[1].Handle != "emulator:pixel-b" || rows[1].Running {
		t.Errorf("extra row = %+v, want the stopped item, not primary", rows[1])
	}
}

// A physical device's "running" is whether adb sees it, not whether an
// emulator console answers — there is no console to ask.
func TestClaimsOfReadsPhysicalPresenceFromAdb(t *testing.T) {
	stubADB(t, nil, nil)
	ws := WorktreeState{
		Branch:     "chat",
		Pools:      map[string]string{"devices": "RFCR921L4DP"},
		PoolExtras: map[string][]string{"devices": {"UNPLUGGED1"}},
	}
	rows := claimsOf(ws, map[string]bool{"RFCR921L4DP": true})
	if !rows[0].Running {
		t.Errorf("attached phone = %+v, want running", rows[0])
	}
	if rows[1].Running {
		t.Errorf("unplugged phone = %+v, want not running", rows[1])
	}
}

// --- state ---

func TestPoolClaimsPutsThePrimaryFirst(t *testing.T) {
	ws := WorktreeState{
		Pools:      map[string]string{"devices": "a"},
		PoolExtras: map[string][]string{"devices": {"b", "c"}},
	}
	got := ws.poolClaims("devices")
	if len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Errorf("poolClaims = %v, want [a b c]", got)
	}
	if got := ws.poolClaims("fixtures"); len(got) != 0 {
		t.Errorf("poolClaims of an unclaimed pool = %v, want none", got)
	}
}
