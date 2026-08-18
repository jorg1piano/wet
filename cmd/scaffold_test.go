package cmd

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The starter config's whole job is to be immediately runnable: `wet setup`
// writes it, the user re-runs, and setup must succeed. A starter that fails
// its own validation would leave the user worse off than the error message it
// replaced.
func TestScaffoldStarterConfigLoadsAndValidates(t *testing.T) {
	dir := t.TempDir()
	path, err := scaffoldStarterConfig(dir)
	if err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	if want := filepath.Join(dir, wetConfigFileName); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	cfg, err := LoadWetConfig(dir)
	if err != nil {
		t.Fatalf("scaffolded config does not load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("scaffolded config does not validate: %v", err)
	}
}

// It must NOT be a copy of the annotated example: that file renders
// .env.example.tpl and claims emulator: pool items, neither of which exists in
// a fresh repo, so a repo seeded with it fails its very next setup.
func TestScaffoldStarterConfigIsNotTheAnnotatedExample(t *testing.T) {
	dir := t.TempDir()
	if _, err := scaffoldStarterConfig(dir); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, wetConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWetConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Render) != 0 {
		t.Errorf("starter declares render targets %+v — they cannot exist in a fresh repo", cfg.Render)
	}
	if len(cfg.Pools) != 0 {
		t.Errorf("starter declares pools %+v — it would claim resources the user never asked for", cfg.Pools)
	}
	if len(cfg.Hooks.PostCreate) != 0 || len(cfg.Hooks.PreRemove) != 0 {
		t.Error("starter declares hooks — it must run no project-specific commands")
	}
	if !strings.Contains(string(body), exampleConfigFileName) {
		t.Errorf("starter does not point at %s, so the user has nowhere to learn the rest of the schema", exampleConfigFileName)
	}
}

// Every derived env: value must resolve against a fresh checkout. The block
// references DEV_HOST (another env: key), PHX_PORT and HYDRA_PUBLIC_PORT
// (declared ports); rendering is missingkey=error, so a starter that declares
// a URL without the port behind it fails the setup that just wrote it.
func TestScaffoldStarterConfigEnvRenders(t *testing.T) {
	dir := t.TempDir()
	if _, err := scaffoldStarterConfig(dir); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	cfg, err := LoadWetConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	ports := cfg.ResolvePorts(1)
	data := BuildTemplateData(cfg, "feature/x", 1, ports, nil)
	resolved, err := ResolveEnvValues(cfg, data, nil)
	if err != nil {
		t.Fatalf("starter env: does not resolve: %v", err)
	}
	// Slot 1 of PHX_PORT base 4000 stride 10; the URL must follow the port
	// and DEV_HOST rather than hardcoding either.
	if got, want := resolved["AUTH_BASE_URL"], "http://localhost:4010"; got != want {
		t.Errorf("AUTH_BASE_URL = %q, want %q", got, want)
	}
	if got, want := resolved["HYDRA_PUBLIC_URL"], "http://localhost:4454"; got != want {
		t.Errorf("HYDRA_PUBLIC_URL = %q, want %q", got, want)
	}
}

// The catalog's google group defaults GOOGLE_REDIRECT_URI to
// '{{.HYDRA_PUBLIC_URL}}/callback/google'. If the starter ever stops defining
// HYDRA_PUBLIC_URL, scaffolding that group produces a config that cannot
// render — which is exactly the breakage this default exists to prevent.
func TestStarterSatisfiesCatalogEnvReferences(t *testing.T) {
	dir := t.TempDir()
	if _, err := scaffoldStarterConfig(dir); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWetConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	catalog, _, err := LoadSecretGroups()
	if err != nil {
		t.Fatal(err)
	}
	ports := cfg.ResolvePorts(0)
	data := BuildTemplateData(cfg, "main", 0, ports, nil)
	if _, err := ResolveEnvValues(cfg, data, nil); err != nil {
		t.Fatal(err)
	}
	ref := regexp.MustCompile(`{{\s*\.([A-Za-z_][A-Za-z0-9_]*)\s*}}`)
	for name, group := range catalog {
		for key, spec := range group.Env {
			for _, m := range ref.FindAllStringSubmatch(spec.Default, -1) {
				if _, ok := data[m[1]]; !ok {
					t.Errorf("catalog group %q key %q references {{.%s}}, which the starter does not define — scaffolding that group yields a config that cannot render",
						name, key, m[1])
				}
			}
		}
	}
}

// The Flutter half must stay commented: FLUTTER_DEVICE_ID reads a pool, and a
// pool of emulator: items makes setup boot an AVD on a repo that may have no
// phone anywhere near it.
func TestScaffoldStarterConfigLeavesFlutterCommented(t *testing.T) {
	dir := t.TempDir()
	if _, err := scaffoldStarterConfig(dir); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWetConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Env["FLUTTER_DEVICE_ID"]; ok {
		t.Error("FLUTTER_DEVICE_ID is live in the starter; it needs a devices pool that is not declared")
	}
	if len(cfg.Pools) != 0 {
		t.Errorf("starter declares pools %+v — setup would claim an item and boot an emulator", cfg.Pools)
	}
	body, err := os.ReadFile(filepath.Join(dir, wetConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "FLUTTER_DEVICE_ID") {
		t.Error("the commented Flutter block is gone; it is the only place the item-vs-handle distinction is explained at the point of use")
	}
}

// Overwriting would destroy a config the user had already written.
func TestScaffoldStarterConfigRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, wetConfigFileName)
	if err := os.WriteFile(path, []byte("project_name: mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := scaffoldStarterConfig(dir); err == nil {
		t.Fatal("expected refusal to overwrite an existing config, got nil")
	}
	body, _ := os.ReadFile(path)
	if string(body) != "project_name: mine\n" {
		t.Fatalf("existing config was modified: %q", body)
	}
}

func TestDefaultProjectName(t *testing.T) {
	cases := []struct{ dir, want string }{
		{"/src/template_framework", "template_framework"},
		{"/src/My-App", "my-app"},
		{"/src/wet.cli", "wet-cli"},
		{"/src/Ærlig Prosjekt", "rlig-prosjekt"},
		{"/src/---", "myapp"},
		{"/src/123go", "123go"},
	}
	for _, c := range cases {
		got := defaultProjectName(c.dir)
		if got != c.want {
			t.Errorf("defaultProjectName(%q) = %q, want %q", c.dir, got, c.want)
		}
		if !projectNameRE.MatchString(got) {
			t.Errorf("defaultProjectName(%q) = %q, which fails projectNameRE", c.dir, got)
		}
	}
}
