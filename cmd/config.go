package cmd

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// Schema
// ---------------------------------------------------------------------------

// WetConfig is the parsed shape of `.wet.yaml` at a repo root. It tells wet
// what to copy, which ports to allocate, what to write into `.env.worktree`,
// and which shell commands to run on create/remove. The CLI contains no
// project-specific knowledge — everything lives here.
type WetConfig struct {
	ProjectName string              `yaml:"project_name"`
	MainBranch  string              `yaml:"main_branch,omitempty"`
	Copy        CopyConfig          `yaml:"copy"`
	PortStride  int                 `yaml:"port_stride"`
	Ports       []PortConfig        `yaml:"ports"`
	Env         map[string]string   `yaml:"env"`
	Hooks       HooksConfig         `yaml:"hooks"`
	Render      []RenderTarget      `yaml:"render,omitempty"`
	Secrets     *SecretsConfig      `yaml:"secrets,omitempty"`
	Pools       map[string][]string `yaml:"pools,omitempty"`
	PoolGrow    map[string]string   `yaml:"pool_grow,omitempty"`
	PR          PRConfig            `yaml:"pr,omitempty"`

	// LocalKeys lists the top-level keys .wet.local.yaml overrode, so setup
	// can say where a surprising value came from. Not part of the file format.
	LocalKeys []string `yaml:"-"`
}

// RenderTarget is one template → output pair, rendered by `wet env populate`
// (and by `wet setup`, before hooks run).
//
// A monorepo has one per package: web_elixir/.env.tpl → web_elixir/.env.dev,
// mobile_elixir/.env.tpl → mobile_elixir/.env. Each carries its own secret
// sources, because packages don't need the same secrets — a backend needs the
// vault, a mobile app usually needs nothing.
//
// Declaring targets here rather than as hook strings is what makes
// `wet env populate --all --dry-run` possible: wet can report every file it
// would write for this slot, which opaque `sh -c` lines can't.
type RenderTarget struct {
	Template    string   `yaml:"template"`
	Out         string   `yaml:"out"`
	SecretsFrom []string `yaml:"secrets_from,omitempty"`
	// Secrets names a section of the top-level secrets: block, whose keys are
	// fetched from the store and merged in like any other --secrets-from
	// source. It is a label, not a path: the same section can feed two
	// targets, and the vault never learns which directory consumes it.
	Secrets string `yaml:"secrets,omitempty"`
}

// PRConfig optionally configures `wet pr create` behavior.
type PRConfig struct {
	Base         string `yaml:"base,omitempty"`          // base branch (default "main")
	BodyTemplate string `yaml:"body_template,omitempty"` // Go text/template for PR body
}

// SecretsConfig describes where this repo's secrets live and which of them
// each render target needs.
//
// Sections are the second dimension: `backend` and `mobile` are labels for
// consumers, not for storage. Both can name the same field of the same item —
// GOOGLE_CLIENT_ID is one credential whether the backend or the app reads it —
// so the vault stays free of per-consumer duplicates, and a rotation is one
// edit.
type SecretsConfig struct {
	// Store is the secret manager. Only "op" (1Password) is implemented; the
	// field exists so a second store is a value here rather than a rewrite.
	Store string `yaml:"store"`
	// Vault and Item may be empty: `wet op vault` then asks, and writes the
	// answer back, so a fresh clone is not a scavenger hunt for names.
	Vault string `yaml:"vault,omitempty"`
	Item  string `yaml:"item,omitempty"`
	// Account is the 1Password account (sign-in address or UUID). Empty means
	// whichever account the shell is signed into, or ask when ambiguous.
	Account string `yaml:"account,omitempty"`
	// Sections maps a section name to its env keys: ENV_KEY -> field name on
	// Item, "Other Item/field" for a different item in the same vault, or a
	// full "op://vault/item/field".
	Sections map[string]map[string]string `yaml:"sections"`
}

// CopyConfig lists files and directories copied from the main repo into each
// new worktree. Paths are relative to the repo root.
type CopyConfig struct {
	Files []string `yaml:"files"`
	Dirs  []string `yaml:"dirs"`
}

// PortConfig declares a single port. Stride defaults to WetConfig.PortStride
// if unset or non-positive.
type PortConfig struct {
	Name   string `yaml:"name"`
	Base   int    `yaml:"base"`
	Stride int    `yaml:"stride,omitempty"`
}

// HooksConfig lists shell commands to run around worktree lifecycle events.
type HooksConfig struct {
	PostCreate []string `yaml:"post_create,omitempty"`
	PreRemove  []string `yaml:"pre_remove,omitempty"`
}

// ---------------------------------------------------------------------------
// Loader
// ---------------------------------------------------------------------------

// ErrConfigMissing is returned when `.wet.yaml` does not exist at the repo
// root. Every command that needs config treats this as fatal.
var ErrConfigMissing = errors.New(".wet.yaml not found at repo root")

const wetConfigFileName = ".wet.yaml"

// exampleConfigFileName is the annotated schema shipped in the binary and
// written beside the installed skill. Named once so the installer, the
// scaffolder, and the docs pointer cannot disagree about it.
const exampleConfigFileName = "wet.example.yaml"

// wetLocalConfigFileName is an optional, gitignored overlay sitting beside
// `.wet.yaml`. It exists because some of the config is a fact about a
// workstation rather than about the repo — a `pools:` list of device IDs is
// the clearest case: `RFCR921L4DP` is meaningless on a teammate's machine, so
// a committed pool either gets hand-edited by everyone or gets committed
// wrong.
//
// Everything is optional: no file at all is normal, and a file names only the
// keys it wants to change. Anything it doesn't mention falls through to
// `.wet.yaml` untouched.
const wetLocalConfigFileName = ".wet.local.yaml"

// LoadWetConfig reads, parses, defaults, and validates `.wet.yaml` from the
// given repo root, overlaying the machine's `.wet.local.yaml` from wherever
// the trunk is checked out (see LocalConfigDir).
func LoadWetConfig(repoRoot string) (*WetConfig, error) {
	return LoadWetConfigWithLocal(repoRoot, LocalConfigDir(repoRoot, repoRoot))
}

// LocalConfigDir picks the directory `.wet.local.yaml` is read from: the
// checkout that has `main_branch:` checked out, falling back to fallbackDir.
//
// The overlay is one per machine and gitignored, so no worktree has a copy and
// it has to be read from some other checkout. Reading it from git's "main
// working tree" is wrong whenever that isn't the checkout anyone works in — a
// first clone left parked on an unrelated branch still answers to
// `--git-common-dir`, and the overrides silently vanish, leaving a worktree
// claiming pool items nobody configured.
//
// The trunk is read from configDir's own `.wet.yaml` without validating it:
// this runs before the config is loaded, and a config that only validates with
// its overlay applied must not fail here first.
func LocalConfigDir(configDir, fallbackDir string) string {
	if dir, ok := checkoutForBranch(configDir, probeMainBranch(configDir)); ok {
		return dir
	}
	return fallbackDir
}

// probeMainBranch reads just `main_branch:` out of a .wet.yaml. Any problem
// with the file is left for the real loader to report.
func probeMainBranch(configDir string) string {
	data, err := os.ReadFile(filepath.Join(configDir, wetConfigFileName))
	if err != nil {
		return DefaultMainBranch
	}
	var probe struct {
		MainBranch string `yaml:"main_branch"`
	}
	if err := yaml.Unmarshal(data, &probe); err != nil || probe.MainBranch == "" {
		return DefaultMainBranch
	}
	return probe.MainBranch
}

// LoadWetConfigWithLocal loads `.wet.yaml` from configDir and overlays
// `.wet.local.yaml` from localDir.
//
// The two are separate because they answer to different scopes. The config can
// come from a worktree's own checkout — that's how a `.wet.yaml` change is
// testable where it's written. The local overlay is one per machine, so it is
// read from the checkout holding the trunk (see LocalConfigDir): it's
// gitignored, so a worktree checkout never has a copy, and looking for one
// beside the worktree's config would silently drop every override — leaving
// the worktree claiming pool items nobody configured.
//
// An empty localDir means "no overlay", used by callers that have already
// decided there is none to apply.
func LoadWetConfigWithLocal(configDir, localDir string) (*WetConfig, error) {
	repoRoot := configDir
	path := filepath.Join(repoRoot, wetConfigFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrConfigMissing
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var base map[string]any
	if err := yaml.Unmarshal(data, &base); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	// An empty localDir means no overlay. Joining onto "" would produce a
	// bare relative path and read whatever .wet.local.yaml happens to sit in
	// the process's cwd.
	var overridden []string
	localPath := ""
	if localDir != "" {
		localPath = filepath.Join(localDir, wetLocalConfigFileName)
		overridden, err = mergeLocalConfig(base, localPath)
		if err != nil {
			return nil, err
		}
	}

	merged, err := yaml.Marshal(base)
	if err != nil {
		return nil, fmt.Errorf("merge %s: %w", localPath, err)
	}
	var cfg WetConfig
	if err := yaml.Unmarshal(merged, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg.LocalKeys = overridden
	applyWetConfigDefaults(&cfg)
	// Validate the merged result, not either half: a local file that breaks
	// the schema has to fail here, where the file name is still in hand.
	if err := cfg.Validate(); err != nil {
		if len(overridden) > 0 {
			return nil, fmt.Errorf("invalid config (%s merged over %s): %w", localPath, path, err)
		}
		return nil, fmt.Errorf("invalid %s: %w", path, err)
	}
	return &cfg, nil
}

// mergeLocalConfig overlays wetLocalConfigFileName onto base in place and
// returns the top-level keys it touched. A missing file is not an error.
//
// Maps merge key by key, so a local file can override one `env:` entry without
// restating the rest. Sequences replace wholesale — a local `pools.devices` is
// *your* device list, not yours appended to a teammate's, and appending would
// hand you items that don't exist on this machine.
func mergeLocalConfig(base map[string]any, localPath string) ([]string, error) {
	data, err := os.ReadFile(localPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", localPath, err)
	}
	var local map[string]any
	if err := yaml.Unmarshal(data, &local); err != nil {
		return nil, fmt.Errorf("parse %s: %w", localPath, err)
	}
	keys := make([]string, 0, len(local))
	for k := range local {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		base[k] = mergeYAMLValue(base[k], local[k])
	}
	return keys, nil
}

// mergeYAMLValue merges one value: maps recurse, everything else replaces.
func mergeYAMLValue(base, local any) any {
	baseMap, baseOK := base.(map[string]any)
	localMap, localOK := local.(map[string]any)
	if !baseOK || !localOK {
		return local
	}
	out := make(map[string]any, len(baseMap)+len(localMap))
	for k, v := range baseMap {
		out[k] = v
	}
	for k, v := range localMap {
		out[k] = mergeYAMLValue(baseMap[k], v)
	}
	return out
}

// LoadWetConfigForWorktree loads `.wet.yaml` for an operation running against
// worktreePath, preferring the worktree's own copy and falling back to the
// main repo root. It returns the config and the path it came from.
//
// The worktree has to win. Hooks act on the worktree's files — its
// docker-compose.yml, its Justfile recipes, its scripts — so a config from
// some other checkout can describe a tree that doesn't exist here. Reading
// only from the main repo also makes a `.wet.yaml` change impossible to test
// where it's written: the edit sits inert in the worktree while setup keeps
// running the main repo's version, and it first takes effect after a merge,
// on whatever checkout the main repo happens to be on. That's a blind deploy
// of the one file whose whole job is to bring a worktree up correctly.
//
// Shared allocation state (.wet-worktrees.json) still lives in the main repo
// — slots and pools are global by definition. Only the config is local.
// LoadWetConfigForWorktreeTestable is LoadWetConfigForWorktree without the
// path it came from, for tests that only care about the merged result.
func LoadWetConfigForWorktreeTestable(worktreePath, repoRoot string) (*WetConfig, error) {
	cfg, _, err := LoadWetConfigForWorktree(worktreePath, repoRoot)
	return cfg, err
}

func LoadWetConfigForWorktree(worktreePath, repoRoot string) (*WetConfig, string, error) {
	cfg, err := LoadWetConfigWithLocal(worktreePath, LocalConfigDir(worktreePath, repoRoot))
	if err == nil {
		return cfg, filepath.Join(worktreePath, wetConfigFileName), nil
	}
	if err != ErrConfigMissing {
		return nil, filepath.Join(worktreePath, wetConfigFileName), err
	}
	cfg, err = LoadWetConfigWithLocal(repoRoot, LocalConfigDir(repoRoot, repoRoot))
	return cfg, filepath.Join(repoRoot, wetConfigFileName), err
}

// DefaultMainBranch is what "the main branch" means with no `main_branch:` in
// the config. A repo whose trunk is called something else (development,
// trunk, …) says so once, in .wet.yaml, rather than in every caller.
const DefaultMainBranch = "main"

// MainBranchName is the branch wet treats as the trunk: what new worktrees
// branch off, what `wet pr create` targets by default, and the branch it
// refuses to open a PR from.
func (c *WetConfig) MainBranchName() string {
	if c != nil && c.MainBranch != "" {
		return c.MainBranch
	}
	return DefaultMainBranch
}

func applyWetConfigDefaults(cfg *WetConfig) {
	if cfg.PortStride <= 0 {
		cfg.PortStride = 10
	}
	for i := range cfg.Ports {
		if cfg.Ports[i].Stride <= 0 {
			cfg.Ports[i].Stride = cfg.PortStride
		}
	}
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

var (
	// Project names embed directly into COMPOSE_PROJECT_NAME, which Docker
	// Compose requires to match [a-z0-9][a-z0-9_-]*.
	projectNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

	// Port names are environment variables, so we use the standard shell
	// convention: [A-Z_][A-Z0-9_]*. This also keeps them distinct from the
	// TitleCase reserved template fields.
	portNameRE = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

	// Pool names are lowercase snake_case so they're visually distinct from
	// UPPER-case ports. They become `$POOL_<UPPER>` env vars for hooks.
	poolNameRE = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
)

// PoolGrowAndroidEmulator is the only `pool_grow:` strategy wet implements:
// on exhaustion it creates a new AVD, cold boots it, and appends the serial to
// the local config. Validate() and AllocatePools both compare against this
// constant so the accepted value and the dispatch cannot drift apart.
const PoolGrowAndroidEmulator = "android-emulator"

// reservedTemplateFields are the template data keys that wet injects
// automatically into every `env:` and hook render. A port or pool declared
// with one of these names (case-insensitively) would shadow the reserved
// field and confuse users, so we reject at load time.
var reservedTemplateFields = []string{"ProjectName", "Branch", "BranchSafe", "Slot", "Pools", "Handles"}

// Validate enforces the schema rules documented in .wet.yaml's comments.
// All error paths return errors actionable from the user's perspective.
func (c *WetConfig) Validate() error {
	if c.ProjectName == "" {
		return errors.New("project_name is required")
	}
	if !projectNameRE.MatchString(c.ProjectName) {
		return fmt.Errorf("project_name %q must match %s", c.ProjectName, projectNameRE.String())
	}

	for _, p := range c.Copy.Files {
		if err := validateCopyPath(p); err != nil {
			return fmt.Errorf("copy.files: %w", err)
		}
	}
	for _, p := range c.Copy.Dirs {
		if err := validateCopyPath(p); err != nil {
			return fmt.Errorf("copy.dirs: %w", err)
		}
	}

	outs := make(map[string]int, len(c.Render))
	for i, r := range c.Render {
		if r.Template == "" || r.Out == "" {
			return fmt.Errorf("render[%d]: both template and out are required", i)
		}
		if err := validateCopyPath(r.Template); err != nil {
			return fmt.Errorf("render[%d].template: %w", i, err)
		}
		if err := validateCopyPath(r.Out); err != nil {
			return fmt.Errorf("render[%d].out: %w", i, err)
		}
		if r.Template == r.Out {
			return fmt.Errorf("render[%d]: template and out are the same file (%s)", i, r.Out)
		}
		// Two targets writing one file means the result depends on ordering,
		// and the loser's secrets silently vanish.
		if prev, dup := outs[r.Out]; dup {
			return fmt.Errorf("render[%d]: out %q already written by render[%d]", i, r.Out, prev)
		}
		outs[r.Out] = i
		stdin := 0
		for _, src := range r.SecretsFrom {
			if src == "" {
				return fmt.Errorf("render[%d].secrets_from: empty source", i)
			}
			if src == "-" {
				stdin++
			}
		}
		if stdin > 1 {
			return fmt.Errorf("render[%d].secrets_from: stdin listed more than once", i)
		}
	}

	seen := make(map[string]bool, len(c.Ports))
	for i, port := range c.Ports {
		if !portNameRE.MatchString(port.Name) {
			return fmt.Errorf("ports[%d]: name %q must match %s", i, port.Name, portNameRE.String())
		}
		if isReservedTemplateField(port.Name) {
			return fmt.Errorf("ports[%d]: name %q collides with a reserved template field (%s)",
				i, port.Name, strings.Join(reservedTemplateFields, ", "))
		}
		if seen[port.Name] {
			return fmt.Errorf("ports[%d]: duplicate name %q", i, port.Name)
		}
		seen[port.Name] = true
		if port.Base <= 0 || port.Base >= 65536 {
			return fmt.Errorf("ports[%d] %q: base %d out of range (1..65535)", i, port.Name, port.Base)
		}
		if port.Stride <= 0 {
			return fmt.Errorf("ports[%d] %q: stride must be positive", i, port.Name)
		}
	}

	// Pools validation — name shape, reserved-name collision, non-empty
	// item list, per-pool item uniqueness, and no empty-string items.
	for name, items := range c.Pools {
		if !poolNameRE.MatchString(name) {
			return fmt.Errorf("pools[%q]: name must match %s", name, poolNameRE.String())
		}
		if isReservedTemplateField(name) {
			return fmt.Errorf("pools[%q]: name collides with a reserved template field (%s)",
				name, strings.Join(reservedTemplateFields, ", "))
		}
		if len(items) == 0 {
			return fmt.Errorf("pools[%q]: item list must not be empty", name)
		}
		seenItems := make(map[string]bool, len(items))
		for i, item := range items {
			if item == "" {
				return fmt.Errorf("pools[%q]: item at index %d must not be empty", name, i)
			}
			if seenItems[item] {
				return fmt.Errorf("pools[%q]: duplicate item %q", name, item)
			}
			seenItems[item] = true
		}
	}

	// pool_grow validation — every key must name a declared pool, and every
	// value must be a strategy wet implements. Without this an unrecognized
	// value is silently ignored and only surfaces much later as a misleading
	// "pool exhausted" error that never mentions pool_grow.
	growNames := make([]string, 0, len(c.PoolGrow))
	for name := range c.PoolGrow {
		growNames = append(growNames, name)
	}
	sort.Strings(growNames)
	for _, name := range growNames {
		if _, ok := c.Pools[name]; !ok {
			return fmt.Errorf("pool_grow[%q]: no pool named %q is declared under pools: — declare the pool or remove this entry", name, name)
		}
		if strategy := c.PoolGrow[name]; strategy != PoolGrowAndroidEmulator {
			return fmt.Errorf("pool_grow[%q]: unknown strategy %q — the only supported value is %q", name, strategy, PoolGrowAndroidEmulator)
		}
	}

	if err := c.validateSecrets(); err != nil {
		return err
	}

	// Parse env templates eagerly so syntax errors fail at load, not at apply.
	for key, tmpl := range c.Env {
		if key == "" {
			return errors.New("env: key must not be empty")
		}
		if _, err := template.New("env:" + key).Parse(tmpl); err != nil {
			return fmt.Errorf("env.%s: %w", key, err)
		}
	}

	for i, cmd := range c.Hooks.PostCreate {
		if _, err := template.New(fmt.Sprintf("hooks.post_create[%d]", i)).Parse(cmd); err != nil {
			return fmt.Errorf("hooks.post_create[%d]: %w", i, err)
		}
	}
	for i, cmd := range c.Hooks.PreRemove {
		if _, err := template.New(fmt.Sprintf("hooks.pre_remove[%d]", i)).Parse(cmd); err != nil {
			return fmt.Errorf("hooks.pre_remove[%d]: %w", i, err)
		}
	}
	return nil
}

// validateSecrets checks the secrets: block and every render target that names
// a section of it. A render target pointing at a section that does not exist
// is the failure mode worth catching at load: without this it would surface as
// a template field that will not resolve, long after the typo.
func (c *WetConfig) validateSecrets() error {
	if c.Secrets != nil {
		if c.Secrets.Store != secretsStoreOP {
			return fmt.Errorf("secrets.store %q is not supported (known: %s)",
				c.Secrets.Store, secretsStoreOP)
		}
		if len(c.Secrets.Sections) == 0 {
			return errors.New("secrets: needs at least one section")
		}
		for section, keys := range c.Secrets.Sections {
			if section == "" {
				return errors.New("secrets.sections: section name must not be empty")
			}
			if len(keys) == 0 {
				return fmt.Errorf("secrets.sections.%s: no keys", section)
			}
			for key, ref := range keys {
				if key == "" {
					return fmt.Errorf("secrets.sections.%s: key must not be empty", section)
				}
				if ref == "" {
					return fmt.Errorf("secrets.sections.%s.%s: empty reference", section, key)
				}
				// The key becomes a template field, so the same names wet
				// reserves for its own data are off limits — a secret named
				// Branch would shadow it and render the wrong value.
				if isReservedTemplateField(key) {
					return fmt.Errorf("secrets.sections.%s.%s: %s is reserved by wet", section, key, key)
				}
			}
		}
	}

	for i, r := range c.Render {
		if r.Secrets == "" {
			continue
		}
		if c.Secrets == nil {
			return fmt.Errorf("render[%d].secrets: %q named, but there is no secrets: block", i, r.Secrets)
		}
		if _, ok := c.Secrets.Sections[r.Secrets]; !ok {
			return fmt.Errorf("render[%d].secrets: unknown section %q (known: %s)",
				i, r.Secrets, strings.Join(sortedKeys(c.Secrets.Sections), ", "))
		}
	}
	return nil
}

// sortedKeys returns a map's keys in a stable order, for error messages that
// must not vary between runs.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// MaskedFailureHooks reports post_create hooks whose shell body can turn a
// failing command into exit 0, and returns one human-readable line per
// offender.
//
// wet's contract is that a nonzero post_create hook aborts setup — a worktree
// that is only half-configured must not go on to boot a stack. That contract
// is only as good as the exit code the hook hands back, and `a || b`, `a | b`,
// and a trailing `; true` all discard it. Such a hook fails invisibly: the
// error text scrolls past, setup prints its success summary, and the breakage
// surfaces much later somewhere unrelated.
//
// This is a warning rather than a load error because `a || exit 1` is a
// legitimate precondition guard and does propagate — wet can't tell the two
// apart without parsing shell. pre_remove is exempt: teardown is best-effort
// by design (killing an emulator that's already gone shouldn't fail a pop).
func MaskedFailureHooks(c *WetConfig) []string {
	var out []string
	for i, body := range c.Hooks.PostCreate {
		trimmed := strings.TrimRight(strings.TrimSpace(body), ";")
		var why string
		switch {
		case strings.Contains(body, "||"):
			why = "contains `||`"
		case strings.HasSuffix(trimmed, "; true") || strings.HasSuffix(trimmed, "&& true"):
			why = "ends in `true`"
		case strings.Contains(body, "|"):
			why = "contains a pipe (only the last command's status is reported)"
		}
		if why == "" {
			continue
		}
		out = append(out, fmt.Sprintf(
			"hooks.post_create[%d] %s, so a failure inside it can exit 0 and wet cannot abort.\n"+
				"    Move the fallback into a script or recipe that handles the one condition it\n"+
				"    means to recover from and lets every other failure propagate.\n"+
				"    Ignore this if the branch ends in `exit`/`return` — that does propagate.\n"+
				"    %s", i, why, body))
	}
	return out
}

func validateCopyPath(p string) error {
	if p == "" {
		return errors.New("path must not be empty")
	}
	if filepath.IsAbs(p) {
		return fmt.Errorf("path %q must be relative to repo root", p)
	}
	clean := filepath.ToSlash(filepath.Clean(p))
	if clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return fmt.Errorf("path %q must not escape the repo root", p)
	}
	return nil
}

func isReservedTemplateField(name string) bool {
	upper := strings.ToUpper(name)
	for _, r := range reservedTemplateFields {
		if strings.ToUpper(r) == upper {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Port allocation
// ---------------------------------------------------------------------------

// ResolvedPort is a declared port's concrete value for a specific slot.
type ResolvedPort struct {
	Name  string
	Value int
}

// ResolvePorts computes concrete port values for `slot`, honoring each port's
// (possibly overridden) stride.
func (c *WetConfig) ResolvePorts(slot int) []ResolvedPort {
	out := make([]ResolvedPort, len(c.Ports))
	for i, p := range c.Ports {
		out[i] = ResolvedPort{
			Name:  p.Name,
			Value: p.Base + slot*p.Stride,
		}
	}
	return out
}

// NextFreeSlot returns the lowest positive integer not present in `used`.
// Slot 0 is implicitly reserved for the main dev environment, so the first
// worktree gets slot 1.
func NextFreeSlot(used []int) int {
	taken := make(map[int]bool, len(used)+1)
	taken[0] = true
	for _, s := range used {
		taken[s] = true
	}
	for i := 1; ; i++ {
		if !taken[i] {
			return i
		}
	}
}

// portsFree reports whether every host port resolved for `slot` is currently
// bindable. The state file (.wet-worktrees.json) only tracks worktrees wet
// itself created, so a stack standing on these ports but unknown to the
// allocator — a second clone's `just dev`, the main `development` checkout
// booted as its own compose project, a worktree popped without teardown — is
// invisible to NextFreeSlot. Probing the actual port is the only way to keep
// the allocator from handing out a slot that then dies at `docker compose up`
// with "port is already allocated".
func (c *WetConfig) portsFree(slot int) bool {
	for _, p := range c.ResolvePorts(slot) {
		// Bind on all interfaces (":port"), matching Docker's default
		// 0.0.0.0 host mapping, so a docker-proxy holding the port is seen.
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", p.Value))
		if err != nil {
			return false
		}
		_ = ln.Close()
	}
	return true
}

// NextFreeSlotWithFreePorts behaves like NextFreeSlot but additionally skips
// any candidate slot whose resolved host ports are already bound. `used`
// carries the slots claimed by tracked worktrees; this layer catches the
// untracked stacks the state file cannot see. It walks upward from the
// state-file candidate, treating each port-occupied slot as taken, until it
// reaches one whose every port is free. Because ports climb with the slot,
// this always terminates above the highest running stack.
func (c *WetConfig) NextFreeSlotWithFreePorts(used []int) int {
	taken := append([]int(nil), used...)
	for {
		slot := NextFreeSlot(taken)
		if c.portsFree(slot) {
			return slot
		}
		taken = append(taken, slot)
	}
}

// ---------------------------------------------------------------------------
// Template data + sanitizer
// ---------------------------------------------------------------------------

// BuildTemplateData assembles the map passed to text/template when rendering
// `env:` values and hook commands. Known fields sit alongside dynamic per-port
// keys in the same map, so `{{.APP_PORT}}` and `{{.ProjectName}}` are both
// single-dot lookups. Pool allocations are exposed as a nested
// `map[string]string` under the reserved key `Pools`, so templates reference
// them as `{{.Pools.devices}}`. Callers that have no pool allocations may
// pass nil — `Pools` still appears in the map as an empty sub-map, so
// `{{.Pools.foo}}` raises the usual missingkey error for undeclared pools.
// BuildTemplateData assembles the values every template and hook sees.
//
// Pools appear twice on purpose. {{.Pools.x}} is the item the pool owns — the
// durable name, what teardown reasons about. {{.Handles.x}} is the address
// that item answers to right now: for an emulator the live adb serial, for
// anything else the item itself. A command targets the handle; the lifecycle
// reasons about the item.
func BuildTemplateData(cfg *WetConfig, branch string, slot int, ports []ResolvedPort, pools map[string]string) map[string]any {
	data := map[string]any{
		"ProjectName": cfg.ProjectName,
		"Branch":      branch,
		"BranchSafe":  SanitizeBranch(branch),
		"Slot":        slot,
	}
	for _, p := range ports {
		data[p.Name] = p.Value
	}
	if pools == nil {
		pools = map[string]string{}
	}
	data["Pools"] = pools
	// Default handles to the items, so a config that never touches emulators
	// (and every code path that doesn't resolve them) still renders.
	handles := make(map[string]string, len(pools))
	for k, v := range pools {
		handles[k] = v
	}
	data["Handles"] = handles
	return data
}

// WithPoolHandles replaces the default handle map with resolved addresses.
func WithPoolHandles(data map[string]any, handles map[string]string) map[string]any {
	if len(handles) > 0 {
		data["Handles"] = handles
	}
	return data
}

// AllocatePools picks one item per declared pool for a new worktree,
// avoiding items already claimed by any entry in `existing`. For each pool,
// iteration is in declared order and the first unclaimed item wins — so a
// claim set of {items[0], items[2]} releases items[1] before items[3].
//
// When a pool is exhausted and pool_grow has a strategy for it (e.g.
// "android-emulator"), the pool is auto-expanded: a new resource is created,
// added to .wet.yaml, and returned. repoRoot is needed to update the config
// file on disk.
//
// Returns a map of pool name → allocated item on success. Returns an error
// naming the pool and listing current claimants if a pool is exhausted and
// no grow strategy is configured.
// Pools are processed in sorted order so exhaustion errors are stable.
//
// Repos without a `pools:` block get an empty map and no error, which is
// the back-compat path: existing configs keep working unchanged.
func AllocatePools(cfg *WetConfig, existing []WorktreeState, repoRoot string, headless ...bool) (map[string]string, error) {
	out := make(map[string]string, len(cfg.Pools))
	if len(cfg.Pools) == 0 {
		return out, nil
	}
	poolNames := make([]string, 0, len(cfg.Pools))
	for name := range cfg.Pools {
		poolNames = append(poolNames, name)
	}
	sort.Strings(poolNames)
	for _, name := range poolNames {
		items := cfg.Pools[name]
		claimed := make(map[string]bool, len(existing))
		for _, ws := range existing {
			if v, ok := ws.Pools[name]; ok && v != "" {
				claimed[v] = true
			}
		}
		picked := ""
		for _, item := range items {
			if !claimed[item] {
				picked = item
				break
			}
		}
		if picked == "" {
			// Check for a grow strategy before erroring.
			strategy := cfg.PoolGrow[name]
			if strategy == PoolGrowAndroidEmulator {
				hl := len(headless) > 0 && headless[0]
				newItem, err := growAndroidEmulatorPool(cfg, name, repoRoot, hl)
				if err != nil {
					return nil, fmt.Errorf("pool %q exhausted and auto-grow failed: %w", name, err)
				}
				picked = newItem
			} else {
				claims := make([]string, 0, len(existing))
				for _, ws := range existing {
					if v, ok := ws.Pools[name]; ok && v != "" {
						claims = append(claims, fmt.Sprintf("branch=%s item=%s", ws.Branch, v))
					}
				}
				return nil, fmt.Errorf(
					"pool %q exhausted — all %d items claimed by existing worktrees (%s). Pop a worktree first or add more items to the pool",
					name, len(items), strings.Join(claims, ", "),
				)
			}
		}
		out[name] = picked
	}
	return out, nil
}

// SanitizeBranch produces a Docker-Compose-safe variant of a git branch name.
//
// Per https://docs.docker.com/compose/how-tos/project-name/, a Compose project
// name must contain only lowercase letters, decimal digits, dashes, and
// underscores, and must begin with a lowercase letter or decimal digit.
//
// We lowercase the input, replace any character outside [a-z0-9_-] with `-`,
// collapse consecutive dashes, and trim leading/trailing `-_`. If the result
// would be empty or start with a non-alphanumeric, we return "branch" as a
// last-resort fallback — callers still compose it as `<project>-<safe>` so
// the full name starts with the project's first character regardless.
func SanitizeBranch(branch string) string {
	var b strings.Builder
	b.Grow(len(branch))
	for _, r := range strings.ToLower(branch) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := b.String()
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	out = strings.Trim(out, "-_")
	if out == "" {
		return "branch"
	}
	return out
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

// envLocalFileName is a gitignored, per-machine override file at the repo
// root. It holds facts that vary by workstation rather than by worktree — a
// LAN IP for a physical device, a personal staging hostname — which is why it
// can't live in the checked-in `.wet.yaml`.
const envLocalFileName = ".env.local"

// LoadEnvLocal reads `.env.local` from repoRoot. A missing file is not an
// error: the file is optional by design.
//
// The format is the env-file subset everyone already writes: KEY=value, one
// per line, `#` comments and blank lines ignored, surrounding single or
// double quotes stripped. No interpolation — these are literal values, and a
// `$VAR` inside one stays a `$VAR`.
func LoadEnvLocal(repoRoot string) (map[string]string, error) {
	path := filepath.Join(repoRoot, envLocalFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	out := map[string]string{}
	for n, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, value, found := strings.Cut(trimmed, "=")
		if !found {
			return nil, fmt.Errorf("%s:%d: expected KEY=value, got %q", path, n+1, trimmed)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("%s:%d: empty key", path, n+1)
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' && value[len(value)-1] == '"' ||
			value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		}
		out[key] = value
	}
	return out, nil
}

// ResolveEnvValues renders every `env:` entry into data, layering machine
// overrides on top, and returns the resolved values in declaration-independent
// order.
//
// Two properties this exists for:
//
//  1. **`env:` entries may reference each other.** A monorepo derives
//     AUTH_BASE_URL from DEV_HOST and PHX_PORT, and every package's template
//     then consumes AUTH_BASE_URL rather than re-deriving it. Rendering in
//     sorted key order can't do that (AUTH_BASE_URL sorts before DEV_HOST), so
//     this repeats passes until a pass resolves nothing new — a fixed point.
//     Whatever is left is either a genuine typo or a reference cycle, and both
//     are reported by name.
//
//  2. **`.env.local` wins over `env:`.** A key present in the machine override
//     file is never re-rendered from config, so `env:` entries act as
//     defaults. That's what lets a checked-in `DEV_HOST: localhost` be
//     overridden by a LAN IP without any per-machine edit to `.wet.yaml`.
func ResolveEnvValues(cfg *WetConfig, data map[string]any, overrides map[string]string) (map[string]string, error) {
	resolved := make(map[string]string, len(cfg.Env)+len(overrides))
	for k, v := range overrides {
		data[k] = v
		resolved[k] = v
	}

	pending := make(map[string]string, len(cfg.Env))
	for k, v := range cfg.Env {
		if _, overridden := overrides[k]; overridden {
			continue // machine override wins; don't re-render it from config
		}
		pending[k] = v
	}

	for len(pending) > 0 {
		keys := make([]string, 0, len(pending))
		for k := range pending {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		progressed := false
		var lastErr error
		for _, k := range keys {
			rendered, err := renderWetTemplate("env:"+k, pending[k], data)
			if err != nil {
				lastErr = err
				continue // may depend on a key a later pass supplies
			}
			data[k] = rendered
			resolved[k] = rendered
			delete(pending, k)
			progressed = true
		}
		if !progressed {
			stuck := make([]string, 0, len(pending))
			for k := range pending {
				stuck = append(stuck, k)
			}
			sort.Strings(stuck)
			return nil, fmt.Errorf("env: cannot resolve %s — unknown field or reference cycle: %w",
				strings.Join(stuck, ", "), lastErr)
		}
	}
	return resolved, nil
}

// RenderEnvFileContent produces the text body of `.env.worktree`. Declared
// ports are written first as `<NAME>=<value>` lines, followed by every
// resolved env value. Keys are emitted in sorted order for deterministic
// output.
func RenderEnvFileContent(cfg *WetConfig, data map[string]any, ports []ResolvedPort, local map[string]string) (string, error) {
	var b strings.Builder
	b.WriteString("# Generated by wet setup. Do not edit by hand.\n")
	for _, p := range ports {
		fmt.Fprintf(&b, "%s=%d\n", p.Name, p.Value)
	}
	values, err := ResolveEnvValues(cfg, data, local)
	if err != nil {
		return "", err
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, values[k])
	}
	return b.String(), nil
}

// RenderHookCommand applies text/template expansion to a single hook command
// string. Shell expansion (of `$VAR` etc.) happens later when the command is
// passed to `sh -c`.
func RenderHookCommand(name, body string, data map[string]any) (string, error) {
	return renderWetTemplate(name, body, data)
}

// BuildHookEnv constructs the environment slice to pass to a hook command's
// `sh -c` invocation. It starts from the caller's current environment and
// layers on: uppercase mirrors of the canonical template fields
// (`PROJECT_NAME`, `BRANCH`, `BRANCH_SAFE`, `SLOT`), one entry per declared
// port (`APP_PORT=8090`, …), one entry per pool allocation
// (`POOL_<UPPER_NAME>=<item>`), and one entry per rendered `cfg.Env` value
// in sorted key order. The same variables that appear in `.env.worktree`
// are therefore available to hooks as real environment variables, so hook
// commands can reference them either via `{{.APP_PORT}}` (template, expanded
// before `sh` runs) or `$APP_PORT` (shell, expanded when the command runs).
//
// Pool names are uppercased for the env var name, so a pool declared as
// `devices` becomes `$POOL_DEVICES`; a pool declared as `test_fixtures`
// becomes `$POOL_TEST_FIXTURES`. Mixed-case pool names are rejected at
// validation time, so there's no ambiguity.
func BuildHookEnv(cfg *WetConfig, data map[string]any, ports []ResolvedPort, local map[string]string) ([]string, error) {
	env := append([]string{}, os.Environ()...)
	env = append(env,
		"PROJECT_NAME="+cfg.ProjectName,
		fmt.Sprintf("BRANCH=%v", data["Branch"]),
		fmt.Sprintf("BRANCH_SAFE=%v", data["BranchSafe"]),
		fmt.Sprintf("SLOT=%v", data["Slot"]),
	)
	for _, p := range ports {
		env = append(env, fmt.Sprintf("%s=%d", p.Name, p.Value))
	}
	// Pool handles — the address to target right now (an emulator's serial).
	if raw, ok := data["Handles"]; ok {
		if handles, ok := raw.(map[string]string); ok && len(handles) > 0 {
			names := make([]string, 0, len(handles))
			for n := range handles {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				env = append(env, fmt.Sprintf("POOL_HANDLE_%s=%s", strings.ToUpper(n), handles[n]))
			}
		}
	}
	// Pool allocations — extract from data["Pools"] so pools stay a single
	// source of truth through BuildTemplateData.
	if poolsRaw, ok := data["Pools"]; ok {
		if pools, ok := poolsRaw.(map[string]string); ok && len(pools) > 0 {
			names := make([]string, 0, len(pools))
			for n := range pools {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				env = append(env, fmt.Sprintf("POOL_%s=%s", strings.ToUpper(n), pools[n]))
			}
		}
	}
	values, err := ResolveEnvValues(cfg, data, local)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+values[k])
	}
	return env, nil
}

func renderWetTemplate(name, body string, data map[string]any) (string, error) {
	t, err := template.New(name).Option("missingkey=error").Parse(body)
	if err != nil {
		return "", fmt.Errorf("%s: parse: %w", name, err)
	}
	var out strings.Builder
	if err := t.Execute(&out, data); err != nil {
		return "", fmt.Errorf("%s: execute: %w", name, err)
	}
	return out.String(), nil
}
