package cmd

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testEnvConfig() *WetConfig {
	return &WetConfig{
		ProjectName: "myapp",
		PortStride:  10,
		Ports: []PortConfig{
			{Name: "APP_PORT", Base: 8080},
			{Name: "GRPC_PORT", Base: 50051},
		},
		Env:   map[string]string{"COMPOSE_PROJECT_NAME": "{{.ProjectName}}-{{.BranchSafe}}"},
		Pools: map[string][]string{"devices": {"emulator-5554"}},
	}
}

func TestReadSecretsScalars(t *testing.T) {
	got, err := parseSecretsJSON([]byte(`{"A":"x","B":42,"C":true,"D":1.5}`), "-")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]string{"A": "x", "B": "42", "C": "true", "D": "1.5"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("key %s = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d keys, want %d", len(got), len(want))
	}
}

// No source means no secrets — and, critically, no read. A hook inherits its
// parent's stdin, so a sniffing implementation would block forever here.
func TestReadSecretsNoSourceDoesNotRead(t *testing.T) {
	got, err := readSecrets("")
	if err != nil || got != nil {
		t.Fatalf("readSecrets(\"\") = %v, %v; want nil, nil", got, err)
	}
}

func TestReadSecretsFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	if err := os.WriteFile(path, []byte(`{"SENTRY_DSN":"https://k@o/1"}`), 0600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := readSecrets(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["SENTRY_DSN"] != "https://k@o/1" {
		t.Errorf("got %v", got)
	}
	if _, err := readSecrets(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("expected an error for a missing --secrets-from file")
	}
}

func TestReadSecretsEmptyIsNoOverrides(t *testing.T) {
	for _, body := range []string{"", "   \n"} {
		got, err := parseSecretsJSON([]byte(body), "-")
		if err != nil {
			t.Fatalf("body %q: unexpected error: %v", body, err)
		}
		if got != nil {
			t.Errorf("body %q: got %v, want nil", body, got)
		}
	}
}

func TestReadSecretsRejectsNested(t *testing.T) {
	cases := map[string]string{
		"object": `{"A":{"nested":1}}`,
		"array":  `{"A":[1,2]}`,
		"null":   `{"A":null}`,
		"notObj": `["a","b"]`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseSecretsJSON([]byte(body), "-"); err == nil {
				t.Fatalf("expected an error for %s", body)
			}
		})
	}
}

func TestMergeOverridesRejectsPortAndReserved(t *testing.T) {
	cfg := testEnvConfig()
	cases := map[string]string{
		"port":     "APP_PORT",
		"reserved": "Slot",
	}
	for name, key := range cases {
		t.Run(name, func(t *testing.T) {
			data := map[string]any{}
			err := mergeOverrides(cfg, data, map[string]string{key: "9999"})
			if err == nil {
				t.Fatalf("expected %s override to be rejected", key)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error should name the offending key, got: %v", err)
			}
		})
	}
}

func TestMergeOverridesBeatsConfigEnv(t *testing.T) {
	cfg := testEnvConfig()
	data := map[string]any{"COMPOSE_PROJECT_NAME": "from-config", "SECRET": "from-config"}
	if err := mergeOverrides(cfg, data, map[string]string{"SECRET": "from-vault"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if data["SECRET"] != "from-vault" {
		t.Errorf("stdin should win over config env, got %v", data["SECRET"])
	}
	if data["COMPOSE_PROJECT_NAME"] != "from-config" {
		t.Errorf("untouched key was clobbered: %v", data["COMPOSE_PROJECT_NAME"])
	}
}

func TestDataFromEnvReadsHookVariables(t *testing.T) {
	cfg := testEnvConfig()
	t.Setenv("SLOT", "2")
	t.Setenv("BRANCH", "feature/x")
	t.Setenv("APP_PORT", "8100")
	t.Setenv("GRPC_PORT", "50071")
	t.Setenv("POOL_DEVICES", "emulator-5556")

	slot, ok := slotFromEnv()
	if !ok || slot != 2 {
		t.Fatalf("slotFromEnv() = %d, %v; want 2, true", slot, ok)
	}
	data, err := dataFromEnv(cfg, slot)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if data["APP_PORT"] != 8100 || data["GRPC_PORT"] != 50071 {
		t.Errorf("ports not read from env: %v %v", data["APP_PORT"], data["GRPC_PORT"])
	}
	if data["Slot"] != 2 || data["BranchSafe"] != "feature-x" {
		t.Errorf("identity fields wrong: %v %v", data["Slot"], data["BranchSafe"])
	}
	if pools, _ := data["Pools"].(map[string]string); pools["devices"] != "emulator-5556" {
		t.Errorf("pool not read from env: %v", data["Pools"])
	}
	if data["COMPOSE_PROJECT_NAME"] != "myapp-feature-x" {
		t.Errorf("cfg.Env not layered in: %v", data["COMPOSE_PROJECT_NAME"])
	}
}

func TestDataFromEnvMissingPortIsAnError(t *testing.T) {
	cfg := testEnvConfig()
	t.Setenv("SLOT", "1")
	t.Setenv("BRANCH", "x")
	t.Setenv("APP_PORT", "8090")
	os.Unsetenv("GRPC_PORT")
	if _, err := dataFromEnv(cfg, 1); err == nil || !strings.Contains(err.Error(), "GRPC_PORT") {
		t.Fatalf("expected an error naming GRPC_PORT, got %v", err)
	}
}

func TestDataFromEnvRequiresBranch(t *testing.T) {
	t.Setenv("SLOT", "1")
	t.Setenv("BRANCH", "")
	if _, err := dataFromEnv(testEnvConfig(), 1); err == nil {
		t.Fatal("expected an error when BRANCH is unset")
	}
}

// A missing key must fail the render rather than producing `port=` — an empty
// value in a Dotenvy `:string!` field is a silent boot failure downstream.
func TestRenderTemplateMissingKeyIsFatal(t *testing.T) {
	data := map[string]any{"APP_PORT": 8090}
	if _, err := renderWetTemplate("t", "port={{.NOPE}}\n", data); err == nil {
		t.Fatal("expected missing key to be an error")
	}
	out, err := renderWetTemplate("t", "port={{.APP_PORT}}\n", data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "port=8090\n" {
		t.Errorf("got %q", out)
	}
}

func TestWorktreeStateForPathMatchesResolvedPath(t *testing.T) {
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("evalsymlinks: %v", err)
	}
	state := &WorktreeConfig{Worktrees: []WorktreeState{
		{Branch: "other", Path: filepath.Join(real, "elsewhere")},
		{Branch: "mine", Path: real},
	}}
	// dir may be a symlink (/var vs /private/var on macOS) while the state
	// file records the resolved path — the lookup has to see through that.
	got := worktreeStateForPath(state, dir)
	if got == nil || got.Branch != "mine" {
		t.Fatalf("expected to match the 'mine' entry, got %v", got)
	}
	if worktreeStateForPath(state, filepath.Join(real, "untracked")) != nil {
		t.Error("expected nil for an untracked path")
	}
}

// Slot 0 is the main checkout's, so its ports must be the declared bases and
// no worktree may ever be handed them.
func TestSlotZeroIsMainAndReserved(t *testing.T) {
	cfg := testEnvConfig()
	ports := cfg.ResolvePorts(0)
	want := map[string]int{"APP_PORT": 8080, "GRPC_PORT": 50051}
	for _, p := range ports {
		if want[p.Name] != p.Value {
			t.Errorf("slot 0 %s = %d, want %d", p.Name, p.Value, want[p.Name])
		}
	}
	if got := NextFreeSlot(nil); got == 0 {
		t.Error("NextFreeSlot handed out slot 0, which belongs to the main checkout")
	}
}

func TestReadAllSecretsMergesInOrder(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		return p
	}
	db := write("db.json", `{"DB_USER":"postgres","SHARED":"from-db"}`)
	vault := write("vault.json", `{"DSN":"https://k@o/1","SHARED":"from-vault"}`)

	got, err := readAllSecrets("", []string{db, vault})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["DB_USER"] != "postgres" || got["DSN"] != "https://k@o/1" {
		t.Errorf("sources not merged: %v", got)
	}
	if got["SHARED"] != "from-vault" {
		t.Errorf("later source should win, got %q", got["SHARED"])
	}

	// Reversed order flips the winner — precedence is positional, not by name.
	got, err = readAllSecrets("", []string{vault, db})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["SHARED"] != "from-db" {
		t.Errorf("reversed order should flip the winner, got %q", got["SHARED"])
	}
}

func TestReadAllSecretsRejectsDoubleStdin(t *testing.T) {
	if _, err := readAllSecrets("", []string{"-", "-"}); err == nil {
		t.Fatal("expected an error when stdin is named twice")
	}
}

func TestReadAllSecretsNoSources(t *testing.T) {
	got, err := readAllSecrets("", nil)
	if err != nil || got != nil {
		t.Fatalf("readAllSecrets(nil) = %v, %v; want nil, nil", got, err)
	}
}

// A source that fails to read must abort the render, not quietly contribute
// nothing — the whole reason to merge here instead of in a shell pipeline.
func TestReadAllSecretsFailsOnBadSource(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "ok.json")
	if err := os.WriteFile(ok, []byte(`{"A":"1"}`), 0600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := readAllSecrets("", []string{ok, filepath.Join(dir, "missing.json")}); err == nil {
		t.Fatal("expected a missing source to abort the merge")
	}
}

func TestResolveEnvValuesChainedDerivation(t *testing.T) {
	cfg := &WetConfig{
		ProjectName: "myapp",
		Env: map[string]string{
			// Deliberately declared so that sorted order alone cannot resolve
			// them: AUTH_BASE_URL < DEV_HOST, and VIPPS < both.
			"AUTH_BASE_URL":      "http://{{.DEV_HOST}}:{{.PHX_PORT}}",
			"DEV_HOST":           "localhost",
			"VIPPS_REDIRECT_URI": "{{.AUTH_BASE_URL}}/callback",
		},
	}
	data := map[string]any{"PHX_PORT": 4010}
	got, err := ResolveEnvValues(cfg, data, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["AUTH_BASE_URL"] != "http://localhost:4010" {
		t.Errorf("AUTH_BASE_URL = %q", got["AUTH_BASE_URL"])
	}
	if got["VIPPS_REDIRECT_URI"] != "http://localhost:4010/callback" {
		t.Errorf("VIPPS_REDIRECT_URI = %q", got["VIPPS_REDIRECT_URI"])
	}
}

// .env.local is a machine override, so it must win over the checked-in
// default AND flow into everything derived from it.
func TestResolveEnvValuesLocalOverrideWinsAndPropagates(t *testing.T) {
	cfg := &WetConfig{
		ProjectName: "myapp",
		Env: map[string]string{
			"DEV_HOST":      "localhost",
			"AUTH_BASE_URL": "http://{{.DEV_HOST}}:{{.PHX_PORT}}",
		},
	}
	data := map[string]any{"PHX_PORT": 4000}
	got, err := ResolveEnvValues(cfg, data, map[string]string{"DEV_HOST": "192.168.1.9"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["DEV_HOST"] != "192.168.1.9" {
		t.Errorf("override lost: %q", got["DEV_HOST"])
	}
	if got["AUTH_BASE_URL"] != "http://192.168.1.9:4000" {
		t.Errorf("override did not propagate into the derived value: %q", got["AUTH_BASE_URL"])
	}
}

func TestResolveEnvValuesReportsCyclesAndTypos(t *testing.T) {
	cases := map[string]map[string]string{
		"cycle": {"A": "{{.B}}", "B": "{{.A}}"},
		"typo":  {"A": "{{.NOPE}}"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := &WetConfig{ProjectName: "x", Env: env}
			_, err := ResolveEnvValues(cfg, map[string]any{}, nil)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), "A") {
				t.Errorf("error should name the stuck key, got: %v", err)
			}
		})
	}
}

func TestLoadEnvLocal(t *testing.T) {
	dir := t.TempDir()
	body := "# comment\n\nDEV_HOST=192.168.1.9\nQUOTED=\"has spaces\"\nSINGLE='sq'\n  SPACED = padded \n"
	if err := os.WriteFile(filepath.Join(dir, envLocalFileName), []byte(body), 0600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := LoadEnvLocal(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]string{"DEV_HOST": "192.168.1.9", "QUOTED": "has spaces", "SINGLE": "sq", "SPACED": "padded"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}

	// A missing file is normal, not an error — the file is optional.
	empty, err := LoadEnvLocal(t.TempDir())
	if err != nil || len(empty) != 0 {
		t.Errorf("missing .env.local should yield an empty map, got %v, %v", empty, err)
	}

	// A malformed line is loud: a silently dropped override would show up as
	// a mystery wrong hostname much later.
	bad := t.TempDir()
	if err := os.WriteFile(filepath.Join(bad, envLocalFileName), []byte("NOT_AN_ASSIGNMENT\n"), 0600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := LoadEnvLocal(bad); err == nil {
		t.Error("expected an error for a line without '='")
	}
}

func TestValidateRenderTargets(t *testing.T) {
	base := func(targets ...RenderTarget) *WetConfig {
		return &WetConfig{ProjectName: "x", Render: targets}
	}
	cases := map[string]*WetConfig{
		"missing out":      base(RenderTarget{Template: "a.tpl"}),
		"missing template": base(RenderTarget{Out: "a"}),
		"escaping path":    base(RenderTarget{Template: "../a.tpl", Out: "a"}),
		"absolute path":    base(RenderTarget{Template: "a.tpl", Out: "/etc/passwd"}),
		"same file":        base(RenderTarget{Template: "a.tpl", Out: "a.tpl"}),
		"duplicate out": base(
			RenderTarget{Template: "a.tpl", Out: "shared"},
			RenderTarget{Template: "b.tpl", Out: "shared"},
		),
		"double stdin": base(RenderTarget{Template: "a.tpl", Out: "a", SecretsFrom: []string{"-", "-"}}),
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if err := cfg.Validate(); err == nil {
				t.Fatalf("expected %s to be rejected", name)
			}
		})
	}

	ok := base(
		RenderTarget{Template: "web/.env.tpl", Out: "web/.env.dev", SecretsFrom: []string{"-"}},
		RenderTarget{Template: "mob/.env.tpl", Out: "mob/.env"},
	)
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid render targets rejected: %v", err)
	}
}

func TestResolveSecretsSourceExpandsHomeAndEnv(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	t.Setenv("WET_TEST_VAULT_DIR", "/opt/vault")

	cases := []struct{ root, in, want string }{
		{"/repo", "-", "-"},
		{"/repo", "secrets.json", "/repo/secrets.json"},
		{"", "secrets.json", "secrets.json"},
		{"/repo", "/abs/secrets.json", "/abs/secrets.json"},
		{"/repo", "~/.config/app/secrets.json", filepath.Join(home, ".config/app/secrets.json")},
		{"/repo", "$WET_TEST_VAULT_DIR/secrets.json", "/opt/vault/secrets.json"},
		{"/repo", "${WET_TEST_VAULT_DIR}/secrets.json", "/opt/vault/secrets.json"},
	}
	for _, c := range cases {
		got, err := resolveSecretsSource(c.root, c.in)
		if err != nil {
			t.Fatalf("resolveSecretsSource(%q, %q): %v", c.root, c.in, err)
		}
		if got != c.want {
			t.Errorf("resolveSecretsSource(%q, %q) = %q, want %q", c.root, c.in, got, c.want)
		}
	}
}

func TestReadAllSecretsAnchorsRelativeToRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "s.json"), []byte(`{"K":"v"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// cwd is the package dir, not `dir` — so this only resolves via root.
	got, err := readAllSecrets(dir, []string{"s.json"})
	if err != nil {
		t.Fatalf("readAllSecrets: %v", err)
	}
	if got["K"] != "v" {
		t.Fatalf("got %v, want K=v", got)
	}
	if _, err := readAllSecrets("", []string{"s.json"}); err == nil {
		t.Fatal("root \"\" should keep cwd semantics and fail to find s.json")
	}
}

// A store key that shadows an `env:` entry is legal (that is how a vault value
// overrides a default) but it is also how a store carrying staging URLs
// silently replaces the ones wet just derived from this worktree's ports. It
// must say so.
func TestMergeOverridesWarnsOnEnvShadow(t *testing.T) {
	cfg := &WetConfig{Env: map[string]string{"AUTH_BASE_URL": "http://localhost:{{.APP_PORT}}"}}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	data := map[string]any{}
	mergeErr := mergeOverrides(cfg, data, map[string]string{
		"AUTH_BASE_URL": "https://auth.staging.example.com",
		"API_KEY":       "sk-test",
	})
	os.Stderr = orig
	w.Close()
	out, _ := io.ReadAll(r)

	if mergeErr != nil {
		t.Fatalf("mergeOverrides: %v", mergeErr)
	}
	if data["AUTH_BASE_URL"] != "https://auth.staging.example.com" {
		t.Errorf("store key should still win, got %v", data["AUTH_BASE_URL"])
	}
	if !strings.Contains(string(out), `"AUTH_BASE_URL"`) {
		t.Errorf("warning should name the shadowed key, got %q", out)
	}
	if strings.Contains(string(out), "API_KEY") {
		t.Errorf("a key with no env: entry must not warn, got %q", out)
	}
}

// A render target may point into a directory that doesn't exist yet — the
// .vscode/launch.json case, where .vscode is gitignored and absent from a
// fresh worktree. Before MkdirAll this failed with a bare ENOENT.
func TestRenderSetupTargetCreatesMissingParentDir(t *testing.T) {
	tree := t.TempDir()
	// The template lives in .vscode/ too, as the documented recipe has it.
	if err := os.MkdirAll(filepath.Join(tree, ".vscode"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, ".vscode", "launch.json.tpl"),
		[]byte(`{"port": {{.APP_PORT}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	// Render into a sibling directory that does not exist yet.
	cfg := &WetConfig{ProjectName: "app"}
	data := map[string]any{"APP_PORT": 4010}
	target := RenderTarget{Template: ".vscode/launch.json.tpl", Out: ".idea/launch.json"}

	if err := renderSetupTarget(cfg, data, tree, target, nil); err != nil {
		t.Fatalf("render into missing parent dir: %v", err)
	}

	out := filepath.Join(tree, ".idea", "launch.json")
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read rendered output: %v", err)
	}
	if want := `{"port": 4010}`; string(got) != want {
		t.Errorf("rendered %q, want %q", got, want)
	}
	// The created directory must not widen access to a file that may hold
	// secrets beyond the owner.
	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Errorf("rendered file mode %o, want 600", got)
	}
}

// --dry-run must not touch the filesystem, including by creating the parent
// directory it would have written into. This is the branch most at risk if
// the MkdirAll is ever hoisted above the dry-run early return.
func TestRenderTargetDryRunCreatesNothing(t *testing.T) {
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "x.tpl"),
		[]byte(`{{.APP_PORT}}`), 0600); err != nil {
		t.Fatal(err)
	}

	restoreAll, restoreDry := envPopulateAll, envPopulateDryRun
	envPopulateAll, envPopulateDryRun = true, true
	defer func() { envPopulateAll, envPopulateDryRun = restoreAll, restoreDry }()

	// Capture stdout: the dry-run branch prints the rendered body, which would
	// otherwise interleave with the test runner's own output. Asserting on it
	// also covers what dry-run is actually for.
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	target := RenderTarget{Template: "x.tpl", Out: "generated/x"}
	renderErr := renderTarget(&WetConfig{ProjectName: "app"},
		map[string]any{"APP_PORT": 4010}, tree, target, nil)

	w.Close()
	os.Stdout = orig
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if renderErr != nil {
		t.Fatalf("dry run: %v", renderErr)
	}

	if !strings.Contains(string(out), "4010") {
		t.Errorf("dry run printed %q; want the rendered value 4010", out)
	}
	if _, err := os.Stat(filepath.Join(tree, "generated")); !os.IsNotExist(err) {
		t.Errorf("dry run created the output directory (stat err: %v)", err)
	}
}

// The --all path renders declared, validated targets, so it creates missing
// parents. A bare --out is an unvalidated user-supplied path and must keep
// failing fast instead of building directory trees from a typo.
func TestRenderTargetBareOutDoesNotCreateParent(t *testing.T) {
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "x.tpl"),
		[]byte(`{{.APP_PORT}}`), 0600); err != nil {
		t.Fatal(err)
	}

	restoreAll, restoreDry := envPopulateAll, envPopulateDryRun
	envPopulateAll, envPopulateDryRun = false, false
	defer func() { envPopulateAll, envPopulateDryRun = restoreAll, restoreDry }()

	target := RenderTarget{
		Template: filepath.Join(tree, "x.tpl"),
		Out:      filepath.Join(tree, "nope", "x"),
	}
	err := renderTarget(&WetConfig{ProjectName: "app"},
		map[string]any{"APP_PORT": 4010}, tree, target, nil)
	if err == nil {
		t.Fatal("expected an error for a missing parent under a bare --out")
	}
	if _, statErr := os.Stat(filepath.Join(tree, "nope")); !os.IsNotExist(statErr) {
		t.Error("bare --out created a directory tree")
	}
}

// The --all path creates missing parent directories (declared targets have
// been through validateCopyPath). Without this, only the negative cases were
// covered and deleting the MkdirAll left the suite green.
func TestRenderTargetAllCreatesMissingParentDir(t *testing.T) {
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "x.tpl"),
		[]byte(`{{.APP_PORT}}`), 0600); err != nil {
		t.Fatal(err)
	}

	restoreAll, restoreDry := envPopulateAll, envPopulateDryRun
	envPopulateAll, envPopulateDryRun = true, false
	defer func() { envPopulateAll, envPopulateDryRun = restoreAll, restoreDry }()

	target := RenderTarget{Template: "x.tpl", Out: "generated/deep/x"}
	if err := renderTarget(&WetConfig{ProjectName: "app"},
		map[string]any{"APP_PORT": 4010}, tree, target, nil); err != nil {
		t.Fatalf("render into missing parent dir: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(tree, "generated", "deep", "x"))
	if err != nil {
		t.Fatalf("read rendered output: %v", err)
	}
	if string(got) != "4010" {
		t.Errorf("rendered %q, want %q", got, "4010")
	}
}
