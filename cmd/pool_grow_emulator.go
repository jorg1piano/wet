package cmd

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// growAndroidEmulatorPool creates a new Android emulator, boots it, waits for
// it to come online, and appends it to the pool as "emulator:<AVD_NAME>".
// Returns the new pool item on success.
//
// Pool items name an AVD, never a runtime serial. An `emulator-NNNN` serial is
// assigned by adb when the emulator boots and means nothing while it is
// stopped — recording it would leave the pool full of items that resolve to
// dead devices after a reboot, and no way to start them again. The AVD name is
// the durable identity, so `emulator:<AVD_NAME>` is the one convention: hooks
// test for the `emulator:` prefix to decide whether to boot something, and
// resolve the serial at claim time.
func growAndroidEmulatorPool(cfg *WetConfig, poolName, repoRoot string, headless bool) (string, error) {
	items := cfg.Pools[poolName]

	// Name the AVD for the project so several repos can grow pools on one
	// machine without colliding, then boot it through the same path a
	// claimed emulator item takes — one implementation, so a grown emulator
	// and a configured one behave identically.
	avdName := nextAVDName(cfg.ProjectName, items)
	fmt.Printf("  Growing pool %q with %s...\n", poolName, avdName)
	if _, err := ensureEmulatorRunning(avdName, headless); err != nil {
		return "", err
	}

	// 6. Record the new AVD in the machine-local config.
	item := emulatorItemPrefix + avdName
	grown := append(append([]string{}, cfg.Pools[poolName]...), item)
	if err := appendPoolItem(repoRoot, poolName, grown); err != nil {
		return "", fmt.Errorf("failed to update %s: %w", wetLocalConfigFileName, err)
	}
	// Also update the in-memory config so the caller sees the new item.
	cfg.Pools[poolName] = grown

	return item, nil
}

// emulatorItemPrefix marks a pool item as an AVD that has to be booted before
// use, as opposed to a physical device that is either plugged in or not.
const emulatorItemPrefix = "emulator:"

// nextEmulatorPort returns the lowest free even console port at or above 5554.
//
// It asks adb rather than parsing the pool, because a pool item names an AVD
// and carries no port, and because an emulator started outside wet holds a
// port just as firmly as one wet booted.
func nextEmulatorPort() (int, error) {
	inUse := map[int]bool{}
	out, err := exec.Command("adb", "devices").Output()
	if err != nil {
		return 0, fmt.Errorf("adb devices: %w (is adb on PATH?)", err)
	}
	re := regexp.MustCompile(`^emulator-(\d+)\b`)
	for _, line := range strings.Split(string(out), "\n") {
		if m := re.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			port, _ := strconv.Atoi(m[1])
			inUse[port] = true
		}
	}
	// 5554..5680 is the range adb scans for emulator consoles; past it a
	// booted emulator would be invisible to `adb devices`.
	for port := 5554; port <= 5680; port += 2 {
		if !inUse[port] {
			return port, nil
		}
	}
	return 0, fmt.Errorf("no free emulator console port in 5554..5680 (%d running)", len(inUse))
}

// nextAVDName returns a project-scoped AVD name not already in the pool.
func nextAVDName(projectName string, items []string) string {
	taken := map[string]bool{}
	for _, it := range items {
		taken[strings.TrimPrefix(it, emulatorItemPrefix)] = true
	}
	for n := 1; ; n++ {
		name := fmt.Sprintf("wet-%s-%d", projectName, n)
		if !taken[name] {
			return name
		}
	}
}

// pickAVDTemplate reads the first existing AVD's config.ini to determine
// its system image and device type.
func pickAVDTemplate() (sysImage, device string, err error) {
	// List existing AVDs
	out, err := exec.Command("avdmanager", "list", "avd", "-c").Output()
	if err != nil {
		return "", "", fmt.Errorf("avdmanager list avd: %w", err)
	}
	names := strings.Fields(strings.TrimSpace(string(out)))
	if len(names) == 0 {
		return "", "", fmt.Errorf("no existing AVDs found — create one manually first")
	}

	// Read the first AVD's config.ini
	avdDir := filepath.Join(os.Getenv("HOME"), ".android", "avd", names[0]+".avd")
	configPath := filepath.Join(avdDir, "config.ini")
	f, err := os.Open(configPath)
	if err != nil {
		return "", "", fmt.Errorf("open %s: %w", configPath, err)
	}
	defer f.Close()

	var imgSysDir, tagId, abi, hwDevice string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		k, v, ok := strings.Cut(line, " = ")
		if !ok {
			k, v, ok = strings.Cut(line, "=")
		}
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		switch k {
		case "image.sysdir.1":
			imgSysDir = v
		case "tag.id":
			tagId = v
		case "abi.type":
			abi = v
		case "hw.device.name":
			hwDevice = v
		}
	}

	// Reconstruct the system image path from image.sysdir.1.
	// Format: "system-images/android-36/google_apis_playstore_ps16k/arm64-v8a/"
	if imgSysDir != "" {
		parts := strings.Split(strings.Trim(imgSysDir, "/"), "/")
		if len(parts) >= 4 {
			sysImage = strings.Join(parts[:4], ";")
		}
	}
	if sysImage == "" && tagId != "" && abi != "" {
		// Fallback: construct from tag + abi
		sysImage = fmt.Sprintf("system-images;android-36;%s;%s", tagId, abi)
	}
	if sysImage == "" {
		return "", "", fmt.Errorf("could not determine system image from %s", configPath)
	}
	if hwDevice == "" {
		hwDevice = "pixel"
	}

	return sysImage, hwDevice, nil
}

// waitForDevice polls `adb devices` until the serial appears as "device" (online).
//
// A failed poll is not fatal. This runs across a cold boot, and the emulator
// restarts the adb daemon on its way up — a single `adb devices` exiting
// nonzero in that window is a blip, not a verdict, and aborting on it would
// kill the boot it is waiting for. A machine with no adb at all is a different
// fact, so that one is checked once, up front. Whatever the last poll said is
// carried into the timeout, so a genuinely broken adb still explains itself.
func waitForDevice(serial string, timeout time.Duration) error {
	if err := requireADB(); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		serials, err := attachedSerials()
		// Cleared on success: a blip in the first second must not be the
		// explanation printed after two minutes of clean polls that simply
		// never saw the serial.
		lastErr = err
		for _, s := range serials {
			if s == serial {
				return nil
			}
		}
		time.Sleep(3 * time.Second)
	}
	if lastErr != nil {
		return fmt.Errorf("timed out waiting for %s to come online (%v); last adb error: %w",
			serial, timeout, lastErr)
	}
	return fmt.Errorf("timed out waiting for %s to come online (%v)", serial, timeout)
}

// appendPoolItem adds a new item to a pool in the on-disk .wet.yaml.
// Uses yaml.Node to preserve comments and formatting.
// appendPoolItem records a grown pool item in `.wet.local.yaml`, creating the
// file if needed.
//
// It never touches `.wet.yaml`: an AVD this machine just created exists only
// on this machine, so writing it to the committed config would dirty a tracked
// file and hand teammates a device they don't have. Because a local sequence
// replaces rather than appends, the full merged list is written out — `items`
// is the pool as the caller sees it, new item included.
func appendPoolItem(repoRoot, poolName string, items []string) error {
	path := filepath.Join(repoRoot, wetLocalConfigFileName)
	root, doc, err := readLocalOverlay(path)
	if err != nil {
		return err
	}

	seq := &yaml.Node{Kind: yaml.SequenceNode}
	for _, it := range items {
		seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: it})
	}
	pools := findOrCreateMapping(root, "pools")
	setMappingValue(pools, poolName, seq)
	return writeLocalOverlay(path, doc)
}

// findOrCreateMapping returns the mapping node stored at key, creating an
// empty one if the key is absent.
func findOrCreateMapping(root *yaml.Node, key string) *yaml.Node {
	for i := 0; i < len(root.Content)-1; i += 2 {
		if root.Content[i].Value == key {
			return root.Content[i+1]
		}
	}
	node := &yaml.Node{Kind: yaml.MappingNode}
	root.Content = append(root.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, node)
	return node
}

// setMappingValue replaces (or adds) key's value, leaving other keys and their
// comments untouched.
func setMappingValue(mapping *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i < len(mapping.Content)-1; i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1] = value
			return
		}
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}
