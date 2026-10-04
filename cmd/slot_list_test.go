package cmd

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteSlotListSortsMarksAndReportsUnclaimed(t *testing.T) {
	main := t.TempDir()
	wt2 := t.TempDir()
	unset := t.TempDir()
	gone := filepath.Join(t.TempDir(), "gone")
	state := &WorktreeConfig{Worktrees: []WorktreeState{
		{Branch: "feat/b", Path: wt2, Index: 2, Ports: map[string]int{"DB": 5452, "APP": 8100},
			Pools: map[string]string{"emulator": "Pixel_2"}, PoolExtras: map[string][]string{"emulator": {"Pixel_3"}}},
		{Branch: "main", Path: main, Index: 0, Ports: map[string]int{"APP": 8080}},
		{Branch: "feat/gone", Path: gone, Index: 1, Ports: map[string]int{"APP": 8090}},
		{Branch: "feat/unset", Path: unset},
	}}
	cfg := &WetConfig{Ports: []PortConfig{{Name: "APP", Base: 8080}}, ClusterSlots: 5}

	var out bytes.Buffer
	writeSlotList(&out, state, cfg, main, wt2)
	lines := strings.Split(out.String(), "\n")

	if !strings.HasPrefix(lines[1], "  0 ") || !strings.Contains(lines[1], "main") {
		t.Fatalf("first row should be main at slot 0:\n%s", out.String())
	}
	if !strings.Contains(lines[2], "feat/unset") || !strings.HasPrefix(lines[2], "  - ") {
		t.Fatalf("worktree that never ran setup should show slot -:\n%s", out.String())
	}
	if !strings.Contains(lines[3], "feat/gone") || !strings.Contains(lines[3], "(missing)") {
		t.Fatalf("missing checkout should be flagged:\n%s", out.String())
	}
	if !strings.HasPrefix(lines[4], "* 2 ") ||
		!strings.Contains(lines[4], "DB=5452 APP=8100") ||
		!strings.Contains(lines[4], "emulator=Pixel_2,Pixel_3") {
		t.Fatalf("current worktree row wrong:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Unclaimed: 3, 4") {
		t.Fatalf("want slots 3 and 4 unclaimed:\n%s", out.String())
	}
}

func TestWriteSlotListWithoutPortsSkipsFreeLine(t *testing.T) {
	state := &WorktreeConfig{Worktrees: []WorktreeState{{Branch: "main", Path: t.TempDir()}}}
	var out bytes.Buffer
	writeSlotList(&out, state, nil, "", "")
	if strings.Contains(out.String(), "Unclaimed") {
		t.Fatalf("no config means no band to report:\n%s", out.String())
	}
}

func TestWriteSlotListEmpty(t *testing.T) {
	var out bytes.Buffer
	writeSlotList(&out, &WorktreeConfig{}, nil, "", "")
	if !strings.Contains(out.String(), "No wet worktrees are claimed") {
		t.Fatalf("got %q", out.String())
	}
}
