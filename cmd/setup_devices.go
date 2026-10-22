package cmd

import (
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"
)

// deviceRequest is what `wet setup --devices N --device ITEM...` asked for.
// Count is -1 when --devices was not passed.
type deviceRequest struct {
	Count int
	Items []string
}

func (r deviceRequest) asked() bool { return r.Count >= 0 || len(r.Items) > 0 }

var setupDevices = deviceRequest{Count: -1}

const devicesUsage = "devices to claim: 0 for web only (no emulator), 2 or more for extras; a re-run keeps the last choice"
const deviceUsage = "claim this device, an emulator:<AVD> or a pool item; the first is the primary (repeatable)"

func addDeviceFlags(cmd *cobra.Command) {
	cmd.Flags().IntVar(&setupDevices.Count, "devices", 0, devicesUsage)
	cmd.Flags().StringArrayVar(&setupDevices.Items, "device", nil, deviceUsage)
	// IntVar wrote the help's default into the variable; a setup that never
	// parses flags (a test, another command) must not read it as web only.
	setupDevices.Count = -1
}

// readDeviceFlags marks --devices as not asked when it was not passed: its
// zero means web only, so the flag's own default cannot stand for "unset".
func readDeviceFlags(cmd *cobra.Command) {
	if !cmd.Flags().Changed("devices") {
		setupDevices.Count = -1
	}
}

func (r deviceRequest) validate() error {
	switch {
	case r.Count < -1:
		return fmt.Errorf("--devices must be 0 or more")
	case r.Count == 0 && len(r.Items) > 0:
		return fmt.Errorf("--devices 0 claims no device, so it cannot take --device %s", r.Items[0])
	case r.Count > 0 && r.Count < len(r.Items):
		return fmt.Errorf("--devices %d is fewer than the %d --device items given", r.Count, len(r.Items))
	}
	for _, item := range r.Items {
		if strings.TrimSpace(item) == "" {
			return fmt.Errorf("--device must not be empty")
		}
	}
	return nil
}

// deviceChoice is the device plan for one setup run, worked out from the
// request and what the checkout chose last time.
type deviceChoice struct {
	// Pool is the device pool; empty when the config declares none and
	// nothing was asked.
	Pool string
	// None: claim nothing from Pool (web only).
	None bool
	// Primary is the item --device pinned, kept on a re-run; empty lets the
	// allocator pick as usual.
	Primary string
	// Extras are the further --device items.
	Extras []string
	// Count is the total to hold, primary included; 0 adds nothing.
	Count int
}

// resolveDeviceChoice merges a request with prev, the checkout's state entry
// from its last setup (nil for a new one). Without a request a re-run keeps
// what it chose: web only stays web only, a pinned primary stays pinned.
func resolveDeviceChoice(cfg *WetConfig, prev *WorktreeState, req deviceRequest) (deviceChoice, error) {
	if !req.asked() {
		var c deviceChoice
		if prev == nil {
			return c, nil
		}
		for _, pool := range prev.NoPools {
			if _, declared := cfg.Pools[pool]; declared {
				return deviceChoice{Pool: pool, None: true}, nil
			}
		}
		for _, pool := range prev.PinnedPools {
			if item := prev.Pools[pool]; item != "" {
				return deviceChoice{Pool: pool, Primary: item}, nil
			}
		}
		return c, nil
	}
	pool, err := devicePoolName(cfg, "")
	if err != nil {
		if req.Count == 0 && len(cfg.Pools) == 0 {
			// Nothing to opt out of.
			return deviceChoice{}, nil
		}
		return deviceChoice{}, err
	}
	c := deviceChoice{Pool: pool, Count: req.Count}
	switch {
	case req.Count == 0:
		c.None = true
	case len(req.Items) > 0:
		c.Primary = req.Items[0]
		c.Extras = append([]string{}, req.Items[1:]...)
		if c.Count < len(req.Items) {
			c.Count = len(req.Items)
		}
	case prev != nil && slices.Contains(prev.PinnedPools, pool):
		c.Primary = prev.Pools[pool]
	}
	if c.Count < 0 {
		c.Count = 0
	}
	return c, nil
}

// skip is the allocator's view: the pools it must leave alone.
func (c deviceChoice) skip() map[string]bool {
	if c.Pool == "" || (!c.None && c.Primary == "") {
		return nil
	}
	return map[string]bool{c.Pool: true}
}

// needsAttached reports whether checking the request needs adb's device list:
// only a named item that is not an emulator does.
func (r deviceRequest) needsAttached() bool {
	for _, item := range r.Items {
		if !strings.HasPrefix(item, emulatorItemPrefix) {
			return true
		}
	}
	return false
}

// applyDeviceChoice fills the device pool of an allocation in place: pools
// and extras are what setup is about to record, others every other claim in
// the project. It returns the entry's NoPools and PinnedPools.
func applyDeviceChoice(cfg *WetConfig, c deviceChoice, pools map[string]string, extras map[string][]string,
	others []WorktreeState, treePath string, requested []string, attached map[string]bool,
	localRoot string, target poolTarget) (noPools, pinned []string, err error) {
	if c.Pool == "" {
		return nil, nil, nil
	}
	if c.None {
		delete(pools, c.Pool)
		delete(extras, c.Pool)
		return []string{c.Pool}, nil, nil
	}
	own := &WorktreeState{Path: treePath}
	if c.Primary != "" {
		if slices.Contains(requested, c.Primary) {
			if err := checkClaimable(cfg, c.Pool, c.Primary, others, own, attached); err != nil {
				return nil, nil, err
			}
		}
		pools[c.Pool] = c.Primary
		pinned = []string{c.Pool}
		extras[c.Pool] = slices.DeleteFunc(extras[c.Pool], func(item string) bool { return item == c.Primary })
	}
	for _, item := range c.Extras {
		if item == pools[c.Pool] || slices.Contains(extras[c.Pool], item) {
			continue
		}
		if err := checkClaimable(cfg, c.Pool, item, others, own, attached); err != nil {
			return nil, nil, err
		}
		extras[c.Pool] = append(extras[c.Pool], item)
	}
	held := func() []WorktreeState {
		return append(append([]WorktreeState{}, others...), WorktreeState{Path: treePath, Pools: pools, PoolExtras: extras})
	}
	for total := 1 + len(extras[c.Pool]); total < c.Count; total++ {
		item, err := allocatePoolItem(cfg, c.Pool, held(), localRoot, target)
		if err != nil {
			return nil, nil, err
		}
		extras[c.Pool] = append(extras[c.Pool], item)
	}
	if len(extras[c.Pool]) == 0 {
		delete(extras, c.Pool)
	}
	return nil, pinned, nil
}
