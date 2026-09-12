package cmd

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestSaveConfigKeepsAnOpenReadersSnapshot(t *testing.T) {
	repo := initCleanupRepo(t)
	t.Chdir(repo)
	before := &WorktreeConfig{Worktrees: []WorktreeState{{Branch: "before"}}}
	if err := saveConfig(before); err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(configFilePath())
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err := saveConfig(&WorktreeConfig{Worktrees: []WorktreeState{{Branch: "after"}}}); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot WorktreeConfig
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Worktrees) != 1 || snapshot.Worktrees[0].Branch != "before" {
		t.Fatalf("a concurrent save modified an open reader's snapshot: %+v", snapshot)
	}
	latest, err := loadConfig()
	if err != nil || len(latest.Worktrees) != 1 || latest.Worktrees[0].Branch != "after" {
		t.Fatalf("new reader did not see the complete replacement: %+v, %v", latest, err)
	}
}

// Parallel worktree creation is the whole point of wet, so the read-modify-
// write that hands out slots has to survive two runs starting at once.
func TestWithStateLockSerializesAllocation(t *testing.T) {
	dir := t.TempDir()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(prev) }()

	// configFilePath falls back to a relative path outside a git repo, which
	// puts both the state file and its lock in this temp dir.
	const workers, perWorker = 8, 5
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				err := withStateLock(func() error {
					state, err := loadConfig()
					if err != nil {
						return err
					}
					// The unguarded version of this is the bug: read the
					// highest slot, then write highest+1. Interleave two of
					// them and both write the same number.
					next := 0
					for _, wt := range state.Worktrees {
						if wt.Index > next {
							next = wt.Index
						}
					}
					state.Worktrees = append(state.Worktrees, WorktreeState{Index: next + 1})
					return saveConfig(state)
				})
				if err != nil {
					t.Errorf("withStateLock: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	state, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Worktrees) != workers*perWorker {
		t.Fatalf("wrote %d entries, want %d — a write was lost", len(state.Worktrees), workers*perWorker)
	}
	seen := map[int]bool{}
	for _, wt := range state.Worktrees {
		if seen[wt.Index] {
			t.Fatalf("slot %d handed out twice", wt.Index)
		}
		seen[wt.Index] = true
	}
}

func TestWithStateLockReleasesOnError(t *testing.T) {
	dir := t.TempDir()
	prev, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(prev) }()

	boom := os.ErrInvalid
	if err := withStateLock(func() error { return boom }); err != boom {
		t.Fatalf("got %v, want the callback's error", err)
	}
	// A lock still held here would block until the timeout instead.
	if err := withStateLock(func() error { return nil }); err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, stateLockFileName)); err != nil {
		t.Fatalf("lock file: %v", err)
	}
}
