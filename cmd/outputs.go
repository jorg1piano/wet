package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// OutputFormatDotenv is the only encoder wet implements. It is a constant so
// adding a second format is a compile error at every place that assumes
// dotenv, rather than a silently mis-encoded file.
const OutputFormatDotenv = "dotenv"

// outputFormats lists every accepted format, for validation and errors.
var outputFormats = []string{OutputFormatDotenv}

// defaultOutputMode is what a generated file gets with no `mode:`. Generated
// env files may carry secrets and outlive the command that wrote them, so the
// default is owner-only rather than the usual 0644.
const defaultOutputMode os.FileMode = 0o600

// dotenvKeyRE is what a generated key may look like.
//
// Without it a key is written raw into the file, so `values:` could declare
// "host\nEVIL" and inject a whole extra assignment, or "a=b" and change the
// value the reader sees — from a config that looked innocuous and validated
// clean. It is deliberately wider than portNameRE: a client's dotenv keys are
// legitimately camelCase (`webBaseUrl`), and every dotenv reader accepts the
// shell identifier shape.
var dotenvKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// outputNameRE constrains `name:` to what reads well as a CLI argument
// (`wet env pull backend`) and cannot be confused with a path or a flag.
var outputNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// reservedOutputPaths are wet's own files, which an output may never write.
var reservedOutputPaths = map[string]bool{
	wetConfigFileName:      true,
	wetLocalConfigFileName: true,
	envLocalFileName:       true,
	envWorktreeFileName:    true,
	".wet-worktrees.json":  true,
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

// ordinaryValueNames is every name an output may read without selecting a
// secret: wet's reserved fields, the declared ports, and the `env:` entries.
//
// It is deliberately static — computed from the config alone, with no worktree
// and no `.env.local` — so a typo in an `include:` fails at load, in every
// command, rather than at the moment a file was about to be written. That also
// fixes `.env.local`'s job as an *override* of declared values: a key that
// exists only on one workstation cannot become an output's contract.
func (c *WetConfig) ordinaryValueNames() map[string]bool {
	names := make(map[string]bool, len(c.Ports)+len(c.Env)+len(reservedTemplateFields))
	for _, f := range reservedTemplateFields {
		// Pools and Handles are maps, not values. Including one would pass
		// validation and then fail at pull time with "is not a scalar", which
		// breaks the promise the rest of this function makes: everything
		// knowable from .wet.yaml fails at load. Their members are reachable
		// as Pools.<name> through `values.from`.
		if isPoolMapField(f) {
			continue
		}
		names[f] = true
	}
	for _, p := range c.Ports {
		names[p.Name] = true
	}
	for k := range c.Env {
		names[k] = true
	}
	return names
}

// knownOrdinaryNames is ordinaryValueNames sorted, for error messages that
// must not vary between runs.
func (c *WetConfig) knownOrdinaryNames() []string {
	return sortedKeys(c.ordinaryValueNames())
}

// validateOutputs checks every declared output against the rest of the config,
// before any secret store is contacted and long before a file is written.
//
// Everything here is knowable from `.wet.yaml` alone, which is the point: a
// misspelled include or a secret an output forgot to select must not first
// surface as a missing key in a running app.
func (c *WetConfig) validateOutputs() error {
	names := make(map[string]int, len(c.Outputs))
	paths := make(map[string]int, len(c.Outputs))
	ordinary := c.ordinaryValueNames()

	for i, o := range c.Outputs {
		where := fmt.Sprintf("outputs[%d]", i)
		if o.Name == "" {
			return fmt.Errorf("%s: name is required — it is how `wet env pull <name>` selects this file", where)
		}
		where = fmt.Sprintf("outputs[%d] (%s)", i, o.Name)
		if !outputNameRE.MatchString(o.Name) {
			return fmt.Errorf("%s: name must match %s", where, outputNameRE.String())
		}
		if prev, dup := names[o.Name]; dup {
			return fmt.Errorf("%s: name already used by outputs[%d]", where, prev)
		}
		names[o.Name] = i

		if o.Out == "" {
			return fmt.Errorf("%s: out is required", where)
		}
		if err := validateCopyPath(o.Out); err != nil {
			return fmt.Errorf("%s.out: %w", where, err)
		}
		// Two outputs writing one file means the result depends on ordering,
		// and the loser's keys silently vanish.
		clean := filepath.ToSlash(filepath.Clean(o.Out))
		// A one-character typo must not make a pull overwrite the config that
		// produced it, or the allocation the worktree depends on.
		if reserved := reservedOutputPaths[clean]; reserved {
			return fmt.Errorf("%s: out %q is one of wet's own files and cannot be generated", where, o.Out)
		}
		if prev, dup := paths[clean]; dup {
			return fmt.Errorf("%s: out %q is already written by outputs[%d]", where, o.Out, prev)
		}
		paths[clean] = i

		if o.Format == "" {
			return fmt.Errorf("%s: format is required (known: %s)", where, strings.Join(outputFormats, ", "))
		}
		if o.Format != OutputFormatDotenv {
			return fmt.Errorf("%s: format %q is not supported (known: %s)", where, o.Format, strings.Join(outputFormats, ", "))
		}
		if _, err := o.FileMode(); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		if len(o.Include)+len(o.Secrets)+len(o.Values.Keys) == 0 {
			return fmt.Errorf("%s: declares no keys — give it include:, secrets:, or values:", where)
		}

		// keys tracks the output keys this file will hold, so a collision
		// between the three ways of declaring one is caught by name.
		keys := make(map[string]string, len(o.Include)+len(o.Secrets)+len(o.Values.Keys))
		claim := func(key, source string) error {
			if !dotenvKeyRE.MatchString(key) {
				return fmt.Errorf("%s: output key %q (from %s) must match %s", where, key, source, dotenvKeyRE.String())
			}
			if prev, dup := keys[key]; dup {
				return fmt.Errorf("%s: output key %q is declared twice (%s and %s)", where, key, prev, source)
			}
			keys[key] = source
			return nil
		}

		for _, name := range o.Include {
			if name == "" {
				return fmt.Errorf("%s.include: empty name", where)
			}
			if c.isSecretName(name) {
				return fmt.Errorf("%s.include: %s is a secret — list it under secrets: instead", where, name)
			}
			if !ordinary[name] {
				return fmt.Errorf("%s.include: unknown value %q (known: %s)", where, name, strings.Join(c.knownOrdinaryNames(), ", "))
			}
			if err := claim(name, "include"); err != nil {
				return err
			}
		}

		for _, name := range o.Secrets {
			if name == "" {
				return fmt.Errorf("%s.secrets: empty name", where)
			}
			if c.Secrets == nil {
				return fmt.Errorf("%s.secrets: %q named, but there is no secrets: block", where, name)
			}
			if _, ok := c.Secrets.Definitions[name]; !ok {
				return fmt.Errorf("%s.secrets: unknown secret %q (defined: %s)", where, name,
					strings.Join(sortedKeys(c.Secrets.Definitions), ", "))
			}
			if err := claim(name, "secrets"); err != nil {
				return err
			}
		}

		for _, key := range o.Values.Keys {
			v := o.Values.By[key]
			switch {
			case v.From != "" && v.Literal != nil:
				return fmt.Errorf("%s.values.%s: from: and literal: are mutually exclusive", where, key)
			case v.From == "" && v.Literal == nil:
				return fmt.Errorf("%s.values.%s: needs either from: or literal:", where, key)
			case v.From != "":
				if c.isSecretName(v.From) {
					return fmt.Errorf("%s.values.%s: %s is a secret; secrets are emitted under their own name "+
						"through secrets:, never renamed", where, key, v.From)
				}
				if !c.knowsValuePath(v.From) {
					return fmt.Errorf("%s.values.%s: unknown value %q (known: %s)", where, key, v.From,
						strings.Join(c.knownOrdinaryNames(), ", "))
				}
			}
			if err := claim(key, "values"); err != nil {
				return err
			}
		}
	}
	return nil
}

// isSecretName reports whether a name is a declared secret definition.
func (c *WetConfig) isSecretName(name string) bool {
	if c.Secrets == nil {
		return false
	}
	_, ok := c.Secrets.Definitions[name]
	return ok
}

// knowsValuePath reports whether `from:` names something wet can resolve: a
// plain ordinary value, or a pool item / live handle addressed as
// `Pools.<name>` / `Handles.<name>`.
func (c *WetConfig) knowsValuePath(name string) bool {
	if prefix, pool, dotted := strings.Cut(name, "."); dotted {
		if prefix != "Pools" && prefix != "Handles" {
			return false
		}
		_, ok := c.Pools[pool]
		return ok
	}
	return c.ordinaryValueNames()[name]
}

// FileMode is the mode the generated file is written with: the declared octal
// `mode:`, or 0600.
func (o OutputConfig) FileMode() (os.FileMode, error) {
	if o.Mode == "" {
		return defaultOutputMode, nil
	}
	n, err := strconv.ParseUint(o.Mode, 8, 32)
	if err != nil || n > 0o777 {
		return 0, fmt.Errorf("mode %q is not an octal file mode like \"0600\"", o.Mode)
	}
	return os.FileMode(n), nil
}

// SelectOutputs returns the outputs to act on: the ones named, in the order
// named, or every declared output that is not `on_demand:`. An unknown name is
// an error listing the ones that exist, because a typo must not quietly
// generate nothing.
//
// Naming an on-demand output selects it — the flag withholds it from the
// sweep, it does not make it unreachable.
func (c *WetConfig) SelectOutputs(only []string) ([]OutputConfig, error) {
	return c.selectOutputs(only, false)
}

// SelectEveryOutput is SelectOutputs without the on_demand: filter, for the
// commands that report the inventory rather than act on it. `wet env explain`
// must list a deploy output — hiding the file it exists to explain would be
// the opposite of its job.
func (c *WetConfig) SelectEveryOutput(only []string) ([]OutputConfig, error) {
	return c.selectOutputs(only, true)
}

func (c *WetConfig) selectOutputs(only []string, includeOnDemand bool) ([]OutputConfig, error) {
	if len(c.Outputs) == 0 {
		return nil, errors.New(".wet.yaml declares no outputs: — nothing to generate")
	}
	if len(only) == 0 {
		out := make([]OutputConfig, 0, len(c.Outputs))
		for _, o := range c.Outputs {
			if o.OnDemand && !includeOnDemand {
				continue
			}
			out = append(out, o)
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("every declared output is on_demand: (%s) — name the one to generate",
				strings.Join(c.outputNames(), ", "))
		}
		return out, nil
	}
	byName := make(map[string]OutputConfig, len(c.Outputs))
	for _, o := range c.Outputs {
		byName[o.Name] = o
	}
	out := make([]OutputConfig, 0, len(only))
	for _, name := range only {
		o, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("unknown output %q (declared: %s)", name, strings.Join(c.outputNames(), ", "))
		}
		out = append(out, o)
	}
	return out, nil
}

// outputConfigNames names a selection, for errors that say which outputs the
// caller asked for.
func outputConfigNames(outs []OutputConfig) []string {
	names := make([]string, 0, len(outs))
	for _, o := range outs {
		names = append(names, o.Name)
	}
	return names
}

// outputNames lists every declared output in declaration order, for errors
// that say what could have been named instead.
func (c *WetConfig) outputNames() []string { return outputConfigNames(c.Outputs) }

// UnusedSecrets reports secret definitions no output selects.
//
// This is the drift the redesign was written to prevent, seen from the other
// side. The old split inventory allowed "a secret can be mapped but never
// consumed"; collapsing the inventory into one file does not by itself make
// that visible — a definition nothing selects is never fetched and never
// written, which reads as wet ignoring a provider you thought you configured.
//
// Non-fatal by design: a credential used only by a deployment is legitimate,
// so this names it and leaves the judgement to the reader.
//
// The spec pairs this with a second diagnostic — `env:` values "used by
// neither outputs nor hooks" — which is deliberately NOT implemented, because
// it is wrong for wet. Every `env:` value is written to `.env.worktree`, and
// that file is consumed by docker compose and by anything a developer sources.
// COMPOSE_PROJECT_NAME is the clearest case: no output includes it, no hook
// names it, and it is load-bearing for every parallel stack in the repo.
// Reporting it as unused would tell someone to delete the value their whole
// worktree isolation depends on, which is worse than saying nothing.
func (c *WetConfig) UnusedSecrets() []string {
	if c.Secrets == nil {
		return nil
	}
	used := map[string]bool{}
	for _, o := range c.Outputs {
		for _, name := range o.Secrets {
			used[name] = true
		}
	}
	// A hook body is shell, so the honest test is a substring match: wet
	// cannot parse it, and a false "used" is far better than telling someone
	// to delete a credential their post_create depends on.
	hooks := strings.Join(allHookBodies(c.Hooks), "\n")

	var unused []string
	for _, name := range sortedKeys(c.Secrets.Definitions) {
		if !used[name] && !strings.Contains(hooks, name) {
			unused = append(unused, name)
		}
	}
	return unused
}

// ---------------------------------------------------------------------------
// Secret resolution
// ---------------------------------------------------------------------------

// SecretReader fetches one op:// reference. It is an interface so the pull
// pipeline can be tested end to end — including the required/fallback/disabled
// policies — without a 1Password session.
type SecretReader interface {
	Read(uri string) (string, error)
}

// prefetcher is the optional half of SecretReader: a store that can resolve a
// whole reference set in one round trip says so by implementing it. Reads
// still go through Read, so a reader without it — and a prefetch that failed —
// behave identically, just at more round trips.
type prefetcher interface {
	// Prefetch returns the batch's error for the caller to classify, not to
	// fail on: resolution continues through Read either way.
	Prefetch(uris []string) error
}

// failingReader stands in for a store client that could not be built at all —
// no CLI installed, an unsupported store name. Every read fails with the
// reason, so the policy decides what happens next in exactly one place.
type failingReader struct{ err error }

func (r failingReader) Read(string) (string, error) { return "", r.err }

// latchingReader answers every read after the first store-level failure with
// that same failure, without spawning the client again.
//
// Without it the bound on a single call is not a bound on the run: a pull
// selecting ten secrets paid ten full timeouts, so the fresh-clone hang the
// timeout was added to fix came back as twenty-one minutes instead of
// forever — which is still a hang from where the user sits.
//
// Only a store-level failure latches. A field missing from a present item is
// a fact about that one secret and says nothing about the next.
type latchingReader struct {
	inner SecretReader
	fatal error
}

// Prefetch forwards the batch to a wrapping-aware inner reader. Without this
// the latch silently costs the optimisation it wraps: resolveSecrets type-
// asserts on the reader it was handed, which is this one.
// Prefetch forwards the batch to a wrapping-aware inner reader. Without this
// the latch silently costs the optimisation it wraps: resolveSecrets type-
// asserts on the reader it was handed, which is this one.
//
// A store-level failure latches here for the same reason it does in Read, and
// it matters more: the batch is now the FIRST thing to touch the store, so an
// unreachable one is discovered by inject. Without latching, a pull paid the
// timeout twice — once for the batch, once for the read that rediscovered it.
func (r *latchingReader) Prefetch(uris []string) error {
	p, ok := r.inner.(prefetcher)
	if !ok {
		return nil
	}
	err := p.Prefetch(uris)
	if err != nil && errors.Is(err, errStoreUnavailable) {
		r.fatal = err
	}
	return err
}

func (r *latchingReader) Read(uri string) (string, error) {
	if r.fatal != nil {
		return "", r.fatal
	}
	value, err := r.inner.Read(uri)
	if err != nil && errors.Is(err, errStoreUnavailable) {
		r.fatal = err
	}
	return value, err
}

// opSecretReader reads through the 1Password CLI, one `op` invocation per
// ITEM rather than per field.
//
// `op read op://vault/item/field` resolves a single field, so a pull selecting
// nine secrets off one item used to spawn nine `op` processes. That is not
// merely slow: on macOS each invocation is a fresh process the OS attributes
// to the terminal, so the user was asked to authorize file access nine times
// for what is conceptually one read. Fetching the item once and taking the
// fields out of the response makes it one process and one prompt.
//
// The cache is per reader, and a reader lives for one pull, so this never
// serves a value that outlived the command that fetched it.
type opSecretReader struct {
	client opClient
	items  map[string]opItem
	// missing records items the store does not have, so a second secret on an
	// absent item does not re-pay the round trip that already answered.
	missing map[string]bool
	// values holds what Prefetch resolved, keyed by reference.
	values map[string]string
}

func newOPSecretReader(client opClient) *opSecretReader {
	return &opSecretReader{
		client:  client,
		items:   map[string]opItem{},
		missing: map[string]bool{},
		values:  map[string]string{},
	}
}

// Prefetch resolves every reference this pull will need in one `op` process.
//
// Fetching per item was already an improvement on per field, but the unit the
// user experiences is neither: it is the process. macOS asks for authorization
// once per `op` invocation, so a repo spreading 26 secrets over 11 items still
// asked 11 times. One inject call makes it one.
//
// A failure here is deliberately not returned. inject is all-or-nothing, so a
// single unresolvable reference would otherwise fail secrets that resolve
// perfectly well and rob the policy of its per-secret fallback. Read() then
// answers from the per-item path, which reports absence one secret at a time.
func (r *opSecretReader) Prefetch(uris []string) error {
	values, err := r.client.inject(uris)
	if err != nil {
		return err
	}
	for uri, value := range values {
		r.values[uri] = value
	}
	return nil
}

func (r *opSecretReader) Read(uri string) (string, error) {
	if value, ok := r.values[uri]; ok {
		return value, nil
	}
	ref, err := parseOPRef(uri)
	if err != nil {
		return "", err
	}
	key := ref.Vault + "/" + ref.Item

	item, cached := r.items[key]
	switch {
	case cached:
	case r.missing[key]:
		return "", fmt.Errorf("item %q not found in vault %q", ref.Item, ref.Vault)
	default:
		var found bool
		// A failure here is the store's, not this field's: item() reports a
		// genuinely absent item as found=false, so any error returned is a
		// store-level one and keeps its errStoreUnavailable marking for the
		// latching reader.
		item, found, err = r.client.item(ref.Vault, ref.Item)
		if err != nil {
			return "", err
		}
		if !found {
			r.missing[key] = true
			return "", fmt.Errorf("item %q not found in vault %q", ref.Item, ref.Vault)
		}
		r.items[key] = item
	}

	value, ok := item.value(ref.Field)
	if !ok {
		// Per-field absence, deliberately not marked as a store failure: the
		// policy may answer this one secret from its default and carry on.
		return "", fmt.Errorf("field %q not found on item %q in vault %q", ref.Field, ref.Item, ref.Vault)
	}
	return value, nil
}

// SecretOrigin says where a resolved secret's value came from, so the pull can
// report provenance without ever printing the value.
type SecretOrigin int

const (
	// SecretFromStore: the configured store answered.
	SecretFromStore SecretOrigin = iota
	// SecretFromDefault: the store could not answer and the declared default
	// was used, as the `fallback` policy permits.
	SecretFromDefault
)

// resolvedSecret is one secret's value plus where it came from.
type resolvedSecret struct {
	value  string
	origin SecretOrigin
}

// parseOPRef splits a full "op://vault/item/field" reference.
func parseOPRef(raw string) (SecretRef, error) {
	if !strings.HasPrefix(raw, "op://") {
		return SecretRef{}, fmt.Errorf("ref %q must be a full op://vault/item/field reference", raw)
	}
	parts := strings.SplitN(strings.TrimPrefix(raw, "op://"), "/", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return SecretRef{}, fmt.Errorf("ref %q is not op://vault/item/field", raw)
	}
	return SecretRef{Vault: parts[0], Item: parts[1], Field: parts[2]}, nil
}

// ResolveSecretRef turns one definition into a concrete place in the store,
// layering the definition's overrides over the block-level vault and item.
//
// env selects among `vaults:` (see SelectEnv); "" is the single-vault config
// and reads `vault:`. A definition's own vault: still wins over both — "this
// one credential lives elsewhere" is a fact about the credential, not about
// the environment.
//
// A full ref: is environment-invariant for the same reason, and more strongly:
// it names its own vault, so it resolves to the same place in every
// environment. That is legitimate — a shared credential does not fork per
// environment — but it is invisible in the generated file, so `wet env
// explain` marks such destinations rather than leaving a reader to infer that
// `--env production` moved every secret but this one.
func (s *SecretsConfig) ResolveSecretRef(name, env string) (SecretRef, error) {
	def, ok := s.Definitions[name]
	if !ok {
		return SecretRef{}, fmt.Errorf("unknown secret %q (defined: %s)", name, strings.Join(sortedKeys(s.Definitions), ", "))
	}
	if def.Ref != "" {
		ref, err := parseOPRef(def.Ref)
		if err != nil {
			return SecretRef{}, fmt.Errorf("secrets.definitions.%s: %w", name, err)
		}
		ref.Key = name
		return ref, nil
	}
	ref := SecretRef{Key: name, Vault: s.VaultForEnv(env), Item: s.Item, Field: def.Field}
	if def.Vault != "" {
		ref.Vault = def.Vault
	}
	if def.Item != "" {
		ref.Item = def.Item
	}
	if ref.Vault == "" {
		if len(s.Vaults) > 0 {
			// Unreachable through SelectEnv, which only ever returns a
			// declared env — so this is a caller that resolved with an env
			// of its own, and saying which envs exist is the whole fix.
			return ref, fmt.Errorf("secrets.definitions.%s: no vault for environment %q (declared: %s)",
				name, env, strings.Join(s.VaultEnvs(), ", "))
		}
		return ref, fmt.Errorf("secrets.definitions.%s: no vault configured\n"+
			"  Add a shared secrets.vault to %s:\n"+
			"    secrets:\n"+
			"      vault: Development\n"+
			"  Use secrets.vaults: instead when staging and production live in different vaults.\n"+
			"  To override only this secret, set secrets.definitions.%s.vault instead.",
			name, wetConfigFileName, name)
	}
	if ref.Item == "" {
		return ref, fmt.Errorf("secrets.definitions.%s: no item — set secrets.item in %s (which item the fields sit on is a project fact) or give this secret its own item", name, wetConfigFileName)
	}
	return ref, nil
}

// resolveSecrets fetches every selected secret exactly once and applies the
// policy.
//
// Fetching once is why the refs are collected across all requested outputs
// before any read: a credential shared by two outputs is one round trip to the
// store, and — more importantly — one authorization prompt.
//
// The returned map is never handed to an output directly. Each output gets a
// view containing only the names it selected (see resolveOutput), because a
// single merged map is exactly how a mobile `.env` ends up carrying a backend
// credential.
func resolveSecrets(cfg *WetConfig, names []string, policy, env string, reader SecretReader) (map[string]resolvedSecret, error) {
	out := make(map[string]resolvedSecret, len(names))
	if len(names) == 0 {
		return out, nil
	}
	if cfg.Secrets == nil {
		return nil, errors.New("outputs select secrets, but .wet.yaml has no secrets: block")
	}

	// byURI dedupes across outputs *and* across names: two definitions may
	// legitimately point at one field.
	byURI := map[string]string{}
	sort.Strings(names)

	// Give a reader that can batch the whole reference set before reading any
	// of it. Only references that resolve are offered: an unconfigured vault
	// is answered by the policy, not by the store.
	if p, ok := reader.(prefetcher); ok && policy != SecretPolicyDisabled {
		var uris []string
		seen := map[string]bool{}
		for _, name := range names {
			ref, err := cfg.Secrets.ResolveSecretRef(name, env)
			if err != nil || seen[ref.URI()] {
				continue
			}
			seen[ref.URI()] = true
			uris = append(uris, ref.URI())
		}
		if len(uris) > 0 {
			// Deliberately ignored: a failed batch is a slower pull, not a
			// failed one. Read answers per secret, and the policy decides.
			_ = p.Prefetch(uris)
		}
	}
	for _, name := range names {
		def := cfg.Secrets.Definitions[name]

		var value string
		// An unresolvable reference is treated as a store failure, not as a
		// config error. The only way to get here is a missing vault or item,
		// which means the repo's secrets: block is still half-written — and
		// the deliberate choice is that such a repo still generates its files
		// from declared defaults instead of refusing to boot, which is what
		// `fallback` promises for every other kind of store failure. It is
		// not silent: every fallback is named out loud, and `wet env explain`
		// reports the missing key. Choose `required` to make it fatal. A
		// malformed ref: was already rejected at load.
		ref, readErr := cfg.Secrets.ResolveSecretRef(name, env)
		switch {
		case readErr != nil:
		case policy == SecretPolicyDisabled:
			readErr = errors.New("secret store disabled")
		default:
			if reader == nil {
				readErr = errors.New("no secret store configured for this run")
			} else if cached, ok := byURI[ref.URI()]; ok {
				value = cached
			} else {
				value, readErr = reader.Read(ref.URI())
				if readErr == nil {
					byURI[ref.URI()] = value
				}
			}
		}

		if readErr == nil {
			out[name] = resolvedSecret{value: value, origin: SecretFromStore}
			continue
		}
		switch policy {
		case SecretPolicyRequired:
			// A declared default deliberately does not hide this: `required`
			// exists so a project can be certain the real credential is what
			// landed in the file.
			return nil, fmt.Errorf("secret %s: %w\n"+
				"  secrets.policy is %q, so a declared default does not substitute for the store.\n"+
				"  Sign in to the store, or run with --secrets=fallback for a local-only environment",
				name, readErr, SecretPolicyRequired)
		case SecretPolicyDisabled:
			if !def.HasDefault() {
				return nil, fmt.Errorf("secret %s: secrets.policy is %q and no default: is declared — "+
					"give it a default or choose another policy", name, SecretPolicyDisabled)
			}
		default:
			if !def.HasDefault() {
				return nil, fmt.Errorf("secret %s: %w\n"+
					"  and no default: is declared, so wet has nothing to write.\n"+
					"  Sign in to the store, or declare a development default in .wet.yaml", name, readErr)
			}
		}
		out[name] = resolvedSecret{value: def.DefaultValue(), origin: SecretFromDefault}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Output resolution + encoding
// ---------------------------------------------------------------------------

// EnvPair is one generated key and its value, in emission order.
type EnvPair struct {
	Key   string
	Value string
}

// ResolvedOutput is one output fully resolved and encoded, but not yet
// written. Producing all of these before touching the filesystem is what makes
// a pull all-or-nothing: a second output that fails validation must not leave
// the first one already replaced on disk.
//
// The guarantee is exact and worth stating exactly: every CONFIGURATION or
// RESOLUTION failure is caught before any file is touched. A write that fails
// on I/O — ENOSPC, a directory turned read-only underneath us — can still
// leave an earlier output replaced, because each file is replaced atomically
// but the set is not. Making the set atomic would need a staging directory
// and a rename loop with its own partial-failure window, which buys less than
// it costs for a case a re-run fixes.
type ResolvedOutput struct {
	Config      OutputConfig
	Pairs       []EnvPair
	Content     string
	Mode        os.FileMode
	FromStore   []string
	FromDefault []string
}

// lookupOrdinary reads one ordinary value out of the worktree data, including
// the dotted `Pools.<name>` / `Handles.<name>` forms.
func lookupOrdinary(data map[string]any, name string) (string, error) {
	if prefix, key, dotted := strings.Cut(name, "."); dotted {
		nested, ok := data[prefix].(map[string]string)
		if !ok {
			return "", fmt.Errorf("%s is not available in this worktree", name)
		}
		v, ok := nested[key]
		if !ok {
			return "", fmt.Errorf("%s is not allocated in this worktree", name)
		}
		return v, nil
	}
	raw, ok := data[name]
	if !ok {
		return "", fmt.Errorf("%s has no value in this worktree", name)
	}
	switch v := raw.(type) {
	case string:
		return v, nil
	case int:
		return strconv.Itoa(v), nil
	case bool:
		return strconv.FormatBool(v), nil
	default:
		return "", fmt.Errorf("%s is not a scalar (got %T)", name, raw)
	}
}

// ResolveOutput turns one declaration into its final key/value list.
//
// `secrets` holds every secret resolved for this pull; only the names this
// output selected are read out of it. The scoping is enforced here rather than
// by pre-filtering the caller's map, so there is exactly one place that
// decides an output may see a credential.
func ResolveOutput(o OutputConfig, data map[string]any, secrets map[string]resolvedSecret) (*ResolvedOutput, error) {
	res := &ResolvedOutput{Config: o}
	mode, err := o.FileMode()
	if err != nil {
		return nil, fmt.Errorf("outputs %s: %w", o.Name, err)
	}
	res.Mode = mode

	for _, name := range o.Include {
		v, err := lookupOrdinary(data, name)
		if err != nil {
			return nil, fmt.Errorf("outputs %s: include %s: %w", o.Name, name, err)
		}
		res.Pairs = append(res.Pairs, EnvPair{Key: name, Value: v})
	}
	for _, name := range o.Secrets {
		s, ok := secrets[name]
		if !ok {
			return nil, fmt.Errorf("outputs %s: secret %s was not resolved", o.Name, name)
		}
		res.Pairs = append(res.Pairs, EnvPair{Key: name, Value: s.value})
		if s.origin == SecretFromDefault {
			res.FromDefault = append(res.FromDefault, name)
		} else {
			res.FromStore = append(res.FromStore, name)
		}
	}
	for _, key := range o.Values.Keys {
		v := o.Values.By[key]
		if v.Literal != nil {
			res.Pairs = append(res.Pairs, EnvPair{Key: key, Value: *v.Literal})
			continue
		}
		val, err := lookupOrdinary(data, v.From)
		if err != nil {
			return nil, fmt.Errorf("outputs %s: values.%s: %w", o.Name, key, err)
		}
		res.Pairs = append(res.Pairs, EnvPair{Key: key, Value: val})
	}

	content, err := EncodeDotenv(res.Pairs)
	if err != nil {
		return nil, fmt.Errorf("outputs %s (%s): %w", o.Name, o.Out, err)
	}
	res.Content = content
	return res, nil
}

// dotenvHeader marks the file as wet's, so an edit lands in .wet.yaml rather
// than in a file the next pull overwrites.
const dotenvHeader = "# Generated by wet. Do not edit — change outputs: in .wet.yaml.\n"

// EncodeDotenv renders key/value pairs as a dotenv file.
//
// A newline in a value is rejected rather than escaped. dotenv dialects
// disagree about multi-line values — some accept `"a\nb"`, some accept a real
// newline inside quotes, some accept neither — and a value that half-parses
// puts the tail of a private key on a line the reader treats as the next
// assignment. Base64-encode PEM material into a single-line secret instead.
func EncodeDotenv(pairs []EnvPair) (string, error) {
	var b strings.Builder
	b.WriteString(dotenvHeader)
	for _, p := range pairs {
		if strings.ContainsAny(p.Value, "\n\r") {
			return "", fmt.Errorf("%s: value contains a newline, which no dotenv dialect reads back reliably — "+
				"store it base64-encoded on a single line", p.Key)
		}
		b.WriteString(p.Key)
		b.WriteString("=")
		b.WriteString(quoteEnvValue(p.Value))
		b.WriteString("\n")
	}
	return b.String(), nil
}

// ---------------------------------------------------------------------------
// The pull pipeline
// ---------------------------------------------------------------------------

// PullRequest is one run of the pipeline that generates declared outputs.
//
// There is exactly one such pipeline, and both `wet env pull` and `wet setup`
// go through it. The previous design had two — setup rendered targets itself
// while `env populate --all` did its own thing — and they diverged: only one
// of them read a target's secrets, so the file setup wrote and the file a
// re-render wrote were not the same file.
type PullRequest struct {
	Cfg *WetConfig
	// TreePath is the checkout being configured; every `out:` resolves
	// against it, so a pull behaves the same from any subdirectory.
	TreePath string
	// Data is the ordinary worktree value set: reserved fields, allocated
	// ports, pools and handles, `.env.local` overrides, and resolved `env:`
	// entries. Secrets are never in here.
	Data map[string]any
	// Only names the outputs to generate; empty means all of them.
	Only []string
	// Policy overrides secrets.policy for this run ("" keeps the config's).
	Policy string
	// Env overrides secrets.default_env for this run ("" keeps the config's,
	// and means nothing at all to a config without secrets.vaults:).
	Env string
	// Reader fetches secrets. Nil means the store named by secrets.store.
	Reader SecretReader
	// DryRun resolves and validates everything, and writes nothing. It still
	// contacts the store, because whether a `required` secret exists is
	// precisely what a dry run is asked to find out.
	DryRun bool
	// Out receives the progress and provenance lines. Nil means stdout.
	Out io.Writer
	// Content, when set, receives the generated file instead of the
	// filesystem: nothing is written, nothing is staged, and the caller pipes
	// the bytes wherever they belong —
	// `wet env pull fly-staging --stdout | fly secrets import`.
	//
	// Exactly one output may be selected: two dotenv files concatenated into
	// one stream is not a file either reader would accept.
	Content io.Writer
}

// PullOutputs runs the whole pipeline: select, resolve ordinary values,
// collect and fetch secrets once, resolve every requested output against its
// own scoped view, validate all of them, and only then write.
func PullOutputs(req PullRequest) error {
	out := req.Out
	if out == nil {
		out = os.Stdout
	}
	selected, err := req.Cfg.SelectOutputs(req.Only)
	if err != nil {
		return err
	}
	if req.Content != nil && len(selected) != 1 {
		return fmt.Errorf("a stream takes exactly one output, and %d were selected (%s) — name the one to write",
			len(selected), strings.Join(outputConfigNames(selected), ", "))
	}

	policy := req.Cfg.Secrets.PolicyOrDefault()
	if req.Policy != "" {
		if !slices.Contains(SecretPolicies, req.Policy) {
			return fmt.Errorf("unknown secret policy %q (known: %s)", req.Policy, strings.Join(SecretPolicies, ", "))
		}
		policy = req.Policy
	}

	env, err := req.Cfg.Secrets.SelectEnv(req.Env)
	if err != nil {
		return err
	}

	// Collect across every requested output first, so a shared credential is
	// one fetch no matter how many files consume it.
	needed := map[string]bool{}
	for _, o := range selected {
		for _, name := range o.Secrets {
			needed[name] = true
		}
	}
	// Named in the run's own output, not only in `explain`. A generated
	// .env.dev carries no record of which environment's vault filled it, so
	// the pull that wrote it is the only place that can say — and only when
	// this run actually reads a vault. Announcing an environment over a pull
	// that resolved no secrets claims an influence it did not have.
	if env != "" && len(needed) > 0 && policy != SecretPolicyDisabled {
		fmt.Fprintf(out, "environment: %s (of %s)\n", env, strings.Join(req.Cfg.Secrets.VaultEnvs(), ", "))
	}

	reader := req.Reader
	if reader == nil && len(needed) > 0 && policy != SecretPolicyDisabled {
		reader, err = newStoreReader(req.Cfg)
		if err != nil {
			// A machine with no `op` installed is precisely the case
			// `fallback` exists for: the client being unavailable is one of
			// the failures it promises to survive. Turning it into a reader
			// that fails every read keeps that promise while still letting
			// `required` report it — and keeps one policy decision instead of
			// two, so the two cannot disagree about what "unavailable" means.
			if policy == SecretPolicyRequired {
				return err
			}
			reader = failingReader{err}
		}
	}
	if reader != nil {
		reader = &latchingReader{inner: reader}
	}
	secrets, err := resolveSecrets(req.Cfg, sortedKeys(needed), policy, env, reader)
	if err != nil {
		return err
	}

	resolved := make([]*ResolvedOutput, 0, len(selected))
	toWrite := make([]*ResolvedOutput, 0, len(selected))
	for _, o := range selected {
		r, err := ResolveOutput(o, req.Data, secrets)
		if err != nil {
			return err
		}
		resolved = append(resolved, r)
	}

	for _, r := range resolved {
		reportSecretProvenance(out, req.Cfg, policy, r)
		// Not an error — a repo may genuinely need a group-readable file for a
		// container running as another user. But widening a file that holds
		// credentials is a decision, and a silent one reads as the default.
		// Warned here rather than in Validate() so it fires once, when a file
		// is about to be written, and not from every command that loads config.
		if len(r.Config.Secrets) > 0 && r.Mode&0o077 != 0 {
			// Through `out` like every other line in this loop: a caller that
			// supplies Out is capturing this run's narrative, and a lone write
			// to os.Stderr interleaves outside its control.
			fmt.Fprintf(out, "  warning: %s carries %d secret(s) at mode %04o, readable beyond its owner\n",
				r.Config.Out, len(r.Config.Secrets), r.Mode)
		}
		if req.DryRun {
			fmt.Fprintf(out, "  would write %s (%d keys)\n", r.Config.Out, len(r.Pairs))
			continue
		}
		toWrite = append(toWrite, r)
	}
	if req.DryRun {
		return nil
	}
	if req.Content != nil {
		r := toWrite[0]
		if _, err := io.WriteString(req.Content, r.Content); err != nil {
			return fmt.Errorf("outputs %s: %w", r.Config.Name, err)
		}
		fmt.Fprintf(out, "  Wrote %s (%d keys) to the stream — no file\n", r.Config.Name, len(r.Pairs))
		return nil
	}
	if err := writeOutputFiles(req.TreePath, toWrite); err != nil {
		return err
	}
	for _, r := range toWrite {
		fmt.Fprintf(out, "  Wrote %s (%d keys)\n", r.Config.Out, len(r.Pairs))
	}
	return nil
}

// writeOutputFiles publishes a whole pull's files in two phases: stage every
// one to a temp file beside its target, and only once all of them are staged,
// rename them into place.
//
// Per-file atomicity is not enough here. Resolution failures already touch
// nothing, but an I/O failure — ENOSPC, a directory that turned read-only,
// a permission wet cannot fix with MkdirAll — hits the second output after the
// first has already been replaced, and the stale-but-current-looking first
// file is what the next `docker compose up` boots from.
//
// This is not true multi-file atomicity, and POSIX offers none: rename N can
// still fail after rename N-1 succeeded. What the split buys is that every
// expensive and failure-prone step (mkdir, allocate blocks, write bytes,
// chmod) happens while nothing is visible, so the residual exposure is a
// handful of renames within already-created directories on a filesystem that
// just accepted the data. Those fail essentially only if the filesystem is
// coming apart, in which case a partial write is not the interesting problem.
func writeOutputFiles(treePath string, outs []*ResolvedOutput) error {
	staged := make([]stagedOutput, 0, len(outs))
	// Every temp file is removed on any failure, including one partway
	// through the rename loop, where the already-renamed entries are no
	// longer temp files and the rest are still garbage on disk.
	committed := false
	defer func() {
		if committed {
			return
		}
		for _, s := range staged {
			os.Remove(s.tmp)
		}
	}()

	for _, r := range outs {
		// A directory wet creates for a secret-bearing file need not be
		// listable by anyone else. It only ever leaks the file's name, which
		// .wet.yaml already publishes, so this is cheap rather than critical.
		dirMode := os.FileMode(0o755)
		if len(r.Config.Secrets) > 0 {
			dirMode = 0o700
		}
		s, err := stageOutputFile(filepath.Join(treePath, r.Config.Out), r.Content, r.Mode, dirMode)
		if err != nil {
			return fmt.Errorf("outputs %s: %w", r.Config.Name, err)
		}
		staged = append(staged, s)
	}
	for i, s := range staged {
		if err := os.Rename(s.tmp, s.path); err != nil {
			return fmt.Errorf("outputs %s: replace %s: %w", outs[i].Config.Name, s.path, err)
		}
	}
	committed = true
	return nil
}

// newStoreReader builds the client for the configured store. The provider is
// never named on the command line — `secrets.store` decides — so this is the
// single place a store name turns into a client.
func newStoreReader(cfg *WetConfig) (SecretReader, error) {
	if cfg.Secrets == nil {
		return nil, errors.New("outputs select secrets, but .wet.yaml has no secrets: block")
	}
	switch cfg.Secrets.Store {
	case secretsStoreOP:
		if err := requireOP(); err != nil {
			return nil, err
		}
		return newOPSecretReader(opClient{account: cfg.Secrets.Account}), nil
	default:
		return nil, fmt.Errorf("secrets.store %q is not supported (known: %s)", cfg.Secrets.Store, secretsStoreOP)
	}
}

// storeLabel is the human name of the configured store, for provenance lines.
func storeLabel(cfg *WetConfig) string {
	if cfg.Secrets != nil && cfg.Secrets.Store == secretsStoreOP {
		return "1Password"
	}
	return "the secret store"
}

// reportSecretProvenance says which names came from the store and which fell
// back to a declared default — names only, never values.
//
// Falling back must not be silent. A developer who believes Sentry and Google
// sign-in are configured, when wet actually wrote empty strings, debugs the
// wrong thing for an hour.
func reportSecretProvenance(w io.Writer, cfg *WetConfig, policy string, r *ResolvedOutput) {
	if len(r.FromStore)+len(r.FromDefault) == 0 {
		return
	}
	// Under `disabled` the store was never contacted, so naming it — even as
	// "0 from 1Password" — invites the reader to think it was tried and had
	// nothing.
	if policy == SecretPolicyDisabled {
		fmt.Fprintf(w, "  Secrets for %s: %d from declared defaults (store disabled)\n",
			r.Config.Out, len(r.FromDefault))
		return
	}
	fmt.Fprintf(w, "  Secrets for %s: %d from %s, %d defaults\n",
		r.Config.Out, len(r.FromStore), storeLabel(cfg), len(r.FromDefault))
	if len(r.FromDefault) > 0 {
		fmt.Fprintf(w, "    defaults: %s\n", strings.Join(r.FromDefault, ", "))
	}
}

// stagedOutput is one output's content sitting in a temp file beside its
// target, fully written and already at its final mode, awaiting the rename
// that makes it visible.
type stagedOutput struct {
	path string
	tmp  string
}

// stageOutputFile does everything a write needs except become visible: create
// the parent, write a sibling temp file, and set the final mode on it.
//
// A truncate-and-write would leave a half-written env file on disk if the
// process died mid-write, and the reader of that file is a booting app. The
// temp file is created 0600 regardless of the final mode, so a secret is never
// briefly world-readable between create and chmod. The caller renames; on any
// failure it removes the temp file instead.
func stageOutputFile(path, content string, mode os.FileMode, dirMode os.FileMode) (stagedOutput, error) {
	dir := filepath.Dir(path)
	// A declared output may live in a directory nothing has created yet — a
	// gitignored .vscode/, a build dir — and a bare ENOENT on a path the user
	// can see is correct is a bad way to say so.
	//
	// MkdirAll leaves an existing directory's mode alone, so this only ever
	// narrows a directory wet itself creates.
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return stagedOutput{}, fmt.Errorf("create parent of %s: %w", path, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".wet-*")
	if err != nil {
		return stagedOutput{}, fmt.Errorf("create temp beside %s: %w", path, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return stagedOutput{}, fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return stagedOutput{}, fmt.Errorf("write %s: %w", path, err)
	}
	// Chmod the temp file rather than the target: the mode is then already
	// correct at the instant the rename makes the content visible.
	if err := os.Chmod(tmpName, mode); err != nil {
		os.Remove(tmpName)
		return stagedOutput{}, fmt.Errorf("chmod %s: %w", path, err)
	}
	return stagedOutput{path: path, tmp: tmpName}, nil
}
