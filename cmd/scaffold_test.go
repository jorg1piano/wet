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

// It must NOT be a copy of the annotated example: that file claims emulator:
// pool items and declares secrets whose vault nobody has, so a repo seeded
// with it fails its very next setup.
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
	if cfg.Secrets != nil {
		t.Errorf("starter declares secrets %+v — a fresh repo has no vault to read them from", cfg.Secrets)
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

// The starter's single output is the one file `wet setup` writes for a fresh
// repo, so every part of it has to hold on a checkout that contains nothing
// but the config: only ordinary keys (there is no store yet), and a path with
// no package directory in it that the user never created.
func TestScaffoldStarterConfigOutput(t *testing.T) {
	dir := t.TempDir()
	if _, err := scaffoldStarterConfig(dir); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWetConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Outputs) != 1 {
		t.Fatalf("starter declares %d outputs, want exactly 1", len(cfg.Outputs))
	}
	out := cfg.Outputs[0]
	// The name has to match what `wet secrets setup` scaffolds into; otherwise
	// scaffolding a provider invents a second output and the starter's file
	// silently never gains the provider's keys.
	if out.Name != defaultScaffoldOutput {
		t.Errorf("output name = %q, want %q so `wet secrets setup` appends to it", out.Name, defaultScaffoldOutput)
	}
	if len(out.Secrets) != 0 {
		t.Errorf("starter output selects secrets %v — none are defined", out.Secrets)
	}
	if strings.Contains(out.Out, "/") {
		t.Errorf("output out = %q — a fresh repo has no package directory to write into", out.Out)
	}
	if out.Format != OutputFormatDotenv {
		t.Errorf("output format = %q, want %q", out.Format, OutputFormatDotenv)
	}
}

// Scaffolding a provider on top of the starter must produce a config that
// still loads. This is the whole path a new user walks — `wet setup` writes
// the starter, the picker adds a group — and the two halves are maintained in
// different files, so nothing but this test notices when they stop fitting.
func TestScaffoldGroupsOntoStarterConfig(t *testing.T) {
	dir := t.TempDir()
	path, err := scaffoldStarterConfig(dir)
	if err != nil {
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
	res, err := ScaffoldGroups(doc, catalog, []string{"core", "google"},
		ScaffoldOptions{Output: defaultScaffoldOutput})
	if err != nil {
		t.Fatalf("ScaffoldGroups: %v", err)
	}
	if res.CreatedOutput {
		t.Errorf("the scaffolder invented an output; the starter already declares %q", defaultScaffoldOutput)
	}
	body, err := MarshalYAMLDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWetConfig(dir)
	if err != nil {
		t.Fatalf("starter + scaffolded groups does not load: %v\n%s", err, body)
	}
	if _, ok := cfg.Secrets.Definitions["GOOGLE_CLIENT_ID"]; !ok {
		t.Error("GOOGLE_CLIENT_ID was not declared under secrets.definitions")
	}
	if _, ok := cfg.Env["GOOGLE_REDIRECT_URI"]; !ok {
		t.Error("the derived GOOGLE_REDIRECT_URI did not stay an ordinary env: value")
	}
}

// The annotated example is documentation, but it is also a config people copy
// to .wet.yaml. Its validity is asserted in config_test.go; what is checked
// here is that it still teaches the CURRENT model — a schema rename that left
// the prose behind would ship a file whose every example is wrong.
func TestExampleConfigDeclaresOutputs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, wetConfigFileName), []byte(wetExampleConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWetConfigWithLocal(dir, "")
	if err != nil {
		t.Fatalf("%s is not a valid config: %v", exampleConfigFileName, err)
	}
	if len(cfg.Outputs) == 0 {
		t.Fatalf("%s documents no outputs:, which is the block it exists to explain", exampleConfigFileName)
	}
	// Both ways of naming a key have to be shown somewhere, since include: and
	// values: are the whole reason there is no .env.tpl any more.
	var sawInclude, sawValues bool
	for _, o := range cfg.Outputs {
		sawInclude = sawInclude || len(o.Include) > 0
		sawValues = sawValues || len(o.Values.Keys) > 0
	}
	if !sawInclude || !sawValues {
		t.Errorf("%s illustrates include:=%v values:=%v, want both", exampleConfigFileName, sawInclude, sawValues)
	}
	for _, removed := range []string{"\nrender:", "secrets_from", "sections:", "env populate", ".env.example.tpl"} {
		if strings.Contains(wetExampleConfig, removed) {
			t.Errorf("%s still mentions the removed %q", exampleConfigFileName, removed)
		}
	}
}

// The starter declares an output, so the starter must also make sure that
// output cannot be committed. A generated env file in git is wrong for every
// checkout but the one that wrote it, and once secrets are declared it is a
// leak — and the starter is the one config nobody reviews first.
func TestScaffoldStarterConfigGitignoresItsOutput(t *testing.T) {
	dir := t.TempDir()
	if _, err := scaffoldStarterConfig(dir); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWetConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatalf("no .gitignore was written: %v", err)
	}
	ignored := map[string]bool{}
	for _, line := range strings.Split(string(body), "\n") {
		ignored[strings.TrimSpace(line)] = true
	}
	for _, o := range cfg.Outputs {
		if !ignored[o.Out] {
			t.Errorf("output %q writes %q, which .gitignore does not cover:\n%s", o.Name, o.Out, body)
		}
	}
}

// Re-running must not restate the entry, and an unrelated .gitignore must
// survive intact — the scaffolder appends to a file the repo owns.
func TestIgnoreStarterOutputsIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".gitignore")
	// No trailing newline, and a near-miss entry: a substring check would read
	// this as "already ignored" and leave the real file committable.
	if err := os.WriteFile(path, []byte("/build\n.env.dev.sample"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ignoreStarterOutputs(dir); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(first), "/build\n.env.dev.sample\n") {
		t.Errorf("the existing .gitignore was mangled:\n%s", first)
	}
	if strings.Count(string(first), "\n"+starterOutputFile+"\n") != 1 {
		t.Errorf("%q was not added exactly once:\n%s", starterOutputFile, first)
	}

	if err := ignoreStarterOutputs(dir); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("a second run changed .gitignore:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

// An output wet invents is a file it has just promised to generate, holding
// whatever credentials that output selects. It has to be born gitignored —
// the starter path already does this, and a second call site that only
// described the guard is the same defect in a different place.
func TestScaffoldedOutputIsGitignored(t *testing.T) {
	dir := t.TempDir()
	if err := ignoreGeneratedOutput(dir, "web/.env.dev"); err != nil {
		t.Fatalf("ignore: %v", err)
	}
	body := readFileText(t, filepath.Join(dir, ".gitignore"))
	if !strings.Contains(body, "\nweb/.env.dev\n") {
		t.Errorf(".gitignore missing the generated path:\n%s", body)
	}
	// Idempotent, and line-exact: a near-miss entry must not be read as
	// already covering the path.
	before := body
	if err := ignoreGeneratedOutput(dir, "web/.env.dev"); err != nil {
		t.Fatal(err)
	}
	if after := readFileText(t, filepath.Join(dir, ".gitignore")); after != before {
		t.Errorf("second run changed the file:\n%s", after)
	}
	if err := ignoreGeneratedOutput(dir, "web/.env.dev.sample"); err != nil {
		t.Fatal(err)
	}
	if got := readFileText(t, filepath.Join(dir, ".gitignore")); !strings.Contains(got, "web/.env.dev.sample") {
		t.Errorf("a near-miss path was treated as already ignored:\n%s", got)
	}
}
