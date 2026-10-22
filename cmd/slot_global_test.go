package cmd

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func globalTestConfig() *WetConfig {
	return &WetConfig{
		ProjectName:  "app",
		Env:          map[string]string{"COMPOSE_PROJECT_NAME": "app-slot-{{.Slot}}"},
		Ports:        []PortConfig{{Name: "APP", Base: 8080}},
		ClusterSlots: 4,
		Pools:        map[string][]string{"devices": {"emulator:app-1", "emulator:app-2", "phone-serial"}},
	}
}

func TestWriteGlobalSlotListOneSectionPerProject(t *testing.T) {
	root := t.TempDir()
	gone := filepath.Join(t.TempDir(), "gone")
	projects := []globalProject{
		{Root: root, Cfg: globalTestConfig(), State: &WorktreeConfig{Worktrees: []WorktreeState{
			{Branch: "main", Path: root, Index: 0, Ports: map[string]int{"APP": 8080}},
		}}},
		{Root: gone, Gone: true},
	}
	var out bytes.Buffer
	writeGlobalSlotList(&out, projects, nil, "")
	got := out.String()
	for _, want := range []string{"==> " + filepath.Base(root), "main", "Unclaimed: 1, 2, 3", "==> gone", "main checkout is gone"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
}
