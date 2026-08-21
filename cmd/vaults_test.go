package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// vaultsConfigBody is a complete .wet.yaml whose secrets: block is whatever
// the case supplies, so a case reads as the block alone.
const vaultsConfigBody = `project_name: app
port_stride: 10
ports:
  - { name: PHX_PORT, base: 4000 }
secrets:
  store: op
  item: I
%s  definitions:
    API_KEY:
      field: api_key
      default: dev-key
outputs:
  - name: backend
    out: backend/.env.dev
    format: dotenv
    secrets: [API_KEY]
  - name: mobile
    out: mobile/.env
    format: dotenv
    values:
      clientId:
        literal: app-mobile
`

func loadVaultsConfig(t *testing.T, secretsBlock string) (*WetConfig, error) {
	t.Helper()
	dir := t.TempDir()
	body := strings.Replace(vaultsConfigBody, "%s", secretsBlock, 1)
	if err := os.WriteFile(filepath.Join(dir, wetConfigFileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return LoadWetConfigWithLocal(dir, "")
}

// Every way a vaults: block can be wrong has to fail at LOAD. Selecting a
// vault nobody declared resolves to no destination, and under the default
// `fallback` policy that is indistinguishable from being offline — wet would
// write every declared default and look configured.
func TestVaultsValidation(t *testing.T) {
	cases := map[string]struct {
		block string
		want  string
	}{
		"vault and vaults together": {
			"  vault: V\n  vaults:\n    - { name: S, env: staging }\n",
			"names both vault:",
		},
		"entry without name": {
			"  vaults:\n    - { env: staging }\n",
			"name: is required",
		},
		"entry without env": {
			"  vaults:\n    - { name: S }\n",
			"env: is required",
		},
		"env with an illegal name": {
			"  vaults:\n    - { name: S, env: Staging }\n",
			"must match",
		},
		"duplicate env": {
			"  vaults:\n    - { name: A, env: staging }\n    - { name: B, env: staging }\n",
			"declared twice",
		},
		"several vaults and no default_env": {
			"  vaults:\n    - { name: A, env: staging }\n    - { name: B, env: production }\n",
			"default_env",
		},
		"default_env naming nothing": {
			"  vaults:\n    - { name: A, env: staging }\n  default_env: prod\n",
			"not one of",
		},
		"default_env without vaults": {
			"  vault: V\n  default_env: staging\n",
			"declares none",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := loadVaultsConfig(t, c.block)
			if err == nil {
				t.Fatalf("%s was accepted", name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to mention %q", err, c.want)
			}
		})
	}
}

// One vault needs no default_env: there is nothing to choose between.
func TestVaultsSingleEntryNeedsNoDefaultEnv(t *testing.T) {
	cfg, err := loadVaultsConfig(t, "  vaults:\n    - { name: Only, env: staging }\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	env, err := cfg.Secrets.SelectEnv("")
	if err != nil {
		t.Fatalf("SelectEnv: %v", err)
	}
	if env != "staging" {
		t.Fatalf("env = %q, want staging", env)
	}
	ref, err := cfg.Secrets.ResolveSecretRef("API_KEY", env)
	if err != nil {
		t.Fatal(err)
	}
	if ref.URI() != "op://Only/I/api_key" {
		t.Errorf("ref = %s", ref.URI())
	}
}

// The whole point: same key, same field, same item — a different vault per
// environment, chosen by default_env or by --env.
func TestVaultsSelectEnvAndResolve(t *testing.T) {
	cfg, err := loadVaultsConfig(t,
		"  vaults:\n    - { name: app_staging, env: staging }\n    - { name: app_prod, env: production }\n  default_env: staging\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cases := map[string]struct{ override, env, uri string }{
		"default":  {"", "staging", "op://app_staging/I/api_key"},
		"override": {"production", "production", "op://app_prod/I/api_key"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			env, err := cfg.Secrets.SelectEnv(c.override)
			if err != nil {
				t.Fatalf("SelectEnv(%q): %v", c.override, err)
			}
			if env != c.env {
				t.Fatalf("env = %q, want %q", env, c.env)
			}
			ref, err := cfg.Secrets.ResolveSecretRef("API_KEY", env)
			if err != nil {
				t.Fatal(err)
			}
			if ref.URI() != c.uri {
				t.Errorf("ref = %s, want %s", ref.URI(), c.uri)
			}
		})
	}

	// An --env nobody declared must fail loudly rather than fall back to the
	// default: a typo'd `--env prodution` that silently wrote staging values
	// over production credentials is the failure this whole feature can
	// cause and must not.
	if _, err := cfg.Secrets.SelectEnv("prodution"); err == nil {
		t.Error("an undeclared --env was accepted")
	} else if !strings.Contains(err.Error(), "staging, production") {
		t.Errorf("err = %v, want it to list the declared environments", err)
	}
}

// --env against a config with no vaults: is a request wet cannot honour.
// Answering it with the single `vault:` would read from the wrong place while
// reporting the environment the user asked for.
func TestVaultsEnvOverrideWithoutVaults(t *testing.T) {
	cfg, err := loadVaultsConfig(t, "  vault: V\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := cfg.Secrets.SelectEnv("production"); err == nil {
		t.Fatal("--env was accepted against a config with no vaults:")
	}
	// The unchanged single-vault path still resolves.
	ref, err := cfg.Secrets.ResolveSecretRef("API_KEY", "")
	if err != nil {
		t.Fatal(err)
	}
	if ref.URI() != "op://V/I/api_key" {
		t.Errorf("ref = %s", ref.URI())
	}
}

// A definition's own vault: is a fact about that credential, so it outranks
// the environment's vault — "payments lives in the payments vault" stays true
// in staging and in production.
func TestVaultsPerSecretOverrideWins(t *testing.T) {
	cfg, err := loadVaultsConfig(t,
		"  vaults:\n    - { name: app_staging, env: staging }\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	def := cfg.Secrets.Definitions["API_KEY"]
	def.Vault = "payments"
	cfg.Secrets.Definitions["API_KEY"] = def
	ref, err := cfg.Secrets.ResolveSecretRef("API_KEY", "staging")
	if err != nil {
		t.Fatal(err)
	}
	if ref.URI() != "op://payments/I/api_key" {
		t.Errorf("ref = %s, want the per-secret vault", ref.URI())
	}
}

// The vaults are committed, and .wet.local.yaml is an ordinary overlay over
// them: a checkout that sets default_env: alone works against production
// while everything else stays on the project's default, and a machine that
// reads differently-named vaults may override the list outright — a sequence
// replaces rather than merging element by element, which is the only sane
// meaning for a list of vaults.
func TestVaultsFromLocalOverlay(t *testing.T) {
	dir := t.TempDir()
	base := `project_name: app
port_stride: 10
ports:
  - { name: PHX_PORT, base: 4000 }
secrets:
  store: op
  item: I
  vaults:
    - { name: app_staging, env: staging }
    - { name: app_prod, env: production }
  default_env: staging
  definitions:
    API_KEY:
      field: api_key
      default: dev-key
outputs:
  - name: backend
    out: backend/.env.dev
    format: dotenv
    secrets: [API_KEY]
`
	if err := os.WriteFile(filepath.Join(dir, wetConfigFileName), []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	// The machine layer renames the vaults without touching the environments
	// — the ordinary override any key gets, on the one machine whose store
	// spells them differently.
	machine := t.TempDir()
	if err := os.WriteFile(filepath.Join(machine, wetLocalConfigFileName), []byte(
		"secrets:\n  vaults:\n    - { name: mine_staging, env: staging }\n    - { name: mine_prod, env: production }\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	// The checkout's own layer names only default_env, which is the whole
	// point of layering: it must not have to restate the vault list.
	if err := os.WriteFile(filepath.Join(dir, wetLocalConfigFileName),
		[]byte("secrets:\n  default_env: production\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWetConfigWithLocal(dir, machine, dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Secrets.Vaults) != 2 || cfg.Secrets.Vaults[0].Name != "mine_staging" {
		t.Fatalf("vaults = %+v, want the machine layer's list to replace the committed one", cfg.Secrets.Vaults)
	}
	env, err := cfg.Secrets.SelectEnv("")
	if err != nil {
		t.Fatal(err)
	}
	if env != "production" {
		t.Fatalf("env = %q, want the checkout's default_env to win", env)
	}
	ref, err := cfg.Secrets.ResolveSecretRef("API_KEY", env)
	if err != nil {
		t.Fatal(err)
	}
	if ref.URI() != "op://mine_prod/I/api_key" {
		t.Errorf("ref = %s", ref.URI())
	}
}

// A pull names its environment, and the values it writes come from that
// environment's vault.
func TestPullOutputsUsesSelectedEnv(t *testing.T) {
	cfg, err := loadVaultsConfig(t,
		"  vaults:\n    - { name: app_staging, env: staging }\n    - { name: app_prod, env: production }\n  default_env: staging\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	reader := newFakeStore(map[string]string{
		"op://app_staging/I/api_key": "staging-value",
		"op://app_prod/I/api_key":    "production-value",
	})
	cases := map[string]struct{ override, want string }{
		"default_env": {"", "staging-value"},
		"--env":       {"production", "production-value"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			tree := t.TempDir()
			var out strings.Builder
			err := PullOutputs(PullRequest{
				Cfg:      cfg,
				TreePath: tree,
				Data:     map[string]any{"PHX_PORT": 4000},
				Env:      c.override,
				Reader:   reader,
				Out:      &out,
			})
			if err != nil {
				t.Fatalf("pull: %v", err)
			}
			body, err := os.ReadFile(filepath.Join(tree, "backend/.env.dev"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), "API_KEY="+c.want) {
				t.Errorf("wrote:\n%s\nwant API_KEY=%s", body, c.want)
			}
		})
	}

	// An undeclared --env must stop the pull rather than fall through to the
	// default and quietly write another environment's values.
	err = PullOutputs(PullRequest{
		Cfg: cfg, TreePath: t.TempDir(),
		Data: map[string]any{"PHX_PORT": 4000},
		Env:  "sandbox", Reader: reader, Out: &strings.Builder{},
	})
	if err == nil {
		t.Error("pull accepted an undeclared --env")
	}
}

// A ref: names its own vault, so it does not follow --env. That is allowed,
// and it is exactly the thing a reader cannot see in the generated file — so
// `explain` has to say so.
func TestVaultsRefIsEnvironmentInvariant(t *testing.T) {
	cfg, err := loadVaultsConfig(t,
		"  vaults:\n    - { name: app_staging, env: staging }\n    - { name: app_prod, env: production }\n  default_env: staging\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfg.Secrets.Definitions["API_KEY"] = SecretDefinition{Ref: "op://shared/I/api_key"}
	for _, env := range []string{"staging", "production"} {
		ref, err := cfg.Secrets.ResolveSecretRef("API_KEY", env)
		if err != nil {
			t.Fatal(err)
		}
		if ref.URI() != "op://shared/I/api_key" {
			t.Errorf("env %s: ref = %s, want the ref's own vault", env, ref.URI())
		}
	}
}

// ResolveSecretRef with an env its caller invented must say which environments
// exist, rather than resolving to the empty vault of a config that has none.
func TestVaultsResolveWithUndeclaredEnv(t *testing.T) {
	cfg, err := loadVaultsConfig(t, "  vaults:\n    - { name: app_staging, env: staging }\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	_, err = cfg.Secrets.ResolveSecretRef("API_KEY", "sandbox")
	if err == nil {
		t.Fatal("an undeclared env resolved")
	}
	if !strings.Contains(err.Error(), "staging") {
		t.Errorf("err = %v, want it to name the declared environments", err)
	}
}

// A pull that reads no vault must not announce an environment: the line would
// claim an influence the run did not have.
func TestPullOutputsNamesEnvOnlyWhenItReadsAVault(t *testing.T) {
	cfg, err := loadVaultsConfig(t,
		"  vaults:\n    - { name: app_staging, env: staging }\n  default_env: staging\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	reader := newFakeStore(map[string]string{"op://app_staging/I/api_key": "v"})

	var withSecrets strings.Builder
	if err := PullOutputs(PullRequest{
		Cfg: cfg, TreePath: t.TempDir(), Data: map[string]any{"PHX_PORT": 4000},
		Reader: reader, Out: &withSecrets,
	}); err != nil {
		t.Fatalf("pull: %v", err)
	}
	if !strings.Contains(withSecrets.String(), "environment: staging") {
		t.Errorf("pull did not name its environment:\n%s", withSecrets.String())
	}

	// The other half of the same rule: an output selecting no secrets reads
	// no vault, so a pull of it alone must not announce one either.
	var noSecrets strings.Builder
	if err := PullOutputs(PullRequest{
		Cfg: cfg, TreePath: t.TempDir(), Data: map[string]any{"PHX_PORT": 4000},
		Only: []string{"mobile"}, Reader: reader, Out: &noSecrets,
	}); err != nil {
		t.Fatalf("pull mobile: %v", err)
	}
	if strings.Contains(noSecrets.String(), "environment:") {
		t.Errorf("a pull selecting no secrets still named an environment:\n%s", noSecrets.String())
	}

	var disabled strings.Builder
	if err := PullOutputs(PullRequest{
		Cfg: cfg, TreePath: t.TempDir(), Data: map[string]any{"PHX_PORT": 4000},
		Policy: SecretPolicyDisabled, Out: &disabled,
	}); err != nil {
		t.Fatalf("pull --secrets=disabled: %v", err)
	}
	if strings.Contains(disabled.String(), "environment:") {
		t.Errorf("a pull that contacted no store still named an environment:\n%s", disabled.String())
	}
}

// opVaultsRepo is opRepo's multi-environment sibling: a real git repo, on
// disk, whose secrets: block declares two vaults. The commands are driven
// through their RunE so the --env flag wiring is exercised, not bypassed.
func opVaultsRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	cfg := `project_name: app
port_stride: 10
ports:
  - name: APP_PORT
    base: 4000
secrets:
  store: op
  policy: fallback
  item: fantasy
  account: my.1password.com
  vaults:
    - name: app_staging
      env: staging
    - name: app_prod
      env: production
  default_env: staging
  definitions:
    SECRET_KEY_BASE:
      field: secret_key_base
      default: dev-only
    SHARED_KEY:
      ref: op://shared/fantasy/shared_key
outputs:
  - name: backend
    out: backend/.env.dev
    format: dotenv
    include: [APP_PORT]
    secrets: [SECRET_KEY_BASE, SHARED_KEY]
  - name: mobile
    out: mobile/.env
    format: dotenv
    values:
      clientId:
        literal: app-mobile
`
	if err := os.WriteFile(filepath.Join(repo, ".wet.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	return repo
}

// explain resolves destinations in the environment it was given, names the
// choice, and marks the one secret --env cannot move.
func TestEnvExplainWithEnvFlag(t *testing.T) {
	opVaultsRepo(t)
	resetEnvFlags(t)

	out := captureStdout(t, func() {
		if err := runEnvExplain(nil, nil); err != nil {
			t.Fatalf("explain: %v", err)
		}
	})
	if !strings.Contains(out, "environment: staging (of staging, production)") {
		t.Errorf("explain did not name the default environment:\n%s", out)
	}
	// Asserted as whole lines. The marker's entire job is telling the two
	// secrets apart, so a bare "the word appears somewhere" check passes just
	// as happily when EVERY destination is marked env-invariant — including
	// the one that demonstrably moves.
	assertLine(t, out, "SECRET_KEY_BASE -> app_staging/fantasy/secret_key_base (has default)")
	assertLine(t, out, "SHARED_KEY -> shared/fantasy/shared_key (no default, same in every environment)")

	envSelectEnv = "production"
	out = captureStdout(t, func() {
		if err := runEnvExplain(nil, nil); err != nil {
			t.Fatalf("explain --env production: %v", err)
		}
	})
	assertLine(t, out, "SECRET_KEY_BASE -> app_prod/fantasy/secret_key_base (has default)")
	assertLine(t, out, "SHARED_KEY -> shared/fantasy/shared_key (no default, same in every environment)")

	// The banner follows the same rule as a pull's: an output selecting no
	// secrets resolves no vault, so announcing an environment over it would
	// claim an influence the choice did not have.
	envSelectEnv = ""
	out = captureStdout(t, func() {
		if err := runEnvExplain(nil, []string{"mobile"}); err != nil {
			t.Fatalf("explain mobile: %v", err)
		}
	})
	if strings.Contains(out, "environment:") {
		t.Errorf("explain named an environment for an output selecting no secrets:\n%s", out)
	}

	envSelectEnv = "sandbox"
	var err error
	_ = captureStdout(t, func() { err = runEnvExplain(nil, nil) })
	if err == nil {
		t.Fatal("explain accepted an undeclared --env")
	}
	// Not just "it failed": a missing worktree or an unreadable config would
	// also fail, and would pass an err != nil check while the flag went
	// unvalidated.
	if !strings.Contains(err.Error(), "not one of") {
		t.Errorf("err = %v, want it to reject the environment by name", err)
	}
}

// assertLine fails unless out contains the exact line, trimmed. Destination
// listings are asserted whole because their tail — the default and the
// environment-invariance marker — is the part that carries the meaning.
func assertLine(t *testing.T, out, want string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == want {
			return
		}
	}
	t.Errorf("missing line %q in:\n%s", want, out)
}

// The --env flag has to reach a pull, not only explain and push. Nothing else
// covers the one line wiring the flag into PullRequest, and its failure is
// silent: `wet env pull --env production` would write staging values with no
// error at all.
func TestEnvPullWithEnvFlag(t *testing.T) {
	requireJQ(t)
	stubOP(t, `{"fantasy":{"secret_key_base":"stored","shared_key":"shared-value"}}`)
	opVaultsRepo(t)
	resetEnvFlags(t)
	worktreeEnv(t, 1)

	out := captureStdout(t, func() {
		if err := runEnvPull(nil, nil); err != nil {
			t.Fatalf("pull: %v", err)
		}
	})
	if !strings.Contains(out, "environment: staging (of staging, production)") {
		t.Errorf("pull did not read the default environment:\n%s", out)
	}

	envSelectEnv = "production"
	out = captureStdout(t, func() {
		if err := runEnvPull(nil, nil); err != nil {
			t.Fatalf("pull --env production: %v", err)
		}
	})
	if !strings.Contains(out, "environment: production (of staging, production)") {
		t.Errorf("--env did not reach the pull:\n%s", out)
	}

	envSelectEnv = "sandbox"
	var err error
	_ = captureStdout(t, func() { err = runEnvPull(nil, nil) })
	if err == nil {
		t.Fatal("pull accepted an undeclared --env")
	}
	if !strings.Contains(err.Error(), "not one of") {
		t.Errorf("err = %v, want it to reject the environment by name", err)
	}
}

// push is the command that writes, so its dry run has to show the environment
// and the vault it would write into — before anyone runs it for real.
func TestEnvPushWithEnvFlag(t *testing.T) {
	requireJQ(t)
	// The item already holds a different value for the ref: secret, so its
	// line reports a CONFLICT. That is the status the marker matters most on:
	// the next move after a conflict is --force, and a --force aimed at what
	// looks like a production-only credential overwrites the one staging
	// reads too.
	stubOP(t, `{"fantasy":{"shared_key":"other"}}`)
	repo := opVaultsRepo(t)
	resetEnvFlags(t)
	worktreeEnv(t, 1)

	// Both secrets are present in the file, so the report covers the ref:
	// definition too — the one --env cannot move, and the one whose line has
	// to say so.
	src := filepath.Join(repo, "import.env")
	if err := os.WriteFile(src, []byte("SECRET_KEY_BASE=real-key\nSHARED_KEY=shared-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envPushFrom = src
	envPushDryRun = true
	envSelectEnv = "production"
	var pushErr error
	out := captureStdout(t, func() { pushErr = runEnvPush(nil, []string{"backend"}) })
	if pushErr == nil || !strings.Contains(pushErr.Error(), "conflict") {
		t.Fatalf("push --dry-run --env production: err = %v, want the conflict report", pushErr)
	}
	if !strings.Contains(out, "environment: production (of staging, production)") {
		t.Errorf("push did not name the environment it would write to:\n%s", out)
	}
	assertLine(t, out, "SECRET_KEY_BASE -> app_prod/fantasy/secret_key_base: would write")
	// The ref: destination keeps its own vault under --env, and the line says
	// so whatever its status — the marker is not a property of "would write".
	assertLine(t, out,
		"SHARED_KEY -> shared/fantasy/shared_key: CONFLICT — store holds a different value (same in every environment)")

	envSelectEnv = "sandbox"
	var err error
	_ = captureStdout(t, func() { err = runEnvPush(nil, []string{"backend"}) })
	if err == nil {
		t.Fatal("push accepted an undeclared --env")
	}
	if !strings.Contains(err.Error(), "not one of") {
		t.Errorf("err = %v, want it to reject the environment by name", err)
	}
}

// The flag reset must cover EVERY env flag, not the ones someone remembered.
// A leaked --env would silently retarget every later push in the process at
// another environment's vault, which is the one mistake this feature can
// cause — so the mechanism is asserted over the whole command tree rather
// than over one variable.
func TestResetEnvFlagsClearsEveryFlag(t *testing.T) {
	// Before the scramble, so the cleanup is registered: a t.Fatal below must
	// not leave every env flag set for the rest of the package.
	resetEnvFlags(t)

	scrambled := map[string]string{}
	for _, c := range append([]*cobra.Command{envCmd}, envCmd.Commands()...) {
		c.Flags().VisitAll(func(f *pflag.Flag) {
			value := "scrambled"
			switch f.Value.Type() {
			case "bool":
				value = "true"
			}
			if err := f.Value.Set(value); err != nil {
				t.Fatalf("scramble --%s: %v", f.Name, err)
			}
			f.Changed = true
			scrambled[c.Name()+" --"+f.Name] = f.DefValue
		})
	}
	if len(scrambled) == 0 {
		t.Fatal("no env flags found — this test is watching nothing")
	}
	if envSelectEnv == "" {
		t.Fatal("--env is not registered on any env command")
	}

	resetEnvFlags(t)

	for _, c := range append([]*cobra.Command{envCmd}, envCmd.Commands()...) {
		c.Flags().VisitAll(func(f *pflag.Flag) {
			if got := f.Value.String(); got != f.DefValue {
				t.Errorf("%s --%s = %q after reset, want the default %q", c.Name(), f.Name, got, f.DefValue)
			}
			if f.Changed {
				t.Errorf("%s --%s still reports Changed after reset", c.Name(), f.Name)
			}
		})
	}
	if envSelectEnv != "" {
		t.Errorf("envSelectEnv = %q after reset", envSelectEnv)
	}
}

// The banner is a multi-environment feature, so a config without `vaults:` —
// which is every config that exists today — must never print it. With the
// `env != ""` guard gone the line degrades to `environment:  (of )` on every
// pull, explain and push those users run, so all three are pinned here.
func TestSingleVaultConfigNeverNamesAnEnvironment(t *testing.T) {
	requireJQ(t)
	stubOP(t, `{"fantasy":{"secret_key_base":"real-key","google_client_id":"id"}}`)
	repo := opRepo(t)
	resetEnvFlags(t)
	worktreeEnv(t, 1)

	src := filepath.Join(repo, "import.env")
	if err := os.WriteFile(src, []byte("SECRET_KEY_BASE=real-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envPushFrom = src
	envPushDryRun = true

	for name, run := range map[string]func() error{
		"explain": func() error { return runEnvExplain(nil, nil) },
		"pull":    func() error { return runEnvPull(nil, nil) },
		"push":    func() error { return runEnvPush(nil, []string{"backend"}) },
	} {
		t.Run(name, func(t *testing.T) {
			var err error
			out := captureStdout(t, func() { err = run() })
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if strings.Contains(out, "environment:") {
				t.Errorf("%s named an environment on a config with no vaults::\n%s", name, out)
			}
		})
	}
}
