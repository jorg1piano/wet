package cmd

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestEmbeddedGroupCatalogParses is the guard on the shipped catalog: it is
// embedded, so a typo cannot be caught by any consuming repo — only here.
func TestEmbeddedGroupCatalogParses(t *testing.T) {
	catalog, order, err := LoadSecretGroups()
	if err != nil {
		t.Fatalf("embedded catalog does not parse: %v", err)
	}
	if len(catalog) == 0 {
		t.Fatal("embedded catalog is empty")
	}
	for _, want := range []string{"core", "google", "apple", "sentry"} {
		if _, ok := catalog[want]; !ok {
			t.Errorf("catalog is missing the %q group", want)
		}
	}
	if len(order) != len(catalog) {
		t.Errorf("order has %d entries for %d groups", len(order), len(catalog))
	}
	for _, name := range order {
		g := catalog[name]
		if len(g.Order) != len(g.Env) {
			t.Errorf("group %q: %d ordered keys for %d env keys", name, len(g.Order), len(g.Env))
		}
		for _, key := range g.Order {
			if _, ok := g.Env[key]; !ok {
				t.Errorf("group %q: ordered key %q is not in env", name, key)
			}
		}
	}

	// The redirect URIs are the reason the catalog distinguishes derived
	// values from stored ones: a URI in the vault would pin every worktree to
	// one URL.
	for _, name := range []string{"google", "apple"} {
		for key, spec := range catalog[name].Env {
			if strings.HasSuffix(key, "_REDIRECT_URI") {
				if spec.Secret != "" {
					t.Errorf("%s.%s names a vault field; a derived URI must not be stored", name, key)
				}
				if !strings.Contains(spec.Default, "{{") {
					t.Errorf("%s.%s = %q, want it derived from a template", name, key, spec.Default)
				}
			}
		}
	}
}

// TestCatalogRejectsStoredDerivedValue proves the rule above is enforced by
// the parser, not just respected by the current file.
func TestCatalogRejectsStoredDerivedValue(t *testing.T) {
	_, _, err := parseSecretGroups(`
bad:
  description: derived and stored at once
  env:
    X_REDIRECT_URI:
      default: '{{.HYDRA_PUBLIC_URL}}/callback/x'
      secret: x_redirect_uri
`)
	if err == nil || !strings.Contains(err.Error(), "derived") {
		t.Fatalf("err = %v, want a refusal naming the derived value", err)
	}

	if _, _, err := parseSecretGroups("bad:\n  env:\n    A: {}\n"); err == nil {
		t.Error("a group with no description was accepted")
	}
	if _, _, err := parseSecretGroups("bad:\n  description: d\n  env: {}\n"); err == nil {
		t.Error("a group with no keys was accepted")
	}
	if _, _, err := parseSecretGroups("bad:\n  description: d\n  env:\n    Branch:\n      secret: b\n"); err == nil {
		t.Error("a group shadowing wet's reserved Branch field was accepted")
	}
}

const scaffoldBaseConfig = `project_name: fantasy
port_stride: 10

# Ports the stack needs.
ports:
  - name: APP_PORT
    base: 4000

env:
  # Derived once here, consumed by every output.
  HYDRA_PUBLIC_URL: 'http://localhost:{{.APP_PORT}}'

outputs:
  - name: backend
    out: backend/.env.dev
    format: dotenv
    include:
      - HYDRA_PUBLIC_URL
`

func scaffold(t *testing.T, body string, groups []string, opts ScaffoldOptions) (string, *ScaffoldResult) {
	t.Helper()
	catalog, _, err := LoadSecretGroups()
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("parse input: %v", err)
	}
	res, err := ScaffoldGroups(&doc, catalog, groups, opts)
	if err != nil {
		t.Fatalf("ScaffoldGroups: %v", err)
	}
	out, err := MarshalYAMLDocument(&doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(out), res
}

// loadScaffolded runs the scaffolder's output through the real loader, which
// is the only check that matters: the command writes this back over a working
// config, so anything Validate() rejects would brick the user's repo.
func loadScaffolded(t *testing.T, out string) *WetConfig {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, wetConfigFileName), []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWetConfigWithLocal(dir, "")
	if err != nil {
		t.Fatalf("scaffolded config does not load: %v\n%s", err, out)
	}
	return cfg
}

func backendOutput(t *testing.T, cfg *WetConfig) OutputConfig {
	t.Helper()
	for _, o := range cfg.Outputs {
		if o.Name == "backend" {
			return o
		}
	}
	t.Fatalf("no output named backend in %+v", cfg.Outputs)
	return OutputConfig{}
}

// TestScaffoldSplitsSecretsFromOrdinaryValues covers the shape of a first run:
// a credential becomes a definition carrying its fallback, a derived value
// stays in env:, and neither appears in both.
func TestScaffoldSplitsSecretsFromOrdinaryValues(t *testing.T) {
	out, res := scaffold(t, scaffoldBaseConfig, []string{"core", "google"}, ScaffoldOptions{Output: "backend"})

	for _, want := range []string{
		"# google",
		"GOOGLE_REDIRECT_URI: '{{.HYDRA_PUBLIC_URL}}/callback/google'",
		"field: google_client_id",
		"Enabled groups: core, google",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	// Comments that were already in the file survive.
	if !strings.Contains(out, "# Ports the stack needs.") ||
		!strings.Contains(out, "# Derived once here") {
		t.Errorf("existing comments were lost:\n%s", out)
	}
	if len(res.AddedSecrets) == 0 || len(res.AddedEnv) == 0 {
		t.Errorf("result = %+v, want both halves reported", res)
	}
	if res.CreatedOutput {
		t.Error("CreatedOutput is set, but the base config already declares backend")
	}

	cfg := loadScaffolded(t, out)
	def, ok := cfg.Secrets.Definitions["GOOGLE_CLIENT_ID"]
	if !ok {
		t.Fatalf("GOOGLE_CLIENT_ID is not a definition: %+v", cfg.Secrets)
	}
	if def.Field != "google_client_id" {
		t.Errorf("field = %q, want google_client_id", def.Field)
	}
	// An empty default must be PRESENT, not absent: absent means "the store
	// must answer or fail", empty means "this provider is simply not
	// configured locally" — and only the second one lets a fresh clone boot.
	if !def.HasDefault() || def.DefaultValue() != "" {
		t.Errorf("default = %v (%q), want an explicit empty string", def.Default, def.DefaultValue())
	}
	if _, ok := cfg.Env["GOOGLE_CLIENT_ID"]; ok {
		t.Error("GOOGLE_CLIENT_ID is also an env: key — a name cannot be both, and the config would not load")
	}
	// The catalog's non-empty fallback has to survive into the definition.
	if got := cfg.Secrets.Definitions["SECRET_KEY_BASE"].DefaultValue(); !strings.Contains(got, "dev-only") {
		t.Errorf("SECRET_KEY_BASE default = %q, want the catalog's dev fallback", got)
	}
	// The derived URI is an ordinary value, never a stored one.
	if _, ok := cfg.Secrets.Definitions["GOOGLE_REDIRECT_URI"]; ok {
		t.Error("the derived redirect URI was declared as a secret")
	}
	if cfg.Env["GOOGLE_REDIRECT_URI"] == "" {
		t.Error("the derived redirect URI did not land in env:")
	}
}

// A scaffolded group nobody consumes is dead config: the definitions exist,
// no file selects them, and `wet env pull` writes exactly what it wrote
// before — which reads as wet ignoring the provider you just added.
func TestScaffoldWiresKeysIntoTheOutput(t *testing.T) {
	out, _ := scaffold(t, scaffoldBaseConfig, []string{"google"}, ScaffoldOptions{Output: "backend"})
	o := backendOutput(t, loadScaffolded(t, out))

	if !slices.Contains(o.Secrets, "GOOGLE_CLIENT_ID") {
		t.Errorf("output secrets = %v, want the scaffolded credential", o.Secrets)
	}
	if !slices.Contains(o.Include, "GOOGLE_REDIRECT_URI") {
		t.Errorf("output include = %v, want the derived value", o.Include)
	}
	// Pre-existing entries are kept, not replaced.
	if !slices.Contains(o.Include, "HYDRA_PUBLIC_URL") {
		t.Errorf("output include = %v, want the key the config already listed", o.Include)
	}
}

// With no output by that name, the scaffolder has to invent one — otherwise
// the keys have nowhere to go — and must say so, since a generated file the
// user never asked for is the one change they need to know about.
func TestScaffoldCreatesMissingOutput(t *testing.T) {
	const noOutputs = `project_name: fantasy

env:
  DEV_HOST: 'localhost'
`
	out, res := scaffold(t, noOutputs, []string{"sentry"}, ScaffoldOptions{Output: "backend"})
	if !res.CreatedOutput || res.Output != "backend" {
		t.Errorf("result = %+v, want a created backend output", res)
	}
	o := backendOutput(t, loadScaffolded(t, out))
	if o.Out != "backend/.env.dev" || o.Format != OutputFormatDotenv {
		t.Errorf("created output = %+v, want backend/.env.dev as dotenv", o)
	}
	if !slices.Contains(o.Secrets, "SENTRY_DSN") || !slices.Contains(o.Include, "SENTRY_ENV") {
		t.Errorf("created output does not carry the group: %+v", o)
	}
}

// TestScaffoldNeverOverwritesFilledValues is the property that makes re-running
// safe: a value you have edited must survive the next scaffold.
func TestScaffoldNeverOverwritesFilledValues(t *testing.T) {
	first, _ := scaffold(t, scaffoldBaseConfig, []string{"google"}, ScaffoldOptions{Output: "backend"})
	edited := strings.Replace(first, "field: google_client_id\n      default: ''",
		"field: google_client_id\n      default: 'my-real-id'", 1)
	if edited == first {
		t.Fatalf("test setup: the placeholder was not found in:\n%s", first)
	}

	second, res := scaffold(t, edited, []string{"google", "apple"}, ScaffoldOptions{Output: "backend"})
	if !strings.Contains(second, "default: 'my-real-id'") {
		t.Errorf("a filled-in value was reset by the second run:\n%s", second)
	}
	if !strings.Contains(second, "field: apple_key_id") {
		t.Errorf("the newly added group was not written:\n%s", second)
	}
	if !slices.Contains(res.KeptEnv, "GOOGLE_CLIENT_ID") {
		t.Errorf("KeptEnv = %v, want it to report the untouched key", res.KeptEnv)
	}
}

// TestScaffoldIsIdempotent: running twice with the same groups changes nothing
// the second time, so it is safe in a setup script. It also proves the output
// lists do not accumulate duplicates — a key listed twice is a load error.
func TestScaffoldIsIdempotent(t *testing.T) {
	once, _ := scaffold(t, scaffoldBaseConfig, []string{"core", "sentry"}, ScaffoldOptions{Output: "backend"})
	twice, res := scaffold(t, once, []string{"core", "sentry"}, ScaffoldOptions{Output: "backend"})
	if once != twice {
		t.Errorf("second run changed the file:\n--- first ---\n%s\n--- second ---\n%s", once, twice)
	}
	if len(res.AddedEnv) != 0 || len(res.AddedSecrets) != 0 {
		t.Errorf("second run added %v / %v, want nothing", res.AddedEnv, res.AddedSecrets)
	}
	loadScaffolded(t, twice)
}

// TestEnabledGroupsAccumulate proves the marker comment is how a later run
// knows what is already there — including through a full write/read cycle.
func TestEnabledGroupsAccumulate(t *testing.T) {
	first, _ := scaffold(t, scaffoldBaseConfig, []string{"google"}, ScaffoldOptions{Output: "backend"})
	second, _ := scaffold(t, first, []string{"apple"}, ScaffoldOptions{Output: "backend"})

	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(second), &doc); err != nil {
		t.Fatal(err)
	}
	got := EnabledGroups(doc.Content[0])
	want := map[string]bool{"google": true, "apple": true}
	if len(got) != len(want) {
		t.Fatalf("EnabledGroups = %v, want google and apple", got)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("EnabledGroups = %v, unexpected %q", got, g)
		}
	}
	// The marker's prose is load-bearing documentation: it is the only place
	// the file says why a credential is absent from env:.
	if strings.Contains(second, "EMPTY here") {
		t.Error("the marker still explains empty env: defaults, which is no longer how secrets work")
	}
	if !strings.Contains(second, "secrets.definitions") {
		t.Errorf("the marker does not point at secrets.definitions:\n%s", second)
	}
}

// A key the file already carries as an ordinary value must stay one. Promoting
// it to a definition would collide with the env: entry — a name cannot be both,
// and the config would stop loading — and would drop whatever value the user
// had filled in there.
func TestScaffoldKeepsAnExistingEnvKeyOrdinary(t *testing.T) {
	const handWritten = `project_name: fantasy

env:
  GOOGLE_CLIENT_ID: 'set-by-hand-before-wet-knew-about-secrets'

outputs:
  - name: backend
    out: backend/.env.dev
    format: dotenv
    include:
      - GOOGLE_CLIENT_ID
`
	out, res := scaffold(t, handWritten, []string{"google"}, ScaffoldOptions{Output: "backend"})

	cfg := loadScaffolded(t, out)
	if cfg.Env["GOOGLE_CLIENT_ID"] != "set-by-hand-before-wet-knew-about-secrets" {
		t.Errorf("GOOGLE_CLIENT_ID = %q, want the hand-written value untouched", cfg.Env["GOOGLE_CLIENT_ID"])
	}
	if cfg.Secrets != nil {
		if _, ok := cfg.Secrets.Definitions["GOOGLE_CLIENT_ID"]; ok {
			t.Error("GOOGLE_CLIENT_ID was declared as a secret while still an env: key — the config cannot load with both")
		}
	}
	if !slices.Contains(res.KeptEnv, "GOOGLE_CLIENT_ID") {
		t.Errorf("KeptEnv = %v, want the untouched key reported", res.KeptEnv)
	}
	// Its sibling, which the file did NOT already carry, still becomes a
	// definition: one legacy key does not disable the model for the rest.
	if _, ok := cfg.Secrets.Definitions["GOOGLE_CLIENT_SECRET"]; !ok {
		t.Errorf("GOOGLE_CLIENT_SECRET was not declared: %+v", cfg.Secrets)
	}
}

// TestScaffoldUnknownGroup: a typo must not half-write the file.
func TestScaffoldUnknownGroup(t *testing.T) {
	catalog, _, err := LoadSecretGroups()
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(scaffoldBaseConfig), &doc); err != nil {
		t.Fatal(err)
	}
	before, err := MarshalYAMLDocument(&doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ScaffoldGroups(&doc, catalog, []string{"goggle"}, ScaffoldOptions{Output: "backend"}); err == nil {
		t.Fatal("an unknown group was accepted")
	}
	after, err := MarshalYAMLDocument(&doc)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("the document was modified before the error:\n%s", after)
	}
}

// Scaffolded keys need a file to appear in, so an empty output name is a
// programming error, not a quiet no-op that loses the whole run's work.
func TestScaffoldRequiresAnOutputName(t *testing.T) {
	catalog, _, err := LoadSecretGroups()
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(scaffoldBaseConfig), &doc); err != nil {
		t.Fatal(err)
	}
	if _, err := ScaffoldGroups(&doc, catalog, []string{"google"}, ScaffoldOptions{}); err == nil {
		t.Fatal("an empty output name was accepted")
	}
}

// TestSpaceOutYAMLBlocks pins the formatting rules, since they decide whether
// a human-maintained config stays readable.
func TestSpaceOutYAMLBlocks(t *testing.T) {
	in := "a: 1\nb: 2\n# comment\nc: 3\nd:\n  # inner\n  e: 4\nf: 5\n"
	want := "a: 1\nb: 2\n\n# comment\nc: 3\nd:\n  # inner\n  e: 4\n\nf: 5\n"
	if got := spaceOutYAMLBlocks(in); got != want {
		t.Errorf("spaceOutYAMLBlocks:\ngot:\n%q\nwant:\n%q", got, want)
	}
}

// A group's derived values reference other keys. Scaffolding one onto a config
// that never declared them writes a template that PARSES fine — Validate()
// only parses env: entries — and then breaks every later `wet setup` and
// `wet env pull` with an unresolvable key. The scaffolder would have bricked
// the config it was asked to improve, so it must refuse before writing.
func TestScaffoldRefusesWhenADerivedValueCannotResolve(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, wetConfigFileName)
	// No HYDRA_PUBLIC_URL, which the google group's redirect URI derives from.
	const minimal = `project_name: app
port_stride: 10
ports:
  - name: APP_PORT
    base: 4000
env:
  DEV_HOST: localhost
outputs:
  - name: backend
    out: backend/.env.dev
    format: dotenv
    include: [DEV_HOST]
`
	if err := os.WriteFile(path, []byte(minimal), 0o644); err != nil {
		t.Fatal(err)
	}
	catalog, _, err := LoadSecretGroups()
	if err != nil {
		t.Fatal(err)
	}
	doc, err := ReadYAMLDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ScaffoldGroups(doc, catalog, []string{"google"}, ScaffoldOptions{Output: "backend"}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	out, err := MarshalYAMLDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	err = validateScaffolded(out, path)
	if err == nil {
		t.Fatal("a config whose scaffolded env: cannot resolve was accepted")
	}
	if !strings.Contains(err.Error(), "HYDRA_PUBLIC_URL") {
		t.Errorf("err should name the missing key, got: %v", err)
	}
	// And the file on disk is untouched, which is the whole point of
	// validating before writing.
	if got := readFileText(t, path); got != minimal {
		t.Errorf("the config was modified despite the refusal:\n%s", got)
	}
}

func readFileText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
