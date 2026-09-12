package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

var mobileSetupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Point this machine's dev URLs at the device you test on",
	Long: `Rewire the host every derived URL dials, for the device you are about
to run on.

An app on an emulator or simulator reaches the host machine as ` + "`localhost`" + `
(via adb reverse, or the simulator sharing the Mac's network). An app on a
real phone cannot: ` + "`localhost`" + ` is the phone, and on iOS there is no
` + "`adb reverse`" + ` to bridge it. The fix is the one .wet.yaml already plans for —
a checked-in ` + "`DEV_HOST: \"localhost\"`" + ` that every URL derives from, overridden
per machine in the gitignored .wet.local.yaml overlay.

This command writes that override. It finds the env: keys declared as
localhost, discovers connected devices (adb, devicectl, simctl), and:

  real phone            records KEY: <this machine's LAN IP> in .wet.local.yaml
  emulator / simulator  removes the override, so the declared localhost wins

On Android it then runs ` + "`adb reverse tcp:P tcp:P`" + ` on the chosen device for
every port this checkout was allocated — the same set ` + "`wet mobile reverse-tcp`" + `
issues. An emulator needs it (that is what makes its localhost this machine);
a phone reached over the LAN IP does not, so there the reverse is a bonus for
anything still dialling localhost and a failure to add it is only reported.

The override lands in the .wet.local.yaml of the checkout you run this in.
Overlays are layered: the trunk checkout's file is the machine layer every
checkout shares (the vault, grown emulators), and a checkout's own file wins
on top of it — so a device is pointed at ONE worktree's stack while every
other checkout keeps its own host. And because the overlay wins over the
committed defaults, every later ` + "`wet setup`" + `, ` + "`wet worktree setup`" + ` and
` + "`wet env pull`" + ` in that checkout keeps generating files for the chosen
device with no further edits. Re-run this when you switch devices — or when
the machine's IP changes.

The phone must be on the same network as this machine — a LAN IP is only
reachable from the LAN.`,
	Args: cobra.NoArgs,
	RunE: runMobileSetup,
}

var (
	mobileSetupDevice string
	mobileSetupHost   string
)

func init() {
	mobileSetupCmd.Flags().StringVar(&mobileSetupDevice, "device", "",
		"device id (or unique prefix/name) to target instead of picking interactively")
	mobileSetupCmd.Flags().StringVar(&mobileSetupHost, "host", "",
		"IP to write instead of auto-detecting the LAN IP (physical devices only)")
	mobileCmd.AddCommand(mobileSetupCmd)
}

// --- device model ---

const (
	kindAndroidPhysical = "android-physical"
	kindAndroidEmulator = "android-emulator"
	kindIOSPhysical     = "ios-physical"
	kindIOSSimulator    = "ios-simulator"
)

// mobileDevice is one connected device a run could target. Kind is what
// decides the host value: an emulator or simulator reaches the machine as
// localhost, a physical device needs the LAN IP.
type mobileDevice struct {
	ID    string
	Label string
	Kind  string
}

func (d mobileDevice) wantsLocalhost() bool {
	return d.Kind == kindAndroidEmulator || d.Kind == kindIOSSimulator
}

// isAndroid reports whether the device is addressable with `adb -s <ID>` —
// which is what decides whether the run can finish the job with `adb reverse`.
// iOS has no equivalent: a simulator shares the Mac's network and a phone has
// only the LAN IP this command records.
func (d mobileDevice) isAndroid() bool {
	return d.Kind == kindAndroidEmulator || d.Kind == kindAndroidPhysical
}

func (d mobileDevice) String() string {
	label := d.Label
	if label == "" {
		label = d.ID
	}
	return fmt.Sprintf("%s (%s) — %s", label, d.ID, d.Kind)
}

// --- host-key detection ---

// hostEnvKeys returns the env: keys whose declared value is a loopback host,
// sorted for stable output.
//
// The contract is deliberately the declared value only: a key reads as a host
// key because the committed .wet.yaml says "localhost" (or "127.0.0.1"), not
// because of whatever the overlay currently holds. That keeps re-runs
// symmetric — the same keys are found whether or not a previous run already
// overrode them, so switching back to an emulator removes exactly what was
// written. Callers must therefore hand this the BASE env:, loaded without
// .wet.local.yaml applied.
func hostEnvKeys(env map[string]string) []string {
	var keys []string
	for k, v := range env {
		switch strings.TrimSpace(v) {
		case "localhost", "127.0.0.1":
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// --- discovery ---

// listIOSDevices and listBootedSimulators are seams like listAttachedSerials:
// tests replace them so the suite never shells out to xcrun.
var listIOSDevices = devicectlDevices

var listBootedSimulators = simctlBootedSimulators

// discoverDevices assembles every connected device the three toolchains can
// see. A toolchain that is absent is skipped with a note — an iOS-only shop
// has no adb, an Android-only one no Xcode — but a present adb that fails is
// an error, for the same reason as in reverseTargets: swallowing it tells the
// user "no devices" while the real message names the broken daemon.
func discoverDevices() ([]mobileDevice, []string, error) {
	var devices []mobileDevice
	var checked []string

	checked = append(checked, "adb")
	if requireADB() != nil {
		fmt.Println("  adb is not installed; skipping Android devices")
	} else {
		serials, err := listAttachedSerials()
		if err != nil {
			return nil, nil, err
		}
		for _, serial := range serials {
			kind := kindAndroidPhysical
			if emulatorSerialRE.MatchString(serial) {
				kind = kindAndroidEmulator
			}
			devices = append(devices, mobileDevice{ID: serial, Label: serial, Kind: kind})
		}
	}

	checked = append(checked, "devicectl")
	ios, err := listIOSDevices()
	if err != nil {
		fmt.Printf("  devicectl is unavailable (%v); skipping iOS devices\n", err)
	} else {
		devices = append(devices, ios...)
	}

	checked = append(checked, "simctl")
	sims, err := listBootedSimulators()
	if err != nil {
		fmt.Printf("  simctl is unavailable (%v); skipping iOS simulators\n", err)
	} else {
		devices = append(devices, sims...)
	}
	return devices, checked, nil
}

// devicectlDevices lists connected physical iOS devices via
// `xcrun devicectl`, which writes its JSON to a file rather than stdout.
func devicectlDevices() ([]mobileDevice, error) {
	if _, err := exec.LookPath("xcrun"); err != nil {
		return nil, errors.New("xcrun not on PATH")
	}
	tmp, err := os.CreateTemp("", "wet-devicectl-*.json")
	if err != nil {
		return nil, err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	if out, err := exec.Command("xcrun", "devicectl", "list", "devices",
		"--json-output", tmp.Name()).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("devicectl list devices: %w: %s", err, strings.TrimSpace(string(out)))
	}
	data, err := os.ReadFile(tmp.Name())
	if err != nil {
		return nil, err
	}
	return parseDevicectlJSON(data)
}

// parseDevicectlJSON extracts connected devices from devicectl's report.
// Fields Apple may omit on older toolchains are tolerated: a device with no
// tunnelState is assumed reachable rather than dropped.
func parseDevicectlJSON(data []byte) ([]mobileDevice, error) {
	var report struct {
		Result struct {
			Devices []struct {
				Identifier       string `json:"identifier"`
				DeviceProperties struct {
					Name string `json:"name"`
				} `json:"deviceProperties"`
				ConnectionProperties struct {
					TunnelState string `json:"tunnelState"`
				} `json:"connectionProperties"`
			} `json:"devices"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		return nil, fmt.Errorf("parse devicectl output: %w", err)
	}
	var devices []mobileDevice
	for _, d := range report.Result.Devices {
		if d.Identifier == "" {
			continue
		}
		if state := strings.ToLower(d.ConnectionProperties.TunnelState); state != "" &&
			state != "connected" {
			continue
		}
		devices = append(devices, mobileDevice{
			ID:    d.Identifier,
			Label: d.DeviceProperties.Name,
			Kind:  kindIOSPhysical,
		})
	}
	return devices, nil
}

// simctlBootedSimulators lists booted iOS simulators via `xcrun simctl`.
func simctlBootedSimulators() ([]mobileDevice, error) {
	if _, err := exec.LookPath("xcrun"); err != nil {
		return nil, errors.New("xcrun not on PATH")
	}
	out, err := exec.Command("xcrun", "simctl", "list", "devices", "booted", "--json").Output()
	if err != nil {
		return nil, fmt.Errorf("simctl list devices: %w", err)
	}
	return parseSimctlJSON(out)
}

// parseSimctlJSON extracts booted simulators. The `booted` filter already
// narrows the list, but State is still checked when present — an unfiltered
// capture pasted into a test must not report shutdown simulators as targets.
func parseSimctlJSON(data []byte) ([]mobileDevice, error) {
	var report struct {
		Devices map[string][]struct {
			UDID  string `json:"udid"`
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		return nil, fmt.Errorf("parse simctl output: %w", err)
	}
	var devices []mobileDevice
	runtimes := make([]string, 0, len(report.Devices))
	for runtime := range report.Devices {
		runtimes = append(runtimes, runtime)
	}
	sort.Strings(runtimes)
	for _, runtime := range runtimes {
		for _, d := range report.Devices[runtime] {
			if d.UDID == "" {
				continue
			}
			if d.State != "" && !strings.EqualFold(d.State, "booted") {
				continue
			}
			devices = append(devices, mobileDevice{ID: d.UDID, Label: d.Name, Kind: kindIOSSimulator})
		}
	}
	return devices, nil
}

// --- selection ---

// matchDevice resolves a --device value against the discovered devices:
// exact ID first, then a unique case-insensitive ID prefix, then a unique
// case-insensitive name match. An unknown value is an error even though it
// looks like an escape hatch — the host decision needs the device's KIND
// (localhost vs LAN IP), and an id wet never saw carries no kind.
func matchDevice(devices []mobileDevice, query string) (mobileDevice, error) {
	for _, d := range devices {
		if d.ID == query {
			return d, nil
		}
	}
	lower := strings.ToLower(query)
	var byPrefix, byName []mobileDevice
	for _, d := range devices {
		if strings.HasPrefix(strings.ToLower(d.ID), lower) {
			byPrefix = append(byPrefix, d)
		}
		if strings.ToLower(d.Label) == lower {
			byName = append(byName, d)
		}
	}
	switch {
	case len(byPrefix) == 1:
		return byPrefix[0], nil
	case len(byPrefix) > 1:
		return mobileDevice{}, fmt.Errorf("--device %q is ambiguous:\n%s", query, deviceLines(byPrefix))
	case len(byName) == 1:
		return byName[0], nil
	case len(byName) > 1:
		return mobileDevice{}, fmt.Errorf("--device %q matches several devices by name:\n%s", query, deviceLines(byName))
	}
	return mobileDevice{}, fmt.Errorf("--device %q matches no connected device — the device's kind "+
		"decides localhost vs LAN IP, so an id wet cannot see cannot be targeted. Connected:\n%s",
		query, deviceLines(devices))
}

func deviceLines(devices []mobileDevice) string {
	lines := make([]string, 0, len(devices))
	for _, d := range devices {
		lines = append(lines, "  "+d.String())
	}
	return strings.Join(lines, "\n")
}

// --- LAN IP detection ---

// ifaceCandidate is one interface's facts, extracted by the thin
// net.Interfaces adapter so the preference logic is a pure function.
type ifaceCandidate struct {
	Name string
	IPs  []net.IP
}

// listLANCandidates is the untested adapter over the real network stack.
var listLANCandidates = netInterfaceCandidates

func netInterfaceCandidates() ([]ifaceCandidate, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []ifaceCandidate
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		c := ifaceCandidate{Name: iface.Name}
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok {
				c.IPs = append(c.IPs, ipnet.IP)
			}
		}
		out = append(out, c)
	}
	return out, nil
}

// chooseLANIP picks the address a phone on the LAN should dial, and reports
// the alternates so a machine with several candidates can be steered with
// --host. Preference: en0 (the Mac's wifi), then the lowest-named en*, then
// anything left. Virtual and link-local interfaces never qualify.
func chooseLANIP(candidates []ifaceCandidate) (chosen string, alternates []string, err error) {
	type pick struct {
		iface string
		ip    string
	}
	var picks []pick
	var scanned []string
	for _, c := range candidates {
		scanned = append(scanned, c.Name)
		if skipLANInterface(c.Name) {
			continue
		}
		for _, ip := range c.IPs {
			v4 := ip.To4()
			if v4 == nil || !v4.IsGlobalUnicast() || v4.IsLinkLocalUnicast() {
				continue
			}
			picks = append(picks, pick{iface: c.Name, ip: v4.String()})
		}
	}
	if len(picks) == 0 {
		return "", nil, fmt.Errorf("no LAN IPv4 found (scanned: %s) — pass --host <ip>",
			strings.Join(scanned, ", "))
	}
	sort.SliceStable(picks, func(i, j int) bool {
		return lanIfaceRank(picks[i].iface) < lanIfaceRank(picks[j].iface)
	})
	for _, p := range picks[1:] {
		alternates = append(alternates, fmt.Sprintf("%s (%s)", p.ip, p.iface))
	}
	return picks[0].ip, alternates, nil
}

func skipLANInterface(name string) bool {
	for _, prefix := range []string{"utun", "bridge", "awdl", "llw"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// lanIfaceRank orders interfaces by how likely they are the LAN-facing one:
// en0 first, then other en* by name, then everything else.
func lanIfaceRank(name string) string {
	switch {
	case name == "en0":
		return "0"
	case strings.HasPrefix(name, "en"):
		return "1" + name
	default:
		return "2" + name
	}
}

// --- .wet.local.yaml overrides ---

// setLocalEnvOverrides records KEY: value under env: in the machine's
// .wet.local.yaml, creating the file if needed — the same overlay pool_grow
// appends emulator items to, and for the same reason: a LAN IP is a fact
// about this machine, so recording it must never dirty the committed
// .wet.yaml. Other keys, blocks and comments are left untouched.
func setLocalEnvOverrides(localDir string, keys []string, value string) (string, error) {
	path := filepath.Join(localDir, wetLocalConfigFileName)
	root, doc, err := readLocalOverlay(path)
	if err != nil {
		return "", err
	}
	env := findOrCreateMapping(root, "env")
	if env.Kind != yaml.MappingNode {
		return "", fmt.Errorf("%s: env: is not a mapping", path)
	}
	for _, k := range keys {
		setMappingValue(env, k, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
	}
	return path, writeLocalOverlay(path, doc)
}

// removeLocalEnvOverrides drops keys from the env: block of .wet.local.yaml
// and reports which were actually present. An emptied env: block is removed
// rather than left behind, and a file emptied entirely is deleted — a machine
// running on all defaults should look like one. A missing file, or one
// without the keys, is a clean no-op.
func removeLocalEnvOverrides(localDir string, keys []string) ([]string, error) {
	path := filepath.Join(localDir, wetLocalConfigFileName)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil
	}
	root, doc, err := readLocalOverlay(path)
	if err != nil {
		return nil, err
	}
	env := findMappingValue(root, "env")
	if env == nil || env.Kind != yaml.MappingNode {
		return nil, nil
	}
	var removed []string
	for _, k := range keys {
		if removeMappingKey(env, k) {
			removed = append(removed, k)
		}
	}
	if len(removed) == 0 {
		return nil, nil
	}
	if len(env.Content) == 0 {
		removeMappingKey(root, "env")
	}
	if len(root.Content) == 0 {
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove %s: %w", path, err)
		}
		return removed, nil
	}
	return removed, writeLocalOverlay(path, doc)
}

// readLocalOverlay parses .wet.local.yaml into a node tree, comments
// included, handing back both the mapping root to edit and the document to
// re-marshal. A missing or empty file starts a fresh document.
func readLocalOverlay(path string) (*yaml.Node, *yaml.Node, error) {
	var doc yaml.Node
	if data, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("unexpected yaml root kind in %s", path)
	}
	return root, &doc, nil
}

// writeLocalOverlay replaces .wet.local.yaml atomically: the content is
// staged beside the file and renamed over it, so a failure partway through
// leaves the old overlay intact rather than a truncated one.
//
// It matters more here than the small size suggests. This file is the only
// record of things nothing else can rederive — the emulators pool_grow
// created, the LAN IP a device was pointed at, the port cluster this clone
// drew — and a half-written one fails every later command in the clone with
// a parse error, in a gitignored file nobody thinks to open.
//
// An existing file keeps its mode; a new one is 0644 like every overlay wet
// has written.
func writeLocalOverlay(path string, doc *yaml.Node) error {
	out, err := MarshalYAMLDocument(doc)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	staged, err := stageOutputFile(path, string(out), mode, 0o755)
	if err != nil {
		return err
	}
	defer os.Remove(staged.tmp)
	if err := os.Rename(staged.tmp, staged.path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

// overlayEnvValue reads one env: value straight out of a .wet.local.yaml,
// without loading a whole config — the machine layer's copy of a key is
// exactly what decides whether removing a checkout's own override is enough
// to restore localhost. A missing or unreadable file answers "not set":
// runMobileSetup has already loaded the merged config, so a genuinely broken
// machine layer failed loudly before this is ever consulted.
func overlayEnvValue(dir, key string) (string, bool) {
	root, _, err := readLocalOverlay(filepath.Join(dir, wetLocalConfigFileName))
	if err != nil {
		return "", false
	}
	env := findMappingValue(root, "env")
	if env == nil || env.Kind != yaml.MappingNode {
		return "", false
	}
	v := findMappingValue(env, key)
	if v == nil {
		return "", false
	}
	return v.Value, true
}

// findMappingValue returns the value node stored at key, or nil.
func findMappingValue(mapping *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

// removeMappingKey deletes key (and its value) from a mapping, reporting
// whether it was there.
func removeMappingKey(mapping *yaml.Node, key string) bool {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
			return true
		}
	}
	return false
}

// --- the command ---

func runMobileSetup(cmd *cobra.Command, args []string) error {
	treePath, err := getRepoRoot()
	if err != nil {
		return err
	}
	repoRoot, err := getMainRepoRoot()
	if err != nil {
		return err
	}
	_, cfgPath, err := LoadWetConfigForWorktree(treePath, repoRoot)
	if err == ErrConfigMissing {
		return fmt.Errorf("`wet mobile setup` requires a .wet.yaml (none at %s or %s)", treePath, repoRoot)
	}
	if err != nil {
		return err
	}

	// Host keys are read from the BASE config, overlay excluded: after one
	// run has recorded a LAN IP, the merged env: says the IP — and detecting
	// on the merged view would find nothing to rewire on the re-run that
	// wants to switch back to an emulator.
	base, err := LoadWetConfigWithLocal(filepath.Dir(cfgPath), "")
	if err != nil {
		return err
	}
	hostKeys := hostEnvKeys(base.Env)
	if len(hostKeys) == 0 {
		return fmt.Errorf("no env: key is declared as localhost — this command rewires keys like\n" +
			"  env:\n" +
			"    DEV_HOST: \"localhost\"\n" +
			"and every URL derived from them. Declare one in .wet.yaml (and derive your\n" +
			"URLs from it, e.g. API_URL: \"http://{{.DEV_HOST}}:{{.APP_PORT}}\"), then re-run")
	}
	local, err := LoadEnvLocalForTree(treePath, repoRoot)
	if err != nil {
		return err
	}
	for _, key := range hostKeys {
		if _, exists := local[key]; exists {
			return fmt.Errorf("%s sets %s and overrides device-specific %s values; remove %s from %s before re-running mobile setup",
				envLocalFileName, key, wetLocalConfigFileName, key, envLocalFileName)
		}
	}

	devices, checked, err := discoverDevices()
	if err != nil {
		return err
	}
	if len(devices) == 0 {
		return fmt.Errorf("no connected device found (checked: %s) — connect one, or pass "+
			"--device=<id> once it is visible", strings.Join(checked, ", "))
	}

	var device mobileDevice
	if mobileSetupDevice != "" {
		device, err = matchDevice(devices, mobileSetupDevice)
		if err != nil {
			return err
		}
	} else {
		device, err = pickDevice(devices)
		if err != nil {
			if errors.Is(err, errPickerCancelled) {
				fmt.Println("Cancelled — nothing written.")
				return nil
			}
			return err
		}
	}

	// The override goes in THIS checkout's own .wet.local.yaml — the layer
	// the loader applies last — so a device is pointed at one worktree's
	// stack while every other checkout keeps its own host.
	localDir := treePath
	machineDir := LocalConfigDir(filepath.Dir(cfgPath), repoRoot)
	sameLayer := resolvePath(machineDir) == resolvePath(localDir)
	path := filepath.Join(localDir, wetLocalConfigFileName)
	fmt.Printf("Target: %s\n", device)
	if device.wantsLocalhost() {
		// Removing this checkout's override only restores localhost when the
		// machine layer underneath doesn't also set the key (an old
		// machine-wide workflow, a hand edit). Such keys are pinned to an
		// explicit localhost here instead — this layer wins.
		var pin, drop []string
		for _, k := range hostKeys {
			if _, shadowed := overlayEnvValue(machineDir, k); shadowed && !sameLayer {
				pin = append(pin, k)
			} else {
				drop = append(drop, k)
			}
		}
		removed, err := removeLocalEnvOverrides(localDir, drop)
		if err != nil {
			return err
		}
		if len(pin) > 0 {
			if _, err := setLocalEnvOverrides(localDir, pin, "localhost"); err != nil {
				return err
			}
			fmt.Printf("  %s: localhost pinned in %s — the machine layer (%s) also sets it, and this layer wins\n",
				strings.Join(pin, ", "), path, filepath.Join(machineDir, wetLocalConfigFileName))
		}
		switch {
		case len(removed) > 0:
			fmt.Printf("  removed %s from %s — the declared localhost wins again\n",
				strings.Join(removed, ", "), path)
		case len(pin) == 0:
			fmt.Printf("  no override recorded in %s — the declared localhost already applies\n", path)
		}
	} else {
		host := mobileSetupHost
		if host == "" {
			candidates, err := listLANCandidates()
			if err != nil {
				return fmt.Errorf("scan network interfaces: %w — pass --host <ip>", err)
			}
			var alternates []string
			host, alternates, err = chooseLANIP(candidates)
			if err != nil {
				return err
			}
			if len(alternates) > 0 {
				fmt.Printf("  using %s; other candidates: %s (override with --host)\n",
					host, strings.Join(alternates, ", "))
			}
		}
		if _, err := setLocalEnvOverrides(localDir, hostKeys, host); err != nil {
			return err
		}
		for _, k := range hostKeys {
			fmt.Printf("  %s: %s recorded in %s\n", k, host, path)
		}
		fmt.Println("  Only this checkout follows the override; other worktrees keep their own")
		fmt.Println("  host. The device must be on the same network as this machine to reach it.")
	}

	// Reload before rendering: cfg was resolved before the overlay changed,
	// and the whole point of the edit is to change what the next render sees.
	cfg, _, err := LoadWetConfigForWorktree(treePath, repoRoot)
	if err != nil {
		return fmt.Errorf("reload config after editing %s: %w", wetLocalConfigFileName, err)
	}

	// Regenerate this checkout's outputs so the change is live immediately —
	// the same pull `wet env pull` runs, nothing more. Other checkouts pick
	// it up on their next setup or pull. A checkout with no allocation yet
	// has nothing to regenerate; the first `wet setup` reads the overlay.
	alloc, err := readWorktreeAllocation(cfg, treePath)
	if err != nil {
		fmt.Printf("  Outputs not regenerated and no ports reversed (%v)\n"+
			"  `wet setup` will pick the override up.\n", err)
		return nil
	}
	data, err := buildWorktreeData(cfg, treePath)
	if err != nil {
		return err
	}
	if cfg.hasAutomaticOutputs() {
		if err := PullOutputs(PullRequest{Cfg: cfg, TreePath: treePath, Data: data}); err != nil {
			return err
		}
	}
	if err := reverseDevicePorts(device, alloc.Ports); err != nil {
		return err
	}
	fmt.Println("  Running services cached their environment at boot: restart the stack (e.g. your")
	fmt.Println("  compose project) so it re-reads the regenerated files — an OIDC issuer especially")
	fmt.Println("  keeps advertising the old host until it does.")
	return nil
}

// reverseDevicePorts finishes the job on Android: `adb reverse tcp:P tcp:P`
// for every port this checkout was allocated, on the device just picked.
// Choosing the device and making it reach the stack are one intent, and
// leaving the second half to a remembered `wet mobile reverse-tcp` is how an
// emulator ends up pointed at a localhost with nothing listening. iOS has no
// adb, so there is nothing to do there.
//
// The failure is fatal only for a device whose URLs say localhost: there the
// reverse IS the bridge, and reporting success without it sends the user
// debugging a stack that never had a listener. A physical phone dials the LAN
// IP this run just recorded, so a reverse it could not add costs it nothing —
// that one is reported and the run still succeeds.
//
// adb itself is not re-checked: an Android device is only in the list because
// discoverDevices asked adb for it.
func reverseDevicePorts(device mobileDevice, ports []ResolvedPort) error {
	if !device.isAndroid() {
		return nil
	}
	if len(ports) == 0 {
		fmt.Println("  No allocated ports to reverse — declare ports: in .wet.yaml and run `wet setup`.")
		return nil
	}
	if device.wantsLocalhost() {
		fmt.Println("  Reversing this worktree's ports, so localhost on the device reaches this stack:")
	} else {
		fmt.Println("  Reversing this worktree's ports too, for anything on the device still dialling")
		fmt.Println("  localhost — the URLs themselves carry the LAN IP:")
	}
	err := reversePorts(device.ID, ports, false)
	if err == nil {
		return nil
	}
	if device.wantsLocalhost() {
		return fmt.Errorf("%w\n  The override is written and the outputs are regenerated; the URLs point at\n"+
			"  localhost, which only reaches this machine once the reverse lands. Fix adb and\n"+
			"  re-run `wet mobile reverse-tcp`", err)
	}
	fmt.Printf("  Reverse incomplete (%v)\n  Harmless here: the URLs carry the LAN IP.\n", err)
	return nil
}

// --- single-select picker ---

type deviceItem struct {
	device mobileDevice
}

func (d deviceItem) Title() string { return d.device.String() }
func (d deviceItem) Description() string {
	if d.device.wantsLocalhost() {
		return "reaches this machine as localhost — override will be removed"
	}
	return "needs this machine's LAN IP"
}
func (d deviceItem) FilterValue() string { return d.device.ID + " " + d.device.Label }

type singleSelectModel struct {
	list      list.Model
	cancelled bool
	done      bool
}

func (m singleSelectModel) Init() tea.Cmd { return nil }

func (m singleSelectModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.list.SetSize(msg.Width, msg.Height-3)
		return m, nil
	case tea.KeyMsg:
		if m.list.FilterState() == list.Filtering {
			break
		}
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			m.cancelled = true
			return m, tea.Quit
		case "enter":
			m.done = true
			return m, tea.Quit
		}
	}
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m singleSelectModel) View() string {
	if m.cancelled || m.done {
		return ""
	}
	help := lipgloss.NewStyle().Faint(true).
		Render("  enter selects · q cancels")
	return m.list.View() + "\n" + help
}

// pickDevice runs a single-select over the discovered devices: enter takes
// the highlighted one. Without a terminal it fails with the discovered ids,
// so a script learns exactly what --device would accept.
func pickDevice(devices []mobileDevice) (mobileDevice, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return mobileDevice{}, fmt.Errorf("no terminal to ask in: pass --device=<id>. Connected:\n%s",
			deviceLines(devices))
	}
	items := make([]list.Item, 0, len(devices))
	for _, d := range devices {
		items = append(items, deviceItem{device: d})
	}
	l := list.New(items, list.NewDefaultDelegate(), 0, 0)
	l.Title = "Which device are you testing on?"
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(true)
	l.Styles.Title = lipgloss.NewStyle().Bold(true).Padding(0, 1)

	final, err := tea.NewProgram(singleSelectModel{list: l}, tea.WithAltScreen()).Run()
	if err != nil {
		return mobileDevice{}, fmt.Errorf("picker: %w", err)
	}
	m := final.(singleSelectModel)
	if m.cancelled {
		return mobileDevice{}, errPickerCancelled
	}
	it, ok := m.list.SelectedItem().(deviceItem)
	if !ok {
		return mobileDevice{}, errPickerCancelled
	}
	return it.device, nil
}
