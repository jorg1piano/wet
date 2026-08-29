package cmd

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func clusterTestConfig() *WetConfig {
	return &WetConfig{
		ProjectName:  "myapp",
		PortStride:   10,
		ClusterSlots: DefaultClusterSlots,
		Ports: []PortConfig{
			{Name: "APP_PORT", Base: 4000, Stride: 10},
			{Name: "DEBUG_PORT", Base: 40000, Stride: 5},
		},
	}
}

// A cluster shifts each port by its OWN stride, not by one flat number, so
// bands stay disjoint whatever mix of strides a config declares. APP_PORT
// (stride 10) moves 100 per cluster; DEBUG_PORT (stride 5) moves 50 — and
// DEBUG_PORT's cluster-0 band tops out at 40045, below cluster 1's 40050.
func TestResolvePortsInShiftsByEachPortsOwnStride(t *testing.T) {
	cfg := clusterTestConfig()
	cases := []struct {
		cluster, slot  int
		app, debugPort int
	}{
		{0, 0, 4000, 40000},
		{0, 9, 4090, 40045},
		{1, 0, 4100, 40050},
		{1, 9, 4190, 40095},
		{2, 0, 4200, 40100},
	}
	for _, c := range cases {
		got := cfg.ResolvePortsIn(c.cluster, c.slot)
		if got[0].Value != c.app || got[1].Value != c.debugPort {
			t.Errorf("ResolvePortsIn(%d, %d) = %d/%d, want %d/%d",
				c.cluster, c.slot, got[0].Value, got[1].Value, c.app, c.debugPort)
		}
	}
}

// ResolvePorts must keep answering exactly as it did before port_cluster
// existed for every config that never sets the key — a whole team's committed
// .wet.yaml files are cluster 0.
func TestResolvePortsUnchangedOnClusterZero(t *testing.T) {
	cfg := clusterTestConfig()
	for slot := 0; slot < 4; slot++ {
		got := cfg.ResolvePorts(slot)
		if want := 4000 + slot*10; got[0].Value != want {
			t.Errorf("slot %d APP_PORT = %d, want %d", slot, got[0].Value, want)
		}
	}
	cfg.PortCluster = 3
	if got := cfg.ResolvePorts(1); got[0].Value != 4310 {
		t.Errorf("cluster 3 slot 1 APP_PORT = %d, want 4310", got[0].Value)
	}
}

// A WetConfig built without the loader has ClusterSlots zero. Reading the
// width through the field rather than the accessor would collapse every
// cluster onto cluster 0's ports — silently, and only outside the loader.
func TestClusterSlotCountDefaultsWithoutLoader(t *testing.T) {
	cfg := &WetConfig{Ports: []PortConfig{{Name: "APP_PORT", Base: 4000, Stride: 10}}}
	if got := cfg.ClusterSlotCount(); got != DefaultClusterSlots {
		t.Fatalf("ClusterSlotCount() = %d, want %d", got, DefaultClusterSlots)
	}
	cfg.PortCluster = 1
	if got := cfg.ResolvePorts(0); got[0].Value != 4100 {
		t.Errorf("cluster 1 slot 0 = %d, want 4100", got[0].Value)
	}
}

// A loopback-only server is the listener the old bind-only probe missed, and
// the one that matters most: a developer running Postgres on 127.0.0.1:5432
// while the config declares `DB_PORT, base: 5432`. Binding the wildcard over
// it succeeds — SO_REUSEADDR permits exactly that — so the port read as free
// and the allocator handed out a band that died at `docker compose up`.
func TestPortFreeSeesALoopbackOnlyServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	// The probe this replaced, kept here as the thing being guarded against:
	// it still binds happily, which is why it cannot be the whole answer.
	if wildcard, err := net.Listen("tcp", fmt.Sprintf(":%d", port)); err == nil {
		wildcard.Close()
	} else {
		t.Skipf("this platform already refuses the wildcard bind over 127.0.0.1:%d", port)
	}

	if portFree(port) {
		t.Errorf("portFree(%d) = true, but a server is listening on 127.0.0.1:%d", port, port)
	}
}

// The other half: nothing listening has to read as free, or every allocation
// walks off looking for a band it will never find.
func TestPortFreeOnAnIdlePort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // release it again; nothing is serving now

	if !portFree(port) {
		t.Errorf("portFree(%d) = false with nothing listening", port)
	}
}

// A cluster containing a loopback-only server must not be handed out.
func TestClusterFreeSeesALoopbackOnlyServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	cfg := &WetConfig{
		ProjectName:  "myapp",
		PortStride:   10,
		ClusterSlots: DefaultClusterSlots,
		Ports:        []PortConfig{{Name: "DB_PORT", Base: port - 20, Stride: 10}},
	}
	if cfg.ClusterFree(0) {
		t.Errorf("ClusterFree(0) = true, but 127.0.0.1:%d (slot 2) is serving", port)
	}
}

// A set that asks for the same port twice can never be satisfied — the two
// worktrees holding them would fight over it — so it must never be handed
// out. Holding every listener until the whole set is acquired is what catches
// this: the second acquisition of the same number fails.
func TestPortSetFreeRejectsASelfCollidingSet(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // the port itself is free; the SET is what's wrong

	if !portSetFree([]int{port}) {
		t.Skipf("port %d is not free on this machine", port)
	}
	if portSetFree([]int{port, port}) {
		t.Error("a set naming the same port twice must not be free")
	}
}

// The listeners have to be held simultaneously, not taken and released one at
// a time — that is the whole point of the set probe. If they were released as
// it went, a set of distinct free ports and a self-colliding set would be
// indistinguishable, which is exactly the bug the case above guards.
func TestPortSetFreeHoldsEveryListenerAtOnce(t *testing.T) {
	var ports []int
	for i := 0; i < 3; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		ports = append(ports, ln.Addr().(*net.TCPAddr).Port)
		ln.Close()
	}
	if !portSetFree(ports) {
		t.Skip("the sampled ports did not stay free")
	}
	// Every listener must be released again once the answer is known, or the
	// caller that acts on "free" finds the ports taken by the probe itself.
	for _, port := range ports {
		ln, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", port))
		if err != nil {
			t.Errorf("port %d still held after the probe returned: %v", port, err)
			continue
		}
		ln.Close()
	}
}

// Two ports that converge on the same number inside a band make that band
// unusable however quiet the host is. Reported as the config bug it is,
// rather than probed, skipped, and eventually blamed on the machine.
func TestFindFreePortClusterNamesASelfCollidingBand(t *testing.T) {
	cfg := &WetConfig{
		ProjectName:  "myapp",
		PortStride:   10,
		ClusterSlots: DefaultClusterSlots,
		Ports: []PortConfig{
			// APP_PORT reaches 4050 at slot 5; DB_PORT starts there.
			{Name: "APP_PORT", Base: 4000, Stride: 10},
			{Name: "DB_PORT", Base: 4050, Stride: 10},
		},
	}
	_, err := findFreePortCluster(cfg)
	if err == nil {
		t.Fatal("expected an error for a band whose own ports collide")
	}
	for _, want := range []string{"APP_PORT", "DB_PORT", "4050"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %q, got: %v", want, err)
		}
	}
}

// The same two ports at a cluster_slots small enough that they never meet is
// a perfectly good config, and must not be rejected.
func TestBandConflictAllowsPortsThatNeverMeet(t *testing.T) {
	cfg := &WetConfig{
		ProjectName:  "myapp",
		PortStride:   10,
		ClusterSlots: 5, // APP_PORT tops out at 4040, below DB_PORT's 4050
		Ports: []PortConfig{
			{Name: "APP_PORT", Base: 4000, Stride: 10},
			{Name: "DB_PORT", Base: 4050, Stride: 10},
		},
	}
	if _, _, _, found := cfg.BandConflict(0); found {
		t.Error("ports that never reach each other inside the band are not a conflict")
	}
}

// A failed overlay write must leave the old file intact rather than a
// truncated one. This file is the only record of the emulators pool_grow
// created and the cluster this clone drew, and half of it fails every later
// command in the clone with a parse error.
func TestWriteLocalOverlayReplacesAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, wetLocalConfigFileName)
	if err := os.WriteFile(path, []byte("port_cluster: 1\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := setLocalPortCluster(dir, 7); err != nil {
		t.Fatalf("setLocalPortCluster: %v", err)
	}

	// The replace preserves the mode it found rather than widening it.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %o, want 600 (the mode the file already had)", got)
	}
	// No staging file is left behind beside it.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected only the overlay to remain, found %v", names)
	}
	if got, _, _ := overlayPortCluster(dir); got != 7 {
		t.Errorf("port_cluster = %d, want 7", got)
	}
}

// bindQuietPort holds a port low enough that a full 64-cluster sweep above it
// still fits under 65535. An OS-assigned port (":0") is drawn from the
// ephemeral range near the top, where the sweep runs out of port space and
// findFreePortCluster reports the ceiling instead of the free band the test
// is about.
func bindQuietPort(t *testing.T) (net.Listener, int) {
	t.Helper()
	for port := 41000; port < 41200; port++ {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err == nil {
			return ln, port
		}
	}
	t.Skip("no free port in 41000-41199 to stand in for a running stack")
	return nil, 0
}

// The band is claimed by a clone, not by a checkout, so a neighbour whose
// main checkout is idle while its fourth worktree runs a stack must still
// make the whole cluster unavailable. Probing only the slot-0 row would hand
// that band out and collide as soon as either clone made one more worktree.
func TestClusterFreeProbesEverySlotInTheBand(t *testing.T) {
	ln, bound := bindQuietPort(t)
	defer ln.Close()

	// Shape the config so cluster 0's slot 3 — not its slot 0 — is the
	// bound port.
	cfg := &WetConfig{
		ProjectName:  "myapp",
		PortStride:   10,
		ClusterSlots: DefaultClusterSlots,
		Ports:        []PortConfig{{Name: "APP_PORT", Base: bound - 30, Stride: 10}},
	}
	if cfg.ResolvePortsIn(0, 3)[0].Value != bound {
		t.Fatalf("precondition: cluster 0 slot 3 is not the bound port")
	}
	if cfg.ClusterFree(0) {
		t.Errorf("ClusterFree(0) = true, want false (slot 3's port %d is bound)", bound)
	}
	got, err := findFreePortCluster(cfg)
	if err != nil {
		t.Fatalf("findFreePortCluster: %v", err)
	}
	if got == 0 {
		t.Errorf("findFreePortCluster returned the occupied cluster 0")
	}
}

// The search must probe every cluster, including one this clone already
// holds. An earlier version took the claimed cluster as a parameter and
// returned it unprobed, which is the rule for a bare run — but it lives in
// runPortClusterNext, where --force can override it. Encoding it in the
// search made --force unable to move off an occupied band at all.
func TestFindFreePortClusterProbesEveryClusterIncludingTheCurrentOne(t *testing.T) {
	ln, bound := bindQuietPort(t)
	defer ln.Close()

	cfg := &WetConfig{
		ProjectName:  "myapp",
		PortStride:   10,
		ClusterSlots: DefaultClusterSlots,
		PortCluster:  0, // this clone's own claim, whose band is the bound one
		Ports:        []PortConfig{{Name: "APP_PORT", Base: bound, Stride: 10}},
	}
	got, err := findFreePortCluster(cfg)
	if err != nil {
		t.Fatalf("findFreePortCluster: %v", err)
	}
	if got == 0 {
		t.Errorf("findFreePortCluster returned the occupied cluster 0; --force could never move")
	}
}

// The search has to end. A config whose every legal cluster is occupied must
// say so rather than climb past 65535 handing out unbindable ports.
func TestFindFreePortClusterStopsAtThePortCeiling(t *testing.T) {
	// Base 65000 with stride 10 and 10 slots per cluster: cluster 0 is the
	// only band that fits, and it is reported occupied via a bound port.
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	bound := ln.Addr().(*net.TCPAddr).Port

	cfg := &WetConfig{
		ProjectName:  "myapp",
		PortStride:   10,
		ClusterSlots: DefaultClusterSlots,
		Ports: []PortConfig{
			{Name: "APP_PORT", Base: bound, Stride: 10},
			{Name: "HIGH_PORT", Base: 65500, Stride: 10},
		},
	}
	if _, err := findFreePortCluster(cfg); err == nil {
		t.Fatal("expected an error when no cluster fits the port range")
	} else if !strings.Contains(err.Error(), "65535") {
		t.Errorf("error should name the ceiling it hit, got: %v", err)
	}
}

// A port_cluster that shifts a base out of the legal range is a config
// mistake, and it has to fail at load — naming the key to lower — rather than
// at `docker compose up`.
func TestValidateRejectsClusterPastThePortCeiling(t *testing.T) {
	cfg := &WetConfig{
		ProjectName:  "myapp",
		PortStride:   10,
		ClusterSlots: DefaultClusterSlots,
		PortCluster:  10,
		Ports:        []PortConfig{{Name: "APP_PORT", Base: 65000, Stride: 10}},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected an error for a cluster past 65535")
	}
	if !strings.Contains(err.Error(), "port_cluster") {
		t.Errorf("error should name port_cluster, got: %v", err)
	}

	// The same config on cluster 0 must still validate: adding the key must
	// not start rejecting configs that were always fine.
	cfg.PortCluster = 0
	if err := cfg.Validate(); err != nil {
		t.Errorf("cluster 0 should still validate, got: %v", err)
	}
}

func TestValidateRejectsNegativeClusterKeys(t *testing.T) {
	cfg := clusterTestConfig()
	cfg.PortCluster = -1
	if err := cfg.Validate(); err == nil {
		t.Error("expected an error for a negative port_cluster")
	}
	cfg.PortCluster = 0
	cfg.ClusterSlots = -1
	if err := cfg.Validate(); err == nil {
		t.Error("expected an error for a negative cluster_slots")
	}
}

// The claim lands in the machine overlay without disturbing what is already
// there — pool_grow's emulator list and the comments around it share the file.
func TestSetLocalPortClusterPreservesTheRestOfTheOverlay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, wetLocalConfigFileName)
	original := "# this machine only\npools:\n  devices:\n    - emulator-5554\n\nenv:\n  API_HOST: 192.168.1.20\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatalf("seed overlay: %v", err)
	}

	if _, ok, err := overlayPortCluster(dir); err != nil || ok {
		t.Fatalf("overlayPortCluster before any claim = (%v, %v), want (false, nil)", ok, err)
	}
	if err := setLocalPortCluster(dir, 2); err != nil {
		t.Fatalf("setLocalPortCluster: %v", err)
	}

	got, ok, err := overlayPortCluster(dir)
	if err != nil || !ok || got != 2 {
		t.Fatalf("overlayPortCluster after claim = (%d, %v, %v), want (2, true, nil)", got, ok, err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	for _, want := range []string{"emulator-5554", "API_HOST", "this machine only", "port_cluster: 2"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("overlay lost %q after the claim:\n%s", want, body)
		}
	}

	// Re-claiming replaces the number rather than appending a second key.
	if err := setLocalPortCluster(dir, 5); err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	body, _ = os.ReadFile(path)
	if n := strings.Count(string(body), "port_cluster:"); n != 1 {
		t.Errorf("port_cluster appears %d times after a re-claim, want 1:\n%s", n, body)
	}
	if got, _, _ := overlayPortCluster(dir); got != 5 {
		t.Errorf("re-claim = %d, want 5", got)
	}
}

// A clone with no overlay at all has claimed nothing; the command must read
// that as "unclaimed" rather than failing on the missing file.
func TestOverlayPortClusterOnAMissingFile(t *testing.T) {
	got, ok, err := overlayPortCluster(t.TempDir())
	if err != nil || ok || got != 0 {
		t.Errorf("overlayPortCluster(missing) = (%d, %v, %v), want (0, false, nil)", got, ok, err)
	}
}

// A recorded `port_cluster: 0` and an absent key both merge to 0, so the
// command reads the overlay directly. They mean opposite things: one clone
// has claimed the bottom band, the other has claimed nothing.
func TestOverlayPortClusterDistinguishesZeroFromAbsent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, wetLocalConfigFileName), []byte("port_cluster: 0\n"), 0o644); err != nil {
		t.Fatalf("seed overlay: %v", err)
	}
	got, ok, err := overlayPortCluster(dir)
	if err != nil || !ok || got != 0 {
		t.Errorf("overlayPortCluster(explicit 0) = (%d, %v, %v), want (0, true, nil)", got, ok, err)
	}
}

func TestOverlayPortClusterRejectsANonNumber(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, wetLocalConfigFileName), []byte("port_cluster: banana\n"), 0o644); err != nil {
		t.Fatalf("seed overlay: %v", err)
	}
	if _, _, err := overlayPortCluster(dir); err == nil {
		t.Error("expected an error for a non-numeric port_cluster")
	}
}

// The overlay is where a cluster is meant to live, so the loader has to carry
// it through the merge and report it as an override like any other key.
func TestLoaderMergesPortClusterFromTheOverlay(t *testing.T) {
	dir := t.TempDir()
	base := "project_name: myapp\nport_stride: 10\nports:\n  - {name: APP_PORT, base: 4000}\n"
	if err := os.WriteFile(filepath.Join(dir, wetConfigFileName), []byte(base), 0o644); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, wetLocalConfigFileName), []byte("port_cluster: 3\n"), 0o644); err != nil {
		t.Fatalf("seed overlay: %v", err)
	}
	cfg, err := LoadWetConfigWithLocal(dir, dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.PortCluster != 3 {
		t.Errorf("PortCluster = %d, want 3", cfg.PortCluster)
	}
	if cfg.ClusterSlots != DefaultClusterSlots {
		t.Errorf("ClusterSlots = %d, want the default %d", cfg.ClusterSlots, DefaultClusterSlots)
	}
	if got := cfg.ResolvePorts(1); got[0].Value != 4310 {
		t.Errorf("slot 1 APP_PORT = %d, want 4310", got[0].Value)
	}
	if !contains(cfg.LocalKeys, "port_cluster") {
		t.Errorf("LocalKeys = %v, want it to report port_cluster as overridden", cfg.LocalKeys)
	}
}

func contains(keys []string, want string) bool {
	for _, k := range keys {
		if k == want {
			return true
		}
	}
	return false
}
