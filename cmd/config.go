package cmd

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	Outputs     []OutputConfig      `yaml:"outputs,omitempty"`
	Secrets     *SecretsConfig      `yaml:"secrets,omitempty"`
	Pools       map[string][]string `yaml:"pools,omitempty"`
	PoolGrow    map[string]string   `yaml:"pool_grow,omitempty"`
	PR          PRConfig            `yaml:"pr,omitempty"`

	// LocalKeys lists the top-level keys .wet.local.yaml overrode, so setup
	// can say where a surprising value came from. Not part of the file format.
	LocalKeys []string `yaml:"-"`
}

// OutputConfig is one generated environment file, declared inline in
// `.wet.yaml`.
//
// It replaces the old `render:` template pairs. A dotenv file needs no
// free-form template: wet already knows every value, so the only thing left to
// declare is which of them the file carries and under what key. Keeping that
// list here rather than in a checked-in `.env.tpl` is what makes `.wet.yaml`
// the single inventory — one file answers which files are generated, which
// keys each holds, and which of those keys are secrets.
//
// Secrets are scoped per output, never global: a mobile app's `.env` cannot
// pick up a backend credential just because the credential is defined.
type OutputConfig struct {
	// Name is the stable CLI identifier: `wet env pull backend`.
	Name string `yaml:"name"`
	// Out is the generated file, relative to the checkout being configured.
	Out string `yaml:"out"`
	// Format selects the encoder. Only "dotenv" is implemented.
	Format string `yaml:"format"`
	// Mode is the file mode as an octal string ("0644"). Empty means 0600,
	// because a generated env file may carry secrets and outlives the command.
	Mode string `yaml:"mode,omitempty"`
	// Include names ordinary values, ports, or reserved fields, keeping their
	// own name as the output key.
	Include []string `yaml:"include,omitempty"`
	// Secrets names entries of secrets.definitions. A definition not named
	// here is invisible to this output.
	Secrets []string `yaml:"secrets,omitempty"`
	// Values carries renamed and literal keys, in declaration order.
	Values OutputValues `yaml:"values,omitempty"`
}

// OutputValues is `values:` — an output key to a source, preserving the order
// written in the YAML so the generated file reads the way the config does.
type OutputValues struct {
	Keys []string
	By   map[string]OutputValue
}

// OutputValue is one `values:` entry. Exactly one of From and Literal is set.
type OutputValue struct {
	// From names an ordinary value, port, or reserved field. Secrets are
	// deliberately not reachable: they are emitted only through `secrets:`
	// and keep their logical name, so a rename can never smuggle one into an
	// output that did not select it.
	From string `yaml:"from,omitempty"`
	// Literal is a fixed scalar. A pointer so an intentional empty literal is
	// distinguishable from "no literal given".
	Literal *string `yaml:"literal,omitempty"`
}

// UnmarshalYAML decodes `values:` while remembering key order. yaml.v3 decodes
// a mapping into a Go map, which loses the order, and the order is what makes
// the generated file readable.
func (v *OutputValues) UnmarshalYAML(node *yaml.Node) error {
	// An explicitly empty `values:` is a mapping the user is still filling
	// in, not a broken config; the "declares no keys" check catches an output
	// that ends up with nothing.
	if node.Tag == "!!null" {
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("values: must be a mapping of output key to {from|literal}")
	}
	v.Keys = make([]string, 0, len(node.Content)/2)
	v.By = make(map[string]OutputValue, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if key == "" {
			return errors.New("values: output key must not be empty")
		}
		if _, dup := v.By[key]; dup {
			return fmt.Errorf("values.%s: declared twice", key)
		}
		var val OutputValue
		if err := node.Content[i+1].Decode(&val); err != nil {
			return fmt.Errorf("values.%s: %w", key, err)
		}
		v.Keys = append(v.Keys, key)
		v.By[key] = val
	}
	return nil
}

// MarshalYAML round-trips the ordered form, so a config wet rewrites (the
// scaffolder) keeps its `values:` order.
func (v OutputValues) MarshalYAML() (any, error) {
	node := &yaml.Node{Kind: yaml.MappingNode}
	for _, k := range v.Keys {
		val := v.By[k]
		valNode := &yaml.Node{}
		if err := valNode.Encode(val); err != nil {
			return nil, err
		}
		node.Content = append(node.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: k}, valNode)
	}
	return node, nil
}

// PRConfig optionally configures `wet pr create` behavior.
type PRConfig struct {
	Base         string `yaml:"base,omitempty"`          // base branch (default "main")
	BodyTemplate string `yaml:"body_template,omitempty"` // Go text/template for PR body
}

// SecretsConfig declares where this repo's secrets live and what each one is
// called, once, for the whole repo.
//
// A definition is a name plus a place in the store, never a value. Which
// outputs may see a definition is decided by the outputs — an output's
// `secrets:` list is the authorization boundary, so defining a credential does
// not hand it to every generated file.
//
// One definition per credential, shared by name: GOOGLE_CLIENT_ID is one
// credential whether the backend or a deployment reads it, so the store stays
// free of per-consumer duplicates and a rotation is one edit.
type SecretsConfig struct {
	// Store is the secret manager. Only "op" (1Password) is implemented; the
	// field exists so a second store is a value here rather than a rewrite.
	Store string `yaml:"store"`
	// Policy decides what happens when the store cannot answer. See
	// SecretPolicy* — the default is "fallback".
	Policy string `yaml:"policy,omitempty"`
	// Vault and Item are the default destination, and they live in DIFFERENT
	// files. Item belongs in the committed .wet.yaml — which item the fields
	// sit on is a fact about the project, and everyone needs the same answer.
	// Vault belongs in the gitignored .wet.local.yaml, since which vault a
	// machine reads from (and which account it is signed into) is a fact
	// about that machine.
	Vault string `yaml:"vault,omitempty"`
	Item  string `yaml:"item,omitempty"`
	// Account is the 1Password account (sign-in address or UUID). Empty means
	// whichever account the shell is signed into, or ask when ambiguous.
	Account string `yaml:"account,omitempty"`
	// Definitions maps a logical secret name to its place in the store.
	Definitions map[string]SecretDefinition `yaml:"definitions"`
}

// SecretDefinition is one credential: what it is called here, where it lives
// in the store, and what stands in for it when the store cannot be reached.
type SecretDefinition struct {
	// Field is the field on the item. Required unless Ref is given.
	Field string `yaml:"field,omitempty"`
	// Vault and Item override the block-level destination for this one secret.
	Vault string `yaml:"vault,omitempty"`
	Item  string `yaml:"item,omitempty"`
	// Ref is a full "op://vault/item/field", mutually exclusive with the
	// component fields above.
	Ref string `yaml:"ref,omitempty"`
	// Default stands in under the `fallback` policy. A pointer because an
	// explicit empty string is a real, deliberate default ("this provider is
	// simply not configured locally") and differs from having none at all,
	// which is an error the developer must see.
	Default *string `yaml:"default,omitempty"`
}

// HasDefault reports whether a usable fallback was declared, empty or not.
func (d SecretDefinition) HasDefault() bool { return d.Default != nil }

// DefaultValue returns the declared fallback, or "" when there is none.
func (d SecretDefinition) DefaultValue() string {
	if d.Default == nil {
		return ""
	}
	return *d.Default
}

// Secret policies decide what wet does when the configured store cannot
// answer. They exist because automatic secret loading must not make a fresh
// clone ambiguous: silently substituting an empty string for SENTRY_DSN reads
// as "error reporting is configured" right up until it isn't.
const (
	// SecretPolicyRequired: the store must answer for every selected secret.
	// A declared default does not paper over a store failure.
	SecretPolicyRequired = "required"
	// SecretPolicyFallback: try the store, fall back to the declared default,
	// and always say out loud which names fell back.
	SecretPolicyFallback = "fallback"
	// SecretPolicyDisabled: never contact the store. For offline work and
	// deterministic tests.
	SecretPolicyDisabled = "disabled"
)

// SecretPolicies lists every accepted policy, for validation and errors.
var SecretPolicies = []string{SecretPolicyRequired, SecretPolicyFallback, SecretPolicyDisabled}

// PolicyOrDefault is the effective policy: what the config says, or
// "fallback". Fallback is the default because it is the only one under which a
// fresh clone with no store access still boots, and it is not silent.
func (s *SecretsConfig) PolicyOrDefault() string {
	if s == nil || s.Policy == "" {
		return SecretPolicyFallback
	}
	return s.Policy
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
	// The merge happens on the parsed node tree, not on a map[string]any.
	// A map has no order, and `outputs[].values:` is order-sensitive by
	// design — its declaration order IS the generated file's key order. A
	// map round trip silently alphabetized it, which turned every pull into
	// a file whose keys had nothing to do with the config it was read from.
	var base yaml.Node
	if err := yaml.Unmarshal(data, &base); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	baseRoot := documentRoot(&base)
	if baseRoot == nil {
		return nil, fmt.Errorf("%s: top level must be a mapping", path)
	}

	// An empty localDir means no overlay. Joining onto "" would produce a
	// bare relative path and read whatever .wet.local.yaml happens to sit in
	// the process's cwd.
	var overridden []string
	localPath := ""
	if localDir != "" {
		localPath = filepath.Join(localDir, wetLocalConfigFileName)
		overridden, err = mergeLocalConfig(baseRoot, localPath)
		if err != nil {
			return nil, err
		}
	}

	// The overlay is checked inside mergeLocalConfig, against its own path, so
	// this one only ever blames the file a removed key actually came from.
	if err := rejectRemovedKeys(baseRoot, path); err != nil {
		return nil, err
	}

	var cfg WetConfig
	if err := baseRoot.Decode(&cfg); err != nil {
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

// rejectRemovedKeys fails on the schema wet used to have.
//
// `render:` and `secrets.sections:` were the two halves of the old split
// inventory: a checked-in `.env.tpl` decided an output's keys, and a section
// decided its secrets. `outputs:` now declares both in one place. Silently
// ignoring the old keys would be the worst outcome — a config that still
// parses, generates nothing, and leaves a stale `.env.dev` on disk looking
// current — so this is a hard error naming the replacement.
func rejectRemovedKeys(root *yaml.Node, path string) error {
	if mappingValue(root, "render") != nil {
		return fmt.Errorf("%s uses the removed `render:` key; declare generated files under `outputs:` "+
			"and delete the external dotenv templates (see `wet install skill` for the current schema)", path)
	}
	if secrets := mappingValue(root, "secrets"); secrets != nil {
		if mappingValue(secrets, "sections") != nil {
			return fmt.Errorf("%s uses the removed `secrets.sections:` key; declare each credential once under "+
				"`secrets.definitions:` and select it per file with an output's `secrets:` list", path)
		}
	}
	return nil
}

// documentRoot unwraps a parsed document down to its top-level mapping, or
// returns nil when the file holds anything else.
func documentRoot(doc *yaml.Node) *yaml.Node {
	n := doc
	if n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			return nil
		}
		n = n.Content[0]
	}
	if n.Kind != yaml.MappingNode {
		return nil
	}
	return n
}

// mergeLocalConfig overlays wetLocalConfigFileName onto base in place and
// returns the top-level keys it touched. A missing file is not an error.
//
// Maps merge key by key, so a local file can override one `env:` entry without
// restating the rest. Sequences replace wholesale — a local `pools.devices` is
// *your* device list, not yours appended to a teammate's, and appending would
// hand you items that don't exist on this machine.
func mergeLocalConfig(base *yaml.Node, localPath string) ([]string, error) {
	data, err := os.ReadFile(localPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", localPath, err)
	}
	var localDoc yaml.Node
	if err := yaml.Unmarshal(data, &localDoc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", localPath, err)
	}
	local := documentRoot(&localDoc)
	if local == nil {
		// An empty overlay is normal — a file someone created and has not
		// filled in yet. Anything else at the top level is a mistake worth
		// naming rather than ignoring.
		if len(localDoc.Content) == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("%s: top level must be a mapping", localPath)
	}
	// Checked before merging, and against the overlay's own path: after the
	// merge the key is indistinguishable from one in the committed file, and
	// telling someone their .wet.yaml uses `render:` when the line is in their
	// gitignored overlay sends them looking in the wrong file.
	if err := rejectRemovedKeys(local, localPath); err != nil {
		return nil, err
	}
	var keys []string
	for i := 0; i+1 < len(local.Content); i += 2 {
		keys = append(keys, local.Content[i].Value)
		mergeYAMLNode(base, local.Content[i], local.Content[i+1])
	}
	sort.Strings(keys)
	return keys, nil
}

// copyYAMLNode deep-copies a node so an edit to the copy cannot reach whatever
// else shares the original.
func copyYAMLNode(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	out := *n
	out.Content = make([]*yaml.Node, len(n.Content))
	for i, c := range n.Content {
		out.Content[i] = copyYAMLNode(c)
	}
	return &out
}

// mergeYAMLNode sets one key on a mapping: an existing mapping value merges
// key by key, anything else replaces outright.
func mergeYAMLNode(into *yaml.Node, key, value *yaml.Node) {
	for i := 0; i+1 < len(into.Content); i += 2 {
		if into.Content[i].Value != key.Value {
			continue
		}
		existing := into.Content[i+1]
		// An alias target would otherwise lose outright: it is not a
		// MappingNode, so the branch below would replace the whole block
		// rather than merge into it, and a .wet.local.yaml supplying just
		// `vault:` would discard every definition behind `secrets: *shared`.
		//
		// The alias is resolved to a COPY, not to the anchored node itself.
		// Merging into the anchor would edit every other alias to it, so
		// overriding one block's vault would silently change another's.
		if existing.Kind == yaml.AliasNode && value.Kind == yaml.MappingNode {
			existing = copyYAMLNode(existing.Alias)
			into.Content[i+1] = existing
		} else if existing.Anchor != "" && value.Kind == yaml.MappingNode {
			// The mirror image, and the more common way people write YAML:
			// `&shared` sits on the key being overridden and the aliases are
			// elsewhere. Merging in place edits the node every alias points
			// at, so a machine-local `secrets.definitions.A.field` would
			// silently rewrite B's field too — on that workstation only.
			// Detach a copy and leave the anchor untouched.
			existing = copyYAMLNode(existing)
			existing.Anchor = ""
			into.Content[i+1] = existing
		}
		if existing.Kind == yaml.MappingNode && value.Kind == yaml.MappingNode {
			for j := 0; j+1 < len(value.Content); j += 2 {
				mergeYAMLNode(existing, value.Content[j], value.Content[j+1])
			}
			return
		}
		into.Content[i+1] = value
		return
	}
	into.Content = append(into.Content, key, value)
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
		// An empty pool is legal only when pool_grow can fill it. That is the
		// bootstrap case — `devices: []` with `pool_grow.devices:
		// android-emulator` says "wet, create the first emulator yourself",
		// and growAndroidEmulatorPool does precisely that. Rejecting it forced
		// someone to hand-create an AVD before the strategy whose whole job is
		// creating AVDs could run. Without a strategy the pool is still
		// rejected here, because it can only ever fail later at allocation,
		// and failing at load names the file to fix.
		if len(items) == 0 && c.PoolGrow[name] == "" {
			return fmt.Errorf("pools[%q]: item list must not be empty — add an item, or "+
				"declare pool_grow[%q] so wet can create the first one", name, name)
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
	if err := c.validateOutputs(); err != nil {
		return err
	}

	// Parse env templates eagerly so syntax errors fail at load, not at apply.
	for key, tmpl := range c.Env {
		if key == "" {
			return errors.New("env: key must not be empty")
		}
		// An env: entry can be `include:`d into a generated file under its own
		// name, so it answers to the same shape as any other output key — an
		// unchecked one could carry a newline and inject an extra assignment.
		if !dotenvKeyRE.MatchString(key) {
			return fmt.Errorf("env.%s: key must match %s", key, dotenvKeyRE.String())
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

// validateSecrets checks the secrets: block on its own. Which outputs may see
// which definition is checked in validateOutputs, where the output name is in
// hand for the error.
func (c *WetConfig) validateSecrets() error {
	if c.Secrets == nil {
		return nil
	}
	s := c.Secrets
	// An overlay-only secrets: block is the shape a user lands in by
	// following the instructions: .wet.local.yaml is where the vault goes, so
	// someone writes one against a .wet.yaml that has no secrets: block at
	// all. The merge then synthesizes a block with an empty store, and the
	// resulting `secrets.store "" is not supported` names a key they never
	// wrote and does not say what is actually missing.
	if (s.Vault != "" || s.Item != "" || s.Account != "") && s.Store == "" && len(s.Definitions) == 0 {
		return fmt.Errorf("secrets: names a destination (vault/item/account) but declares no store: or definitions: — "+
			"the credentials themselves belong in %s; %s carries only the machine-local vault and account",
			wetConfigFileName, wetLocalConfigFileName)
	}
	if s.Store != secretsStoreOP {
		return fmt.Errorf("secrets.store %q is not supported (known: %s)", s.Store, secretsStoreOP)
	}
	if s.Policy != "" && !slices.Contains(SecretPolicies, s.Policy) {
		return fmt.Errorf("secrets.policy %q is not one of %s", s.Policy, strings.Join(SecretPolicies, ", "))
	}
	if len(s.Definitions) == 0 {
		return errors.New("secrets: needs at least one entry under definitions:")
	}
	for _, name := range sortedKeys(s.Definitions) {
		def := s.Definitions[name]
		if name == "" {
			return errors.New("secrets.definitions: name must not be empty")
		}
		// A secret becomes an output key, so it answers to the same shape as a
		// port: an env-var name.
		if !portNameRE.MatchString(name) {
			return fmt.Errorf("secrets.definitions.%s: name must match %s", name, portNameRE.String())
		}
		if isReservedTemplateField(name) {
			return fmt.Errorf("secrets.definitions.%s: %s is reserved by wet", name, name)
		}
		// A secret sharing a name with a port or an ordinary value makes the
		// output key ambiguous — and worse, invites a store value to shadow an
		// allocated port, which would hand two worktrees the same port.
		for _, p := range c.Ports {
			if p.Name == name {
				return fmt.Errorf("secrets.definitions.%s: collides with the port of the same name", name)
			}
		}
		if _, ok := c.Env[name]; ok {
			return fmt.Errorf("secrets.definitions.%s: collides with the env: entry of the same name — "+
				"a secret and an ordinary value cannot share a name; move the fallback into the definition's default:", name)
		}
		if def.Ref != "" {
			if def.Field != "" || def.Vault != "" || def.Item != "" {
				return fmt.Errorf("secrets.definitions.%s: ref: is the whole reference — "+
					"drop field:/vault:/item: or drop ref:", name)
			}
			if _, err := parseOPRef(def.Ref); err != nil {
				return fmt.Errorf("secrets.definitions.%s: %w", name, err)
			}
			continue
		}
		if def.Field == "" {
			return fmt.Errorf("secrets.definitions.%s: field: is required (or give a full ref:)", name)
		}
		if strings.Contains(def.Field, "/") {
			return fmt.Errorf("secrets.definitions.%s: field %q must be a bare field name — "+
				"name another item with item:, or give a full ref:", name, def.Field)
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

// LoadEnvLocalForTree reads the machine's `.env.local` for an operation
// running against treePath.
//
// This exists because taking a bare directory was a footgun that went off.
// `.env.local` is gitignored and one per machine, so — exactly like
// `.wet.local.yaml` — it lives beside the checkout holding the trunk, which
// is NOT reliably git's main working tree. `wet setup` resolved it through
// LocalConfigDir; `wet env pull` and `wet worktree pop` passed
// getMainRepoRoot() straight in. The result was that a pull silently reverted
// every override setup had just applied, and pre_remove hooks saw a different
// value set than post_create did.
//
// Callers get this function rather than a directory so the wrong directory is
// no longer reachable. LoadEnvLocal remains for tests that genuinely mean "read
// the file in this exact directory".
func LoadEnvLocalForTree(treePath, repoRoot string) (map[string]string, error) {
	return LoadEnvLocal(LocalConfigDir(treePath, repoRoot))
}

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
	declaredPorts := make(map[string]bool, len(cfg.Ports))
	for _, p := range cfg.Ports {
		declaredPorts[p.Name] = true
	}
	for _, k := range sortedKeys(overrides) {
		// A machine override may not take over an allocated port or a
		// reserved field. Letting it would defeat the striding this whole
		// tool exists for: one `DB_PORT=5433` in .env.local and every
		// worktree generates the same port, while .env.worktree still
		// carries the allocated one — two generated files disagreeing with
		// the state file, and a "port is already allocated" nobody traces
		// back to a one-line override.
		if declaredPorts[k] {
			return nil, fmt.Errorf("%s: %s is a port allocated per worktree and cannot be overridden — "+
				"change its base: in .wet.yaml, or override ports: in .wet.local.yaml", envLocalFileName, k)
		}
		if isReservedTemplateField(k) {
			return nil, fmt.Errorf("%s: %s is reserved by wet (%s)", envLocalFileName, k,
				strings.Join(reservedTemplateFields, ", "))
		}
		// A secret set here would be silently ineffective in the one place it
		// was meant for — outputs read secrets from the store, never from the
		// value set — while still landing in .env.worktree, which is 0644 and
		// which the docs promise carries no secrets. Both halves are wrong,
		// and the developer sees their key in one generated file and not the
		// other and debugs the wrong thing.
		if cfg.isSecretName(k) {
			return nil, fmt.Errorf("%s: %s is a declared secret and cannot be set here — "+
				"put the real value in the store (`wet env push`) or change its default: in .wet.yaml", envLocalFileName, k)
		}
		data[k] = overrides[k]
		resolved[k] = overrides[k]
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
		// Quoted for the same reason the generated outputs are: this file is
		// SOURCED — `set -a; . ./.env.worktree` in a recipe, and
		// `docker compose --env-file` — so an unquoted `&` in a URL forks a
		// background job and hands the value back EMPTY, and an unquoted ` # `
		// truncates it. Ports are integers and come out bare.
		fmt.Fprintf(&b, "%s=%s\n", k, quoteEnvValue(values[k]))
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
