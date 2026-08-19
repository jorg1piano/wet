package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

var (
	envPopulateOut     string
	envPopulateDryRun  bool
	envPopulateAll     bool
	envPopulateSecrets []string
)

var envCmd = &cobra.Command{
	Use:   "env",
	Short: "Render env files from templates",
}

var envPopulateCmd = &cobra.Command{
	Use:   "populate <template>",
	Short: "Render a template with this worktree's values (plus optional JSON on stdin)",
	Long: `Render <template> through Go text/template using this worktree's values and
write the result to --out.

Available template fields are the same ones hooks get: ProjectName, Branch,
BranchSafe, Slot, every declared port (e.g. {{.APP_PORT}}), {{.Pools.<name>}},
and every rendered entry from .wet.yaml's env: block.

Anything wet doesn't know about — typically secrets — comes in through
--secrets-from as a flat JSON object. wet talks to no secret store itself;
provenance is the caller's business:

  op item get myapp --format json | jq '<shape>' \
    | wet env populate .env.tpl -o .env --secrets-from -

  wet env populate .env.tpl -o .env --secrets-from ./secrets.json

The flag is repeatable, and later sources win on a key collision — so several
providers compose without a shell-side merge:

  docker compose config --format json | jq '.services.db.environment' > /tmp/db.json
  op item get myapp --format json | jq 'from_entries' \
    | wet env populate .env.tpl -o .env \
        --secrets-from /tmp/db.json --secrets-from -

The source is always explicit — wet never sniffs stdin, because a hook
inherits its parent's stdin and guessing wrong would block forever.

Supplied keys override .wet.yaml env: entries. They may NOT override a
declared port or a reserved field — that would silently defeat slot
allocation, so it is an error instead.

With no --secrets-from the template renders from wet's own values alone. A
{{.KEY}} with no value is always a hard error, never an empty string.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runEnvPopulate,
}

func init() {
	envPopulateCmd.Flags().StringVarP(&envPopulateOut, "out", "o", "", "output file (required unless --dry-run)")
	envPopulateCmd.Flags().BoolVar(&envPopulateDryRun, "dry-run", false, "write the rendered result to stdout instead of a file")
	envPopulateCmd.Flags().BoolVar(&envPopulateAll, "all", false, "render every target in .wet.yaml's render: list")
	envPopulateCmd.Flags().StringArrayVar(&envPopulateSecrets, "secrets-from", nil,
		`read extra values from a flat JSON file, or "-" for stdin. Repeatable; later sources win on a key collision`)
	envCmd.AddCommand(envPopulateCmd)
	rootCmd.AddCommand(envCmd)
}

func runEnvPopulate(cmd *cobra.Command, args []string) error {
	if envPopulateAll && len(args) > 0 {
		return fmt.Errorf("--all renders the targets in .wet.yaml; don't also name a template")
	}
	if !envPopulateAll && len(args) == 0 {
		return fmt.Errorf("name a template, or pass --all to render .wet.yaml's render: targets")
	}
	if !envPopulateAll && envPopulateOut == "" && !envPopulateDryRun {
		return fmt.Errorf("--out is required (or pass --dry-run to print to stdout)")
	}

	treePath, err := getRepoRoot()
	if err != nil {
		return err
	}
	repoRoot, err := getMainRepoRoot()
	if err != nil {
		return err
	}
	cfg, _, err := LoadWetConfigForWorktree(treePath, repoRoot)
	if err != nil {
		if err == ErrConfigMissing {
			return fmt.Errorf("`wet env populate` requires a .wet.yaml (none at %s or %s)", treePath, repoRoot)
		}
		return err
	}

	data, err := buildPopulateData(cfg, treePath)
	if err != nil {
		return err
	}

	if !envPopulateAll {
		overrides, err := readAllSecrets("", envPopulateSecrets)
		if err != nil {
			return err
		}
		return renderTarget(cfg, data, treePath,
			RenderTarget{Template: args[0], Out: envPopulateOut}, overrides)
	}

	if len(cfg.Render) == 0 {
		return fmt.Errorf("--all: .wet.yaml declares no render: targets")
	}
	// --secrets-from on the command line applies to every target, layered
	// UNDER each target's own sources so a target can still specialize.
	shared, err := readAllSecrets("", envPopulateSecrets)
	if err != nil {
		return err
	}
	for i, target := range cfg.Render {
		overrides, err := readAllSecrets(treePath, target.SecretsFrom)
		if err != nil {
			return fmt.Errorf("render[%d] (%s): %w", i, target.Out, err)
		}
		// A target's own secrets: section is the base layer. Files and stdin
		// are layered over it, keeping the existing "later sources win" rule
		// intact: an explicit --secrets-from can still override one key
		// without the vault having to know.
		fromVault, err := readSectionSecrets(cfg, target.Secrets)
		if err != nil {
			return fmt.Errorf("render[%d] (%s): %w", i, target.Out, err)
		}
		merged := map[string]string{}
		for k, v := range fromVault {
			merged[k] = v
		}
		for k, v := range shared {
			merged[k] = v
		}
		for k, v := range overrides {
			merged[k] = v
		}
		// Each target renders against its own copy of the data, so one
		// target's secrets can never leak into the next one's output.
		perTarget := make(map[string]any, len(data))
		for k, v := range data {
			perTarget[k] = v
		}
		if err := renderTarget(cfg, perTarget, treePath, target, merged); err != nil {
			return fmt.Errorf("render[%d] (%s): %w", i, target.Out, err)
		}
	}
	return nil
}

// renderSetupTarget renders a declared target during `wet setup`, where paths
// are always relative to the checkout being configured.
func renderSetupTarget(cfg *WetConfig, data map[string]any, treePath string, target RenderTarget, overrides map[string]string) error {
	body, err := os.ReadFile(filepath.Join(treePath, target.Template))
	if err != nil {
		return fmt.Errorf("read template: %w", err)
	}
	if err := mergeOverrides(cfg, data, overrides); err != nil {
		return err
	}
	rendered, err := renderWetTemplate("template:"+filepath.Base(target.Template), string(body), data)
	if err != nil {
		return err
	}
	out := filepath.Join(treePath, target.Out)
	// A target may render into a directory that doesn't exist yet — .vscode/
	// is gitignored in most repos, so a fresh worktree has no such directory
	// until something makes one. Without this, such a target fails with a
	// bare ENOENT naming a path the user can see is right.
	if err := os.MkdirAll(filepath.Dir(out), 0755); err != nil {
		return fmt.Errorf("create parent of %s: %w", out, err)
	}
	if err := os.WriteFile(out, []byte(rendered), 0600); err != nil {
		return fmt.Errorf("write %s: %w", out, err)
	}
	fmt.Printf("  Rendered: %s → %s\n", target.Template, target.Out)
	return nil
}

// renderTarget renders one template → out pair. Paths are relative to the
// checkout being configured, not to the caller's cwd, so `--all` behaves the
// same from any subdirectory.
func renderTarget(cfg *WetConfig, data map[string]any, treePath string, target RenderTarget, overrides map[string]string) error {
	tplPath := target.Template
	outPath := target.Out
	if envPopulateAll {
		tplPath = filepath.Join(treePath, tplPath)
		if outPath != "" {
			outPath = filepath.Join(treePath, outPath)
		}
	}

	body, err := os.ReadFile(tplPath)
	if err != nil {
		return fmt.Errorf("read template: %w", err)
	}
	if err := mergeOverrides(cfg, data, overrides); err != nil {
		return err
	}
	rendered, err := renderWetTemplate("template:"+filepath.Base(tplPath), string(body), data)
	if err != nil {
		return err
	}

	if envPopulateDryRun {
		if envPopulateAll {
			fmt.Printf("# ---- %s → %s ----\n", target.Template, target.Out)
		}
		fmt.Print(rendered)
		return nil
	}
	// Same reason as renderSetupTarget: the output's directory may not exist
	// yet, and ENOENT on a correct-looking path is a bad way to say so.
	if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
		return fmt.Errorf("create parent of %s: %w", outPath, err)
	}
	// 0600: the whole point of the secrets channel is that this file may hold
	// secrets, and it lands in a tree that outlives the command.
	if err := os.WriteFile(outPath, []byte(rendered), 0600); err != nil {
		return fmt.Errorf("write %s: %w", outPath, err)
	}
	fmt.Printf("  Rendered %s → %s\n", target.Template, target.Out)
	return nil
}

// readAllSecrets merges every --secrets-from source in the order given, so a
// later source overrides an earlier one on a shared key. Merging here rather
// than in a shell pipeline keeps that precedence visible in the command, and
// makes a failing source an error instead of a silently missing key: with
// `producer | jq -s add` a producer that dies still yields valid JSON from
// the survivors.
//
// Only one source may be stdin — a second "-" would read an exhausted pipe
// and contribute nothing, which is a typo, not an intent.
//
// root anchors relative paths. A `secrets_from:` entry in .wet.yaml is a fact
// about the repo, so it resolves against the checkout being configured and
// means the same thing no matter which directory `wet setup` was invoked from.
// A `--secrets-from` flag is a fact about the invocation, so it passes root ""
// and keeps ordinary cwd-relative semantics.
func readAllSecrets(root string, sources []string) (map[string]string, error) {
	merged := map[string]string{}
	seenStdin := false
	for _, src := range sources {
		if src == "-" {
			if seenStdin {
				return nil, fmt.Errorf(`--secrets-from - given twice; stdin can only be read once`)
			}
			seenStdin = true
		}
		resolved, err := resolveSecretsSource(root, src)
		if err != nil {
			return nil, err
		}
		values, err := readSecrets(resolved)
		if err != nil {
			return nil, err
		}
		for k, v := range values {
			merged[k] = v
		}
	}
	if len(merged) == 0 {
		return nil, nil
	}
	return merged, nil
}

// readSectionSecrets fetches the keys of one secrets: section from the store.
//
// It runs per render target rather than once for the whole config, because
// that is what keeps a section from leaking: the mobile app's env file gets
// the mobile section's keys and no others, even though both sections live in
// one vault item. An empty section name means the target declared none, which
// is not an error — most targets need no secrets at all.
func readSectionSecrets(cfg *WetConfig, section string) (map[string]string, error) {
	if section == "" || cfg.Secrets == nil {
		return nil, nil
	}
	if err := requireOP(); err != nil {
		return nil, err
	}
	refs, err := cfg.Secrets.ResolveSection(section)
	if err != nil {
		return nil, err
	}
	client := opClient{account: cfg.Secrets.Account}
	out := make(map[string]string, len(refs))
	for _, ref := range refs {
		value, err := client.read(ref.URI())
		if err != nil {
			return nil, err
		}
		out[ref.Key] = value
	}
	return out, nil
}

// resolveSecretsSource expands a secrets source into a path readSecrets can
// open.
//
// A secret store is machine-global far more often than it is per-checkout —
// one `op`/vault dump feeding every worktree — but a committed .wet.yaml
// cannot spell out `/Users/<someone>/…`. So `~` and `$VAR` expand here, which
// lets the shared config name a per-machine location portably:
//
//	secrets_from: ["~/.config/myapp/secrets.json"]
//
// Anything still relative is anchored to root (see readAllSecrets).
func resolveSecretsSource(root, src string) (string, error) {
	if src == "-" || src == "" {
		return src, nil
	}
	expanded := os.ExpandEnv(src)
	if expanded == "" {
		return "", fmt.Errorf("secrets source %q expanded to an empty path", src)
	}
	if expanded == "~" || strings.HasPrefix(expanded, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("secrets source %q: expand ~: %w", src, err)
		}
		expanded = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(expanded, "~"), "/"))
	}
	if !filepath.IsAbs(expanded) && root != "" {
		expanded = filepath.Join(root, expanded)
	}
	return expanded, nil
}

// readSecrets loads extra template values from one flat JSON object.
//
// The source is always explicit: a path, "-" for stdin, or empty for none.
// wet deliberately does NOT sniff stdin to decide. A hook inherits the
// parent's stdin, so an inherited-but-empty pipe (a script, CI, an agent
// harness) is indistinguishable from a pipe carrying JSON — and guessing
// wrong means blocking forever on a read that will never deliver. An
// explicit flag cannot hang.
//
// Values must be scalars. A nested object or array is rejected by name
// rather than stringified, because `{"a":1}` landing in an env file is a bug
// the user would only find at runtime.
func readSecrets(source string) (map[string]string, error) {
	if source == "" {
		return nil, nil
	}
	var raw []byte
	var err error
	if source == "-" {
		raw, err = io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}
	} else {
		raw, err = os.ReadFile(source)
		if err != nil {
			return nil, fmt.Errorf("read --secrets-from: %w", err)
		}
	}
	return parseSecretsJSON(raw, source)
}

func parseSecretsJSON(raw []byte, source string) (map[string]string, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, nil
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("%s: expected a flat JSON object: %w", source, err)
	}
	out := make(map[string]string, len(parsed))
	for k, v := range parsed {
		switch val := v.(type) {
		case string:
			out[k] = val
		case bool:
			out[k] = strconv.FormatBool(val)
		case float64:
			out[k] = strconv.FormatFloat(val, 'f', -1, 64)
		case nil:
			return nil, fmt.Errorf("%s: key %q is null — omit it or give it a value", source, k)
		default:
			return nil, fmt.Errorf("%s: key %q is not a scalar (got %T) — flatten it with jq first", source, k, v)
		}
	}
	return out, nil
}

// buildPopulateData assembles the template data for a populate run.
//
// The environment wins over the state file, and has to: post_create hooks run
// BEFORE `wet worktree setup` persists .wet-worktrees.json, so a hook-invoked
// populate would find no entry for its own worktree. BuildHookEnv has already
// exported everything needed (SLOT, BRANCH, ports, POOL_*), so in that context
// the environment is both authoritative and the only source available.
//
// Outside a hook (a hand-run populate after setup), the state file is the
// record of what this worktree was allocated.
func buildPopulateData(cfg *WetConfig, worktreePath string) (map[string]any, error) {
	if slot, ok := slotFromEnv(); ok {
		return dataFromEnv(cfg, slot)
	}
	state, err := loadConfig()
	if err != nil {
		return nil, fmt.Errorf("load wet state: %w", err)
	}
	wt := worktreeStateForPath(state, worktreePath)
	if wt == nil {
		return nil, fmt.Errorf("no wet state for %s — run `wet setup` first "+
			"(it works in the main repo and in worktrees), or invoke populate "+
			"from a .wet.yaml hook", worktreePath)
	}
	ports := make([]ResolvedPort, 0, len(wt.Ports))
	for _, p := range cfg.Ports {
		if v, ok := wt.Ports[p.Name]; ok {
			ports = append(ports, ResolvedPort{Name: p.Name, Value: v})
		}
	}
	return withRenderedEnv(cfg, BuildTemplateData(cfg, wt.Branch, wt.Index, ports, wt.Pools))
}

func slotFromEnv() (int, bool) {
	raw, ok := os.LookupEnv("SLOT")
	if !ok {
		return 0, false
	}
	slot, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, false
	}
	return slot, true
}

// dataFromEnv reconstructs template data from the variables BuildHookEnv
// exported. Ports and pools are read back by declared name so the values
// always match the ones the hook itself sees as $APP_PORT / $POOL_DEVICES.
func dataFromEnv(cfg *WetConfig, slot int) (map[string]any, error) {
	branch := os.Getenv("BRANCH")
	if branch == "" {
		return nil, fmt.Errorf("SLOT is set but BRANCH is not — refusing to guess the worktree's identity")
	}
	ports := make([]ResolvedPort, 0, len(cfg.Ports))
	for _, p := range cfg.Ports {
		raw, ok := os.LookupEnv(p.Name)
		if !ok {
			return nil, fmt.Errorf("port %s declared in .wet.yaml but missing from the hook environment", p.Name)
		}
		v, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("port %s=%q is not a number", p.Name, raw)
		}
		ports = append(ports, ResolvedPort{Name: p.Name, Value: v})
	}
	pools := map[string]string{}
	for name := range cfg.Pools {
		if v, ok := os.LookupEnv("POOL_" + strings.ToUpper(name)); ok {
			pools[name] = v
		}
	}
	return withRenderedEnv(cfg, BuildTemplateData(cfg, branch, slot, ports, pools))
}

// withRenderedEnv layers .env.local and the resolved cfg.Env values onto the
// base template data, so a template can reference {{.AUTH_BASE_URL}} exactly
// as .env.worktree and hooks do — derived once in `env:`, consumed everywhere.
func withRenderedEnv(cfg *WetConfig, data map[string]any) (map[string]any, error) {
	repoRoot, err := getMainRepoRoot()
	if err != nil {
		return nil, err
	}
	local, err := LoadEnvLocal(repoRoot)
	if err != nil {
		return nil, err
	}
	if _, err := ResolveEnvValues(cfg, data, local); err != nil {
		return nil, err
	}
	return data, nil
}

// mergeOverrides applies piped-in keys over the config-derived data. Ports and
// reserved fields are off-limits: letting a vault key named APP_PORT win would
// hand two worktrees the same port and surface as a mystery bind conflict.
func mergeOverrides(cfg *WetConfig, data map[string]any, overrides map[string]string) error {
	if len(overrides) == 0 {
		return nil
	}
	declared := make(map[string]bool, len(cfg.Ports))
	for _, p := range cfg.Ports {
		declared[p.Name] = true
	}
	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if declared[k] {
			return fmt.Errorf("secrets key %q collides with a port declared in .wet.yaml — "+
				"ports are allocated per worktree and cannot be overridden", k)
		}
		if isReservedTemplateField(k) {
			return fmt.Errorf("secrets key %q collides with a reserved template field (%s)",
				k, strings.Join(reservedTemplateFields, ", "))
		}
		// Shadowing an `env:` entry is legal — that is how a vault value
		// overrides a default — but it is also how a store that happens to
		// carry a staging URL silently replaces the worktree-derived one, and
		// the result reads as a mystery wrong hostname much later. Name it.
		if _, derived := cfg.Env[k]; derived {
			fmt.Fprintf(os.Stderr, "  warning: secrets key %q overrides the env: entry of the same name\n", k)
		}
		data[k] = overrides[k]
	}
	return nil
}

// worktreeStateForPath finds the state entry whose Path matches worktreePath,
// comparing resolved absolute paths so a symlinked or relative entry still
// matches. Returns nil when the worktree isn't tracked.
func worktreeStateForPath(state *WorktreeConfig, worktreePath string) *WorktreeState {
	want := resolvePath(worktreePath)
	for i := range state.Worktrees {
		if resolvePath(state.Worktrees[i].Path) == want {
			return &state.Worktrees[i]
		}
	}
	return nil
}

func resolvePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}
