package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// execCommand runs a helper command and returns its combined output, so a
// missing git skips rather than fails.
func execCommand(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

// fakeStore is an in-memory SecretReader. Unlike the `op` stub in op_test.go
// it can be told to fail, which is the only way to exercise the three
// policies deterministically, and it counts reads so "fetch each reference at
// most once" is an assertion rather than a hope.
type fakeStore struct {
	values map[string]string
	reads  map[string]int
	fail   error
}

func newFakeStore(values map[string]string) *fakeStore {
	return &fakeStore{values: values, reads: map[string]int{}}
}

func (f *fakeStore) Read(uri string) (string, error) {
	f.reads[uri]++
	if f.fail != nil {
		return "", f.fail
	}
	v, ok := f.values[uri]
	if !ok {
		return "", fmt.Errorf("%s: not found", uri)
	}
	return v, nil
}

// loadTestConfig parses a .wet.yaml body through the real loader, so every
// test here exercises the same validation a user's config does.
func loadTestConfig(t *testing.T, body string) *WetConfig {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, wetConfigFileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWetConfigWithLocal(dir, "")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

// worktreeData builds the ordinary value set for a slot the way setup and
// pull both do: reserved fields, strided ports, pools, then the resolved env:
// block layered over `.env.local`.
func worktreeData(t *testing.T, cfg *WetConfig, slot int, local map[string]string) map[string]any {
	t.Helper()
	pools := map[string]string{}
	for name, items := range cfg.Pools {
		pools[name] = items[0]
	}
	data := BuildTemplateData(cfg, "feature/x", slot, cfg.ResolvePorts(slot), pools)
	if _, err := ResolveEnvValues(cfg, data, local); err != nil {
		t.Fatalf("resolve env: %v", err)
	}
	return data
}

// pullInto runs the pipeline into a temp tree and returns the tree plus the
// pipeline's own log, so provenance reporting is testable without stdout
// capture.
func pullInto(t *testing.T, req PullRequest) (string, string, error) {
	t.Helper()
	if req.TreePath == "" {
		req.TreePath = t.TempDir()
	}
	var log strings.Builder
	req.Out = &log
	err := PullOutputs(req)
	return req.TreePath, log.String(), err
}

func readOutput(t *testing.T, tree, rel string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(tree, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(body)
}

// fullConfig is the design's worked example, trimmed: two outputs, one with
// secrets and one without, a derived env: chain, and a renamed port.
const fullConfig = `project_name: app
port_stride: 10
ports:
  - { name: PHX_PORT, base: 4000 }
  - { name: GRPC_PORT, base: 50051 }
env:
  DEV_HOST: localhost
  PHX_HOST: "{{.DEV_HOST}}"
  AUTH_BASE_URL: "http://{{.DEV_HOST}}:{{.PHX_PORT}}"
  GOOGLE_REDIRECT_URI: "{{.AUTH_BASE_URL}}/callback/google"
  SENTRY_ENV: dev
secrets:
  store: op
  policy: fallback
  vault: V
  item: I
  definitions:
    SECRET_KEY_BASE:
      field: secret_key_base
      default: dev-only-key
    ADMIN_EMAILS:
      field: admin_emails
      default: ""
    SHARED_ID:
      field: shared_id
      default: ""
outputs:
  - name: backend
    out: backend/.env.dev
    format: dotenv
    include: [PHX_HOST, AUTH_BASE_URL, GOOGLE_REDIRECT_URI, SENTRY_ENV]
    secrets: [SECRET_KEY_BASE, ADMIN_EMAILS, SHARED_ID]
  - name: mobile
    out: mobile/.env
    format: dotenv
    values:
      host:
        from: DEV_HOST
      port:
        from: GRPC_PORT
      webBaseUrl:
        from: AUTH_BASE_URL
      oidcClientId:
        literal: app-mobile
      oidcRedirectUri:
        literal: "app://auth/callback"
`

// The whole point of wet: two outputs, one config, and every port-derived
// value carrying THIS worktree's slot. A rename must not lose the striding.
func TestPullStridedPortsReachEveryOutput(t *testing.T) {
	cfg := loadTestConfig(t, fullConfig)
	store := newFakeStore(map[string]string{
		"op://V/I/secret_key_base": "real-key",
		"op://V/I/admin_emails":    "a@b.c",
		"op://V/I/shared_id":       "sid",
	})
	tree, _, err := pullInto(t, PullRequest{
		Cfg: cfg, Data: worktreeData(t, cfg, 3, nil), Reader: store,
	})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}

	backend := readOutput(t, tree, "backend/.env.dev")
	// PHX_PORT at slot 3 is 4000 + 3*10, derived once in env: and consumed
	// twice — the derivation a per-package template used to repeat.
	for _, want := range []string{
		"PHX_HOST=localhost",
		"AUTH_BASE_URL=http://localhost:4030",
		"GOOGLE_REDIRECT_URI=http://localhost:4030/callback/google",
		"SECRET_KEY_BASE=real-key",
	} {
		if !strings.Contains(backend, want) {
			t.Errorf("backend missing %q:\n%s", want, backend)
		}
	}

	mobile := readOutput(t, tree, "mobile/.env")
	for _, want := range []string{
		"host=localhost",
		"port=50081",
		"webBaseUrl=http://localhost:4030",
		"oidcClientId=app-mobile",
		"oidcRedirectUri=app://auth/callback",
	} {
		if !strings.Contains(mobile, want) {
			t.Errorf("mobile missing %q:\n%s", want, mobile)
		}
	}
}

// A secret must never appear in an output that did not name it. This is the
// property that makes it safe to declare a backend credential and a mobile
// output in the same file.
func TestSecretsDoNotLeakBetweenOutputs(t *testing.T) {
	cfg := loadTestConfig(t, fullConfig)
	store := newFakeStore(map[string]string{
		"op://V/I/secret_key_base": "TOP-SECRET",
		"op://V/I/admin_emails":    "a@b.c",
		"op://V/I/shared_id":       "sid",
	})
	tree, _, err := pullInto(t, PullRequest{
		Cfg: cfg, Data: worktreeData(t, cfg, 1, nil), Reader: store,
	})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	mobile := readOutput(t, tree, "mobile/.env")
	for _, leaked := range []string{"TOP-SECRET", "SECRET_KEY_BASE", "ADMIN_EMAILS", "sid"} {
		if strings.Contains(mobile, leaked) {
			t.Fatalf("mobile output leaked %q:\n%s", leaked, mobile)
		}
	}
}

// Two definitions may name one field — one credential under two names — and
// that must cost one round trip, and one authorization prompt.
//
// This deliberately uses two NAMES on one field rather than one name selected
// by two outputs: the latter is collapsed by the `needed` set before any read
// happens, so it would pass with the byURI cache deleted entirely and prove
// nothing about it.
func TestSharedSecretRefIsFetchedOnce(t *testing.T) {
	cfg := loadTestConfig(t, strings.Replace(fullConfig,
		`    SHARED_ID:
      field: shared_id`,
		`    SHARED_ID:
      field: shared_id
    SHARED_ALIAS:
      field: shared_id`, 1))
	// One name in each output — so this exercises the cross-output collection
	// loop the spec names ("references shared by outputs") — plus a second
	// name on the same field, which is the only case the byURI cache can
	// actually be seen by: two names cannot be collapsed by the `needed` set.
	cfg.Outputs[0].Secrets = append(cfg.Outputs[0].Secrets, "SHARED_ALIAS")
	cfg.Outputs[1].Secrets = append(cfg.Outputs[1].Secrets, "SHARED_ID")
	store := newFakeStore(map[string]string{
		"op://V/I/secret_key_base": "k",
		"op://V/I/admin_emails":    "a",
		"op://V/I/shared_id":       "sid",
	})
	tree, _, err := pullInto(t, PullRequest{
		Cfg: cfg, Data: worktreeData(t, cfg, 1, nil), Reader: store,
	})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if n := store.reads["op://V/I/shared_id"]; n != 1 {
		t.Errorf("shared reference read %d times, want 1", n)
	}
	// Both names still land in the file, each carrying the one value.
	if got := readOutput(t, tree, "mobile/.env"); !strings.Contains(got, "SHARED_ID=sid") {
		t.Errorf("the second output did not receive the shared credential:\n%s", got)
	}
	body := readOutput(t, tree, "backend/.env.dev")
	for _, want := range []string{"SHARED_ID=sid", "SHARED_ALIAS=sid"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q:\n%s", want, body)
		}
	}
}

// An explicitly empty default is a real answer — "this provider is simply not
// configured locally" — and must survive as `KEY=`, not vanish.
func TestEmptyDefaultStaysPresent(t *testing.T) {
	cfg := loadTestConfig(t, fullConfig)
	store := newFakeStore(nil)
	store.fail = errors.New("not signed in")
	tree, _, err := pullInto(t, PullRequest{
		Cfg: cfg, Data: worktreeData(t, cfg, 1, nil), Reader: store,
	})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	backend := readOutput(t, tree, "backend/.env.dev")
	if !strings.Contains(backend, "\nADMIN_EMAILS=\n") {
		t.Errorf("empty default should render as ADMIN_EMAILS=, got:\n%s", backend)
	}
	if !strings.Contains(backend, "SECRET_KEY_BASE=dev-only-key") {
		t.Errorf("declared default not used:\n%s", backend)
	}
}

// Falling back must not be silent: a developer who believes Sentry and Google
// sign-in are configured, when wet wrote empty strings, debugs the wrong thing
// for an hour. Names are reported; values never are.
func TestFallbackReportsProvenanceWithoutValues(t *testing.T) {
	cfg := loadTestConfig(t, fullConfig)
	store := newFakeStore(map[string]string{"op://V/I/secret_key_base": "real-key"})
	_, log, err := pullInto(t, PullRequest{
		Cfg: cfg, Data: worktreeData(t, cfg, 1, nil), Reader: store,
	})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if !strings.Contains(log, "1 from 1Password, 2 defaults") {
		t.Errorf("log should count provenance, got:\n%s", log)
	}
	for _, name := range []string{"ADMIN_EMAILS", "SHARED_ID"} {
		if !strings.Contains(log, name) {
			t.Errorf("log should name the fallen-back secret %s, got:\n%s", name, log)
		}
	}
	if strings.Contains(log, "real-key") {
		t.Fatalf("log printed a secret value:\n%s", log)
	}
}

func TestSecretPolicies(t *testing.T) {
	t.Run("required refuses to substitute a default", func(t *testing.T) {
		cfg := loadTestConfig(t, fullConfig)
		store := newFakeStore(nil)
		store.fail = errors.New("not signed in")
		tree, _, err := pullInto(t, PullRequest{
			Cfg: cfg, Data: worktreeData(t, cfg, 1, nil), Reader: store,
			Policy: SecretPolicyRequired,
		})
		if err == nil {
			t.Fatal("required policy accepted a store failure")
		}
		// Which secret fails first is not the point; that the error names a
		// secret and says the policy refused a default is.
		if !strings.Contains(err.Error(), "secret ") || !strings.Contains(err.Error(), SecretPolicyRequired) {
			t.Errorf("err should name a secret and the policy, got: %v", err)
		}
		if _, statErr := os.Stat(filepath.Join(tree, "backend/.env.dev")); !os.IsNotExist(statErr) {
			t.Error("required policy wrote a file before failing")
		}
	})

	t.Run("disabled never contacts the store", func(t *testing.T) {
		cfg := loadTestConfig(t, fullConfig)
		store := newFakeStore(map[string]string{"op://V/I/secret_key_base": "real-key"})
		tree, _, err := pullInto(t, PullRequest{
			Cfg: cfg, Data: worktreeData(t, cfg, 1, nil), Reader: store,
			Policy: SecretPolicyDisabled,
		})
		if err != nil {
			t.Fatalf("pull: %v", err)
		}
		if len(store.reads) != 0 {
			t.Errorf("disabled policy contacted the store: %v", store.reads)
		}
		if got := readOutput(t, tree, "backend/.env.dev"); !strings.Contains(got, "SECRET_KEY_BASE=dev-only-key") {
			t.Errorf("disabled policy should use declared defaults:\n%s", got)
		}
	})

	t.Run("disabled without a default is an error", func(t *testing.T) {
		cfg := loadTestConfig(t, strings.Replace(fullConfig,
			"      field: secret_key_base\n      default: dev-only-key\n",
			"      field: secret_key_base\n", 1))
		_, _, err := pullInto(t, PullRequest{
			Cfg: cfg, Data: worktreeData(t, cfg, 1, nil),
			Policy: SecretPolicyDisabled,
		})
		if err == nil || !strings.Contains(err.Error(), "SECRET_KEY_BASE") {
			t.Fatalf("err = %v, want it to name the undefaultable secret", err)
		}
	})

	t.Run("fallback without a default fails before any write", func(t *testing.T) {
		cfg := loadTestConfig(t, strings.Replace(fullConfig,
			"      field: secret_key_base\n      default: dev-only-key\n",
			"      field: secret_key_base\n", 1))
		store := newFakeStore(nil)
		store.fail = errors.New("not signed in")
		tree, _, err := pullInto(t, PullRequest{
			Cfg: cfg, Data: worktreeData(t, cfg, 1, nil), Reader: store,
		})
		if err == nil {
			t.Fatal("a secret with neither a store value nor a default was accepted")
		}
		if _, statErr := os.Stat(filepath.Join(tree, "mobile/.env")); !os.IsNotExist(statErr) {
			t.Error("an unrelated output was written despite the failure")
		}
	})
}

// A pull is all-or-nothing: an output that cannot be resolved must not leave
// an earlier one already replaced on disk, because the file left behind would
// look current to whatever boots next.
//
// The first output's content must CHANGE between the two runs, or the test
// cannot see the failure it is named for. An earlier version broke the second
// output by editing a value the first one did not carry, so the first file was
// byte-identical either way and "unchanged" was true whether or not it had
// been rewritten — write-as-you-resolve passed it.
func TestPullIsAllOrNothing(t *testing.T) {
	cfg := loadTestConfig(t, fullConfig)
	tree := t.TempDir()
	store := newFakeStore(map[string]string{
		"op://V/I/secret_key_base": "k", "op://V/I/admin_emails": "a", "op://V/I/shared_id": "s",
	})
	if _, _, err := pullInto(t, PullRequest{
		Cfg: cfg, TreePath: tree, Data: worktreeData(t, cfg, 1, nil), Reader: store,
	}); err != nil {
		t.Fatalf("first pull: %v", err)
	}
	before := readOutput(t, tree, "backend/.env.dev")
	if !strings.Contains(before, "AUTH_BASE_URL=http://localhost:4010") {
		t.Fatalf("fixture changed; the first pull did not carry the expected value:\n%s", before)
	}

	broken := worktreeData(t, cfg, 1, nil)
	// backend WOULD be rewritten with a visibly different value...
	broken["AUTH_BASE_URL"] = "http://rewritten:9999"
	// ...while mobile, resolved after it, fails encoding. GRPC_PORT reaches
	// only mobile, so backend resolves cleanly and is the output at risk.
	broken["GRPC_PORT"] = "50061\nINJECTED=1"
	if _, _, err := pullInto(t, PullRequest{
		Cfg: cfg, TreePath: tree, Data: broken, Reader: store,
	}); err == nil {
		t.Fatal("expected the newline-bearing value to be rejected")
	}

	after := readOutput(t, tree, "backend/.env.dev")
	if strings.Contains(after, "rewritten") {
		t.Errorf("backend was written before the later output failed to resolve:\n%s", after)
	}
	if after != before {
		t.Errorf("backend changed despite the failed pull:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// The all-or-nothing promise has to survive an I/O failure, not just a
// resolution failure. Every output resolves cleanly here; the second one
// simply cannot be written, because a regular file sits where its parent
// directory must be. Staging every file before renaming any is what keeps the
// first output out of it — writing as you go leaves a file that looks current
// to whatever boots next while its sibling was never generated.
func TestPullDoesNotWriteAnyOutputWhenALaterWriteFails(t *testing.T) {
	blockMobile := func(t *testing.T, tree string) {
		t.Helper()
		// mobile/.env needs mobile/ to be a directory; a regular file there
		// fails MkdirAll with ENOTDIR on every platform wet runs on. An
		// earlier pull may have created the directory, so clear it first.
		if err := os.RemoveAll(filepath.Join(tree, "mobile")); err != nil {
			t.Fatalf("clear mobile: %v", err)
		}
		if err := os.WriteFile(filepath.Join(tree, "mobile"), []byte("not a directory\n"), 0o644); err != nil {
			t.Fatalf("block mobile: %v", err)
		}
	}
	newStore := func() SecretReader {
		return newFakeStore(map[string]string{
			"op://V/I/secret_key_base": "k", "op://V/I/admin_emails": "a", "op://V/I/shared_id": "s",
		})
	}

	t.Run("first output did not exist", func(t *testing.T) {
		cfg := loadTestConfig(t, fullConfig)
		tree := t.TempDir()
		blockMobile(t, tree)

		_, _, err := pullInto(t, PullRequest{
			Cfg: cfg, TreePath: tree, Data: worktreeData(t, cfg, 1, nil), Reader: newStore(),
		})
		if err == nil {
			t.Fatal("expected the unwritable output to fail the pull")
		}
		if _, statErr := os.Stat(filepath.Join(tree, "backend/.env.dev")); !os.IsNotExist(statErr) {
			t.Errorf("backend was created despite a later output failing to write (stat: %v)", statErr)
		}
		assertNoStagedLeftovers(t, tree)
	})

	t.Run("first output existed with different content", func(t *testing.T) {
		cfg := loadTestConfig(t, fullConfig)
		tree := t.TempDir()
		store := newStore()
		if _, _, err := pullInto(t, PullRequest{
			Cfg: cfg, TreePath: tree, Data: worktreeData(t, cfg, 1, nil), Reader: store,
		}); err != nil {
			t.Fatalf("first pull: %v", err)
		}
		before := readOutput(t, tree, "backend/.env.dev")
		blockMobile(t, tree)

		// Slot 2 strides every port, so backend's content genuinely differs
		// from the previous run: an unchanged file here proves it was not
		// rewritten, rather than merely rewritten with identical bytes.
		changed := worktreeData(t, cfg, 2, nil)
		_, _, err := pullInto(t, PullRequest{
			Cfg: cfg, TreePath: tree, Data: changed, Reader: newStore(),
		})
		if err == nil {
			t.Fatal("expected the unwritable output to fail the pull")
		}
		after := readOutput(t, tree, "backend/.env.dev")
		if after != before {
			t.Errorf("backend was replaced despite a later output failing to write:\nbefore:\n%s\nafter:\n%s", before, after)
		}
		assertNoStagedLeftovers(t, tree)
	})
}

// A failed pull must not leave its staged temp files behind, in a directory
// the user is about to inspect to find out what wet actually generated.
func assertNoStagedLeftovers(t *testing.T, tree string) {
	t.Helper()
	err := filepath.WalkDir(tree, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.Contains(d.Name(), ".wet-") {
			t.Errorf("staged temp file left behind: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", tree, err)
	}
}

// --dry-run resolves and validates exactly what a real pull would, and writes
// nothing — including the parent directory it would have created.
func TestPullDryRunWritesNothing(t *testing.T) {
	cfg := loadTestConfig(t, fullConfig)
	store := newFakeStore(map[string]string{
		"op://V/I/secret_key_base": "k", "op://V/I/admin_emails": "a", "op://V/I/shared_id": "s",
	})
	tree, log, err := pullInto(t, PullRequest{
		Cfg: cfg, Data: worktreeData(t, cfg, 1, nil), Reader: store, DryRun: true,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !strings.Contains(log, "would write backend/.env.dev") {
		t.Errorf("dry run should report what it would write, got:\n%s", log)
	}
	if strings.Contains(log, "\nk\n") || strings.Contains(log, "=k") {
		t.Errorf("dry run disclosed a secret value:\n%s", log)
	}
	for _, dir := range []string{"backend", "mobile"} {
		if _, statErr := os.Stat(filepath.Join(tree, dir)); !os.IsNotExist(statErr) {
			t.Errorf("dry run created %s/", dir)
		}
	}
	// The store still has to be consulted: whether a `required` secret exists
	// is precisely what a dry run is asked to find out.
	if len(store.reads) == 0 {
		t.Error("dry run skipped the store, so it cannot validate a required policy")
	}
}

// Generated files may carry secrets and outlive the command, so the default is
// owner-only — and a declared mode is honored on overwrite too, which
// os.WriteFile alone would not do.
func TestOutputFileModes(t *testing.T) {
	cfg := loadTestConfig(t, strings.Replace(fullConfig,
		"    out: mobile/.env\n    format: dotenv\n",
		"    out: mobile/.env\n    format: dotenv\n    mode: \"0644\"\n", 1))
	store := newFakeStore(map[string]string{
		"op://V/I/secret_key_base": "k", "op://V/I/admin_emails": "a", "op://V/I/shared_id": "s",
	})
	tree := t.TempDir()
	// Pre-create both with the wrong mode: a re-pull must correct it.
	for _, rel := range []string{"backend/.env.dev", "mobile/.env"} {
		full := filepath.Join(tree, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("old"), 0o666); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := pullInto(t, PullRequest{
		Cfg: cfg, TreePath: tree, Data: worktreeData(t, cfg, 1, nil), Reader: store,
	}); err != nil {
		t.Fatalf("pull: %v", err)
	}
	for rel, want := range map[string]os.FileMode{"backend/.env.dev": 0o600, "mobile/.env": 0o644} {
		info, err := os.Stat(filepath.Join(tree, rel))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", rel, got, want)
		}
	}
}

// Emission order is declaration order — include, then secrets, then values —
// so the generated file reads the way the config does and a re-pull produces a
// byte-identical file rather than a spurious diff.
func TestDotenvOrderIsDeterministic(t *testing.T) {
	cfg := loadTestConfig(t, fullConfig)
	store := newFakeStore(map[string]string{
		"op://V/I/secret_key_base": "k", "op://V/I/admin_emails": "a", "op://V/I/shared_id": "s",
	})
	var first string
	for i := 0; i < 3; i++ {
		tree, _, err := pullInto(t, PullRequest{
			Cfg: cfg, Data: worktreeData(t, cfg, 1, nil), Reader: store,
		})
		if err != nil {
			t.Fatalf("pull: %v", err)
		}
		got := readOutput(t, tree, "backend/.env.dev")
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("pull %d differed:\n%s\n---\n%s", i, first, got)
		}
	}
	wantOrder := []string{"PHX_HOST", "AUTH_BASE_URL", "GOOGLE_REDIRECT_URI", "SENTRY_ENV",
		"SECRET_KEY_BASE", "ADMIN_EMAILS", "SHARED_ID"}
	at := -1
	for _, key := range wantOrder {
		i := strings.Index(first, "\n"+key+"=")
		if i < 0 {
			t.Fatalf("%s missing from:\n%s", key, first)
		}
		if i < at {
			t.Errorf("%s is out of declaration order:\n%s", key, first)
		}
		at = i
	}
}

// `.env.local` is the machine override for ordinary values, and it must still
// win over an env: default — that is what lets a checked-in DEV_HOST be a LAN
// IP on one workstation without editing .wet.yaml.
func TestEnvLocalOverridesOrdinaryValues(t *testing.T) {
	cfg := loadTestConfig(t, fullConfig)
	store := newFakeStore(map[string]string{
		"op://V/I/secret_key_base": "k", "op://V/I/admin_emails": "a", "op://V/I/shared_id": "s",
	})
	tree, _, err := pullInto(t, PullRequest{
		Cfg: cfg, Reader: store,
		Data: worktreeData(t, cfg, 1, map[string]string{"DEV_HOST": "192.168.1.5"}),
	})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	// The override has to propagate through the derivation chain, not just
	// land on its own key.
	if got := readOutput(t, tree, "backend/.env.dev"); !strings.Contains(got, "AUTH_BASE_URL=http://192.168.1.5:4010") {
		t.Errorf(".env.local override did not propagate:\n%s", got)
	}
	if got := readOutput(t, tree, "mobile/.env"); !strings.Contains(got, "host=192.168.1.5") {
		t.Errorf(".env.local override missing from mobile:\n%s", got)
	}
}

// `.env.local` may not supply a secret. Secrets enter generated files only
// through the configured store or a declared default, so a name that is a
// secret definition is not an ordinary value and cannot be declared as one.
func TestEnvLocalCannotSupplyASecret(t *testing.T) {
	body := strings.Replace(fullConfig,
		"    include: [PHX_HOST,", "    include: [SECRET_KEY_BASE, PHX_HOST,", 1)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, wetConfigFileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadWetConfigWithLocal(dir, "")
	if err == nil || !strings.Contains(err.Error(), "SECRET_KEY_BASE") {
		t.Fatalf("err = %v, want including a secret as an ordinary value to be rejected", err)
	}
}

// Every rule below is knowable from .wet.yaml alone, so it must fail at load —
// before any store is contacted and long before a file is written.
//
// Each case asserts on a distinctive piece of the message, not merely that an
// error occurred. Two of these rules are defense in depth over a broader check
// that also fires, so "an error happened" passed with the rule deleted — and
// what would have been lost silently is the actionable half of the message.
func TestValidateOutputsRejects(t *testing.T) {
	cases := map[string]struct{ patch, with, want string }{
		"unknown include": {
			"    include: [PHX_HOST,", "    include: [PHX_HSOT,", "unknown value",
		},
		"include names a secret": {
			"    include: [PHX_HOST,", "    include: [SECRET_KEY_BASE, PHX_HOST,",
			"list it under secrets: instead",
		},
		"unknown secret": {
			"    secrets: [SECRET_KEY_BASE,", "    secrets: [SECRET_KEY_BAES,", "unknown secret",
		},
		"unselected secret via values.from": {
			"      host:\n        from: DEV_HOST", "      host:\n        from: SECRET_KEY_BASE",
			"never renamed",
		},
		"unknown values.from": {
			"      host:\n        from: DEV_HOST", "      host:\n        from: NOPE", "unknown value",
		},
		"from and literal together": {
			"      host:\n        from: DEV_HOST", "      host:\n        from: DEV_HOST\n        literal: x",
			"mutually exclusive",
		},
		"neither from nor literal": {
			"      host:\n        from: DEV_HOST", "      host: {}", "needs either",
		},
		"duplicate output key": {
			"    include: [PHX_HOST,", "    include: [SENTRY_ENV, PHX_HOST,", "declared twice",
		},
		"duplicate output name": {
			"  - name: mobile", "  - name: backend", "name already used",
		},
		"duplicate output path": {
			"    out: mobile/.env\n", "    out: backend/.env.dev\n", "already written by",
		},
		"escaping output path": {
			"    out: mobile/.env\n", "    out: ../outside/.env\n", "escape",
		},
		"missing format": {
			"    out: mobile/.env\n    format: dotenv\n", "    out: mobile/.env\n", "format is required",
		},
		"unknown format": {
			"    out: mobile/.env\n    format: dotenv\n", "    out: mobile/.env\n    format: toml\n",
			"is not supported",
		},
		"bad mode": {
			"    out: mobile/.env\n    format: dotenv\n",
			"    out: mobile/.env\n    format: dotenv\n    mode: \"rw-\"\n", "octal file mode",
		},
		"unknown policy": {
			"  policy: fallback", "  policy: maybe", "secrets.policy",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			body := strings.Replace(fullConfig, c.patch, c.with, 1)
			if body == fullConfig {
				t.Fatalf("patch %q did not apply — the fixture changed", c.patch)
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, wetConfigFileName), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := LoadWetConfigWithLocal(dir, "")
			if err == nil {
				t.Fatalf("%s was accepted", name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v\nwant it to contain %q — a different rule fired, so this "+
					"subtest is not testing what it names", err, c.want)
			}
		})
	}
}

// An output with no keys generates a file holding nothing but wet's header,
// which is a config mistake wearing a success message.
func TestValidateOutputsRejectsEmptyOutput(t *testing.T) {
	cfg := &WetConfig{
		ProjectName: "app",
		Outputs:     []OutputConfig{{Name: "x", Out: "x/.env", Format: OutputFormatDotenv}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("an output declaring no keys was accepted")
	}
}

// A secret sharing a name with a port would let a store value shadow an
// allocated port and hand two worktrees the same one; sharing a name with an
// env: entry makes the output key ambiguous.
//
// Each case is a purpose-built config rather than a patch of the shared
// fixture. Patching it made an unrelated rule fire first every time — a
// dangling `secrets:` entry, or the same key claimed by `include:` and
// `secrets:` — so the subcases passed with the rule under test deleted.
func TestSecretNameCollisions(t *testing.T) {
	config := func(secretName string) string {
		return `project_name: app
port_stride: 10
ports:
  - { name: PHX_PORT, base: 4000 }
env:
  SENTRY_ENV: dev
secrets:
  store: op
  vault: V
  item: I
  definitions:
    ` + secretName + `:
      field: f
      default: ''
outputs:
  - name: backend
    out: backend/.env.dev
    format: dotenv
    secrets: [` + secretName + `]
`
	}
	// A name that collides with nothing must be accepted, or the cases below
	// would pass for the wrong reason.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, wetConfigFileName), []byte(config("API_KEY")), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWetConfigWithLocal(dir, ""); err != nil {
		t.Fatalf("control config rejected: %v", err)
	}

	for name, secretName := range map[string]string{
		"port":     "PHX_PORT",
		"env":      "SENTRY_ENV",
		"reserved": "BRANCH",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, wetConfigFileName), []byte(config(secretName)), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := LoadWetConfigWithLocal(dir, "")
			if err == nil {
				t.Fatalf("a secret colliding with a %s name was accepted", name)
			}
			// It must be the collision, not an incidental rule.
			for _, masking := range []string{"unknown secret", "declared twice"} {
				if strings.Contains(err.Error(), masking) {
					t.Fatalf("masked by %q: %v", masking, err)
				}
			}
			if !strings.Contains(err.Error(), secretName) {
				t.Errorf("err should name the colliding secret, got: %v", err)
			}
		})
	}
}

// Silently ignoring the removed keys would be the worst outcome: a config that
// still parses, generates nothing, and leaves a stale .env.dev on disk looking
// current. Both must be a hard error that names the replacement.
func TestRemovedKeysAreRejected(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"render": {`project_name: app
render:
  - template: a.tpl
    out: a
`, "outputs:"},
		"secrets.sections": {`project_name: app
secrets:
  store: op
  sections:
    backend:
      K: f
`, "secrets.definitions:"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, wetConfigFileName), []byte(c.body), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := LoadWetConfigWithLocal(dir, "")
			if err == nil {
				t.Fatalf("%s was accepted", name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to point at %s", err, c.want)
			}
		})
	}
}

// `values:` order is the emission order, and it has to survive the LOADER,
// not just a direct unmarshal. The loader merges .wet.local.yaml over
// .wet.yaml, and doing that through a map[string]any silently alphabetized
// every output key — a file whose order had nothing to do with the config it
// was read from. This goes through LoadWetConfigWithLocal for that reason.
func TestOutputValuesOrderSurvivesTheLoader(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, wetConfigFileName), []byte(fullConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	// An overlay that touches an unrelated key still forces the merge path.
	if err := os.WriteFile(filepath.Join(dir, wetLocalConfigFileName),
		[]byte("env:\n  SENTRY_ENV: local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWetConfigWithLocal(dir, dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	mobile := cfg.Outputs[1]
	want := []string{"host", "port", "webBaseUrl", "oidcClientId", "oidcRedirectUri"}
	if len(mobile.Values.Keys) != len(want) {
		t.Fatalf("Keys = %v, want %v", mobile.Values.Keys, want)
	}
	for i, k := range want {
		if mobile.Values.Keys[i] != k {
			t.Fatalf("Keys = %v, want %v", mobile.Values.Keys, want)
		}
	}
	if cfg.Env["SENTRY_ENV"] != "local" {
		t.Errorf("overlay did not apply: %q", cfg.Env["SENTRY_ENV"])
	}
}

func TestOutputValuesPreserveDeclarationOrder(t *testing.T) {
	var v OutputValues
	if err := yaml.Unmarshal([]byte("zeta:\n  literal: z\nalpha:\n  literal: a\nmid:\n  from: X\n"), &v); err != nil {
		t.Fatal(err)
	}
	want := []string{"zeta", "alpha", "mid"}
	for i, k := range want {
		if v.Keys[i] != k {
			t.Fatalf("Keys = %v, want %v", v.Keys, want)
		}
	}
	if v.By["mid"].From != "X" || *v.By["alpha"].Literal != "a" {
		t.Errorf("decoded wrong: %+v", v.By)
	}
}

// SelectOutputs is what `wet env pull backend` and `wet env push backend` both
// go through, so a typo must name the alternatives rather than quietly doing
// nothing.
func TestSelectOutputs(t *testing.T) {
	cfg := loadTestConfig(t, fullConfig)
	all, err := cfg.SelectOutputs(nil)
	if err != nil || len(all) != 2 {
		t.Fatalf("SelectOutputs(nil) = %d outputs, %v", len(all), err)
	}
	one, err := cfg.SelectOutputs([]string{"mobile"})
	if err != nil || len(one) != 1 || one[0].Name != "mobile" {
		t.Fatalf("SelectOutputs(mobile) = %+v, %v", one, err)
	}
	_, err = cfg.SelectOutputs([]string{"backnd"})
	if err == nil || !strings.Contains(err.Error(), "backend") {
		t.Fatalf("err = %v, want it to list the declared names", err)
	}
}

// `wet setup` owns lifecycle allocation and nothing else about env files: it
// finishes by running the same pull pipeline. The previous design had setup
// render targets itself, and the two paths diverged — only one of them fetched
// a target's secrets, so the file setup wrote and the file a re-render wrote
// were different files. This pins the delegation by observing its result: a
// setup run alone produces every declared output.
func TestSetupGeneratesDeclaredOutputs(t *testing.T) {
	repo := t.TempDir()
	if out, err := execCommand("git", "-C", repo, "init", "-q"); err != nil {
		t.Skipf("git unavailable: %v\n%s", err, out)
	}
	cfg := `project_name: app
port_stride: 10
ports:
  - { name: APP_PORT, base: 41000 }
env:
  DEV_HOST: localhost
  APP_URL: "http://{{.DEV_HOST}}:{{.APP_PORT}}"
outputs:
  - name: backend
    out: backend/.env.dev
    format: dotenv
    include: [APP_URL]
  - name: mobile
    out: mobile/.env
    format: dotenv
    values:
      host:
        from: DEV_HOST
`
	if err := os.WriteFile(filepath.Join(repo, wetConfigFileName), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)

	if err := runSetupCore(repo, repo, "main", true, true); err != nil {
		t.Fatalf("setup: %v", err)
	}
	// The main checkout is slot 0, so it gets the declared base port — and
	// .env.worktree must exist before the outputs, because a post_create hook
	// that boots the stack reads it.
	if _, err := os.Stat(filepath.Join(repo, ".env.worktree")); err != nil {
		t.Fatalf(".env.worktree: %v", err)
	}
	if got := readOutput(t, repo, "backend/.env.dev"); !strings.Contains(got, "APP_URL=http://localhost:41000") {
		t.Errorf("setup did not generate the backend output:\n%s", got)
	}
	if got := readOutput(t, repo, "mobile/.env"); !strings.Contains(got, "host=localhost") {
		t.Errorf("setup did not generate the mobile output:\n%s", got)
	}
}

// A repo that has not named its destination yet — no vault, no item — can
// resolve no reference at all. Under `fallback` that is a store failure like
// any other and the declared defaults must carry the setup, so `wet setup`
// works on a config whose secrets: block is still half-written rather than
// dying on it. (The same path carries a fresh clone that has the committed
// vault but no signed-in `op` account.)
func TestUnnamedDestinationFallsBack(t *testing.T) {
	cfg := loadTestConfig(t, strings.Replace(fullConfig, "  vault: V\n  item: I\n", "", 1))
	tree, log, err := pullInto(t, PullRequest{
		Cfg: cfg, Data: worktreeData(t, cfg, 1, nil), Reader: newFakeStore(nil),
	})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if got := readOutput(t, tree, "backend/.env.dev"); !strings.Contains(got, "SECRET_KEY_BASE=dev-only-key") {
		t.Errorf("declared default not used:\n%s", got)
	}
	if !strings.Contains(log, "3 defaults") {
		t.Errorf("log should report all three as defaults, got:\n%s", log)
	}

	// `required` is the opposite promise, so the same config must fail there.
	if _, _, err := pullInto(t, PullRequest{
		Cfg: cfg, Data: worktreeData(t, cfg, 1, nil), Reader: newFakeStore(nil),
		Policy: SecretPolicyRequired,
	}); err == nil {
		t.Fatal("required policy accepted an unresolvable reference")
	}
}

func TestEnvFileStoreReadsSecretsFromMainBranchEnv(t *testing.T) {
	git := func(dir string, args ...string) {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	base := t.TempDir()
	main := filepath.Join(base, "main")
	worktree := filepath.Join(base, "feature")
	if err := os.MkdirAll(main, 0o755); err != nil {
		t.Fatal(err)
	}
	git(main, "init", "-q", "-b", "main")
	git(main, "config", "user.email", "t@t")
	git(main, "config", "user.name", "t")
	cfgBody := `project_name: app
ports:
  - { name: PHX_PORT, base: 4000 }
secrets:
  store: env_file
  policy: required
  definitions:
    SECRET_KEY_BASE:
      field: secret_key_base
    GOOGLE_CLIENT_ID:
      ref: envfile://GOOGLE_CLIENT_ID
outputs:
  - name: backend
    out: backend/.env.dev
    format: dotenv
    secrets: [SECRET_KEY_BASE, GOOGLE_CLIENT_ID]
`
	if err := os.WriteFile(filepath.Join(main, wetConfigFileName), []byte(cfgBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(main, ".env"), []byte("SECRET_KEY_BASE=from-main\nGOOGLE_CLIENT_ID=google-main\nNOT_SELECTED=ignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(main, "add", wetConfigFileName)
	git(main, "commit", "-qm", "cfg")
	git(main, "worktree", "add", "-q", "-b", "feature/x", worktree, "main")

	cfg, err := LoadWetConfig(worktree)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	_, log, err := pullInto(t, PullRequest{Cfg: cfg, TreePath: worktree, Data: worktreeData(t, cfg, 1, nil)})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	got := readOutput(t, worktree, "backend/.env.dev")
	if !strings.Contains(got, "SECRET_KEY_BASE=from-main") || !strings.Contains(got, "GOOGLE_CLIENT_ID=google-main") {
		t.Fatalf("output did not use main .env secrets:\n%s", got)
	}
	if !strings.Contains(log, "2 from main branch .env") {
		t.Fatalf("provenance did not name env_file store:\n%s", log)
	}
}

// A machine with no `op` installed is exactly what `fallback` exists for: the
// client being unavailable is one of the failures it promises to survive. If
// building the client were fatal, a fresh clone on a laptop without the
// 1Password CLI could not run `wet setup` at all.
func TestFallbackSurvivesAnUnbuildableStoreClient(t *testing.T) {
	cfg := loadTestConfig(t, fullConfig)
	// An empty PATH makes requireOP fail the way a missing CLI does.
	t.Setenv("PATH", t.TempDir())

	tree, log, err := pullInto(t, PullRequest{Cfg: cfg, Data: worktreeData(t, cfg, 1, nil)})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if got := readOutput(t, tree, "backend/.env.dev"); !strings.Contains(got, "SECRET_KEY_BASE=dev-only-key") {
		t.Errorf("declared default not used:\n%s", got)
	}
	if !strings.Contains(log, "3 defaults") {
		t.Errorf("log should report the fallbacks, got:\n%s", log)
	}

	if _, _, err := pullInto(t, PullRequest{
		Cfg: cfg, Data: worktreeData(t, cfg, 1, nil), Policy: SecretPolicyRequired,
	}); err == nil {
		t.Fatal("required policy accepted a missing store client")
	}
}

// The encoder must be checked against a reader that is NOT wet's own.
//
// quoteEnvValue and unquoteEnvValue are inverses by construction, so a
// round trip through ParseEnvFile cannot fail for any value — it proves
// nothing about the consumers these files are written for. `sh` is the
// reader every one of them ultimately agrees with: docker compose's
// `env_file:`, a `set -a; . ./.env`, and the dotenv libraries all model
// themselves on it.
func TestEncodeDotenvIsReadableByTheShell(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	pairs := []EnvPair{
		{"PLAIN", "abc123"},
		{"URL", "http://localhost:4010/callback?a=b&c=d"},
		{"SPACED", "two words"},
		{"DOLLAR", "$NOT_EXPANDED"},
		{"BACKTICK", "a`whoami`b"},
		{"HASH", "no # comment"},
		{"QUOTE", `a"b'c`},
		{"BACKSLASH", `a\b`},
		{"EMPTY", ""},
		{"EQUALS", "k=v"},
		{"BASE64", "LS0tLS1CRUdJTiBLRVktLS0tLQo="},
	}
	body, err := EncodeDotenv(pairs)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// `set -a; . file` is exactly what a compose stack and a `just` recipe do.
	// printf %s\0 keeps values with newlines or trailing space intact.
	var script strings.Builder
	script.WriteString("set -a; . '" + envPath + "'; set +a\n")
	for _, p := range pairs {
		script.WriteString("printf '%s\\0' \"$" + p.Key + "\"\n")
	}
	out, err := exec.Command("sh", "-c", script.String()).Output()
	if err != nil {
		t.Fatalf("sh could not read the generated file: %v\n%s", err, body)
	}
	got := strings.Split(string(out), "\x00")
	for i, p := range pairs {
		if i >= len(got) {
			t.Fatalf("sh produced %d values, want %d:\n%s", len(got), len(pairs), body)
		}
		if got[i] != p.Value {
			t.Errorf("%s read back as %q, want %q\nfile:\n%s", p.Key, got[i], p.Value, body)
		}
	}
}

// `wet env explain` is a spec-mandated command whose whole contract is that it
// shows names and provenance and never a value.
func TestEnvExplainShowsProvenanceNeverValues(t *testing.T) {
	repo := t.TempDir()
	if out, err := execCommand("git", "-C", repo, "init", "-q"); err != nil {
		t.Skipf("git unavailable: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(repo, wetConfigFileName), []byte(fullConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	// A .env.local holding a real-looking value must not surface either.
	if err := os.WriteFile(filepath.Join(repo, envLocalFileName), []byte("DEV_HOST=10.0.0.5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)

	out := captureStdout(t, func() {
		if err := runEnvExplain(nil, nil); err != nil {
			t.Fatalf("explain: %v", err)
		}
	})

	for _, want := range []string{
		"backend — backend/.env.dev (dotenv)",
		"SECRET_KEY_BASE -> V/I/secret_key_base (has default)",
		"port = from GRPC_PORT (allocated port)",
		"AUTH_BASE_URL = env:",
		"oidcClientId = literal",
		"secrets: 0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("explain output missing %q:\n%s", want, out)
		}
	}
	// The declared defaults ARE values. explain reports that a default exists,
	// never what it is.
	for _, leaked := range []string{"dev-only-key", "app-mobile", "10.0.0.5", "{{"} {
		if strings.Contains(out, leaked) {
			t.Errorf("explain disclosed %q:\n%s", leaked, out)
		}
	}
}

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		_, _ = io.Copy(&b, r)
		done <- b.String()
	}()
	fn()
	os.Stdout = orig
	_ = w.Close()
	return <-done
}

// `.env.local` overrides ordinary values, but an allocated port is not one.
// Letting it through defeats the striding wet exists for: one DB_PORT line
// and every worktree generates the same port, while .env.worktree still
// carries the allocated one — two generated files disagreeing with the state
// file, and a bind conflict nobody traces back to a one-line override.
func TestEnvLocalCannotOverrideAPortOrReservedField(t *testing.T) {
	cfg := loadTestConfig(t, fullConfig)
	for name, local := range map[string]map[string]string{
		"port":     {"PHX_PORT": "9999"},
		"reserved": {"Branch": "somewhere-else"},
	} {
		t.Run(name, func(t *testing.T) {
			data := BuildTemplateData(cfg, "feature/x", 1, cfg.ResolvePorts(1), nil)
			_, err := ResolveEnvValues(cfg, data, local)
			if err == nil {
				t.Fatalf("%s override was accepted", name)
			}
			if !strings.Contains(err.Error(), envLocalFileName) {
				t.Errorf("err = %v, want it to name the file the override came from", err)
			}
		})
	}
}

// A key is written raw into the generated file, so an unchecked one can
// inject a whole extra assignment or change the value the reader sees. Both
// shapes below validate clean without the check and produce a broken file.
func TestOutputKeyShapeIsValidated(t *testing.T) {
	cases := map[string]string{
		"newline in a values key": "      host:\n        from: DEV_HOST",
		"equals in a values key":  "      host:\n        from: DEV_HOST",
	}
	replacements := map[string]string{
		"newline in a values key": "      \"host\\nEVIL\":\n        from: DEV_HOST",
		"equals in a values key":  "      \"a=b\":\n        from: DEV_HOST",
	}
	for name, patch := range cases {
		t.Run(name, func(t *testing.T) {
			body := strings.Replace(fullConfig, patch, replacements[name], 1)
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, wetConfigFileName), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadWetConfigWithLocal(dir, ""); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}

	// An env: key can be `include:`d under its own name, so it answers to the
	// same shape.
	dir := t.TempDir()
	body := strings.Replace(fullConfig, "  SENTRY_ENV: dev", "  \"SENTRY ENV\": dev", 1)
	if err := os.WriteFile(filepath.Join(dir, wetConfigFileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWetConfigWithLocal(dir, ""); err == nil {
		t.Fatal("an env: key that is not a valid dotenv key was accepted")
	}
}

// An anchored block that an overlay overrides must merge into a COPY: merging
// into the anchor itself would edit every other alias to it, so overriding one
// block's vault would silently change an unrelated block's too.
func TestLocalOverlayMergesIntoAnAliasedBlockWithoutSharing(t *testing.T) {
	dir := t.TempDir()
	// `defaults` aliases the same anchor, so if the merge mutates the anchor
	// rather than a copy, the override leaks into it.
	base := `project_name: app
defaults: &shared
  store: op
  vault: BASE
  definitions:
    K: { field: k, default: '' }
secrets: *shared
outputs:
  - name: backend
    out: backend/.env.dev
    format: dotenv
    secrets: [K]
`
	if err := os.WriteFile(filepath.Join(dir, wetConfigFileName), []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, wetLocalConfigFileName),
		[]byte("secrets:\n  vault: MINE\n  item: I\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWetConfigWithLocal(dir, dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// The override applied...
	if cfg.Secrets.Vault != "MINE" || cfg.Secrets.Item != "I" {
		t.Errorf("overlay did not merge: vault=%q item=%q", cfg.Secrets.Vault, cfg.Secrets.Item)
	}
	// ...the rest of the anchored block survived rather than being replaced
	// wholesale by the two keys the overlay named...
	if _, ok := cfg.Secrets.Definitions["K"]; !ok {
		t.Errorf("overlay discarded the anchored definitions: %+v", cfg.Secrets.Definitions)
	}
	if cfg.Secrets.Store != secretStoreProviderOnePassword {
		t.Errorf("overlay discarded store: %q", cfg.Secrets.Store)
	}

	// ...and the anchor itself is untouched, so nothing else aliasing it moved.
	var raw struct {
		Defaults struct {
			Vault string `yaml:"vault"`
			Item  string `yaml:"item"`
		} `yaml:"defaults"`
	}
	doc := mergedDocFor(t, dir)
	if err := doc.Decode(&raw); err != nil {
		t.Fatalf("decode merged doc: %v", err)
	}
	if raw.Defaults.Vault != "BASE" || raw.Defaults.Item != "" {
		t.Errorf("the overlay leaked into the shared anchor: %+v", raw.Defaults)
	}
}

// mergedDocFor re-runs the loader's merge and hands back the node tree, so a
// test can inspect a key the WetConfig struct does not model.
func mergedDocFor(t *testing.T, dir string) *yaml.Node {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, wetConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	root := documentRoot(&doc)
	if root == nil {
		t.Fatal("no mapping at the top level")
	}
	if _, err := mergeLocalConfig(root, filepath.Join(dir, wetLocalConfigFileName)); err != nil {
		t.Fatal(err)
	}
	return root
}

// The other reader these files are written for is docker compose's
// `env_file:`, which is NOT sh — it is Go, with its own quoting rules. The
// design asks for compatibility with the actual readers in use rather than an
// assumption that every dialect agrees, and this is the half that `sh` cannot
// speak for. Skipped where docker is unavailable; it must not be the only
// coverage of the encoder, and it is not.
func TestEncodeDotenvIsReadableByDockerCompose(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	pairs := []EnvPair{
		{"PLAIN", "plain"},
		{"URL", "http://h/cb?a=1&b=2"},
		{"SPACED", "a b # c"},
		{"EQ", "k=v"},
		{"EMPTY", ""},
		{"QUOTE", `a"b'c`},
	}
	body, err := EncodeDotenv(pairs)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"),
		[]byte("services:\n  x:\n    image: alpine\n    env_file: [.env]\n    command: [\"true\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("docker", "compose", "config", "--format", "json")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("docker compose unavailable or not running: %v", err)
	}
	var parsed struct {
		Services map[string]struct {
			Environment map[string]string `json:"environment"`
		} `json:"services"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("parse compose config: %v", err)
	}
	got := parsed.Services["x"].Environment
	for _, p := range pairs {
		if got[p.Key] != p.Value {
			t.Errorf("compose read %s as %q, want %q\nfile:\n%s", p.Key, got[p.Key], p.Value, body)
		}
	}
}

// `.env.worktree` is SOURCED — `set -a; . ./.env.worktree` in a recipe, and
// `docker compose --env-file` — so it needs the same quoting the generated
// outputs get. It was written raw, and an `&` in a derived URL forked a
// background job and handed the value back EMPTY.
func TestEnvWorktreeIsReadableByTheShell(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	cfg := &WetConfig{
		ProjectName: "app",
		PortStride:  10,
		Ports:       []PortConfig{{Name: "APP_PORT", Base: 4000, Stride: 10}},
		Env: map[string]string{
			"CALLBACK": "http://localhost:{{.APP_PORT}}/cb?a=1&b=2",
			"SPACED":   "a b # c",
			"PLAIN":    "plain",
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	ports := cfg.ResolvePorts(1)
	data := BuildTemplateData(cfg, "feature/x", 1, ports, nil)
	body, err := RenderEnvFileContent(cfg, data, ports, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// A port is an integer and must stay bare: compose interpolates
	// ${APP_PORT} into a port mapping, where '4010' is not a valid port.
	if !strings.Contains(body, "\nAPP_PORT=4010\n") {
		t.Errorf("port should be written bare, got:\n%s", body)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, ".env.worktree")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sh", "-c",
		"set -a; . '"+path+"'; set +a; printf '%s\\0' \"$CALLBACK\" \"$SPACED\" \"$PLAIN\" \"$APP_PORT\"").Output()
	if err != nil {
		t.Fatalf("sh could not read .env.worktree: %v\n%s", err, body)
	}
	got := strings.Split(string(out), "\x00")
	want := []string{"http://localhost:4010/cb?a=1&b=2", "a b # c", "plain", "4010"}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("value %d read back as %q, want %q\nfile:\n%s", i, got[i], w, body)
		}
	}
}

// A secret set in `.env.local` is silently ineffective where it was meant to
// act — outputs read secrets from the store, never from the value set — while
// still landing in .env.worktree, which is 0644 and documented as carrying no
// secrets. Both halves are wrong, so it is refused by name.
func TestEnvLocalCannotSupplyASecretAtResolveTime(t *testing.T) {
	cfg := loadTestConfig(t, fullConfig)
	data := BuildTemplateData(cfg, "feature/x", 1, cfg.ResolvePorts(1), nil)
	_, err := ResolveEnvValues(cfg, data, map[string]string{"SECRET_KEY_BASE": "my-real-key"})
	if err == nil {
		t.Fatal("a secret set in .env.local was accepted")
	}
	if !strings.Contains(err.Error(), "SECRET_KEY_BASE") || !strings.Contains(err.Error(), "wet env push") {
		t.Errorf("err = %v, want it to name the key and say where the value belongs", err)
	}
}

// A one-character typo must not make a pull overwrite the config that produced
// it, or the allocation the worktree depends on.
func TestOutputCannotWriteWetsOwnFiles(t *testing.T) {
	for _, path := range []string{".wet.yaml", ".wet.local.yaml", ".env.local", ".env.worktree", ".wet-worktrees.json", "./.env.worktree"} {
		t.Run(path, func(t *testing.T) {
			cfg := &WetConfig{
				ProjectName: "app",
				Outputs: []OutputConfig{{
					Name: "x", Out: path, Format: OutputFormatDotenv, Include: []string{"Branch"},
				}},
			}
			if err := cfg.Validate(); err == nil {
				t.Fatalf("an output writing %s was accepted", path)
			}
		})
	}
}

// Pools and Handles are maps, not values. Including one used to validate
// clean and then fail at pull time with "is not a scalar", breaking the
// promise the rest of validateOutputs makes: everything knowable from
// .wet.yaml fails at load.
func TestIncludingAPoolContainerIsRejectedAtLoad(t *testing.T) {
	for _, name := range []string{"Pools", "Handles"} {
		cfg := &WetConfig{
			ProjectName: "app",
			Pools:       map[string][]string{"devices": {"a"}},
			Outputs: []OutputConfig{{
				Name: "x", Out: "x/.env", Format: OutputFormatDotenv, Include: []string{name},
			}},
		}
		if err := cfg.Validate(); err == nil {
			t.Errorf("include: %s was accepted", name)
		}
	}
	// A member of one is still reachable through a rename, which is the
	// supported way to consume a pool.
	cfg := &WetConfig{
		ProjectName: "app",
		Pools:       map[string][]string{"devices": {"a"}},
		Outputs: []OutputConfig{{
			Name: "x", Out: "x/.env", Format: OutputFormatDotenv,
			Values: OutputValues{Keys: []string{"device"}, By: map[string]OutputValue{
				"device": {From: "Pools.devices"},
			}},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("values.from Pools.devices should be allowed: %v", err)
	}
}

// The mirror of the alias case, and the more common way people write YAML:
// the anchor sits on the key being overridden and the aliases are elsewhere.
// Merging in place edits the node every alias points at, so a machine-local
// override silently rewrites a block it never named — on that workstation only.
func TestLocalOverlayDoesNotMutateAnAnchorOthersAlias(t *testing.T) {
	dir := t.TempDir()
	base := `project_name: app
secrets: &shared
  store: op
  vault: BASE
  definitions:
    A: &def { field: shared_field, default: dev }
    B: *def
mirror: *shared
outputs:
  - name: backend
    out: backend/.env.dev
    format: dotenv
    secrets: [A, B]
`
	if err := os.WriteFile(filepath.Join(dir, wetConfigFileName), []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, wetLocalConfigFileName),
		[]byte("secrets:\n  vault: MINE\n  item: I\n  definitions:\n    A:\n      field: a_only\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWetConfigWithLocal(dir, dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Secrets.Vault != "MINE" {
		t.Errorf("overlay did not apply: vault=%q", cfg.Secrets.Vault)
	}
	// B aliases the same definition A does. Overriding A must not move B, or
	// this machine reads the wrong 1Password field for B and no other does.
	if got := cfg.Secrets.Definitions["B"].Field; got != "shared_field" {
		t.Errorf("overriding A moved B: B.field = %q, want shared_field", got)
	}
	if got := cfg.Secrets.Definitions["A"].Field; got != "a_only" {
		t.Errorf("A.field = %q, want a_only", got)
	}

	// And the unrelated key aliasing the whole block did not move either.
	var raw struct {
		Mirror struct {
			Vault string `yaml:"vault"`
			Item  string `yaml:"item"`
		} `yaml:"mirror"`
	}
	if err := mergedDocFor(t, dir).Decode(&raw); err != nil {
		t.Fatalf("decode merged doc: %v", err)
	}
	if raw.Mirror.Vault != "BASE" || raw.Mirror.Item != "" {
		t.Errorf("the overlay leaked through the anchor into `mirror`: %+v", raw.Mirror)
	}
}

// A `render:` line in the gitignored overlay must not be blamed on the
// committed config — that sends the reader looking in the wrong file.
func TestRemovedKeyInTheOverlayIsBlamedOnTheOverlay(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, wetConfigFileName), []byte("project_name: app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, wetLocalConfigFileName),
		[]byte("render:\n  - template: a.tpl\n    out: a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadWetConfigWithLocal(dir, dir)
	if err == nil {
		t.Fatal("a removed key in the overlay was accepted")
	}
	if !strings.Contains(err.Error(), wetLocalConfigFileName) {
		t.Errorf("err = %v, want it to name %s", err, wetLocalConfigFileName)
	}
}

// A file carrying credentials at a mode readable beyond its owner is a
// decision, and a silent one reads as the default. It is reported through the
// run's own output stream, not os.Stderr, so a caller capturing the narrative
// sees it in order.
func TestSecretBearingOutputWarnsOnAWidenedMode(t *testing.T) {
	cfg := loadTestConfig(t, strings.Replace(fullConfig,
		"    out: backend/.env.dev\n    format: dotenv\n",
		"    out: backend/.env.dev\n    format: dotenv\n    mode: \"0644\"\n", 1))

	_, log, err := pullInto(t, PullRequest{
		Cfg: cfg, Data: worktreeData(t, cfg, 1, nil), Policy: SecretPolicyDisabled,
	})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if !strings.Contains(log, "backend/.env.dev") || !strings.Contains(log, "0644") {
		t.Errorf("no warning for a secret-bearing 0644 output, got:\n%s", log)
	}
	// The mobile output selects no secrets, so widening it is nobody's
	// business and must not produce noise.
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "warning") && strings.Contains(line, "mobile") {
			t.Errorf("warned about an output that carries no secrets: %q", line)
		}
	}
}

// `.env.local` is gitignored and one per machine, so — exactly like
// `.wet.local.yaml` — it lives beside the checkout holding the trunk, which is
// NOT reliably git's main working tree.
//
// `wet setup` resolved it through LocalConfigDir while `wet env pull` passed
// git's main working tree straight in, so a pull silently reverted every
// override setup had just applied. That is the two-paths-disagree failure the
// shared pipeline exists to prevent, so it is pinned in the three-checkout
// shape where the two directories actually differ.
func TestSetupAndPullReadTheSameEnvLocal(t *testing.T) {
	git := func(dir string, args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Skipf("git %v: %v\n%s", args, err, out)
		}
	}
	base := t.TempDir()
	stale := filepath.Join(base, "stale") // git's main working tree, on "main"
	trunk := filepath.Join(base, "trunk") // the checkout on "development"
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	git(stale, "init", "-q", "-b", "main")
	git(stale, "config", "user.email", "t@t")
	git(stale, "config", "user.name", "t")
	cfg := `project_name: demo
main_branch: development
port_stride: 10
ports:
  - { name: APP_PORT, base: 41000 }
env:
  DEV_HOST: localhost
  APP_URL: "http://{{.DEV_HOST}}:{{.APP_PORT}}"
outputs:
  - name: backend
    out: backend/.env.dev
    format: dotenv
    include: [APP_URL]
`
	if err := os.WriteFile(filepath.Join(stale, ".wet.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	git(stale, "add", ".wet.yaml")
	git(stale, "commit", "-qm", "cfg")
	git(stale, "branch", "development")
	git(stale, "worktree", "add", "-q", trunk, "development")

	// The override exists ONLY beside the trunk, which is how a gitignored
	// per-machine file actually sits on disk.
	if err := os.WriteFile(filepath.Join(trunk, envLocalFileName),
		[]byte("DEV_HOST=192.168.1.5\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Chdir(trunk)
	if err := runSetupCore(stale, trunk, "development", true, false); err != nil {
		t.Fatalf("setup: %v", err)
	}
	afterSetup := readOutput(t, trunk, "backend/.env.dev")
	if !strings.Contains(afterSetup, "APP_URL=http://192.168.1.5:") {
		t.Fatalf("setup did not apply the trunk's .env.local:\n%s", afterSetup)
	}

	resetEnvFlags(t)
	if err := runEnvPull(nil, nil); err != nil {
		t.Fatalf("pull: %v", err)
	}
	if afterPull := readOutput(t, trunk, "backend/.env.dev"); afterPull != afterSetup {
		t.Errorf("pull did not reproduce what setup wrote:\nsetup:\n%s\npull:\n%s", afterSetup, afterPull)
	}
}

// The rules below are each named by the design's validation list. They were
// correct in the code but unguarded, so a regression would have been silent.
func TestSecretDefinitionValidation(t *testing.T) {
	base := `project_name: app
port_stride: 10
ports:
  - { name: PHX_PORT, base: 4000 }
secrets:
  store: op
  vault: V
  item: I
  definitions:
    API_KEY:
%s
outputs:
  - name: backend
    out: backend/.env.dev
    format: dotenv
    secrets: [API_KEY]
`
	cases := map[string]struct {
		def  string
		want string
	}{
		"ref and field together": {
			"      ref: op://v/i/f\n      field: f\n", "ref:",
		},
		"ref and vault together": {
			"      ref: op://v/i/f\n      vault: other\n", "ref:",
		},
		"malformed ref": {
			"      ref: op://v/i\n", "op://vault/item/field",
		},
		"no field and no ref": {
			"      default: ''\n", "field:",
		},
		"slash in field": {
			"      field: Other Item/f\n", "bare field name",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			body := fmt.Sprintf(base, c.def)
			if err := os.WriteFile(filepath.Join(dir, wetConfigFileName), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := LoadWetConfigWithLocal(dir, "")
			if err == nil {
				t.Fatalf("%s was accepted", name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to mention %q", err, c.want)
			}
		})
	}

	// A ref: on its own is the supported way to point one secret somewhere
	// else entirely, so it must still be accepted.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, wetConfigFileName),
		[]byte(fmt.Sprintf(base, "      ref: op://other/item/field\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWetConfigWithLocal(dir, "")
	if err != nil {
		t.Fatalf("a standalone ref: was rejected: %v", err)
	}
	ref, err := cfg.Secrets.ResolveSecretRef("API_KEY", "")
	if err != nil {
		t.Fatal(err)
	}
	if ref.URI() != "op://other/item/field" {
		t.Errorf("ref resolved to %s", ref.URI())
	}
}

// The secrets: block's own shape, and the store name.
func TestSecretsBlockValidation(t *testing.T) {
	cases := map[string]string{
		"unknown store": `project_name: app
secrets:
  store: vault
  definitions:
    K: { field: k }
`,
		"no definitions": `project_name: app
secrets:
  store: op
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, wetConfigFileName), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadWetConfigWithLocal(dir, ""); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}

// `name:` is the CLI identifier, and `mode:` is a file mode. Neither was
// guarded, so a bad one would have shipped.
func TestOutputNameAndModeValidation(t *testing.T) {
	for name, patch := range map[string]struct{ from, to string }{
		"uppercase name":  {"  - name: mobile", "  - name: Mobile"},
		"name with slash": {"  - name: mobile", "  - name: mo/bile"},
		"empty name":      {"  - name: mobile", "  - name: \"\""},
		"mode too large":  {"    out: mobile/.env\n", "    out: mobile/.env\n    mode: \"7777\"\n"},
		"mode not octal":  {"    out: mobile/.env\n", "    out: mobile/.env\n    mode: \"0899\"\n"},
	} {
		t.Run(name, func(t *testing.T) {
			body := strings.Replace(fullConfig, patch.from, patch.to, 1)
			if body == fullConfig {
				t.Fatalf("patch %q did not apply — the fixture changed", patch.from)
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, wetConfigFileName), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadWetConfigWithLocal(dir, ""); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}

// A secret nothing selects is inert, and the spec asks for it to be reported.
//
// The spec's companion diagnostic — `env:` values nothing reads — is
// deliberately not implemented, and this pins that decision: every `env:`
// value reaches `.env.worktree`, which docker compose consumes, so reporting
// one as unused would tell someone to delete a value their stack depends on.
func TestUnusedSecretsAreReported(t *testing.T) {
	cfg := loadTestConfig(t, `project_name: app
port_stride: 10
ports:
  - { name: APP_PORT, base: 4000 }
env:
  COMPOSE_PROJECT_NAME: "{{.ProjectName}}-{{.BranchSafe}}"
  BASE_URL: "http://localhost:{{.APP_PORT}}"
secrets:
  store: op
  vault: V
  item: I
  definitions:
    SELECTED:
      field: sel
      default: ''
    USED_BY_HOOK:
      field: hooked
      default: ''
    NEVER_SELECTED:
      field: never
      default: ''
hooks:
  post_create:
    - "echo $USED_BY_HOOK"
outputs:
  - name: backend
    out: backend/.env.dev
    format: dotenv
    include: [BASE_URL]
    secrets: [SELECTED]
`)
	got := cfg.UnusedSecrets()
	if len(got) != 1 || got[0] != "NEVER_SELECTED" {
		t.Fatalf("unused = %v, want [NEVER_SELECTED]", got)
	}

	var out strings.Builder
	reportUnusedSecrets(&out, cfg)
	if !strings.Contains(out.String(), "NEVER_SELECTED") {
		t.Errorf("report missing the unused secret:\n%s", out.String())
	}
	// COMPOSE_PROJECT_NAME is included by no output and named by no hook, and
	// must still never be reported: it reaches compose through .env.worktree.
	for _, unwanted := range []string{"COMPOSE_PROJECT_NAME", "USED_BY_HOOK", "BASE_URL"} {
		if strings.Contains(out.String(), unwanted) {
			t.Errorf("report named %q, which is consumed:\n%s", unwanted, out.String())
		}
	}

	// A config where every definition is selected says nothing at all.
	var quiet strings.Builder
	reportUnusedSecrets(&quiet, loadTestConfig(t, fullConfig))
	if quiet.Len() != 0 {
		t.Errorf("a fully-consumed config produced a diagnostic:\n%s", quiet.String())
	}
}

// The vault belongs in the committed config, but the overlay may override it
// — and on a machine whose store spells it differently, that override is the
// only thing standing between the repo and a silent fallback: get the merge
// wrong and every secret falls back to its default: under `policy: fallback`,
// with a generated file that looks configured. The assertion that matters is
// end-to-end — not that the keys parsed, but that a reference actually
// RESOLVES to them afterwards, which is the thing the store call needs.
func TestLocalOverlayOverridesTheVaultAndResolvesReferences(t *testing.T) {
	repo := t.TempDir()
	// The committed config names a vault and an item, as it should. The
	// overlay adds the machine's account and overrides the vault — the shape
	// this test exists for.
	if err := os.WriteFile(filepath.Join(repo, wetConfigFileName), []byte(fullConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, wetLocalConfigFileName),
		[]byte("secrets:\n  account: my.1password.com\n  vault: Development\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadWetConfigWithLocal(repo, repo)
	if err != nil {
		t.Fatalf("load with overlay: %v", err)
	}
	if cfg.Secrets.Vault != "Development" || cfg.Secrets.Account != "my.1password.com" {
		t.Errorf("overlay did not reach the loader: account=%q vault=%q",
			cfg.Secrets.Account, cfg.Secrets.Vault)
	}
	// The committed half has to survive the merge intact. A count alone would
	// not catch a merge that kept the mapping but emptied a definition, and
	// the field name is what the store call is actually built from.
	if def, ok := cfg.Secrets.Definitions["SECRET_KEY_BASE"]; !ok || def.Field != "secret_key_base" {
		t.Fatalf("overlay damaged the committed definitions: %+v", cfg.Secrets.Definitions)
	}
	// The point of the override: references resolve to the machine's vault,
	// while the item stays the committed one nothing overrode.
	ref, err := cfg.Secrets.ResolveSecretRef("SECRET_KEY_BASE", "")
	if err != nil {
		t.Fatalf("reference unresolvable after the override: %v", err)
	}
	if ref.Vault != "Development" || ref.Item != "I" {
		t.Errorf("resolved to %s/%s/%s", ref.Vault, ref.Item, ref.Field)
	}
}

// `wet env explain` is the command you run to find out WHY nothing resolves,
// so a missing destination must be reported as the gap it is rather than
// failing the command — and it must name the RIGHT gap. A missing item and a
// missing vault are different keys with different fixes, so one summary
// message for both sends half the readers to edit the wrong line.
func TestEnvExplainReportsAMissingDestination(t *testing.T) {
	for _, tc := range []struct {
		name, strip, wantKey, wantFile string
		alsoWant                       []string
	}{
		{"no vault", "  vault: V\n", "secrets.vault", wetConfigFileName,
			[]string{"vault: Development", "secrets.vaults:", "secrets.definitions.SECRET_KEY_BASE.vault"}},
		{"no item", "  item: I\n", "secrets.item", wetConfigFileName, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			if out, err := execCommand("git", "-C", repo, "init", "-q"); err != nil {
				t.Skipf("git unavailable: %v\n%s", err, out)
			}
			stripped := strings.Replace(fullConfig, tc.strip, "", 1)
			if stripped == fullConfig {
				t.Fatalf("fixture no longer contains %q", tc.strip)
			}
			if err := os.WriteFile(filepath.Join(repo, wetConfigFileName), []byte(stripped), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Chdir(repo)

			out := captureStdout(t, func() {
				if err := runEnvExplain(nil, nil); err != nil {
					t.Fatalf("explain must report the gap, not fail on it: %v", err)
				}
			})
			if !strings.Contains(out, "no destination:") {
				t.Fatalf("explain did not report the gap:\n%s", out)
			}
			if !strings.Contains(out, tc.wantKey) || !strings.Contains(out, tc.wantFile) {
				t.Errorf("explain should send the reader to %s in %s:\n%s", tc.wantKey, tc.wantFile, out)
			}
			for _, want := range tc.alsoWant {
				if !strings.Contains(out, want) {
					t.Errorf("explain should include %q:\n%s", want, out)
				}
			}
		})
	}
}

// onDemandConfig adds a deploy output to the worked example: the file that is
// not this checkout's at all, whose credentials belong to a running system.
const onDemandConfig = fullConfig + `  - name: deploy
    out: deploy/.env.staging
    format: dotenv
    on_demand: true
    values:
      PHX_HOST:
        literal: app.example.com
    secrets: [SECRET_KEY_BASE]
`

// The reason on_demand: exists. `wet setup` and `wet worktree create` pull
// with no selection, so without the filter every new worktree would fetch a
// deploy environment's credentials and write them to a developer's disk —
// which is strictly worse than the checked-in injection template the flag was
// added to replace.
func TestPullSweepSkipsOnDemandOutputs(t *testing.T) {
	cfg := loadTestConfig(t, onDemandConfig)
	store := newFakeStore(map[string]string{
		"op://V/I/secret_key_base": "real-key",
		"op://V/I/admin_emails":    "a@b.c",
		"op://V/I/shared_id":       "sid",
	})
	tree, _, err := pullInto(t, PullRequest{
		Cfg: cfg, Data: worktreeData(t, cfg, 3, nil), Reader: store,
	})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	readOutput(t, tree, "backend/.env.dev")
	if _, err := os.Stat(filepath.Join(tree, "deploy/.env.staging")); !os.IsNotExist(err) {
		t.Fatalf("a bare pull wrote the on_demand output (stat err = %v)", err)
	}
}

// Withheld from the sweep, not made unreachable: naming it is how it is
// generated, and that is the whole interface a deploy recipe has.
func TestPullByNameGeneratesOnDemandOutput(t *testing.T) {
	cfg := loadTestConfig(t, onDemandConfig)
	store := newFakeStore(map[string]string{"op://V/I/secret_key_base": "real-key"})
	tree, _, err := pullInto(t, PullRequest{
		Cfg: cfg, Data: worktreeData(t, cfg, 3, nil), Reader: store, Only: []string{"deploy"},
	})
	if err != nil {
		t.Fatalf("pull deploy: %v", err)
	}
	body := readOutput(t, tree, "deploy/.env.staging")
	for _, want := range []string{"PHX_HOST=app.example.com", "SECRET_KEY_BASE=real-key"} {
		if !strings.Contains(body, want) {
			t.Errorf("deploy output missing %q:\n%s", want, body)
		}
	}
}

// The stream is the point: a deploy environment's credentials go from the
// store into whatever consumes them without ever existing as a file.
func TestPullToStreamWritesNoFile(t *testing.T) {
	cfg := loadTestConfig(t, onDemandConfig)
	store := newFakeStore(map[string]string{"op://V/I/secret_key_base": "real-key"})
	var stream strings.Builder
	tree, log, err := pullInto(t, PullRequest{
		Cfg: cfg, Data: worktreeData(t, cfg, 3, nil), Reader: store,
		Only: []string{"deploy"}, Content: &stream,
	})
	if err != nil {
		t.Fatalf("pull deploy --stdout: %v", err)
	}
	if !strings.Contains(stream.String(), "SECRET_KEY_BASE=real-key") {
		t.Errorf("stream missing the resolved secret:\n%s", stream.String())
	}
	if _, err := os.Stat(filepath.Join(tree, "deploy/.env.staging")); !os.IsNotExist(err) {
		t.Fatalf("--stdout wrote a file anyway (stat err = %v)", err)
	}
	// Provenance belongs on the caller's log writer, never in the stream: a
	// "Wrote …" line inside a dotenv is a syntax error to whatever reads it.
	if strings.Contains(stream.String(), "Wrote") {
		t.Errorf("narration leaked into the stream:\n%s", stream.String())
	}
	if !strings.Contains(log, "Wrote deploy") {
		t.Errorf("log did not report the write:\n%s", log)
	}
}

// Two dotenv files concatenated into one stream is not a file either reader
// would accept, so the ambiguity is refused rather than resolved.
func TestPullToStreamRefusesMoreThanOneOutput(t *testing.T) {
	cfg := loadTestConfig(t, onDemandConfig)
	var stream strings.Builder
	_, _, err := pullInto(t, PullRequest{
		Cfg: cfg, Data: worktreeData(t, cfg, 3, nil), Reader: newFakeStore(nil), Content: &stream,
	})
	if err == nil || !strings.Contains(err.Error(), "exactly one output") {
		t.Fatalf("err = %v, want a refusal naming the one-output rule", err)
	}
	if stream.Len() != 0 {
		t.Errorf("refused pull still wrote to the stream: %q", stream.String())
	}
}

// explain reports the inventory rather than acting on it, so it must list the
// output a pull withholds — hiding the file whose existence it explains is the
// opposite of its job.
func TestSelectEveryOutputKeepsOnDemand(t *testing.T) {
	cfg := loadTestConfig(t, onDemandConfig)
	swept, err := cfg.SelectOutputs(nil)
	if err != nil {
		t.Fatalf("SelectOutputs(nil): %v", err)
	}
	if got := outputConfigNames(swept); !slices.Equal(got, []string{"backend", "mobile"}) {
		t.Errorf("sweep = %v, want the two checkout outputs", got)
	}
	every, err := cfg.SelectEveryOutput(nil)
	if err != nil {
		t.Fatalf("SelectEveryOutput(nil): %v", err)
	}
	if got := outputConfigNames(every); !slices.Equal(got, []string{"backend", "mobile", "deploy"}) {
		t.Errorf("inventory = %v, want every declared output", got)
	}
}

// A config whose every output is on_demand: has nothing to sweep, and a
// silent success there reads as "your env files are up to date".
func TestSelectOutputsRefusesAnAllOnDemandSweep(t *testing.T) {
	cfg := loadTestConfig(t, `project_name: app
port_stride: 10
ports:
  - { name: APP_PORT, base: 4000 }
env:
  DEV_HOST: localhost
outputs:
  - name: deploy
    out: deploy/.env.staging
    format: dotenv
    on_demand: true
    include: [DEV_HOST]
`)
	_, err := cfg.SelectOutputs(nil)
	if err == nil || !strings.Contains(err.Error(), "on_demand") {
		t.Fatalf("err = %v, want it to say every output is on_demand", err)
	}
}
