package cmd

import (
	"strings"
	"testing"
)

// These tests exercise the `pools:` primitive end to end: schema validation
// at load time, the AllocatePools claim-scan / first-free picker, and the
// template-data and hook-env plumbing that exposes allocations to user
// templates and shell commands. Everything is hand-crafted WetConfig
// structs — no real git worktree or state file involved, since the
// allocator is pure logic over passed-in state slices.

func validPoolsCfg() *WetConfig {
	return &WetConfig{
		ProjectName: "myapp",
		PortStride:  10,
		Pools: map[string][]string{
			"devices": {"item-a", "item-b", "item-c"},
			"fixtures": {
				"fresh-snapshot-a",
				"fresh-snapshot-b",
			},
		},
	}
}

func TestPoolsValidate_OK(t *testing.T) {
	cfg := validPoolsCfg()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPoolsValidate_EmptyList(t *testing.T) {
	cfg := &WetConfig{
		ProjectName: "myapp",
		PortStride:  10,
		Pools:       map[string][]string{"devices": {}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected empty-list error, got %v", err)
	}
}

func TestPoolsValidate_BadName(t *testing.T) {
	for _, bad := range []string{"", "Devices", "1devices", "with space", "with-dash"} {
		t.Run(bad, func(t *testing.T) {
			cfg := &WetConfig{
				ProjectName: "myapp",
				PortStride:  10,
				Pools:       map[string][]string{bad: {"a"}},
			}
			if err := cfg.Validate(); err == nil {
				t.Fatalf("expected name error for pool name %q", bad)
			}
		})
	}
}

func TestPoolsValidate_ReservedName(t *testing.T) {
	// `Pools` is the specifically-added reserved name for this feature.
	// The existing reserved set (`ProjectName`, `Branch`, `BranchSafe`,
	// `Slot`) is capitalized and therefore can't pass the lowercase pool
	// name regex — so "pools" is the meaningful test case.
	cfg := &WetConfig{
		ProjectName: "myapp",
		PortStride:  10,
		Pools:       map[string][]string{"pools": {"a"}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("expected reserved-name error for pool 'pools', got %v", err)
	}
}

func TestPoolsValidate_DuplicateItem(t *testing.T) {
	cfg := &WetConfig{
		ProjectName: "myapp",
		PortStride:  10,
		Pools:       map[string][]string{"devices": {"x", "y", "x"}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate-item error, got %v", err)
	}
}

func TestPoolsValidate_EmptyItem(t *testing.T) {
	cfg := &WetConfig{
		ProjectName: "myapp",
		PortStride:  10,
		Pools:       map[string][]string{"devices": {"x", ""}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected empty-item error")
	}
}

func TestAllocatePools_FirstCreate(t *testing.T) {
	cfg := validPoolsCfg()
	got, err := AllocatePools(cfg, nil, "")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got["devices"] != "item-a" {
		t.Errorf("devices = %q, want item-a", got["devices"])
	}
	if got["fixtures"] != "fresh-snapshot-a" {
		t.Errorf("fixtures = %q, want fresh-snapshot-a", got["fixtures"])
	}
}

func TestAllocatePools_SecondCreate(t *testing.T) {
	cfg := validPoolsCfg()
	existing := []WorktreeState{
		{Branch: "feature/one", Pools: map[string]string{"devices": "item-a", "fixtures": "fresh-snapshot-a"}},
	}
	got, err := AllocatePools(cfg, existing, "")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got["devices"] != "item-b" {
		t.Errorf("devices = %q, want item-b", got["devices"])
	}
	if got["fixtures"] != "fresh-snapshot-b" {
		t.Errorf("fixtures = %q, want fresh-snapshot-b", got["fixtures"])
	}
}

func TestAllocatePools_ReleasedSlot(t *testing.T) {
	// Items 0 and 2 are claimed; item 1 should be picked next, not item 3.
	// This is the released-slot case: popping worktree #2 frees item-b and
	// a subsequent create should claim it even though worktree #3 still
	// holds item-c.
	cfg := &WetConfig{
		ProjectName: "myapp",
		PortStride:  10,
		Pools:       map[string][]string{"devices": {"item-a", "item-b", "item-c", "item-d"}},
	}
	existing := []WorktreeState{
		{Branch: "one", Pools: map[string]string{"devices": "item-a"}},
		{Branch: "three", Pools: map[string]string{"devices": "item-c"}},
	}
	got, err := AllocatePools(cfg, existing, "")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got["devices"] != "item-b" {
		t.Errorf("devices = %q, want item-b (the lowest-indexed free item)", got["devices"])
	}
}

func TestAllocatePools_Exhausted(t *testing.T) {
	cfg := &WetConfig{
		ProjectName: "myapp",
		PortStride:  10,
		Pools:       map[string][]string{"devices": {"item-a", "item-b"}},
	}
	existing := []WorktreeState{
		{Branch: "one", Pools: map[string]string{"devices": "item-a"}},
		{Branch: "two", Pools: map[string]string{"devices": "item-b"}},
	}
	_, err := AllocatePools(cfg, existing, "")
	if err == nil {
		t.Fatal("expected exhaustion error")
	}
	msg := err.Error()
	for _, want := range []string{"devices", "exhausted", "branch=one", "branch=two", "item-a", "item-b", "Pop a worktree"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q\nfull: %s", want, msg)
		}
	}
}

func TestAllocatePools_NoPoolsConfigured(t *testing.T) {
	// Back-compat path: config without `pools:` returns empty map, no error.
	cfg := &WetConfig{ProjectName: "myapp", PortStride: 10}
	got, err := AllocatePools(cfg, []WorktreeState{{Branch: "x"}}, "")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got = %v, want empty map", got)
	}
}

func TestAllocatePools_IgnoresLegacyStateEntries(t *testing.T) {
	// Legacy WorktreeState (no Pools map) must not crash the allocator.
	cfg := &WetConfig{
		ProjectName: "myapp",
		PortStride:  10,
		Pools:       map[string][]string{"devices": {"item-a", "item-b"}},
	}
	existing := []WorktreeState{
		{Branch: "legacy"}, // no Pools field
	}
	got, err := AllocatePools(cfg, existing, "")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got["devices"] != "item-a" {
		t.Errorf("devices = %q, want item-a", got["devices"])
	}
}

func TestBuildTemplateData_WithPools(t *testing.T) {
	cfg := validPoolsCfg()
	data := BuildTemplateData(cfg, "feature/foo", 1, nil, map[string]string{
		"devices":  "item-a",
		"fixtures": "fresh-snapshot-a",
	})
	poolsRaw, ok := data["Pools"]
	if !ok {
		t.Fatal("data missing 'Pools' key")
	}
	pools, ok := poolsRaw.(map[string]string)
	if !ok {
		t.Fatalf("data['Pools'] is %T, want map[string]string", poolsRaw)
	}
	if pools["devices"] != "item-a" {
		t.Errorf("pools[devices] = %q, want item-a", pools["devices"])
	}
	if pools["fixtures"] != "fresh-snapshot-a" {
		t.Errorf("pools[fixtures] = %q, want fresh-snapshot-a", pools["fixtures"])
	}

	// Verify template resolution: {{.Pools.devices}} should render to
	// "item-a" when passed through the same render pipeline env: values use.
	rendered, err := renderWetTemplate("poolcheck", "device={{.Pools.devices}}", data)
	if err != nil {
		t.Fatalf("unexpected render error: %v", err)
	}
	if rendered != "device=item-a" {
		t.Errorf("rendered = %q, want 'device=item-a'", rendered)
	}
}

func TestBuildTemplateData_PoolsKeyPresentWhenNil(t *testing.T) {
	// Callers with no allocations pass nil. Pools should still be present
	// in data as an empty map so `{{.Pools}}` itself resolves (individual
	// lookups via `{{.Pools.foo}}` still fail with missingkey=error — which
	// is correct: the caller referenced an undeclared pool).
	cfg := &WetConfig{ProjectName: "myapp", PortStride: 10}
	data := BuildTemplateData(cfg, "feature/foo", 1, nil, nil)
	poolsRaw, ok := data["Pools"]
	if !ok {
		t.Fatal("data missing 'Pools' key when pools=nil")
	}
	if _, ok := poolsRaw.(map[string]string); !ok {
		t.Errorf("data['Pools'] is %T, want map[string]string", poolsRaw)
	}
}

func TestBuildHookEnv_WithPools(t *testing.T) {
	cfg := &WetConfig{
		ProjectName: "myapp",
		PortStride:  10,
		Pools: map[string][]string{
			"devices":       {"item-a"},
			"test_fixtures": {"snap-1"},
		},
	}
	data := BuildTemplateData(cfg, "feature/foo", 1, nil, map[string]string{
		"devices":       "item-a",
		"test_fixtures": "snap-1",
	})
	env, err := BuildHookEnv(cfg, data, nil, nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	seen := make(map[string]bool, len(env))
	for _, kv := range env {
		seen[kv] = true
	}
	for _, want := range []string{
		"POOL_DEVICES=item-a",
		"POOL_TEST_FIXTURES=snap-1",
	} {
		if !seen[want] {
			t.Errorf("env missing %q", want)
		}
	}
}
