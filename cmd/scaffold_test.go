package cmd

import (
	"os"
	"path/filepath"
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
