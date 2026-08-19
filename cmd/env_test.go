package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testEnvConfig() *WetConfig {
	return &WetConfig{
		ProjectName: "myapp",
		PortStride:  10,
		Ports: []PortConfig{
			{Name: "APP_PORT", Base: 8080},
			{Name: "GRPC_PORT", Base: 50051},
		},
		Env:   map[string]string{"COMPOSE_PROJECT_NAME": "{{.ProjectName}}-{{.BranchSafe}}"},
		Pools: map[string][]string{"devices": {"emulator-5554"}},
	}
}

func TestDataFromEnvReadsHookVariables(t *testing.T) {
	cfg := testEnvConfig()
	t.Setenv("SLOT", "2")
	t.Setenv("BRANCH", "feature/x")
	t.Setenv("APP_PORT", "8100")
	t.Setenv("GRPC_PORT", "50071")
	t.Setenv("POOL_DEVICES", "emulator-5556")

	slot, ok := slotFromEnv()
	if !ok || slot != 2 {
		t.Fatalf("slotFromEnv() = %d, %v; want 2, true", slot, ok)
	}
	data, err := dataFromEnv(cfg, t.TempDir(), slot)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if data["APP_PORT"] != 8100 || data["GRPC_PORT"] != 50071 {
		t.Errorf("ports not read from env: %v %v", data["APP_PORT"], data["GRPC_PORT"])
	}
	if data["Slot"] != 2 || data["BranchSafe"] != "feature-x" {
		t.Errorf("identity fields wrong: %v %v", data["Slot"], data["BranchSafe"])
	}
	if pools, _ := data["Pools"].(map[string]string); pools["devices"] != "emulator-5556" {
		t.Errorf("pool not read from env: %v", data["Pools"])
	}
	if data["COMPOSE_PROJECT_NAME"] != "myapp-feature-x" {
		t.Errorf("cfg.Env not layered in: %v", data["COMPOSE_PROJECT_NAME"])
	}
}

func TestDataFromEnvMissingPortIsAnError(t *testing.T) {
	cfg := testEnvConfig()
	t.Setenv("SLOT", "1")
	t.Setenv("BRANCH", "x")
	t.Setenv("APP_PORT", "8090")
	os.Unsetenv("GRPC_PORT")
	if _, err := dataFromEnv(cfg, t.TempDir(), 1); err == nil || !strings.Contains(err.Error(), "GRPC_PORT") {
		t.Fatalf("expected an error naming GRPC_PORT, got %v", err)
	}
}

// BRANCH must be the ONLY thing missing, or the port loop errors first and the
// test passes with the guard deleted — asserting on the message is what makes
// it a test of the guard rather than a test that something went wrong.
func TestDataFromEnvRequiresBranch(t *testing.T) {
	t.Setenv("SLOT", "1")
	t.Setenv("BRANCH", "")
	t.Setenv("APP_PORT", "8090")
	t.Setenv("GRPC_PORT", "50061")
	_, err := dataFromEnv(testEnvConfig(), t.TempDir(), 1)
	if err == nil {
		t.Fatal("expected an error when BRANCH is unset")
	}
	if !strings.Contains(err.Error(), "BRANCH") {
		t.Fatalf("error should name BRANCH, got: %v", err)
	}
}

// A missing key must fail the render rather than producing `port=` — an empty
// value in a Dotenvy `:string!` field is a silent boot failure downstream.
func TestRenderTemplateMissingKeyIsFatal(t *testing.T) {
	data := map[string]any{"APP_PORT": 8090}
	if _, err := renderWetTemplate("t", "port={{.NOPE}}\n", data); err == nil {
		t.Fatal("expected missing key to be an error")
	}
	out, err := renderWetTemplate("t", "port={{.APP_PORT}}\n", data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "port=8090\n" {
		t.Errorf("got %q", out)
	}
}

func TestWorktreeStateForPathMatchesResolvedPath(t *testing.T) {
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("evalsymlinks: %v", err)
	}
	state := &WorktreeConfig{Worktrees: []WorktreeState{
		{Branch: "other", Path: filepath.Join(real, "elsewhere")},
		{Branch: "mine", Path: real},
	}}
	// dir may be a symlink (/var vs /private/var on macOS) while the state
	// file records the resolved path — the lookup has to see through that.
	got := worktreeStateForPath(state, dir)
	if got == nil || got.Branch != "mine" {
		t.Fatalf("expected to match the 'mine' entry, got %v", got)
	}
	if worktreeStateForPath(state, filepath.Join(real, "untracked")) != nil {
		t.Error("expected nil for an untracked path")
	}
}

// Slot 0 is the main checkout's, so its ports must be the declared bases and
// no worktree may ever be handed them.
func TestSlotZeroIsMainAndReserved(t *testing.T) {
	cfg := testEnvConfig()
	ports := cfg.ResolvePorts(0)
	want := map[string]int{"APP_PORT": 8080, "GRPC_PORT": 50051}
	for _, p := range ports {
		if want[p.Name] != p.Value {
			t.Errorf("slot 0 %s = %d, want %d", p.Name, p.Value, want[p.Name])
		}
	}
	if got := NextFreeSlot(nil); got == 0 {
		t.Error("NextFreeSlot handed out slot 0, which belongs to the main checkout")
	}
}

func TestResolveEnvValuesChainedDerivation(t *testing.T) {
	cfg := &WetConfig{
		ProjectName: "myapp",
		Env: map[string]string{
			// Deliberately declared so that sorted order alone cannot resolve
			// them: AUTH_BASE_URL < DEV_HOST, and VIPPS < both.
			"AUTH_BASE_URL":      "http://{{.DEV_HOST}}:{{.PHX_PORT}}",
			"DEV_HOST":           "localhost",
			"VIPPS_REDIRECT_URI": "{{.AUTH_BASE_URL}}/callback",
		},
	}
	data := map[string]any{"PHX_PORT": 4010}
	got, err := ResolveEnvValues(cfg, data, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["AUTH_BASE_URL"] != "http://localhost:4010" {
		t.Errorf("AUTH_BASE_URL = %q", got["AUTH_BASE_URL"])
	}
	if got["VIPPS_REDIRECT_URI"] != "http://localhost:4010/callback" {
		t.Errorf("VIPPS_REDIRECT_URI = %q", got["VIPPS_REDIRECT_URI"])
	}
}

// .env.local is a machine override, so it must win over the checked-in
// default AND flow into everything derived from it.
func TestResolveEnvValuesLocalOverrideWinsAndPropagates(t *testing.T) {
	cfg := &WetConfig{
		ProjectName: "myapp",
		Env: map[string]string{
			"DEV_HOST":      "localhost",
			"AUTH_BASE_URL": "http://{{.DEV_HOST}}:{{.PHX_PORT}}",
		},
	}
	data := map[string]any{"PHX_PORT": 4000}
	got, err := ResolveEnvValues(cfg, data, map[string]string{"DEV_HOST": "192.168.1.9"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["DEV_HOST"] != "192.168.1.9" {
		t.Errorf("override lost: %q", got["DEV_HOST"])
	}
	if got["AUTH_BASE_URL"] != "http://192.168.1.9:4000" {
		t.Errorf("override did not propagate into the derived value: %q", got["AUTH_BASE_URL"])
	}
}

func TestResolveEnvValuesReportsCyclesAndTypos(t *testing.T) {
	cases := map[string]map[string]string{
		"cycle": {"A": "{{.B}}", "B": "{{.A}}"},
		"typo":  {"A": "{{.NOPE}}"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := &WetConfig{ProjectName: "x", Env: env}
			_, err := ResolveEnvValues(cfg, map[string]any{}, nil)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), "A") {
				t.Errorf("error should name the stuck key, got: %v", err)
			}
		})
	}
}

func TestLoadEnvLocal(t *testing.T) {
	dir := t.TempDir()
	body := "# comment\n\nDEV_HOST=192.168.1.9\nQUOTED=\"has spaces\"\nSINGLE='sq'\n  SPACED = padded \n"
	if err := os.WriteFile(filepath.Join(dir, envLocalFileName), []byte(body), 0600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := LoadEnvLocal(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]string{"DEV_HOST": "192.168.1.9", "QUOTED": "has spaces", "SINGLE": "sq", "SPACED": "padded"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}

	// A missing file is normal, not an error — the file is optional.
	empty, err := LoadEnvLocal(t.TempDir())
	if err != nil || len(empty) != 0 {
		t.Errorf("missing .env.local should yield an empty map, got %v, %v", empty, err)
	}

	// A malformed line is loud: a silently dropped override would show up as
	// a mystery wrong hostname much later.
	bad := t.TempDir()
	if err := os.WriteFile(filepath.Join(bad, envLocalFileName), []byte("NOT_AN_ASSIGNMENT\n"), 0600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := LoadEnvLocal(bad); err == nil {
		t.Error("expected an error for a line without '='")
	}
}
