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
	"time"

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
	ProjectName string       `yaml:"project_name"`
	MainBranch  string       `yaml:"main_branch,omitempty"`
	Copy        CopyConfig   `yaml:"copy"`
	PortStride  int          `yaml:"port_stride"`
	Ports       []PortConfig `yaml:"ports"`
	// SlotPool lets released slots keep warm resources bound to their ports
	// (for example a reusable DB container). New worktrees then reuse the
	// lowest unclaimed slot from the state file without rejecting it just
	// because one of its own warm resources is still listening.
	SlotPool bool `yaml:"slot_pool,omitempty"`
	// Multiplexer controls whether worktree create/mount opens an extra terminal
	// multiplexer window. It belongs in .wet.local.yaml when it is personal:
	// one developer may want tmux, another may use Herdr and want none.
	Multiplexer string `yaml:"multiplexer,omitempty"`

	// PortCluster shifts every declared base clear of another clone of this
	// repo on the same machine. Slots spread ports out *within* a checkout;
	// nothing spreads two clones apart, because both start at slot 0 and
	// both resolve to the declared bases. A cluster is the band of ports one
	// clone owns across all of its worktrees, and the number says which band
	// this clone took: cluster 1 sits ClusterSlots slots above cluster 0, so
	// clone A can fill every slot it has room for without ever reaching
	// clone B's first one.
	//
	// It belongs in .wet.local.yaml, not in .wet.yaml: which clone on which
	// laptop drew which band is a fact about a machine, and committing it
	// would hand the whole team one clone's answer. `wet port-cluster next`
	// writes it there.
	PortCluster int `yaml:"port_cluster,omitempty"`
	// ClusterSlots is how many slots' worth of room one cluster reserves per
	// port — the headroom a clone gets before its band would touch the next
	// one's. Committed in .wet.yaml when the default is too tight, because
	// every clone has to agree on the width or their bands interleave.
	ClusterSlots int                 `yaml:"cluster_slots,omitempty"`
	Env          map[string]string   `yaml:"env"`
	Hooks        HooksConfig         `yaml:"hooks"`
	Outputs      []OutputConfig      `yaml:"outputs,omitempty"`
	Secrets      *SecretsConfig      `yaml:"secrets,omitempty"`
	Pools        map[string][]string `yaml:"pools,omitempty"`
	PoolGrow     map[string]string   `yaml:"pool_grow,omitempty"`
	PR           PRConfig            `yaml:"pr,omitempty"`

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
	// OnDemand keeps this output out of the sweep that configures a checkout:
	// `wet setup`, `wet worktree create` and a bare `wet env pull` skip it,
	// and only naming it generates it. For the files that are not a
	// worktree's own — a deploy environment's dotenv, whose credentials have
	// no business on a developer's disk just because they made a worktree.
	OnDemand bool `yaml:"on_demand,omitempty"`
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
	// Vault and Item are the default destination, and both belong in the
	// committed .wet.yaml: which vault holds the project's credentials and
	// which item the fields sit on are facts about the project, and everyone
	// who clones the repo needs the same answer.
	//
	// Like every other key, either can be overridden in .wet.local.yaml —
	// that file is an ordinary overlay, not a special home for the vault.
	Vault string `yaml:"vault,omitempty"`
	Item  string `yaml:"item,omitempty"`
	// Vaults is the multi-environment form of Vault: one vault per deploy
	// environment, so a checkout can read staging credentials today and
	// production ones tomorrow without editing the file. The definitions are
	// unchanged — the same field on the same item, in a different vault — so
	// this adds no second home for values and no per-environment key list.
	//
	// Mutually exclusive with Vault, and committed for the same reason Vault
	// is: the vault names are the same for everyone who clones the repo.
	Vaults []VaultConfig `yaml:"vaults,omitempty"`
	// DefaultEnv names the Vaults entry used when no --env is given. It may
	// be omitted when exactly one vault is declared. Because .wet.local.yaml
	// overlays the committed file per checkout, overriding it there is how
	// one worktree works against staging while the others stay on the
	// project's default.
	DefaultEnv string `yaml:"default_env,omitempty"`
	// Account is the 1Password account (sign-in address or UUID). Empty means
	// whichever account the shell is signed into, or ask when ambiguous. It
	// is the one key here that is genuinely per-workstation, so it is the one
	// that usually lives in the gitignored .wet.local.yaml.
	Account string `yaml:"account,omitempty"`
	// Definitions maps a logical secret name to its place in the store.
	Definitions map[string]SecretDefinition `yaml:"definitions"`
}

// VaultConfig is one deploy environment's vault: the environment's name as
// wet knows it, and the vault holding that environment's credentials.
type VaultConfig struct {
	// Name is the vault, exactly as the store names it.
	Name string `yaml:"name"`
	// Env is the label selected by default_env or `--env`.
	Env string `yaml:"env"`
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
	PostCreate  []string `yaml:"post_create,omitempty"`
	PreRemove   []string `yaml:"pre_remove,omitempty"`
	SlotReset   []string `yaml:"slot_reset,omitempty"`
	SlotStart   []string `yaml:"slot_start,omitempty"`
	SlotStop    []string `yaml:"slot_stop,omitempty"`
	SlotRebuild []string `yaml:"slot_rebuild,omitempty"`
	SlotDoctor  []string `yaml:"slot_doctor,omitempty"`
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
// the trunk is checked out (see LocalConfigDir) and then the checkout's own,
// when it has one.
func LoadWetConfig(repoRoot string) (*WetConfig, error) {
	return LoadWetConfigWithLocal(repoRoot, LocalConfigDir(repoRoot, repoRoot), repoRoot)
}

// LocalConfigDir picks the directory the MACHINE layer of `.wet.local.yaml`
// is read from: the checkout that has `main_branch:` checked out, falling
// back to fallbackDir. (A checkout's own overlay, when it has one, is layered
// on top of this — see LoadWetConfigWithLocal.)
//
// The machine layer is gitignored, so no worktree has a copy and
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

// LoadWetConfigWithLocal loads `.wet.yaml` from configDir and overlays a
// `.wet.local.yaml` from each localDir in turn — a later layer wins where
// they collide, mapping keys merging one by one exactly as a single overlay
// does.
//
// The layers answer to different scopes. The config can come from a
// worktree's own checkout — that's how a `.wet.yaml` change is testable
// where it's written. The first overlay is the machine's, read from the
// checkout holding the trunk (see LocalConfigDir): the account this
// workstation signs into, the emulators pool_grow created — facts every
// checkout shares. A checkout may then carry its own `.wet.local.yaml` on
// top, holding facts about that checkout alone — the LAN IP `wet mobile
// setup` points one worktree's stack at while the others keep localhost.
// It's gitignored, so a fresh worktree simply has none and lives on the
// machine layer.
//
// An empty localDir means "no overlay"; a dir naming the same place twice
// (the trunk checkout IS the checkout) is read once, not applied twice.
func LoadWetConfigWithLocal(configDir string, localDirs ...string) (*WetConfig, error) {
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
	var overridden, applied []string
	seenDirs := map[string]bool{}
	for _, localDir := range localDirs {
		if localDir == "" {
			continue
		}
		// The same directory offered twice is one layer, not two: when the
		// checkout IS the trunk, applying its file again would report every
		// key as overridden twice and re-run the merge for nothing.
		resolved := resolvePath(localDir)
		if seenDirs[resolved] {
			continue
		}
		seenDirs[resolved] = true
		localPath := filepath.Join(localDir, wetLocalConfigFileName)
		keys, err := mergeLocalConfig(baseRoot, localPath)
		if err != nil {
			return nil, err
		}
		if len(keys) > 0 {
			applied = append(applied, localPath)
		}
		overridden = append(overridden, keys...)
	}
	overridden = dedupeSorted(overridden)

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
		if len(applied) > 0 {
			return nil, fmt.Errorf("invalid config (%s merged over %s): %w",
				strings.Join(applied, " and "), path, err)
		}
		return nil, fmt.Errorf("invalid %s: %w", path, err)
	}
	return &cfg, nil
}

// dedupeSorted sorts keys and drops repeats — a key both layers override is
// still one override in LocalKeys.
func dedupeSorted(keys []string) []string {
	sort.Strings(keys)
	out := keys[:0]
	for i, k := range keys {
		if i == 0 || k != keys[i-1] {
			out = append(out, k)
		}
	}
	return out
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
	// Two overlay layers, machine first: the trunk's .wet.local.yaml, then
	// the worktree's own — see LoadWetConfigWithLocal. The worktree layer is
	// listed either way; when this checkout IS the trunk the loader reads it
	// once.
	cfg, err := LoadWetConfigWithLocal(worktreePath, LocalConfigDir(worktreePath, repoRoot), worktreePath)
	if err == nil {
		return cfg, filepath.Join(worktreePath, wetConfigFileName), nil
	}
	if err != ErrConfigMissing {
		return nil, filepath.Join(worktreePath, wetConfigFileName), err
	}
	cfg, err = LoadWetConfigWithLocal(repoRoot, LocalConfigDir(repoRoot, repoRoot), worktreePath)
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

// DefaultClusterSlots is how many slots one port cluster reserves room for
// with no `cluster_slots:` in the config: a main checkout plus nine
// worktrees. It matches the default `port_stride:` of 10, so a base of 4000
// gives each clone a clean round hundred (4000-4090, 4100-4190, ...) — the
// band a reader can work out in their head from the number in the file.
const DefaultClusterSlots = 10

// ClusterSlotCount is `cluster_slots:` with its default applied. Read through
// this rather than the field: a WetConfig built in a test or by a caller that
// never went through the loader has a zero there, and a zero width collapses
// every cluster onto cluster 0's ports.
func (c *WetConfig) ClusterSlotCount() int {
	if c != nil && c.ClusterSlots > 0 {
		return c.ClusterSlots
	}
	return DefaultClusterSlots
}

func applyWetConfigDefaults(cfg *WetConfig) {
	if cfg.PortStride <= 0 {
		cfg.PortStride = 10
	}
	if cfg.ClusterSlots <= 0 {
		cfg.ClusterSlots = DefaultClusterSlots
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

	// Environment names appear on the command line (`--env staging`) and in
	// .wet.yaml under secrets.vaults:. Lowercase word characters keep them
	// typable and keep them from being mistaken for a vault name, which may
	// contain anything.
	secretEnvNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

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
var reservedTemplateFields = []string{"ProjectName", "Branch", "BranchSafe", "Slot", "Pools", "Handles", "AllPools", "AllHandles"}

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

	if c.PortCluster < 0 {
		return fmt.Errorf("port_cluster %d must not be negative", c.PortCluster)
	}
	if c.ClusterSlots < 0 {
		return fmt.Errorf("cluster_slots %d must not be negative", c.ClusterSlots)
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
		// Only the shift the cluster itself introduces is checked, not the
		// top of the band: a config on cluster 0 has to validate exactly as
		// it did before this key existed, or adding the key would start
		// rejecting configs that have worked for years.
		if c.PortCluster > 0 {
			shifted := port.Base + c.PortCluster*c.ClusterSlotCount()*port.Stride
			if shifted >= 65536 {
				return fmt.Errorf("ports[%d] %q: port_cluster %d puts base %d at %d, past 65535 — "+
					"lower port_cluster, or lower this port's base so the clusters fit",
					i, port.Name, c.PortCluster, port.Base, shifted)
			}
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
	if c.Multiplexer != "" && c.Multiplexer != "tmux" && c.Multiplexer != "none" {
		return fmt.Errorf("multiplexer %q is not supported (known: tmux, none)", c.Multiplexer)
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

	hookSets := map[string][]string{
		"post_create":  c.Hooks.PostCreate,
		"pre_remove":   c.Hooks.PreRemove,
		"slot_reset":   c.Hooks.SlotReset,
		"slot_start":   c.Hooks.SlotStart,
		"slot_stop":    c.Hooks.SlotStop,
		"slot_rebuild": c.Hooks.SlotRebuild,
		"slot_doctor":  c.Hooks.SlotDoctor,
	}
	for name, hooks := range hookSets {
		for i, cmd := range hooks {
			if _, err := template.New(fmt.Sprintf("hooks.%s[%d]", name, i)).Parse(cmd); err != nil {
				return fmt.Errorf("hooks.%s[%d]: %w", name, i, err)
			}
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
	// An overlay-only secrets: block is a shape people land in: someone adds
	// `secrets: {account: ...}` — or a vault override — to .wet.local.yaml
	// against a .wet.yaml that has no secrets: block at all. The merge then
	// synthesizes a block with an empty store, and the resulting
	// `secrets.store "" is not supported` names a key they never wrote and
	// does not say what is actually missing.
	if (s.Vault != "" || len(s.Vaults) > 0 || s.Item != "" || s.Account != "") && s.Store == "" && len(s.Definitions) == 0 {
		return fmt.Errorf("secrets: names a destination (vault/vaults/item/account) but declares no store: or definitions: — "+
			"declare the block in %s (store:, the vault, the item, and the definitions). An overlay may add keys, "+
			"but a secrets: block that exists only in %s is missing the half that makes it mean anything",
			wetConfigFileName, wetLocalConfigFileName)
	}
	if err := s.validateVaults(); err != nil {
		return err
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

// validateVaults checks the multi-environment vault list: that it is not
// mixed with the single `vault:`, that every entry is complete and uniquely
// named, and that `default_env:` names an environment that exists.
//
// A bad env reference has to fail at load rather than at read. Selecting a
// vault nobody declared resolves to no destination at all, and under the
// default `fallback` policy that is indistinguishable from being offline: wet
// would quietly write every declared default and report a store it never
// meant to skip.
func (s *SecretsConfig) validateVaults() error {
	if len(s.Vaults) == 0 {
		if s.DefaultEnv != "" {
			return fmt.Errorf("secrets.default_env %q names an environment, but secrets.vaults: declares none — "+
				"list the vaults as `- {name: ..., env: ...}` or drop default_env", s.DefaultEnv)
		}
		return nil
	}
	if s.Vault != "" {
		// The two can arrive from different layers — a `vault:` in the
		// committed file under a `vaults:` in an overlay — and by the time
		// this runs the merge has erased which file each came from. So the
		// message describes the fix rather than pointing at a line.
		return fmt.Errorf("secrets: names both vault: (%s) and vaults: — one vault or a list of "+
			"per-environment vaults, not both.\n"+
			"  Replace `vault: %s` with the vaults: list wherever it is declared (%s, or the %s overlaying it).",
			s.Vault, s.Vault, wetConfigFileName, wetLocalConfigFileName)
	}
	seen := map[string]bool{}
	for i, v := range s.Vaults {
		if v.Name == "" {
			return fmt.Errorf("secrets.vaults[%d]: name: is required (the vault as the store names it)", i)
		}
		if v.Env == "" {
			return fmt.Errorf("secrets.vaults[%d] (%s): env: is required — it is the label default_env and --env select by", i, v.Name)
		}
		if !secretEnvNameRE.MatchString(v.Env) {
			return fmt.Errorf("secrets.vaults[%d]: env %q must match %s", i, v.Env, secretEnvNameRE.String())
		}
		if seen[v.Env] {
			return fmt.Errorf("secrets.vaults: env %q is declared twice — one vault per environment", v.Env)
		}
		seen[v.Env] = true
	}
	if s.DefaultEnv == "" {
		if len(s.Vaults) > 1 {
			return fmt.Errorf("secrets.vaults declares %d environments (%s), so secrets.default_env: must say which one is used "+
				"when no --env is given", len(s.Vaults), strings.Join(s.VaultEnvs(), ", "))
		}
		return nil
	}
	if !seen[s.DefaultEnv] {
		return fmt.Errorf("secrets.default_env %q is not one of secrets.vaults: (%s)", s.DefaultEnv, strings.Join(s.VaultEnvs(), ", "))
	}
	return nil
}

// VaultEnvs lists the declared environments in declaration order, for errors
// and for `wet env explain`.
func (s *SecretsConfig) VaultEnvs() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.Vaults))
	for _, v := range s.Vaults {
		out = append(out, v.Env)
	}
	return out
}

// SelectEnv resolves the environment one command runs against: the --env
// override if given, otherwise default_env, otherwise the only declared
// environment. It returns "" when the config has no vaults: block at all,
// which is the single-vault shape every existing config has.
//
// An override is validated even when there are no vaults, because `--env
// production` against a config that has none is a request wet cannot honour —
// answering it with the one vault there is would read from the wrong place.
func (s *SecretsConfig) SelectEnv(override string) (string, error) {
	if s == nil || len(s.Vaults) == 0 {
		if override != "" {
			return "", fmt.Errorf("--env %s: no secrets.vaults: declares per-environment vaults — "+
				"add them to %s as `- {name: ..., env: %s}`", override, wetConfigFileName, override)
		}
		return "", nil
	}
	if override != "" {
		for _, v := range s.Vaults {
			if v.Env == override {
				return override, nil
			}
		}
		return "", fmt.Errorf("--env %s: not one of secrets.vaults: (%s)", override, strings.Join(s.VaultEnvs(), ", "))
	}
	if s.DefaultEnv != "" {
		return s.DefaultEnv, nil
	}
	// Exactly one vault and no default_env — validation guarantees it.
	return s.Vaults[0].Env, nil
}

// VaultForEnv is the vault a resolution in env reads from. An empty env is
// the single-vault config, which answers with `vault:`.
func (s *SecretsConfig) VaultForEnv(env string) string {
	if s == nil {
		return ""
	}
	if env == "" {
		return s.Vault
	}
	for _, v := range s.Vaults {
		if v.Env == env {
			return v.Name
		}
	}
	return ""
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

func allHookBodies(h HooksConfig) []string {
	var out []string
	out = append(out, h.PostCreate...)
	out = append(out, h.PreRemove...)
	out = append(out, h.SlotReset...)
	out = append(out, h.SlotStart...)
	out = append(out, h.SlotStop...)
	out = append(out, h.SlotRebuild...)
	out = append(out, h.SlotDoctor...)
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

// ResolvePorts computes concrete port values for `slot` in the cluster this
// config is on, honoring each port's (possibly overridden) stride.
func (c *WetConfig) ResolvePorts(slot int) []ResolvedPort {
	return c.ResolvePortsIn(c.PortCluster, slot)
}

// ResolvePortsIn computes port values for `slot` as if the config were on
// `cluster`, which is what lets `wet port-cluster next` probe a band it has
// not taken yet.
//
// The cluster shift is per-port rather than one flat number added to every
// base, because a port carries its own stride. Shifting DEBUG_PORT (stride 5)
// and APP_PORT (stride 10) by the same amount would leave one of them either
// wasting range or — for a stride wider than the flat shift — walking into
// the next cluster's band at a high enough slot. Scaling each port's shift by
// its own stride makes the bands provably disjoint for every port.
func (c *WetConfig) ResolvePortsIn(cluster, slot int) []ResolvedPort {
	offset := cluster*c.ClusterSlotCount() + slot
	out := make([]ResolvedPort, len(c.Ports))
	for i, p := range c.Ports {
		out[i] = ResolvedPort{
			Name:  p.Name,
			Value: p.Base + offset*p.Stride,
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
// with "port is already allocated". See portFree for what "currently
// bindable" has to mean to catch every way a dev machine holds a port.
func (c *WetConfig) portsFree(slot int) bool {
	return portsBindable(c.ResolvePorts(slot))
}

// portProbeTimeout bounds the connect half of the probe. Loopback refuses
// instantly when nothing is listening, so this only ever matters if something
// filters loopback traffic — in which case the answer wet needs is "assume
// it's free" rather than "hang".
const portProbeTimeout = 200 * time.Millisecond

// portsBindable reports whether every port in the set is free on this host,
// holding each listener until the whole set has been acquired rather than
// taking and releasing them one at a time.
//
// Holding buys two things a port-at-a-time probe cannot have. It narrows the
// window in which something else takes a port wet has already walked past and
// called free — the set is only free if it can ALL be held at one instant.
// And it catches a set that collides with itself: two declared ports that
// resolve to the same number make the second acquisition fail, so a band that
// could never work is never handed out.
func portsBindable(ports []ResolvedPort) bool {
	values := make([]int, len(ports))
	for i, p := range ports {
		values[i] = p.Value
	}
	return portSetFree(values)
}

// portSetFree is portsBindable's arithmetic-free core.
func portSetFree(ports []int) bool {
	// Dial before binding anything. A listener of wet's own on :P answers a
	// dial to 127.0.0.1:P, so probing in the other order would make every
	// port in the set look occupied by the very sockets used to test it.
	for _, port := range ports {
		if port <= 0 || port > 65535 {
			return false
		}
		if servedOnLoopback(port) {
			return false
		}
	}

	held := make([]net.Listener, 0, 2*len(ports))
	defer func() {
		for _, ln := range held {
			_ = ln.Close()
		}
	}()
	for _, port := range ports {
		// Each family explicitly, never the dual-stack ":port" form. Two
		// reasons, and they point the same way:
		//
		// A dual-stack listen alone reports an IPv4-only server free, because
		// an IPv6 wildcard bind does not conflict with an IPv4 one. And a
		// dual-stack listener wet is HOLDING covers IPv4 too, so the IPv4
		// probe that follows collides with wet's own socket and calls a free
		// port busy — which is only survivable if each listener is released
		// before the next is taken, and releasing them is the thing this
		// function exists not to do.
		//
		// Go sets IPV6_V6ONLY on a "tcp6" listener, so these two carve the
		// families apart cleanly and can be held at the same instant:
		//
		//	nothing listening        both held
		//	IPv4 wildcard            tcp4 refused
		//	IPv6 dual-stack (docker) tcp4 refused
		//	IPv6-only                tcp6 refused
		for _, probe := range [][2]string{
			{"tcp4", fmt.Sprintf("0.0.0.0:%d", port)},
			{"tcp6", fmt.Sprintf("[::]:%d", port)},
		} {
			ln, err := net.Listen(probe[0], probe[1])
			if err != nil {
				return false
			}
			held = append(held, ln)
		}
	}
	return true
}

// portFree reports whether nothing is serving on `port`.
//
// It asks two different questions because on macOS neither one alone is
// right, and the version that only tried to bind the wildcard called ports
// free that plainly weren't. Measured against three real listeners:
//
//	                            bind :P   bind 0.0.0.0:P   dial loopback
//	docker, IPv6 wildcard        busy         busy             busy
//	postgres, loopback only      FREE         FREE             busy
//	a server on 0.0.0.0 only     FREE         busy             busy
//
// Binding misses those two because SO_REUSEADDR — which Go sets on every
// listener — lets a wildcard bind sit happily over a socket bound to a
// specific address, and lets an IPv6 wildcard sit over an IPv4 one. A
// loopback-only Postgres is exactly the case that matters here: it is what a
// developer has running when they declare `DB_PORT, base: 5432`.
//
// Dialing catches all three, so why still bind? Because the two failures are
// not the same failure. Dialing finds a socket that ANSWERS; binding finds a
// port this process could not take even if nothing answers on it — a
// listener with a full accept queue, a socket held on a non-loopback
// interface. A port is free only when both agree it is.
func portFree(port int) bool {
	return portSetFree([]int{port})
}

// servedOnLoopback reports whether something answers on `port` on either
// loopback address — the check that finds a server bound to 127.0.0.1 or
// [::1] alone, which no wildcard bind can detect. Both are tried, for the
// server that listens on only one of them.
func servedOnLoopback(port int) bool {
	for _, addr := range []string{
		fmt.Sprintf("127.0.0.1:%d", port),
		fmt.Sprintf("[::1]:%d", port),
	} {
		if c, err := net.DialTimeout("tcp", addr, portProbeTimeout); err == nil {
			_ = c.Close()
			return true
		}
	}
	return false
}

// ClusterFree reports whether an entire cluster's band is bindable — every
// declared port at every slot the cluster reserves, not just the slot-0 row.
//
// The whole band has to be probed because a cluster is claimed by a clone,
// not by a checkout. A neighbouring clone sitting idle on its main checkout
// while its third worktree runs a stack shows nothing at slot 0; taking that
// band anyway would collide the moment either side made one more worktree.
//
// Listeners are held one slot-row at a time rather than across the entire
// band. A row is the set that is actually used together, so that is where
// holding earns its keep, and it bounds the open descriptors to twice the
// declared port count — a wide cluster_slots on a config with many ports
// would otherwise ask for thousands at once and start failing to bind for a
// reason that has nothing to do with the ports being taken.
func (c *WetConfig) ClusterFree(cluster int) bool {
	for slot := 0; slot < c.ClusterSlotCount(); slot++ {
		if !portsBindable(c.ResolvePortsIn(cluster, slot)) {
			return false
		}
	}
	return true
}

// BandConflict reports two declared ports that land on the same number
// somewhere inside `cluster`'s band, which makes the band unusable however
// free the host is: one of the two worktrees holding them could never boot.
//
// Within a single slot the simultaneous hold in portSetFree already catches
// this — the second bind fails. Across slots it cannot, because those rows
// are probed separately, and that is the case a config drifts into: two
// ports with different strides converge at a high enough slot, so the config
// works until someone creates the seventh worktree.
func (c *WetConfig) BandConflict(cluster int) (first, second string, port int, found bool) {
	owner := make(map[int]string)
	for slot := 0; slot < c.ClusterSlotCount(); slot++ {
		for _, p := range c.ResolvePortsIn(cluster, slot) {
			if prev, taken := owner[p.Value]; taken {
				return prev, p.Name, p.Value, true
			}
			owner[p.Value] = p.Name
		}
	}
	return "", "", 0, false
}

// ClusterFitsPortRange reports whether shifting to `cluster` keeps every
// declared base a legal port. It bounds the search in `wet port-cluster
// next`, which would otherwise climb past 65535 on a repo whose every
// cluster is occupied.
func (c *WetConfig) ClusterFitsPortRange(cluster int) bool {
	for _, p := range c.Ports {
		if p.Base+cluster*c.ClusterSlotCount()*p.Stride >= 65536 {
			return false
		}
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
	// The list forms start as the primary alone. WithPoolExtras appends the
	// extra claims; until then a one-device worktree and a config that never
	// heard of extras render {{index .AllHandles.devices 0}} identically to
	// {{.Handles.devices}}.
	allPools := make(map[string][]string, len(pools))
	allHandles := make(map[string][]string, len(pools))
	for k, v := range pools {
		allPools[k] = []string{v}
		allHandles[k] = []string{v}
	}
	data["AllPools"] = allPools
	data["AllHandles"] = allHandles
	return data
}

// isPoolMapField reports whether a reserved template field is one of the
// pool maps rather than a scalar — the ones an `include:` cannot name.
func isPoolMapField(name string) bool {
	switch name {
	case "Pools", "Handles", "AllPools", "AllHandles":
		return true
	}
	return false
}

// WithPoolHandles replaces the default handle map with resolved addresses.
// The primary's slot in AllHandles follows, so the two views never disagree
// about what the first device is called.
func WithPoolHandles(data map[string]any, handles map[string]string) map[string]any {
	if len(handles) == 0 {
		return data
	}
	data["Handles"] = handles
	allHandles, _ := data["AllHandles"].(map[string][]string)
	if allHandles == nil {
		allHandles = map[string][]string{}
		data["AllHandles"] = allHandles
	}
	for pool, handle := range handles {
		if len(allHandles[pool]) == 0 {
			allHandles[pool] = []string{handle}
			continue
		}
		allHandles[pool][0] = handle
	}
	return data
}

// WithPoolExtras appends a worktree's extra claims to the list views —
// `{{.AllPools.x}}` gets the items, `{{.AllHandles.x}}` the addresses they
// answer to. handles may be nil, in which case each extra's handle is the
// item itself, exactly as BuildTemplateData defaults the primary. Pools and
// Handles are untouched: an extra device is never the one a template that
// asks for *the* device gets.
func WithPoolExtras(data map[string]any, extras map[string][]string, handles map[string][]string) map[string]any {
	if len(extras) == 0 {
		return data
	}
	allPools, _ := data["AllPools"].(map[string][]string)
	allHandles, _ := data["AllHandles"].(map[string][]string)
	if allPools == nil {
		allPools = map[string][]string{}
		data["AllPools"] = allPools
	}
	if allHandles == nil {
		allHandles = map[string][]string{}
		data["AllHandles"] = allHandles
	}
	for pool, items := range extras {
		if len(items) == 0 {
			continue
		}
		resolved := handles[pool]
		// Truncate to the primary before appending, so applying the same
		// extras twice (a caller reusing data) cannot double them.
		allPools[pool] = append(firstOrNone(allPools[pool]), items...)
		heads := firstOrNone(allHandles[pool])
		for i, item := range items {
			if i < len(resolved) && resolved[i] != "" {
				heads = append(heads, resolved[i])
			} else {
				heads = append(heads, item)
			}
		}
		allHandles[pool] = heads
	}
	return data
}

func firstOrNone(list []string) []string {
	if len(list) == 0 {
		return nil
	}
	return []string{list[0]}
}

// AllocatePools picks one item per declared pool for a new worktree,
// avoiding items already claimed by any entry in `existing` — primary picks
// and `wet mobile add` extras alike. For each pool, iteration is in declared
// order and the first unclaimed item wins — so a claim set of {items[0],
// items[2]} releases items[1] before items[3].
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
	hl := len(headless) > 0 && headless[0]
	return AllocatePoolsKeeping(cfg, existing, nil, repoRoot, hl)
}

// AllocatePoolsKeeping is AllocatePools for a worktree that already holds
// picks: any entry in keep that still names an item of its pool is kept
// rather than re-picked. A setup re-run otherwise hands a worktree the
// lowest free item, which can be a different AVD from the one it has been
// running on — and, once a tree can hold extras, could be one of its own
// extras. A kept item is not checked against existing, because the caller
// has already excluded this worktree's own entry from that list and the
// item was this worktree's to begin with.
func AllocatePoolsKeeping(cfg *WetConfig, existing []WorktreeState, keep map[string]string, repoRoot string, headless bool) (map[string]string, error) {
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
		if kept := keep[name]; kept != "" && slices.Contains(cfg.Pools[name], kept) {
			out[name] = kept
			continue
		}
		picked, err := allocatePoolItem(cfg, name, existing, repoRoot, headless)
		if err != nil {
			return nil, err
		}
		out[name] = picked
	}
	return out, nil
}

// allocatePoolItem claims one more item from a single pool: the first
// declared item nobody in existing holds, or a grown one when the pool is
// dry and has a strategy. It is the unit both a fresh setup (once per pool)
// and `wet mobile add` (once, for the pool it names) are built from, so the
// two cannot disagree about what "free" means.
func allocatePoolItem(cfg *WetConfig, name string, existing []WorktreeState, repoRoot string, headless bool) (string, error) {
	items := cfg.Pools[name]
	claimed := make(map[string]bool, len(existing))
	for _, ws := range existing {
		for _, item := range ws.poolClaims(name) {
			claimed[item] = true
		}
	}
	for _, item := range items {
		if !claimed[item] {
			return item, nil
		}
	}
	// Check for a grow strategy before erroring.
	if cfg.PoolGrow[name] == PoolGrowAndroidEmulator {
		newItem, err := growAndroidEmulatorPool(cfg, name, repoRoot, headless)
		if err != nil {
			return "", fmt.Errorf("pool %q exhausted and auto-grow failed: %w", name, err)
		}
		return newItem, nil
	}
	return "", fmt.Errorf(
		"pool %q exhausted — all %d items claimed by existing worktrees (%s). Pop a worktree first or add more items to the pool",
		name, len(items), describePoolClaims(existing, name),
	)
}

// describePoolClaims lists who holds what from one pool, for an exhaustion
// message: `branch=main item=emulator:a, branch=chat item=emulator:b`.
func describePoolClaims(existing []WorktreeState, name string) string {
	var claims []string
	for _, ws := range existing {
		for _, item := range ws.poolClaims(name) {
			claims = append(claims, fmt.Sprintf("branch=%s item=%s", ws.Branch, item))
		}
	}
	return strings.Join(claims, ", ")
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
	// Every claim, primary first, space-separated — so a hook written as
	// `for h in $POOL_HANDLE_DEVICES_ALL` is right for one device and for
	// three, and a teardown that loops over it kills what `wet mobile add`
	// booted as well as what setup did.
	for _, field := range []struct{ key, prefix string }{
		{"AllPools", "POOL_%s_ALL"},
		{"AllHandles", "POOL_HANDLE_%s_ALL"},
	} {
		lists, ok := data[field.key].(map[string][]string)
		if !ok || len(lists) == 0 {
			continue
		}
		names := make([]string, 0, len(lists))
		for n := range lists {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			env = append(env, fmt.Sprintf(field.prefix+"=%s", strings.ToUpper(n), strings.Join(lists[n], " ")))
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
