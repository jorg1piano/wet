package cmd

import (
	"os"
	"path/filepath"
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
  # Derived once here, consumed by every template.
  HYDRA_PUBLIC_URL: 'http://localhost:{{.APP_PORT}}'

render:
  - template: backend/.env.tpl
    out: backend/.env
`

func scaffold(t *testing.T, body string, groups []string, section string) (string, *ScaffoldResult) {
	t.Helper()
	catalog, _, err := LoadSecretGroups()
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("parse input: %v", err)
	}
	res, err := ScaffoldGroups(&doc, catalog, groups, section)
	if err != nil {
		t.Fatalf("ScaffoldGroups: %v", err)
	}
	out, err := MarshalYAMLDocument(&doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(out), res
}

// TestScaffoldWritesBothHalves covers the shape of a first run.
func TestScaffoldWritesBothHalves(t *testing.T) {
	out, res := scaffold(t, scaffoldBaseConfig, []string{"core", "google"}, "backend")

	for _, want := range []string{
		"# core", "# google",
		"GOOGLE_CLIENT_ID: ''",
		"GOOGLE_REDIRECT_URI: '{{.HYDRA_PUBLIC_URL}}/callback/google'",
		"GOOGLE_CLIENT_ID: google_client_id",
		"Enabled groups: core, google",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	// The derived URI is a template field, never a vault field.
	if strings.Contains(out, "GOOGLE_REDIRECT_URI: google_redirect_uri") {
		t.Error("the derived redirect URI was added to the secrets section")
	}
	// Comments that were already in the file survive.
	if !strings.Contains(out, "# Ports the stack needs.") ||
		!strings.Contains(out, "# Derived once here") {
		t.Errorf("existing comments were lost:\n%s", out)
	}
	if len(res.AddedSecrets) == 0 || len(res.AddedEnv) == 0 {
		t.Errorf("result = %+v, want both halves reported", res)
	}

	// It must load, since the command writes it back over a working config.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".wet.yaml"), []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWetConfigWithLocal(dir, "")
	if err != nil {
		t.Fatalf("scaffolded config does not load: %v\n%s", err, out)
	}
	if cfg.Secrets == nil || cfg.Secrets.Sections["backend"]["GOOGLE_CLIENT_ID"] != "google_client_id" {
		t.Errorf("loaded secrets = %+v, want the scaffolded section", cfg.Secrets)
	}
	if cfg.Env["GOOGLE_CLIENT_ID"] != "" {
		t.Errorf("GOOGLE_CLIENT_ID default = %q, want empty so a fresh clone still renders", cfg.Env["GOOGLE_CLIENT_ID"])
	}
}

// TestScaffoldNeverOverwritesFilledValues is the property that makes re-running
// safe: a value you have edited must survive the next scaffold.
func TestScaffoldNeverOverwritesFilledValues(t *testing.T) {
	first, _ := scaffold(t, scaffoldBaseConfig, []string{"google"}, "backend")
	edited := strings.Replace(first, "GOOGLE_CLIENT_ID: ''", "GOOGLE_CLIENT_ID: 'my-real-id'", 1)
	if edited == first {
		t.Fatal("test setup: the placeholder was not found")
	}

	second, res := scaffold(t, edited, []string{"google", "apple"}, "backend")
	if !strings.Contains(second, "GOOGLE_CLIENT_ID: 'my-real-id'") {
		t.Errorf("a filled-in value was reset by the second run:\n%s", second)
	}
	if !strings.Contains(second, "APPLE_KEY_ID: ''") {
		t.Errorf("the newly added group was not written:\n%s", second)
	}
	for _, kept := range res.KeptEnv {
		if kept == "GOOGLE_CLIENT_ID" {
			return
		}
	}
	t.Errorf("KeptEnv = %v, want it to report the untouched key", res.KeptEnv)
}

// TestScaffoldIsIdempotent: running twice with the same groups changes nothing
// the second time, so it is safe in a setup script.
func TestScaffoldIsIdempotent(t *testing.T) {
	once, _ := scaffold(t, scaffoldBaseConfig, []string{"core", "sentry"}, "backend")
	twice, res := scaffold(t, once, []string{"core", "sentry"}, "backend")
	if once != twice {
		t.Errorf("second run changed the file:\n--- first ---\n%s\n--- second ---\n%s", once, twice)
	}
	if len(res.AddedEnv) != 0 || len(res.AddedSecrets) != 0 {
		t.Errorf("second run added %v / %v, want nothing", res.AddedEnv, res.AddedSecrets)
	}
}

// TestEnabledGroupsAccumulate proves the marker comment is how a later run
// knows what is already there — including through a full write/read cycle.
func TestEnabledGroupsAccumulate(t *testing.T) {
	first, _ := scaffold(t, scaffoldBaseConfig, []string{"google"}, "backend")
	second, _ := scaffold(t, first, []string{"apple"}, "backend")

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
}

// TestScaffoldNoVault covers --no-vault: env defaults only, and no secrets
// block invented for a repo that does not want one.
func TestScaffoldNoVault(t *testing.T) {
	out, res := scaffold(t, scaffoldBaseConfig, []string{"google"}, "")
	if strings.Contains(out, "secrets:") {
		t.Errorf("--no-vault created a secrets block:\n%s", out)
	}
	if len(res.AddedSecrets) != 0 {
		t.Errorf("AddedSecrets = %v, want none", res.AddedSecrets)
	}
	if !strings.Contains(out, "GOOGLE_CLIENT_ID: ''") {
		t.Error("env defaults were not written")
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
	if _, err := ScaffoldGroups(&doc, catalog, []string{"goggle"}, "backend"); err == nil {
		t.Fatal("an unknown group was accepted")
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
