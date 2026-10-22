package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// devicesSetupRepo is a repo whose devices pool holds three phones, so setup
// never boots an emulator, and a worktree of it with the cwd inside.
func devicesSetupRepo(t *testing.T) (repo, wt string) {
	t.Helper()
	isolateMachine(t)
	repo = initCleanupRepo(t)
	config := `project_name: app
env:
  FLUTTER_DEVICE_ID: '{{.Handles.devices}}'
pools:
  devices: [phone-a, phone-b, phone-c]
`
	if err := os.WriteFile(filepath.Join(repo, wetConfigFileName), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	wt = addWorktree(t, repo, "feature")
	t.Chdir(wt)
	prev := listAttachedSerials
	listAttachedSerials = func() ([]string, error) { return []string{"phone-a", "phone-b", "phone-c"}, nil }
	t.Cleanup(func() {
		listAttachedSerials = prev
		setupDevices = deviceRequest{Count: -1}
	})
	return repo, wt
}

func setupWith(t *testing.T, repo, wt string, req deviceRequest) *WorktreeState {
	t.Helper()
	setupDevices = req
	if err := runSetupCore(repo, wt, "feature", false, false); err != nil {
		t.Fatal(err)
	}
	state, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	own := worktreeStateForPath(state, wt)
	if own == nil {
		t.Fatal("setup recorded no entry")
	}
	return own
}

func TestSetupDevicesZeroClaimsNothingAndKeepsItOnARerun(t *testing.T) {
	repo, wt := devicesSetupRepo(t)

	own := setupWith(t, repo, wt, deviceRequest{Count: 0})
	if len(own.poolClaimsAll()) != 0 || !reflect.DeepEqual(own.NoPools, []string{"devices"}) {
		t.Fatalf("entry = %+v, want no device and devices opted out", own)
	}
	env, err := os.ReadFile(filepath.Join(wt, envWorktreeFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(env), "FLUTTER_DEVICE_ID=\n") && !strings.Contains(string(env), "FLUTTER_DEVICE_ID=''") &&
		!strings.Contains(string(env), `FLUTTER_DEVICE_ID=""`) {
		t.Fatalf("a web-only worktree must render an empty device:\n%s", env)
	}

	own = setupWith(t, repo, wt, deviceRequest{Count: -1})
	if len(own.poolClaimsAll()) != 0 {
		t.Fatalf("a plain re-run claimed %v; web only must stay web only", own.poolClaimsAll())
	}

	own = setupWith(t, repo, wt, deviceRequest{Count: 1})
	if own.Pools["devices"] == "" || len(own.NoPools) != 0 {
		t.Fatalf("--devices 1 after web only = %+v, want a primary and no opt-out", own)
	}
}

func TestSetupDevicesThreeClaimsTheExtras(t *testing.T) {
	repo, wt := devicesSetupRepo(t)
	own := setupWith(t, repo, wt, deviceRequest{Count: 3})
	if got := own.poolClaims("devices"); !reflect.DeepEqual(got, []string{"phone-a", "phone-b", "phone-c"}) {
		t.Fatalf("claims = %v, want all three phones", got)
	}

	// Asking for fewer keeps the extras, as `wet mobile ensure` does.
	own = setupWith(t, repo, wt, deviceRequest{Count: 1})
	if len(own.poolClaims("devices")) != 3 {
		t.Fatalf("--devices 1 released extras: %v", own.poolClaims("devices"))
	}

	own = setupWith(t, repo, wt, deviceRequest{Count: 0})
	if len(own.poolClaimsAll()) != 0 {
		t.Fatalf("--devices 0 must release every device, still holds %v", own.poolClaimsAll())
	}
}

func TestSetupDevicePinsThePrimary(t *testing.T) {
	repo, wt := devicesSetupRepo(t)
	own := setupWith(t, repo, wt, deviceRequest{Count: -1, Items: []string{"phone-c", "phone-a"}})
	if own.Pools["devices"] != "phone-c" || !reflect.DeepEqual(own.PoolExtras["devices"], []string{"phone-a"}) {
		t.Fatalf("entry = %+v, want phone-c primary and phone-a extra", own)
	}
	if !slices.Contains(own.PinnedPools, "devices") {
		t.Fatalf("a --device primary must be pinned: %+v", own)
	}

	own = setupWith(t, repo, wt, deviceRequest{Count: -1})
	if own.Pools["devices"] != "phone-c" {
		t.Fatalf("a re-run moved the pinned primary to %s", own.Pools["devices"])
	}
}

func TestSetupDeviceRefusesAnotherWorktreesDevice(t *testing.T) {
	repo, wt := devicesSetupRepo(t)
	other := addWorktree(t, repo, "other")
	state := &WorktreeConfig{Worktrees: []WorktreeState{
		{Branch: "other", Path: other, Index: 1, Pools: map[string]string{"devices": "phone-b"}},
	}}
	if err := saveConfigTo(projectStatePath(repo), state); err != nil {
		t.Fatal(err)
	}
	setupDevices = deviceRequest{Count: -1, Items: []string{"phone-b"}}
	err := runSetupCore(repo, wt, "feature", false, false)
	if err == nil || !strings.Contains(err.Error(), "claimed by worktree 'other'") {
		t.Fatalf("err = %v, want phone-b refused as other's", err)
	}
}

func TestDeviceRequestValidate(t *testing.T) {
	bad := []deviceRequest{
		{Count: -2},
		{Count: 0, Items: []string{"phone-a"}},
		{Count: 1, Items: []string{"phone-a", "phone-b"}},
		{Count: -1, Items: []string{" "}},
	}
	for _, r := range bad {
		if r.validate() == nil {
			t.Errorf("%+v should be refused", r)
		}
	}
	for _, r := range []deviceRequest{{Count: -1}, {Count: 0}, {Count: 3, Items: []string{"phone-a"}}} {
		if err := r.validate(); err != nil {
			t.Errorf("%+v: %v", r, err)
		}
	}
}

func TestBuildTemplateDataRendersAnUnclaimedDeclaredPoolEmpty(t *testing.T) {
	cfg := &WetConfig{ProjectName: "app", Pools: map[string][]string{"devices": {"phone-a"}}}
	data := BuildTemplateData(cfg, "feature", 1, nil, nil)
	got, err := renderWetTemplate("t", "[{{.Handles.devices}}][{{range .AllHandles.devices}}x{{end}}]", data)
	if err != nil {
		t.Fatal(err)
	}
	if got != "[][]" {
		t.Fatalf("rendered %q, want an empty device", got)
	}
	if _, err := renderWetTemplate("t", "{{.Handles.phones}}", data); err == nil {
		t.Fatal("an undeclared pool must still fail")
	}
}

func TestFormatSlotPoolsShowsWebOnly(t *testing.T) {
	if got := formatSlotPools(WorktreeState{NoPools: []string{"devices"}}); got != "devices=none" {
		t.Fatalf("got %q", got)
	}
}

// slot_reset hooks run `wet mobile reverse-tcp`; on a web-only worktree that
// must be a no-op rather than fail the setup.
func TestWorktreeIsWebOnly(t *testing.T) {
	repo, wt := devicesSetupRepo(t)
	setupWith(t, repo, wt, deviceRequest{Count: 0})
	if !worktreeIsWebOnly(wt) {
		t.Fatal("a --devices 0 worktree is web only")
	}
	setupWith(t, repo, wt, deviceRequest{Count: 1})
	if worktreeIsWebOnly(wt) {
		t.Fatal("a worktree holding a device is not web only")
	}
}
