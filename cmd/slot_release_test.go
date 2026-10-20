package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// addWorktree makes a real linked worktree of repo on a new branch.
func addWorktree(t *testing.T, repo, branch string) string {
	t.Helper()
	path := filepath.Join(filepath.Dir(repo), "worktree-"+branch)
	out, err := exec.Command("git", "-C", repo, "worktree", "add", "-q", "-b", branch, path).CombinedOutput()
	if err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
	return path
}

func TestClaimIsDeadOnlyForCheckoutsThatAreGone(t *testing.T) {
	repo := initCleanupRepo(t)
	live := addWorktree(t, repo, "live")
	gone := addWorktree(t, repo, "gone")
	if out, err := exec.Command("git", "-C", repo, "worktree", "remove", "--force", gone).CombinedOutput(); err != nil {
		t.Fatalf("git worktree remove: %v\n%s", err, out)
	}
	husk := filepath.Join(filepath.Dir(repo), "worktree-husk")
	if err := os.MkdirAll(filepath.Join(husk, "backend", "_build"), 0o755); err != nil {
		t.Fatal(err)
	}

	cases := map[string]bool{repo: false, live: false, gone: true, husk: true}
	for path, want := range cases {
		if got := claimIsDead(repo, WorktreeState{Path: path}); got != want {
			t.Errorf("claimIsDead(%s) = %v, want %v", filepath.Base(path), got, want)
		}
	}
}

// The leak this exists for: a worktree removed with `git worktree remove` or
// `herdr worktree remove` kept its slot and emulator forever.
func TestSweepDeadClaimsFreesThisAndOtherProjects(t *testing.T) {
	isolateMachine(t)
	own := initCleanupRepo(t)
	other := initCleanupRepo(t)
	ownLive := addWorktree(t, own, "live")
	state := &WorktreeConfig{Worktrees: []WorktreeState{
		{Branch: "main", Path: own},
		{Branch: "live", Path: ownLive, Index: 1},
		{Branch: "gone", Path: filepath.Join(own, "..", "worktree-gone"), Index: 2, Pools: map[string]string{"devices": "emulator:a"}},
	}}
	otherState := &WorktreeConfig{Worktrees: []WorktreeState{
		{Branch: "main", Path: other},
		{Branch: "old", Path: filepath.Join(other, "..", "worktree-old"), Index: 3},
	}}
	if err := saveConfigTo(projectStatePath(other), otherState); err != nil {
		t.Fatal(err)
	}
	vanished := filepath.Join(t.TempDir(), "deleted-clone")
	m := &MachineState{Projects: []MachineProject{{Root: own}, {Root: other}, {Root: vanished}}}

	released := sweepDeadClaims(m, own, state)

	if len(state.Worktrees) != 2 || state.Worktrees[1].Branch != "live" {
		t.Fatalf("own state after sweep = %+v, want main and live", state.Worktrees)
	}
	back, err := loadProjectState(other)
	if err != nil || len(back.Worktrees) != 1 || back.Worktrees[0].Branch != "main" {
		t.Fatalf("other project's state after sweep = %+v, %v", back, err)
	}
	if m.hasProject(vanished) || !m.hasProject(own) || !m.hasProject(other) {
		t.Fatalf("projects after sweep = %+v", m.Projects)
	}
	joined := strings.Join(released, "\n")
	for _, want := range []string{"slot 2 (gone)", "slot 3 (old)", "deleted-clone"} {
		if !strings.Contains(joined, want) {
			t.Errorf("released lines miss %q:\n%s", want, joined)
		}
	}
}

func TestReleaseClaimRefusesALiveCheckoutUnlessForced(t *testing.T) {
	isolateMachine(t)
	repo := initCleanupRepo(t)
	live := addWorktree(t, repo, "live")
	inDir(t, repo)
	if err := saveConfig(&WorktreeConfig{Worktrees: []WorktreeState{
		{Branch: "main", Path: repo},
		{Branch: "live", Path: live, Index: 1},
	}}); err != nil {
		t.Fatal(err)
	}

	if err := releaseClaim(repo, repo, true); err == nil {
		t.Fatal("the main checkout must never be released")
	}
	if err := releaseClaim(repo, live, false); err == nil || !strings.Contains(err.Error(), "wet worktree pop") {
		t.Fatalf("err = %v, want a refusal pointing at pop", err)
	}
	if err := releaseClaim(repo, live, true); err != nil {
		t.Fatal(err)
	}
	state, _ := loadConfig()
	if len(state.Worktrees) != 1 {
		t.Fatalf("state after forced release = %+v", state.Worktrees)
	}
}

// --all frees every slot but main's and leaves the checkouts on disk.
func TestReleaseAllClaimsKeepsMainAndTheCheckouts(t *testing.T) {
	isolateMachine(t)
	repo := initCleanupRepo(t)
	live := addWorktree(t, repo, "live")
	inDir(t, repo)
	gone := filepath.Join(filepath.Dir(repo), "worktree-gone")
	if err := saveConfig(&WorktreeConfig{Worktrees: []WorktreeState{
		{Branch: "live", Path: live, Index: 1, Pools: map[string]string{"devices": "emulator:a"}},
		{Branch: "main", Path: repo},
		{Branch: "gone", Path: gone, Index: 2},
	}}); err != nil {
		t.Fatal(err)
	}

	if err := releaseAllClaims(repo); err != nil {
		t.Fatal(err)
	}
	state, _ := loadConfig()
	if len(state.Worktrees) != 1 || state.Worktrees[0].Branch != "main" {
		t.Fatalf("state after release --all = %+v, want only main", state.Worktrees)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("release --all touched the live checkout: %v", err)
	}
	if err := releaseAllClaims(repo); err != nil {
		t.Fatalf("a second release --all with nothing to free: %v", err)
	}
}

func TestReleaseClaimFreesARemovedCheckout(t *testing.T) {
	isolateMachine(t)
	repo := initCleanupRepo(t)
	inDir(t, repo)
	gone := filepath.Join(filepath.Dir(repo), "worktree-gone")
	if err := saveConfig(&WorktreeConfig{Worktrees: []WorktreeState{
		{Branch: "main", Path: repo},
		{Branch: "gone", Path: gone, Index: 4},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := releaseClaim(repo, gone, false); err != nil {
		t.Fatal(err)
	}
	state, _ := loadConfig()
	if len(state.Worktrees) != 1 || state.Worktrees[0].Branch != "main" {
		t.Fatalf("state after release = %+v", state.Worktrees)
	}
}

// A reused slot holds the previous checkout's data until slot_reset runs, so
// setup runs it on a new claim, once, and not again on a re-run.
func TestSetupResetsANewlyClaimedSlotOnce(t *testing.T) {
	isolateMachine(t)
	repo := initCleanupRepo(t)
	config := `project_name: app
hooks:
  slot_reset: ['echo x >> reset-count']
`
	if err := os.WriteFile(filepath.Join(repo, wetConfigFileName), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	wt := addWorktree(t, repo, "feature")
	if err := os.WriteFile(filepath.Join(wt, wetConfigFileName), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(wt)
	resets := func() int {
		data, _ := os.ReadFile(filepath.Join(wt, "reset-count"))
		return strings.Count(string(data), "x")
	}

	if err := runSetupCore(repo, wt, "feature", false, false); err != nil {
		t.Fatal(err)
	}
	if resets() != 1 {
		t.Fatalf("slot_reset ran %d times on a new claim, want 1", resets())
	}
	state, _ := loadConfig()
	if own := worktreeStateForPath(state, wt); own == nil || own.PendingReset {
		t.Fatalf("entry after setup = %+v, want claimed with no reset pending", own)
	}
	if err := runSetupCore(repo, wt, "feature", false, false); err != nil {
		t.Fatal(err)
	}
	if resets() != 1 {
		t.Fatal("a re-run of setup must keep this checkout's data")
	}
}

func TestSetupNoResetLeavesTheResetPending(t *testing.T) {
	isolateMachine(t)
	repo := initCleanupRepo(t)
	config := "project_name: app\nhooks:\n  slot_reset: ['touch reset-ran']\n"
	if err := os.WriteFile(filepath.Join(repo, wetConfigFileName), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	wt := addWorktree(t, repo, "feature")
	t.Chdir(wt)
	setupNoReset = true
	t.Cleanup(func() { setupNoReset = false })

	if err := runSetupCore(repo, wt, "feature", false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wt, "reset-ran")); !os.IsNotExist(err) {
		t.Fatal("--no-reset must not run slot_reset")
	}
	state, _ := loadConfig()
	if own := worktreeStateForPath(state, wt); own == nil || !own.PendingReset {
		t.Fatalf("entry = %+v, want the reset still pending", own)
	}
	if err := clearPendingReset(wt); err != nil {
		t.Fatal(err)
	}
	state, _ = loadConfig()
	if worktreeStateForPath(state, wt).PendingReset {
		t.Fatal("clearPendingReset must clear the mark")
	}
}
