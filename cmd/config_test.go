package cmd

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSanitizeBranch(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"feature/foo", "feature-foo"},
		{"Feat/BAR", "feat-bar"},
		{"user/name//double", "user-name-double"},
		{"  spaces  ", "spaces"},
		{"--leading-trailing--", "leading-trailing"},
		{"with:colon", "with-colon"},
		{"unicode-æøå", "unicode"},
		{"---", "branch"},
		{"", "branch"},
		{"simple", "simple"},
		{"keep_underscore", "keep_underscore"},
		{"digits123", "digits123"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := SanitizeBranch(c.in)
			if got != c.want {
				t.Errorf("SanitizeBranch(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestNextFreeSlot(t *testing.T) {
	cases := []struct {
		name string
		used []int
		want int
	}{
		{"empty", nil, 1},
		{"slot1 taken", []int{1}, 2},
		{"gap", []int{1, 3}, 2},
		{"packed", []int{1, 2, 3}, 4},
		{"ignores slot 0", []int{0}, 1},
		{"unordered", []int{3, 1, 2}, 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NextFreeSlot(c.used); got != c.want {
				t.Errorf("NextFreeSlot(%v) = %d, want %d", c.used, got, c.want)
			}
		})
	}
}

// TestNextFreeSlotWithFreePorts_SkipsBoundPort reproduces the collision that
// stranded the `participants` worktree: the state file tracked one worktree
// (slot 1), so NextFreeSlot handed slot 2 to the next worktree — but slot 2's
// ports were already held by the main `development` checkout's stack, which
// the state file never tracks. `docker compose up` then died with "port is
// already allocated". The port-probing allocator must skip the occupied slot.
func TestNextFreeSlotWithFreePorts_SkipsBoundPort(t *testing.T) {
	// Hold an OS-assigned port on all interfaces, standing in for a Docker
	// stack the state file doesn't know about.
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	bound := ln.Addr().(*net.TCPAddr).Port

	// Shape the config so slot 1's port lands exactly on the bound port.
	cfg := &WetConfig{
		ProjectName: "myapp",
		PortStride:  10,
		Ports:       []PortConfig{{Name: "APP_PORT", Base: bound - 10, Stride: 10}},
	}

	// Baseline: the state-file-only allocator hands out the colliding slot 1.
	if got := NextFreeSlot(nil); got != 1 {
		t.Fatalf("precondition NextFreeSlot(nil) = %d, want 1", got)
	}
	// Fixed: port probing finds slot 1's port bound and advances to slot 2.
	if got := cfg.NextFreeSlotWithFreePorts(nil); got != 2 {
		t.Errorf("NextFreeSlotWithFreePorts(nil) = %d, want 2 (slot 1 port %d is bound)", got, bound)
	}
}

func TestValidateAcceptsMyappShape(t *testing.T) {
	cfg := &WetConfig{
		ProjectName: "myapp",
		Copy: CopyConfig{
			Files: []string{".env", ".env.dev", ".claude/settings.json"},
			Dirs:  []string{".vscode"},
		},
		PortStride: 10,
		Ports: []PortConfig{
			{Name: "APP_PORT", Base: 8080, Stride: 10},
			{Name: "DB_PORT", Base: 5432, Stride: 10},
			{Name: "DEBUG_PORT", Base: 40000, Stride: 10},
		},
		Env: map[string]string{
			"COMPOSE_PROJECT_NAME": "{{.ProjectName}}-{{.BranchSafe}}",
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateRejectsReservedPortName(t *testing.T) {
	for _, reserved := range []string{"SLOT", "BRANCH", "BRANCHSAFE", "PROJECTNAME"} {
		t.Run(reserved, func(t *testing.T) {
			cfg := &WetConfig{
				ProjectName: "x",
				PortStride:  10,
				Ports:       []PortConfig{{Name: reserved, Base: 8080, Stride: 10}},
			}
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), "reserved") {
				t.Fatalf("expected reserved-field error, got %v", err)
			}
		})
	}
}

func TestValidateRejectsBadProjectName(t *testing.T) {
	for _, name := range []string{"", "Myapp", "with space", "has/slash", "-leading"} {
		t.Run(name, func(t *testing.T) {
			cfg := &WetConfig{ProjectName: name, PortStride: 10}
			if err := cfg.Validate(); err == nil {
				t.Fatalf("expected error for project_name=%q", name)
			}
		})
	}
}

func TestValidateRejectsPathEscape(t *testing.T) {
	cfg := &WetConfig{
		ProjectName: "x",
		PortStride:  10,
		Copy:        CopyConfig{Files: []string{"../escape"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for `..` path")
	}
}

func TestResolvePorts(t *testing.T) {
	cfg := &WetConfig{
		ProjectName: "myapp",
		PortStride:  10,
		Ports: []PortConfig{
			{Name: "APP_PORT", Base: 8080, Stride: 10},
			{Name: "DB_PORT", Base: 5432, Stride: 10},
			{Name: "DEBUG_PORT", Base: 40000, Stride: 10},
		},
	}
	got := cfg.ResolvePorts(2)
	want := []ResolvedPort{
		{"APP_PORT", 8100},
		{"DB_PORT", 5452},
		{"DEBUG_PORT", 40020},
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestLoadWetConfigRoundTrip(t *testing.T) {
	// Full happy path: write a realistic .wet.yaml to a temp repo root, load
	// it, and verify every field parsed. Catches yaml tag typos that the
	// struct-only Validate tests miss.
	const yaml = `
project_name: myapp
copy:
  files:
    - .env
    - .env.dev
    - .claude/settings.json
  dirs:
    - .vscode
port_stride: 10
ports:
  - { name: APP_PORT,   base: 8080 }
  - { name: DB_PORT,    base: 5432 }
  - { name: DEBUG_PORT, base: 40000, stride: 5 }
env:
  COMPOSE_PROJECT_NAME: "{{.ProjectName}}-{{.BranchSafe}}"
  LITERAL: "hello"
hooks:
  post_create:
    - "echo creating {{.Branch}}"
  pre_remove:
    - "echo removing {{.Branch}}"
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".wet.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWetConfig(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.ProjectName != "myapp" {
		t.Errorf("ProjectName = %q", cfg.ProjectName)
	}
	if len(cfg.Copy.Files) != 3 || cfg.Copy.Files[0] != ".env" {
		t.Errorf("Copy.Files = %v", cfg.Copy.Files)
	}
	if len(cfg.Copy.Dirs) != 1 || cfg.Copy.Dirs[0] != ".vscode" {
		t.Errorf("Copy.Dirs = %v", cfg.Copy.Dirs)
	}
	if cfg.PortStride != 10 {
		t.Errorf("PortStride = %d", cfg.PortStride)
	}
	if len(cfg.Ports) != 3 {
		t.Fatalf("len(Ports) = %d", len(cfg.Ports))
	}
	// Port 0: inherited stride from top-level.
	if cfg.Ports[0].Name != "APP_PORT" || cfg.Ports[0].Base != 8080 || cfg.Ports[0].Stride != 10 {
		t.Errorf("Ports[0] = %+v", cfg.Ports[0])
	}
	// Port 2: explicit per-port stride override.
	if cfg.Ports[2].Name != "DEBUG_PORT" || cfg.Ports[2].Stride != 5 {
		t.Errorf("Ports[2] = %+v (expected per-port stride override)", cfg.Ports[2])
	}
	if cfg.Env["COMPOSE_PROJECT_NAME"] != "{{.ProjectName}}-{{.BranchSafe}}" {
		t.Errorf("Env[COMPOSE_PROJECT_NAME] = %q", cfg.Env["COMPOSE_PROJECT_NAME"])
	}
	if cfg.Env["LITERAL"] != "hello" {
		t.Errorf("Env[LITERAL] = %q", cfg.Env["LITERAL"])
	}
	if len(cfg.Hooks.PostCreate) != 1 || cfg.Hooks.PostCreate[0] != "echo creating {{.Branch}}" {
		t.Errorf("Hooks.PostCreate = %v", cfg.Hooks.PostCreate)
	}
	if len(cfg.Hooks.PreRemove) != 1 {
		t.Errorf("Hooks.PreRemove = %v", cfg.Hooks.PreRemove)
	}
}

func TestLoadWetConfigMissingReturnsSentinel(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadWetConfig(dir)
	if err != ErrConfigMissing {
		t.Fatalf("expected ErrConfigMissing, got %v", err)
	}
}

func TestLoadWetConfigRejectsInvalidTemplate(t *testing.T) {
	const yaml = `
project_name: x
port_stride: 10
env:
  BAD: "{{ .Unclosed"
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".wet.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWetConfig(dir); err == nil {
		t.Fatal("expected parse error for malformed template")
	}
}

func TestBuildHookEnv(t *testing.T) {
	cfg := &WetConfig{
		ProjectName: "myapp",
		PortStride:  10,
		Ports: []PortConfig{
			{Name: "APP_PORT", Base: 8080, Stride: 10},
			{Name: "DB_PORT", Base: 5432, Stride: 10},
		},
		Env: map[string]string{
			"COMPOSE_PROJECT_NAME": "{{.ProjectName}}-{{.BranchSafe}}",
		},
	}
	ports := cfg.ResolvePorts(1)
	data := BuildTemplateData(cfg, "feature/foo", 1, ports, nil)
	env, err := BuildHookEnv(cfg, data, ports, nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	// Collapse into a set for easy lookup, and remember we expect the
	// caller's os.Environ to be in there too — so don't assert exact length.
	seen := make(map[string]bool, len(env))
	for _, kv := range env {
		seen[kv] = true
	}
	for _, want := range []string{
		"PROJECT_NAME=myapp",
		"BRANCH=feature/foo",
		"BRANCH_SAFE=feature-foo",
		"SLOT=1",
		"APP_PORT=8090",
		"DB_PORT=5442",
		"COMPOSE_PROJECT_NAME=myapp-feature-foo",
	} {
		if !seen[want] {
			t.Errorf("env missing %q", want)
		}
	}
}

func TestRenderEnvFileContent(t *testing.T) {
	cfg := &WetConfig{
		ProjectName: "myapp",
		PortStride:  10,
		Ports: []PortConfig{
			{Name: "APP_PORT", Base: 8080, Stride: 10},
		},
		Env: map[string]string{
			"COMPOSE_PROJECT_NAME": "{{.ProjectName}}-{{.BranchSafe}}",
			"LITERAL":              "hello",
		},
	}
	ports := cfg.ResolvePorts(1)
	data := BuildTemplateData(cfg, "feature/foo", 1, ports, nil)
	out, err := RenderEnvFileContent(cfg, data, ports, nil)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	for _, want := range []string{
		"APP_PORT=8090",
		"COMPOSE_PROJECT_NAME=myapp-feature-foo",
		"LITERAL=hello",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
}

func TestMaskedFailureHooks(t *testing.T) {
	cases := []struct {
		name string
		hook string
		want bool
	}{
		{"plain command propagates", "just populate-env-dev", false},
		{"and-chain propagates", "cd web_elixir && just ensure-env-dev", false},
		{"or-fallback masks", "just populate-env-dev || just seed-env-dev x", true},
		{"or-fallback in braces masks", "{ a || b; }", true},
		{"trailing true masks", "adb emu kill; true", true},
		{"trailing true after semicolons masks", "esac; true;", true},
		{"pipe masks", "cat foo | grep bar", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &WetConfig{Hooks: HooksConfig{PostCreate: []string{c.hook}}}
			got := MaskedFailureHooks(cfg)
			if (len(got) > 0) != c.want {
				t.Fatalf("MaskedFailureHooks(%q) = %v, want flagged=%v", c.hook, got, c.want)
			}
		})
	}

	t.Run("pre_remove is exempt", func(t *testing.T) {
		cfg := &WetConfig{Hooks: HooksConfig{PreRemove: []string{"adb emu kill || true"}}}
		if got := MaskedFailureHooks(cfg); len(got) != 0 {
			t.Fatalf("pre_remove should not be flagged, got %v", got)
		}
	})
}

func TestLoadWetConfigForWorktreePrefersWorktree(t *testing.T) {
	write := func(dir, name string) {
		body := "project_name: " + name + "\nports:\n  - { name: APP_PORT, base: 8000 }\n"
		if err := os.WriteFile(filepath.Join(dir, ".wet.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	repoRoot := t.TempDir()
	worktree := t.TempDir()
	write(repoRoot, "from-main")

	// No config in the worktree — fall back to the main repo.
	cfg, path, err := LoadWetConfigForWorktree(worktree, repoRoot)
	if err != nil {
		t.Fatalf("fallback load: %v", err)
	}
	if cfg.ProjectName != "from-main" {
		t.Fatalf("project_name = %q, want from-main", cfg.ProjectName)
	}
	if filepath.Dir(path) != repoRoot {
		t.Fatalf("path = %q, want under %q", path, repoRoot)
	}

	// Worktree has its own config — it wins, so a change can be tested where
	// it's written instead of only after it merges into the main checkout.
	write(worktree, "from-worktree")
	cfg, path, err = LoadWetConfigForWorktree(worktree, repoRoot)
	if err != nil {
		t.Fatalf("worktree load: %v", err)
	}
	if cfg.ProjectName != "from-worktree" {
		t.Fatalf("project_name = %q, want from-worktree", cfg.ProjectName)
	}
	if filepath.Dir(path) != worktree {
		t.Fatalf("path = %q, want under %q", path, worktree)
	}
}

func TestLoadWetConfigForWorktreeSurfacesInvalidWorktreeConfig(t *testing.T) {
	repoRoot := t.TempDir()
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".wet.yaml"),
		[]byte("project_name: fine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A broken worktree config must fail loudly, not silently fall back to
	// the main repo's — that would hide the very edit being tested.
	if err := os.WriteFile(filepath.Join(worktree, ".wet.yaml"),
		[]byte("project_name: \"Bad Name!\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadWetConfigForWorktree(worktree, repoRoot); err == nil {
		t.Fatal("expected invalid worktree config to error, got nil")
	}
}

// The example config is the documentation of record for the schema. If it
// stops parsing or validating, the docs are lying to anyone who copies it.
func TestExampleConfigIsValid(t *testing.T) {
	body, err := os.ReadFile("wet.example.yaml")
	if err != nil {
		t.Fatalf("read wet.example.yaml: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, wetConfigFileName), body, 0644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	cfg, err := LoadWetConfig(dir)
	if err != nil {
		t.Fatalf("example config is invalid: %v", err)
	}
	if cfg.ProjectName == "" || len(cfg.Ports) == 0 || len(cfg.Pools) == 0 {
		t.Errorf("example config lost its illustrative content: %+v", cfg)
	}
	// Every env: value must render against the data a real worktree supplies,
	// or the example teaches a template that fails on first use.
	ports := cfg.ResolvePorts(1)
	pools := map[string]string{}
	for name, items := range cfg.Pools {
		pools[name] = items[0]
	}
	data := BuildTemplateData(cfg, "feature/example", 1, ports, pools)
	if _, err := RenderEnvFileContent(cfg, data, ports, nil); err != nil {
		t.Fatalf("example env: block does not render: %v", err)
	}
	for i, h := range append(append([]string{}, cfg.Hooks.PostCreate...), cfg.Hooks.PreRemove...) {
		if _, err := RenderHookCommand(fmt.Sprintf("example hook %d", i), h, data); err != nil {
			t.Fatalf("example hook %q does not render: %v", h, err)
		}
	}
}

// The example config is now a complete worked example on its own: there is no
// second half to keep in sync, because the outputs it declares carry their own
// key list. What still has to hold is that the example generates real files
// whose per-slot values actually differ — an example whose output is identical
// in every worktree teaches the one thing wet exists to prevent.
func TestExampleConfigGeneratesPerSlotOutputs(t *testing.T) {
	dir := t.TempDir()
	cfgBody, err := os.ReadFile("wet.example.yaml")
	if err != nil {
		t.Fatalf("read example config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, wetConfigFileName), cfgBody, 0644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	cfg, err := LoadWetConfig(dir)
	if err != nil {
		t.Fatalf("load example config: %v", err)
	}
	if len(cfg.Outputs) == 0 {
		t.Fatal("example config declares no outputs:; the worked example lost its point")
	}

	pullAt := func(slot int) string {
		tree := t.TempDir()
		ports := cfg.ResolvePorts(slot)
		pools := map[string]string{}
		for name, items := range cfg.Pools {
			pools[name] = items[0]
		}
		data := BuildTemplateData(cfg, "feature/example", slot, ports, pools)
		if _, err := ResolveEnvValues(cfg, data, nil); err != nil {
			t.Fatalf("slot %d: env: block does not resolve: %v", slot, err)
		}
		var log strings.Builder
		if err := PullOutputs(PullRequest{
			Cfg: cfg, TreePath: tree, Data: data,
			Policy: SecretPolicyDisabled, Out: &log,
		}); err != nil {
			t.Fatalf("slot %d: example outputs do not generate: %v", slot, err)
		}
		var all strings.Builder
		for _, o := range cfg.Outputs {
			body, err := os.ReadFile(filepath.Join(tree, o.Out))
			if err != nil {
				t.Fatalf("slot %d: %s was not written: %v", slot, o.Out, err)
			}
			all.Write(body)
		}
		return all.String()
	}

	main, worktree := pullAt(0), pullAt(1)
	if main == worktree {
		t.Error("slot 0 and slot 1 generated identical files — no output carries a per-slot value")
	}
	if !strings.Contains(main, "localhost:4000") {
		t.Errorf("slot 0 should carry the declared base port, got:\n%s", main)
	}
	if !strings.Contains(worktree, "localhost:4010") {
		t.Errorf("slot 1 should carry base+stride, got:\n%s", worktree)
	}
}

func TestLocalConfigOverlay(t *testing.T) {
	base := `
project_name: myapp
port_stride: 10
ports:
  - { name: PHX_PORT, base: 4000 }
env:
  DEV_HOST: "localhost"
  COMPOSE_PROJECT_NAME: "{{.ProjectName}}-{{.BranchSafe}}"
pools:
  devices: [shared-emulator]
`
	write := func(t *testing.T, local string) *WetConfig {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, wetConfigFileName), []byte(base), 0644); err != nil {
			t.Fatal(err)
		}
		if local != "" {
			if err := os.WriteFile(filepath.Join(dir, wetLocalConfigFileName), []byte(local), 0644); err != nil {
				t.Fatal(err)
			}
		}
		cfg, err := LoadWetConfig(dir)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		return cfg
	}

	t.Run("absent file changes nothing", func(t *testing.T) {
		cfg := write(t, "")
		if len(cfg.LocalKeys) != 0 || cfg.Pools["devices"][0] != "shared-emulator" {
			t.Errorf("unexpected: %+v", cfg.Pools)
		}
	})

	t.Run("pools replace wholesale", func(t *testing.T) {
		cfg := write(t, "pools:\n  devices: [RFCR921L4DP, 'emulator:wet-myapp-1']\n")
		got := cfg.Pools["devices"]
		if len(got) != 2 || got[0] != "RFCR921L4DP" {
			t.Errorf("local pool should replace, not append: %v", got)
		}
	})

	t.Run("env merges key by key", func(t *testing.T) {
		cfg := write(t, "env:\n  DEV_HOST: \"192.168.1.9\"\n")
		if cfg.Env["DEV_HOST"] != "192.168.1.9" {
			t.Errorf("override lost: %q", cfg.Env["DEV_HOST"])
		}
		if cfg.Env["COMPOSE_PROJECT_NAME"] == "" {
			t.Error("unmentioned env key was dropped by the merge")
		}
	})

	// Anything is overridable, because the merge happens before the config is
	// unmarshalled — no call site knows the local file exists.
	t.Run("stride and ports are overridable", func(t *testing.T) {
		cfg := write(t, "port_stride: 100\nports:\n  - { name: PHX_PORT, base: 5000 }\n")
		if got := cfg.ResolvePorts(2)[0].Value; got != 5200 {
			t.Errorf("slot 2 PHX_PORT = %d, want 5200 (base 5000 + 2*100)", got)
		}
	})

	t.Run("reports which keys it overrode", func(t *testing.T) {
		cfg := write(t, "port_stride: 20\npools:\n  devices: [x]\n")
		if len(cfg.LocalKeys) != 2 || cfg.LocalKeys[0] != "pools" || cfg.LocalKeys[1] != "port_stride" {
			t.Errorf("LocalKeys = %v, want sorted [pools port_stride]", cfg.LocalKeys)
		}
	})

	t.Run("a local file that breaks the schema fails by name", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, wetConfigFileName), []byte(base), 0644)
		os.WriteFile(filepath.Join(dir, wetLocalConfigFileName), []byte("ports:\n  - { name: lowercase, base: 1 }\n"), 0644)
		_, err := LoadWetConfig(dir)
		if err == nil || !strings.Contains(err.Error(), wetLocalConfigFileName) {
			t.Fatalf("error should name the local file, got: %v", err)
		}
	})
}

func TestNextAVDNameSkipsTaken(t *testing.T) {
	items := []string{"RFCR921L4DP", "emulator:wet-myapp-1", "emulator:wet-myapp-2"}
	if got := nextAVDName("myapp", items); got != "wet-myapp-3" {
		t.Errorf("nextAVDName = %q, want wet-myapp-3", got)
	}
	if got := nextAVDName("myapp", nil); got != "wet-myapp-1" {
		t.Errorf("nextAVDName(empty) = %q, want wet-myapp-1", got)
	}
}

// Grown emulators are machine-local, so they must never land in the committed
// config — and the local file has to carry the whole list, since sequences
// replace rather than append.
func TestAppendPoolItemWritesLocalFileOnly(t *testing.T) {
	dir := t.TempDir()
	committed := "project_name: myapp\npools:\n  devices: [shared]\n"
	if err := os.WriteFile(filepath.Join(dir, wetConfigFileName), []byte(committed), 0644); err != nil {
		t.Fatal(err)
	}
	if err := appendPoolItem(dir, "devices", []string{"shared", "emulator:wet-myapp-1"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, wetConfigFileName))
	if string(after) != committed {
		t.Errorf(".wet.yaml was modified:\n%s", after)
	}
	cfg, err := LoadWetConfig(dir)
	if err != nil {
		t.Fatalf("load after grow: %v", err)
	}
	if len(cfg.Pools["devices"]) != 2 || cfg.Pools["devices"][1] != "emulator:wet-myapp-1" {
		t.Errorf("merged pool = %v", cfg.Pools["devices"])
	}

	// Growing again keeps the list whole rather than clobbering it.
	if err := appendPoolItem(dir, "devices", append(cfg.Pools["devices"], "emulator:wet-myapp-2")); err != nil {
		t.Fatalf("second append: %v", err)
	}
	cfg, _ = LoadWetConfig(dir)
	if len(cfg.Pools["devices"]) != 3 {
		t.Errorf("second grow lost items: %v", cfg.Pools["devices"])
	}
}

func TestMainBranchName(t *testing.T) {
	if got := (&WetConfig{}).MainBranchName(); got != "main" {
		t.Errorf("empty config = %q, want main", got)
	}
	if got := (&WetConfig{MainBranch: "development"}).MainBranchName(); got != "development" {
		t.Errorf("configured = %q, want development", got)
	}
	var nilCfg *WetConfig
	if got := nilCfg.MainBranchName(); got != "main" {
		t.Errorf("nil config = %q, want main", got)
	}
}

func TestMainBranchIsOverridableLocally(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".wet.yaml", "project_name: demo\nmain_branch: development\n")
	cfg, err := LoadWetConfigWithLocal(dir, dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.MainBranchName(); got != "development" {
		t.Fatalf("from .wet.yaml = %q, want development", got)
	}

	write(".wet.local.yaml", "main_branch: my-fork\n")
	cfg, err = LoadWetConfigWithLocal(dir, dir)
	if err != nil {
		t.Fatalf("load with local: %v", err)
	}
	if got := cfg.MainBranchName(); got != "my-fork" {
		t.Fatalf("with .wet.local.yaml = %q, want my-fork", got)
	}
}

// The machine-local overlay must be read from the checkout holding
// `main_branch:`, not from whatever git calls the main working tree. A first
// clone parked on an unrelated branch is still git's "main", and reading the
// overlay from there silently drops every override.
func TestLocalConfigComesFromTheTrunkCheckout(t *testing.T) {
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	base := t.TempDir()
	stale := filepath.Join(base, "stale") // git's main working tree, on "main"
	trunk := filepath.Join(base, "trunk") // the checkout on "development"
	wt := filepath.Join(base, "feature")  // a worktree, like any other

	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	git(stale, "init", "-q", "-b", "main")
	git(stale, "config", "user.email", "t@t")
	git(stale, "config", "user.name", "t")
	cfg := "project_name: demo\nmain_branch: development\npools:\n  devices: [committed-device]\n"
	if err := os.WriteFile(filepath.Join(stale, ".wet.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	git(stale, "add", ".wet.yaml")
	git(stale, "commit", "-qm", "cfg")
	git(stale, "branch", "development")
	git(stale, "worktree", "add", "-q", trunk, "development")
	git(stale, "worktree", "add", "-q", "-b", "feature", wt, "development")

	// The overlay exists ONLY beside the trunk checkout, which is how a
	// gitignored per-machine file actually sits on disk.
	local := "pools:\n  devices: [my-phone]\n"
	if err := os.WriteFile(filepath.Join(trunk, ".wet.local.yaml"), []byte(local), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := LoadWetConfigForWorktreeTestable(wt, stale)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got.Pools["devices"]) != 1 || got.Pools["devices"][0] != "my-phone" {
		t.Fatalf("pools = %v, want [my-phone] from the trunk checkout's overlay", got.Pools["devices"])
	}
}

// TestExampleConfigCoversSchema is the guard that keeps cmd/wet.example.yaml
// honest: the file calls itself "the full `.wet.yaml` schema", and the only
// way that claim survives a new field being added to WetConfig is if a test
// reflects over the struct and insists every yaml key is documented.
// (`main_branch:` had been a real key for a while before anyone noticed the
// example never mentioned it.)
//
// The check is path-aware rather than a `strings.Contains(body, key+":")`,
// because leaf names collide: `base:` is both a PortConfig field and a
// PRConfig field, so a loose check would let `pr.base` alone vouch for
// `ports[].base`. Live keys are therefore matched against paths walked out of
// the parsed YAML tree.
//
// A key that is only illustrated in a commented-out block can't appear in
// the parsed tree at all, so those get an explicit escape hatch:
// commentedSchemaKeys, below. It is deliberately an allowlist and not a
// fallback. The previous version accepted ANY schema key that appeared after
// a `#` anywhere in the file, which meant a live key could rot into a comment
// and still pass — commenting out `port_stride: 10` was undetectable. It was
// also path-blind (it matched on the leaf name alone), so the commented
// `outputs:` block vouched for an `out:` or `format:` leaf on any struct.
// Now the escape hatch is a reviewable list of paths, each pinned to
// the shape it is allowed to be documented as.
func TestExampleConfigCoversSchema(t *testing.T) {
	body, err := os.ReadFile("wet.example.yaml")
	if err != nil {
		t.Fatalf("read wet.example.yaml: %v", err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(body, &root); err != nil {
		t.Fatalf("parse wet.example.yaml: %v", err)
	}
	live := map[string]bool{}
	collectYAMLPaths(&root, "", live)

	for _, path := range schemaYAMLPaths(t, reflect.TypeOf(WetConfig{}), "") {
		if livePath(live, path) {
			if _, exempt := commentedSchemaKeys[path]; exempt {
				t.Errorf("schema key %q is now a live key in cmd/wet.example.yaml; "+
					"drop it from commentedSchemaKeys so the strict check applies", path)
			}
			continue
		}
		re, exempt := commentedSchemaKeys[path]
		if !exempt {
			t.Errorf("schema key %q is not documented in cmd/wet.example.yaml "+
				"(add it to the config, or — only if it is meant to be shown "+
				"commented out — add it to commentedSchemaKeys)", path)
			continue
		}
		if !re.Match(body) {
			t.Errorf("schema key %q is exempt as documented-by-example, but no commented "+
				"line matching %s is left in cmd/wet.example.yaml", path, re)
		}
	}
}

// livePath reports whether a schema path is present in the parsed document,
// treating a "*" segment as "any one user-chosen key" — that is how a map of
// structs like secrets.definitions is expressed.
func livePath(live map[string]bool, path string) bool {
	if !strings.Contains(path, "*") {
		return live[path]
	}
	want := strings.Split(path, ".")
	for got := range live {
		parts := strings.Split(got, ".")
		if len(parts) != len(want) {
			continue
		}
		match := true
		for i := range want {
			if want[i] != "*" && want[i] != parts[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// commentedSchemaKeys are the schema paths cmd/wet.example.yaml documents by
// showing them commented out rather than live, mapped to the exact commented
// line each must still have. Both halves matter: without the map the key
// would look undocumented, and without the regex the exemption would be a
// blanket pass that survives the illustration being deleted.
//
// Adding an entry here is how you turn the strict check off for one path, so
// it should stay short and each addition should be argued for in review.
var commentedSchemaKeys = map[string]*regexp.Regexp{
	// Optional per-pool auto-grow strategy; shown commented so the example
	// config doesn't create emulators on anyone's machine.
	"pool_grow": regexp.MustCompile(`(?m)^# pool_grow:$`),
	// A PR body override; the default body is what most repos want.
	"pr.body_template": regexp.MustCompile(`(?m)^\s+# body_template: \|$`),
	// A non-default file mode is the exception, so it is illustrated rather
	// than applied to the example's own outputs.
	"outputs[].mode": regexp.MustCompile(`(?m)^\s*#\s+mode: "0\d{3}"`),
	// The whole secrets: block is optional and names a real vault, so a live
	// example would send anyone running the example config at a vault that
	// isn't theirs. Each leaf is pinned to the shape it is documented as.
	"secrets":                       regexp.MustCompile(`(?m)^# secrets:$`),
	"secrets.store":                 regexp.MustCompile(`(?m)^#\s+store: op$`),
	"secrets.policy":                regexp.MustCompile(`(?m)^#\s+policy: \w+`),
	"secrets.vault":                 regexp.MustCompile(`(?m)^#\s+vault: \S+`),
	"secrets.item":                  regexp.MustCompile(`(?m)^#\s+item: \S+`),
	"secrets.account":               regexp.MustCompile(`(?m)^#\s+account: \S+`),
	"secrets.definitions":           regexp.MustCompile(`(?m)^#\s+definitions:$`),
	"secrets.definitions.*.field":   regexp.MustCompile(`(?m)^#\s+field: \S+`),
	"secrets.definitions.*.default": regexp.MustCompile(`(?m)^#\s+default:`),
	"secrets.definitions.*.vault":   regexp.MustCompile(`(?m)^#\s+vault: \S+`),
	"secrets.definitions.*.item":    regexp.MustCompile(`(?m)^#\s+item: \S+`),
	"secrets.definitions.*.ref":     regexp.MustCompile(`(?m)^#\s+ref: op://`),
	// An output's secrets: list is meaningless without a secrets: block, so
	// it is illustrated in the same commented example.
	"outputs[].secrets": regexp.MustCompile(`(?m)^\s*#\s+secrets:$`),
}

// schemaYAMLPaths walks a config struct and returns the dotted path of every
// yaml key it can produce. Slices of structs contribute a "[]" segment so a
// list element's fields are distinguishable from a mapping's.
//
// Map-typed fields (env:, pools:) stop the walk deliberately: their keys are
// user-chosen, not schema. That is only sound while the map's values carry no
// schema of their own, so a map of structs fails the test rather than being
// silently skipped — the walker cannot express a path through it, and the
// nested keys would go undocumented with nothing to notice.
func schemaYAMLPaths(t *testing.T, typ reflect.Type, prefix string) []string {
	t.Helper()
	var out []string
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tag := f.Tag.Get("yaml")
		name, _, _ := strings.Cut(tag, ",")
		if name == "-" || name == "" {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		out = append(out, path)

		ft := f.Type
		for ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Map {
			elem := ft.Elem()
			for elem.Kind() == reflect.Ptr || elem.Kind() == reflect.Slice {
				elem = elem.Elem()
			}
			// A map of structs (secrets.definitions) carries schema under a
			// user-chosen key, so the walk continues through a "*" segment
			// that matches any one key. Stopping here instead would let a new
			// SecretDefinition field go undocumented with nothing to notice.
			if elem.Kind() == reflect.Struct {
				out = append(out, schemaYAMLPaths(t, elem, path+".*")...)
			}
			continue
		}
		if ft.Kind() == reflect.Slice {
			ft = ft.Elem()
			path += "[]"
		}
		if ft.Kind() == reflect.Struct {
			out = append(out, schemaYAMLPaths(t, ft, path)...)
		}
	}
	return out
}

// collectYAMLPaths records the dotted path of every mapping key present in a
// parsed document, mirroring schemaYAMLPaths' "[]" convention for sequences.
func collectYAMLPaths(n *yaml.Node, prefix string, out map[string]bool) {
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			collectYAMLPaths(c, prefix, out)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := n.Content[i].Value
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			out[path] = true
			collectYAMLPaths(n.Content[i+1], path, out)
		}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			collectYAMLPaths(c, prefix+"[]", out)
		}
	}
}

func TestValidatePoolGrow(t *testing.T) {
	cases := []struct {
		name     string
		pools    map[string][]string
		poolGrow map[string]string
		wantErr  string // substring; empty means the config must validate
	}{
		{
			name:     "valid android-emulator strategy",
			pools:    map[string][]string{"devices": {"emulator-5554"}},
			poolGrow: map[string]string{"devices": PoolGrowAndroidEmulator},
		},
		{
			name:     "no pool_grow block at all",
			pools:    map[string][]string{"devices": {"emulator-5554"}},
			poolGrow: nil,
		},
		{
			name:     "misspelled strategy",
			pools:    map[string][]string{"devices": {"emulator-5554"}},
			poolGrow: map[string]string{"devices": "android_emulator"},
			wantErr:  `pool_grow["devices"]: unknown strategy "android_emulator"`,
		},
		{
			name:     "script path mistaken for a strategy",
			pools:    map[string][]string{"devices": {"emulator-5554"}},
			poolGrow: map[string]string{"devices": "./scripts/create-emulator.sh"},
			wantErr:  `unknown strategy "./scripts/create-emulator.sh"`,
		},
		{
			name:     "empty strategy value",
			pools:    map[string][]string{"devices": {"emulator-5554"}},
			poolGrow: map[string]string{"devices": ""},
			wantErr:  `unknown strategy ""`,
		},
		{
			name:     "strategy for an undeclared pool",
			pools:    map[string][]string{"devices": {"emulator-5554"}},
			poolGrow: map[string]string{"device": PoolGrowAndroidEmulator},
			wantErr:  `pool_grow["device"]: no pool named "device" is declared under pools:`,
		},
		{
			name:     "strategy with no pools block at all",
			pools:    nil,
			poolGrow: map[string]string{"devices": PoolGrowAndroidEmulator},
			wantErr:  `no pool named "devices" is declared`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &WetConfig{
				ProjectName: "myapp",
				PortStride:  10,
				Pools:       c.pools,
				PoolGrow:    c.poolGrow,
			}
			err := cfg.Validate()
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error %q does not contain %q", err, c.wantErr)
			}
		})
	}
}

// A bad pool_grow value used to survive load and only surface later as a
// generic "pool exhausted" error that never mentioned pool_grow. Guard that
// it is now caught at load time instead.
func TestValidateRejectsUnknownPoolGrowBeforeExhaustion(t *testing.T) {
	cfg := &WetConfig{
		ProjectName: "myapp",
		PortStride:  10,
		Pools:       map[string][]string{"devices": {"emulator-5554"}},
		PoolGrow:    map[string]string{"devices": "android_emulator"},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected Validate to reject the typo'd strategy")
	}
	if !strings.Contains(err.Error(), "pool_grow") {
		t.Fatalf("error must name pool_grow, got %q", err)
	}
	if strings.Contains(err.Error(), "exhausted") {
		t.Fatalf("error should not be the misleading exhaustion message, got %q", err)
	}
}
