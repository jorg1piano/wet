package cmd

import (
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestHostEnvKeys(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want []string
	}{
		{
			name: "localhost and loopback ip",
			env: map[string]string{
				"DEV_HOST":  "localhost",
				"API_HOST":  "127.0.0.1",
				"OTHER":     "example.com",
				"AUTH_URL":  "http://{{.DEV_HOST}}:4000",
				"WITH_PAD":  "  localhost  ",
				"NOT_QUITE": "localhost:4000",
			},
			want: []string{"API_HOST", "DEV_HOST", "WITH_PAD"},
		},
		{name: "none found", env: map[string]string{"A": "b"}, want: nil},
		{name: "empty env", env: nil, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hostEnvKeys(tt.env)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("hostEnvKeys() = %v, want %v", got, tt.want)
			}
		})
	}
}

const devicectlFixture = `{
  "info": {"outcome": "success"},
  "result": {
    "devices": [
      {
        "identifier": "00008120-001A2B3C4D5E6F7G",
        "deviceProperties": {"name": "Jorgen's iPhone"},
        "connectionProperties": {"tunnelState": "connected", "pairingState": "paired"}
      },
      {
        "identifier": "00008120-UNAVAILABLE",
        "deviceProperties": {"name": "Old iPad"},
        "connectionProperties": {"tunnelState": "unavailable"}
      },
      {
        "identifier": "00008120-NOFIELDS"
      },
      {
        "deviceProperties": {"name": "no identifier at all"}
      }
    ]
  }
}`

func TestParseDevicectlJSON(t *testing.T) {
	devices, err := parseDevicectlJSON([]byte(devicectlFixture))
	if err != nil {
		t.Fatalf("parseDevicectlJSON: %v", err)
	}
	want := []mobileDevice{
		{ID: "00008120-001A2B3C4D5E6F7G", Label: "Jorgen's iPhone", Kind: kindIOSPhysical},
		// no tunnelState means "assume reachable", missing name tolerated
		{ID: "00008120-NOFIELDS", Label: "", Kind: kindIOSPhysical},
	}
	if !reflect.DeepEqual(devices, want) {
		t.Errorf("devices = %v, want %v", devices, want)
	}
}

func TestParseDevicectlJSONEmpty(t *testing.T) {
	devices, err := parseDevicectlJSON([]byte(`{"result":{"devices":[]}}`))
	if err != nil {
		t.Fatalf("parseDevicectlJSON: %v", err)
	}
	if len(devices) != 0 {
		t.Errorf("expected no devices, got %v", devices)
	}
	if _, err := parseDevicectlJSON([]byte("not json")); err == nil {
		t.Error("expected an error for malformed JSON")
	}
}

const simctlFixture = `{
  "devices": {
    "com.apple.CoreSimulator.SimRuntime.iOS-17-5": [
      {"udid": "AAAA-1111", "name": "iPhone 15", "state": "Booted"},
      {"udid": "BBBB-2222", "name": "iPhone SE", "state": "Shutdown"},
      {"udid": "CCCC-3333", "name": "no state field"}
    ],
    "com.apple.CoreSimulator.SimRuntime.iOS-16-4": [
      {"name": "missing udid", "state": "Booted"}
    ]
  }
}`

func TestParseSimctlJSON(t *testing.T) {
	devices, err := parseSimctlJSON([]byte(simctlFixture))
	if err != nil {
		t.Fatalf("parseSimctlJSON: %v", err)
	}
	want := []mobileDevice{
		{ID: "AAAA-1111", Label: "iPhone 15", Kind: kindIOSSimulator},
		// a missing state is tolerated (the `booted` filter already applied)
		{ID: "CCCC-3333", Label: "no state field", Kind: kindIOSSimulator},
	}
	if !reflect.DeepEqual(devices, want) {
		t.Errorf("devices = %v, want %v", devices, want)
	}
}

func TestParseSimctlJSONEmpty(t *testing.T) {
	devices, err := parseSimctlJSON([]byte(`{"devices":{}}`))
	if err != nil {
		t.Fatalf("parseSimctlJSON: %v", err)
	}
	if len(devices) != 0 {
		t.Errorf("expected no devices, got %v", devices)
	}
}

func TestMatchDevice(t *testing.T) {
	devices := []mobileDevice{
		{ID: "emulator-5554", Label: "emulator-5554", Kind: kindAndroidEmulator},
		{ID: "RFCR921L4DP", Label: "RFCR921L4DP", Kind: kindAndroidPhysical},
		{ID: "00008120-001A", Label: "Jorgen's iPhone", Kind: kindIOSPhysical},
		{ID: "00008120-002B", Label: "Spare iPhone", Kind: kindIOSPhysical},
	}
	tests := []struct {
		name    string
		query   string
		wantID  string
		wantErr string
	}{
		{name: "exact id", query: "RFCR921L4DP", wantID: "RFCR921L4DP"},
		{name: "unique prefix", query: "emul", wantID: "emulator-5554"},
		{name: "prefix is case-insensitive", query: "rfcr", wantID: "RFCR921L4DP"},
		{name: "unique name", query: "jorgen's iphone", wantID: "00008120-001A"},
		{name: "ambiguous prefix", query: "00008120", wantErr: "ambiguous"},
		{name: "unknown", query: "nope", wantErr: "matches no connected device"},
		{name: "unknown explains why it cannot be used", query: "some-unseen-id", wantErr: "kind"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := matchDevice(devices, tt.query)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("matchDevice: %v", err)
			}
			if got.ID != tt.wantID {
				t.Errorf("matched %q, want %q", got.ID, tt.wantID)
			}
		})
	}
}

func TestMatchDeviceAmbiguousName(t *testing.T) {
	devices := []mobileDevice{
		{ID: "a1", Label: "Phone", Kind: kindIOSPhysical},
		{ID: "b2", Label: "Phone", Kind: kindIOSPhysical},
	}
	if _, err := matchDevice(devices, "phone"); err == nil ||
		!strings.Contains(err.Error(), "several devices by name") {
		t.Fatalf("err = %v, want ambiguous-name error", err)
	}
}

func TestWantsLocalhostPerKind(t *testing.T) {
	tests := []struct {
		kind string
		want bool
	}{
		{kindAndroidEmulator, true},
		{kindIOSSimulator, true},
		{kindAndroidPhysical, false},
		{kindIOSPhysical, false},
	}
	for _, tt := range tests {
		if got := (mobileDevice{Kind: tt.kind}).wantsLocalhost(); got != tt.want {
			t.Errorf("%s: wantsLocalhost = %v, want %v", tt.kind, got, tt.want)
		}
	}
}

func TestSetLocalEnvOverridesCreatesFile(t *testing.T) {
	dir := t.TempDir()
	path, err := setLocalEnvOverrides(dir, []string{"DEV_HOST", "API_HOST"}, "192.168.1.7")
	if err != nil {
		t.Fatalf("setLocalEnvOverrides: %v", err)
	}
	if path != filepath.Join(dir, wetLocalConfigFileName) {
		t.Errorf("wrote %s, want %s in %s", path, wetLocalConfigFileName, dir)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "env:\n  DEV_HOST: 192.168.1.7\n  API_HOST: 192.168.1.7\n"
	if string(data) != want {
		t.Errorf("file = %q, want %q", string(data), want)
	}
}

func TestSetLocalEnvOverridesPreservesExistingContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, wetLocalConfigFileName)
	existing := "# grown by wet\npools:\n  devices:\n    - emulator:pixel-a\nenv:\n  OTHER: keep\n  DEV_HOST: 10.0.0.2\n"
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := setLocalEnvOverrides(dir, []string{"DEV_HOST"}, "192.168.1.7"); err != nil {
		t.Fatalf("setLocalEnvOverrides: %v", err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(first)
	for _, keep := range []string{"# grown by wet", "emulator:pixel-a", "OTHER: keep", "DEV_HOST: 192.168.1.7"} {
		if !strings.Contains(got, keep) {
			t.Errorf("file lost %q:\n%s", keep, got)
		}
	}
	if strings.Contains(got, "10.0.0.2") {
		t.Errorf("stale host survived:\n%s", got)
	}
	// Idempotent: a second identical run leaves identical bytes.
	if _, err := setLocalEnvOverrides(dir, []string{"DEV_HOST"}, "192.168.1.7"); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(second) != got {
		t.Errorf("not idempotent:\nfirst  %q\nsecond %q", got, string(second))
	}
}

func TestSetLocalEnvOverridesRejectsNonMappingEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, wetLocalConfigFileName)
	if err := os.WriteFile(path, []byte("env: not-a-mapping\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := setLocalEnvOverrides(dir, []string{"DEV_HOST"}, "192.168.1.7"); err == nil ||
		!strings.Contains(err.Error(), "env: is not a mapping") {
		t.Fatalf("err = %v, want the non-mapping refusal", err)
	}
}

func TestRemoveLocalEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, wetLocalConfigFileName)

	// Missing file: a clean no-op, nothing created.
	removed, err := removeLocalEnvOverrides(dir, []string{"DEV_HOST"})
	if err != nil || removed != nil {
		t.Fatalf("missing file: removed=%v err=%v, want nil/nil", removed, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a no-op remove must not create %s", path)
	}

	// Keys absent: the file is left byte-identical.
	before := "pools:\n  devices:\n    - emulator:pixel-a\n"
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	if removed, err = removeLocalEnvOverrides(dir, []string{"DEV_HOST"}); err != nil || removed != nil {
		t.Fatalf("absent keys: removed=%v err=%v, want nil/nil", removed, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != before {
		t.Errorf("file changed on a no-op remove:\nbefore %q\nafter  %q", before, string(after))
	}

	// Removing our key keeps other env keys and other blocks.
	full := "pools:\n  devices:\n    - emulator:pixel-a\nenv:\n  OTHER: keep\n  DEV_HOST: 192.168.1.7\n"
	if err := os.WriteFile(path, []byte(full), 0o644); err != nil {
		t.Fatal(err)
	}
	removed, err = removeLocalEnvOverrides(dir, []string{"DEV_HOST", "NOT_THERE"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(removed, []string{"DEV_HOST"}) {
		t.Errorf("removed = %v, want [DEV_HOST]", removed)
	}
	after, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(after)
	if !strings.Contains(got, "OTHER: keep") || !strings.Contains(got, "emulator:pixel-a") {
		t.Errorf("remove dropped unrelated content:\n%s", got)
	}
	if strings.Contains(got, "DEV_HOST") {
		t.Errorf("DEV_HOST survived removal:\n%s", got)
	}
}

func TestRemoveLocalEnvOverridesDropsEmptyBlocksAndFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, wetLocalConfigFileName)

	// Emptied env: block vanishes; the pools: block stays.
	if err := os.WriteFile(path, []byte("pools:\n  devices:\n    - emulator:pixel-a\nenv:\n  DEV_HOST: 192.168.1.7\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := removeLocalEnvOverrides(dir, []string{"DEV_HOST"}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(after), "env:") {
		t.Errorf("emptied env: block left behind:\n%s", string(after))
	}

	// A file holding only our override is deleted outright.
	if err := os.WriteFile(path, []byte("env:\n  DEV_HOST: 192.168.1.7\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := removeLocalEnvOverrides(dir, []string{"DEV_HOST"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("an emptied %s should be deleted, stat err = %v", wetLocalConfigFileName, err)
	}
}

// TestLocalEnvOverridesRoundTripThroughLoader is the property the whole
// command rests on: what setLocalEnvOverrides writes is exactly what the
// config loader overlays — and hostEnvKeys still finds the keys in the BASE
// config afterwards, so a re-run can switch back.
func TestLocalEnvOverridesRoundTripThroughLoader(t *testing.T) {
	dir := t.TempDir()
	base := "project_name: demo\nenv:\n  DEV_HOST: \"localhost\"\n  AUTH_URL: \"http://{{.DEV_HOST}}:4000\"\n"
	if err := os.WriteFile(filepath.Join(dir, wetConfigFileName), []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := setLocalEnvOverrides(dir, []string{"DEV_HOST"}, "192.168.1.7"); err != nil {
		t.Fatal(err)
	}

	merged, err := LoadWetConfigWithLocal(dir, dir)
	if err != nil {
		t.Fatalf("merged config does not load: %v", err)
	}
	if merged.Env["DEV_HOST"] != "192.168.1.7" {
		t.Errorf("merged DEV_HOST = %q, want the overlay's IP", merged.Env["DEV_HOST"])
	}

	baseCfg, err := LoadWetConfigWithLocal(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if keys := hostEnvKeys(baseCfg.Env); !reflect.DeepEqual(keys, []string{"DEV_HOST"}) {
		t.Errorf("hostEnvKeys on the base config = %v, want [DEV_HOST]", keys)
	}
	if keys := hostEnvKeys(merged.Env); len(keys) != 0 {
		t.Errorf("hostEnvKeys on the merged config = %v — the doc comment promises callers must use the base", keys)
	}
}

func TestChooseLANIP(t *testing.T) {
	ip := func(s string) net.IP { return net.ParseIP(s) }
	tests := []struct {
		name       string
		candidates []ifaceCandidate
		want       string
		wantAlts   int
		wantErr    string
	}{
		{
			name: "en0 wins over other en and ethernet",
			candidates: []ifaceCandidate{
				{Name: "en5", IPs: []net.IP{ip("10.0.0.5")}},
				{Name: "en0", IPs: []net.IP{ip("192.168.1.7")}},
				{Name: "eth0", IPs: []net.IP{ip("10.0.0.9")}},
			},
			want:     "192.168.1.7",
			wantAlts: 2,
		},
		{
			name: "lowest en without en0",
			candidates: []ifaceCandidate{
				{Name: "en5", IPs: []net.IP{ip("10.0.0.5")}},
				{Name: "en1", IPs: []net.IP{ip("10.0.0.1")}},
			},
			want:     "10.0.0.1",
			wantAlts: 1,
		},
		{
			name: "virtual and link-local skipped",
			candidates: []ifaceCandidate{
				{Name: "utun3", IPs: []net.IP{ip("100.64.0.1")}},
				{Name: "awdl0", IPs: []net.IP{ip("10.9.9.9")}},
				{Name: "bridge100", IPs: []net.IP{ip("10.8.8.8")}},
				{Name: "llw0", IPs: []net.IP{ip("10.7.7.7")}},
				{Name: "en0", IPs: []net.IP{ip("169.254.10.10"), ip("fe80::1"), ip("192.168.1.7")}},
			},
			want: "192.168.1.7",
		},
		{
			name: "ipv6 only does not qualify",
			candidates: []ifaceCandidate{
				{Name: "en0", IPs: []net.IP{ip("2001:db8::1")}},
			},
			wantErr: "no LAN IPv4 found",
		},
		{
			name:    "nothing scanned",
			wantErr: "no LAN IPv4 found",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, alts, err := chooseLANIP(tt.candidates)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("chooseLANIP: %v", err)
			}
			if got != tt.want {
				t.Errorf("chose %q, want %q", got, tt.want)
			}
			if len(alts) != tt.wantAlts {
				t.Errorf("alternates = %v, want %d of them", alts, tt.wantAlts)
			}
		})
	}
}

func TestPickDeviceNonTTY(t *testing.T) {
	// Under `go test` stdin/stdout are not terminals, so the picker must
	// refuse with the --device hint and the discovered ids.
	devices := []mobileDevice{{ID: "RFCR921L4DP", Label: "Pixel", Kind: kindAndroidPhysical}}
	_, err := pickDevice(devices)
	if err == nil {
		t.Skip("running on a terminal; the non-TTY guard is unreachable here")
	}
	if !strings.Contains(err.Error(), "--device") || !strings.Contains(err.Error(), "RFCR921L4DP") {
		t.Errorf("err = %v, want the --device hint listing discovered ids", err)
	}
}
