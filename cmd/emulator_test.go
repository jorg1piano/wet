package cmd

import "testing"

// resolvePoolHandles must leave physical devices alone — they are already the
// address you target, and touching adb for them would make a pool of phones
// depend on an emulator toolchain being installed.
func TestResolvePoolHandlesPassesThroughNonEmulators(t *testing.T) {
	items := map[string]string{
		"devices":  "RFCR921L4DP",
		"fixtures": "snapshot-a",
	}
	got, err := resolvePoolHandles(items, false)
	if err != nil {
		t.Fatalf("resolvePoolHandles: %v", err)
	}
	for pool, want := range items {
		if got[pool] != want {
			t.Errorf("%s = %q, want %q", pool, got[pool], want)
		}
	}
}

// The handle map defaults to the items, so a config with no emulator pool —
// and every path that never resolves — still renders {{.Handles.x}}.
func TestBuildTemplateDataDefaultsHandlesToItems(t *testing.T) {
	cfg := &WetConfig{ProjectName: "demo"}
	data := BuildTemplateData(cfg, "feature/x", 2, nil, map[string]string{"devices": "emulator:pixel-a"})
	handles, ok := data["Handles"].(map[string]string)
	if !ok {
		t.Fatalf("Handles missing or wrong type: %T", data["Handles"])
	}
	if handles["devices"] != "emulator:pixel-a" {
		t.Errorf("default handle = %q, want the raw item", handles["devices"])
	}

	WithPoolHandles(data, map[string]string{"devices": "emulator-5554"})
	handles = data["Handles"].(map[string]string)
	if handles["devices"] != "emulator-5554" {
		t.Errorf("resolved handle = %q, want emulator-5554", handles["devices"])
	}
	// The item itself must survive: teardown asks "is this mine to kill?"
	if pools := data["Pools"].(map[string]string); pools["devices"] != "emulator:pixel-a" {
		t.Errorf("Pools.devices = %q, want the raw item", pools["devices"])
	}
}

func TestHandlesIsAReservedTemplateField(t *testing.T) {
	if !isReservedTemplateField("Handles") {
		t.Error("Handles must be reserved, or a pool named `handles` would shadow it")
	}
}
