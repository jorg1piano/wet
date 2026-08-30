package cmd

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// enabledGroupsPrefix marks the comment that records which groups have been
// scaffolded, so a later run can add to the list instead of asking again.
const enabledGroupsPrefix = "Enabled groups: "

// ScaffoldOptions says where a scaffold run puts what it writes.
//
// A struct for one field, because the next option is cheap to add here and
// costs a signature change at every call site otherwise.
//
// There is deliberately no "write the credentials as env: defaults instead"
// option. A definition's `default:` under `policy: fallback` already gives a
// clone with no vault something to boot on, and a second way to model the same
// thing is what this schema exists to remove — worse, keys written that way
// can never be promoted, since a name cannot be an env: entry and a definition
// at once.
type ScaffoldOptions struct {
	// Output is the name of the outputs: entry that consumes the scaffolded
	// keys. A group nobody consumes is dead config, so the scaffolder always
	// wires its keys into one file — creating the output when it is missing.
	Output string
}

// ScaffoldResult reports what a scaffold run changed, so the command can print
// it and the tests can assert on it without reparsing YAML.
type ScaffoldResult struct {
	// AddedEnv are the ordinary env: keys written: the catalog's derived and
	// plainly public values.
	AddedEnv []string
	// AddedSecrets are the secrets.definitions: entries written.
	AddedSecrets []string
	// KeptEnv are catalog keys that were already in the file and left alone,
	// whichever block they were found in.
	KeptEnv []string
	// Output is the outputs: entry the keys were wired into, and
	// CreatedOutput says whether this run had to invent it.
	Output        string
	CreatedOutput bool
	// OutputPath is the invented output's `out:`, so the caller can gitignore
	// the file wet has just promised to generate. Empty unless CreatedOutput.
	OutputPath string
	Groups     []string
}

// ScaffoldGroups adds the selected groups' keys to a .wet.yaml document.
//
// It edits the parsed node tree rather than re-marshalling a struct, because
// a .wet.yaml is mostly comments — the annotated schema is the documentation —
// and a struct round trip would delete every one of them.
//
// A catalog key with a `secret:` field becomes a secrets.definitions: entry
// carrying the catalog's fallback in `default:`; a key without one is a
// derived or plainly public value and stays in env:. The two are disjoint on
// purpose — a definition sharing a name with an env: key fails validation,
// because then the output key would be ambiguous and a store value could
// shadow a computed one.
//
// Existing keys are never touched. Re-running after you have filled in a real
// value must not reset it to the catalog's default, and that rule is what
// makes the command safe to run again when you add a provider.
func ScaffoldGroups(doc *yaml.Node, catalog map[string]SecretGroup, groups []string, opts ScaffoldOptions) (*ScaffoldResult, error) {
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf(".wet.yaml: top level must be a mapping")
	}
	if opts.Output == "" {
		return nil, fmt.Errorf("scaffold: no output named — scaffolded keys need a file to appear in")
	}
	root := doc.Content[0]
	res := &ScaffoldResult{Groups: groups, Output: opts.Output}

	for _, name := range groups {
		if _, ok := catalog[name]; !ok {
			return nil, fmt.Errorf("unknown group %q (known: %s)",
				name, strings.Join(sortedKeys(catalog), ", "))
		}
	}

	envNode := mappingValue(root, "env")
	if envNode == nil {
		envNode = &yaml.Node{Kind: yaml.MappingNode}
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "env"}, envNode)
	}
	if envNode.Kind != yaml.MappingNode {
		return nil, fmt.Errorf(".wet.yaml: env: must be a mapping")
	}

	// The two lists the output ends up selecting. Collected across all groups
	// first so the output entry is touched once, and so a key the file already
	// carries is still wired up rather than assumed to be selected already.
	var includeKeys, secretKeys []string

	for _, name := range groups {
		group := catalog[name]
		first := true
		for _, key := range group.Order {
			spec := group.Env[key]
			// A credential the file already carries as an ordinary value stays
			// one. Turning it into a definition would collide with the env:
			// entry and make the config fail to load, and deleting the value
			// the user may have filled in is not a scaffold's call to make.
			asSecret := spec.Secret != "" && mappingValue(envNode, key) == nil
			if asSecret {
				secretKeys = append(secretKeys, key)
				continue
			}
			includeKeys = append(includeKeys, key)
			if mappingValue(envNode, key) != nil {
				res.KeptEnv = append(res.KeptEnv, key)
				continue
			}
			value := &yaml.Node{Kind: yaml.ScalarNode, Value: spec.Default, Tag: "!!str"}
			// Quote every default, including the empty one: a bare empty
			// value parses as null, and `KEY: null` renders as "<no
			// value>" rather than an empty string.
			value.Style = yaml.SingleQuotedStyle
			keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: key}
			if first {
				keyNode.HeadComment = name
			}
			envNode.Content = append(envNode.Content, keyNode, value)
			res.AddedEnv = append(res.AddedEnv, key)
			first = false
		}
	}

	if err := scaffoldSecretDefinitions(root, catalog, groups, secretKeys, res); err != nil {
		return nil, err
	}
	if err := scaffoldOutput(root, opts.Output, includeKeys, secretKeys, res); err != nil {
		return nil, err
	}
	markEnabledGroups(root, envNode, groups)
	return res, nil
}

// scaffoldSecretDefinitions writes one secrets.definitions: entry per
// vault-backed catalog key, creating the secrets: block if there is none.
//
// The catalog's fallback goes into `default:`, not into env:. Under the
// default `fallback` policy that is what lets a clone that cannot reach the
// store — not signed in, `op` not installed, vault not named yet — still
// render and boot, while keeping the name a secret everywhere else: only
// an output that names it may see it, and `wet env push` refuses to send the
// placeholder back to the store.
//
// Only keys carrying `secret:` go in. A derived value like a redirect URI is
// computed per worktree, so putting it in the vault would hand every checkout
// the same URL — the failure this split exists to prevent.
func scaffoldSecretDefinitions(root *yaml.Node, catalog map[string]SecretGroup, groups []string, secretKeys []string, res *ScaffoldResult) error {
	if len(secretKeys) == 0 {
		return nil
	}
	secretsNode := mappingValue(root, "secrets")
	if secretsNode == nil {
		secretsNode = &yaml.Node{Kind: yaml.MappingNode}
		secretsNode.Content = append(secretsNode.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "store"},
			&yaml.Node{Kind: yaml.ScalarNode, Value: secretStoreProviderOnePassword},
			&yaml.Node{Kind: yaml.ScalarNode, Value: "policy"},
			&yaml.Node{Kind: yaml.ScalarNode, Value: SecretPolicyFallback})
		key := &yaml.Node{Kind: yaml.ScalarNode, Value: "secrets"}
		key.HeadComment = "Every credential this repo has, declared once. `policy: fallback` reads\n" +
			"the store and falls back to a definition's `default:` when it cannot —\n" +
			"naming which ones fell back, so an empty placeholder never passes for a\n" +
			"configured provider. Add `item:` and `vault:` here — which item the fields\n" +
			"sit on, and which vault holds it, are facts about the project. Only the\n" +
			"account is per-workstation; it goes in the gitignored " + wetLocalConfigFileName + "."
		root.Content = append(root.Content, key, secretsNode)
	}
	if secretsNode.Kind != yaml.MappingNode {
		return fmt.Errorf(".wet.yaml: secrets: must be a mapping")
	}

	defsNode := mappingValue(secretsNode, "definitions")
	if defsNode == nil {
		defsNode = &yaml.Node{Kind: yaml.MappingNode}
		secretsNode.Content = append(secretsNode.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "definitions"}, defsNode)
	}
	if defsNode.Kind != yaml.MappingNode {
		return fmt.Errorf(".wet.yaml: secrets.definitions: must be a mapping")
	}

	wanted := map[string]bool{}
	for _, key := range secretKeys {
		wanted[key] = true
	}
	for _, name := range groups {
		group := catalog[name]
		first := true
		for _, key := range group.Order {
			spec := group.Env[key]
			if !wanted[key] {
				continue
			}
			if mappingValue(defsNode, key) != nil {
				res.KeptEnv = append(res.KeptEnv, key)
				continue
			}
			def := &yaml.Node{Kind: yaml.MappingNode}
			def.Content = append(def.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: "field"},
				&yaml.Node{Kind: yaml.ScalarNode, Value: spec.Secret},
				&yaml.Node{Kind: yaml.ScalarNode, Value: "default"},
				// Quoted, and written even when the catalog default is empty:
				// an omitted default is "this must come from the store or
				// fail", an empty one is "this provider is simply not
				// configured locally". A bare empty scalar would decode as
				// null and silently mean the first.
				&yaml.Node{Kind: yaml.ScalarNode, Value: spec.Default, Tag: "!!str", Style: yaml.SingleQuotedStyle})
			keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: key}
			if first {
				keyNode.HeadComment = name
			}
			defsNode.Content = append(defsNode.Content, keyNode, def)
			res.AddedSecrets = append(res.AddedSecrets, key)
			first = false
		}
	}
	return nil
}

// scaffoldOutput wires the scaffolded keys into the named outputs: entry,
// creating it when the file has none by that name.
//
// Without this a scaffolded group is inert: the definitions exist, no
// generated file selects them, and `wet env pull` writes exactly what it wrote
// before — which reads as wet ignoring the provider you just added.
func scaffoldOutput(root *yaml.Node, name string, includeKeys, secretKeys []string, res *ScaffoldResult) error {
	outputsNode := mappingValue(root, "outputs")
	if outputsNode == nil {
		outputsNode = &yaml.Node{Kind: yaml.SequenceNode}
		key := &yaml.Node{Kind: yaml.ScalarNode, Value: "outputs"}
		key.HeadComment = "The environment files this repo generates. Each one lists the keys it\n" +
			"carries, so .wet.yaml alone answers what a package reads and which of\n" +
			"those are secrets — and a secret is visible only to an output that\n" +
			"names it."
		root.Content = append(root.Content, key, outputsNode)
	}
	if outputsNode.Kind != yaml.SequenceNode {
		return fmt.Errorf(".wet.yaml: outputs: must be a list")
	}

	var target *yaml.Node
	for _, entry := range outputsNode.Content {
		if entry.Kind != yaml.MappingNode {
			continue
		}
		if n := mappingValue(entry, "name"); n != nil && n.Value == name {
			target = entry
			break
		}
	}
	if target == nil {
		// One literal: the path written into `out:` and the path reported as
		// OutputPath are the same file, and OutputPath is what the caller
		// gitignores and prints. Two copies means a change to one of them
		// leaves wet ignoring — and naming — a file it does not generate.
		outPath := name + "/.env.dev"
		target = &yaml.Node{Kind: yaml.MappingNode}
		target.Content = append(target.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "name"},
			&yaml.Node{Kind: yaml.ScalarNode, Value: name},
			&yaml.Node{Kind: yaml.ScalarNode, Value: "out"},
			&yaml.Node{Kind: yaml.ScalarNode, Value: outPath},
			&yaml.Node{Kind: yaml.ScalarNode, Value: "format"},
			&yaml.Node{Kind: yaml.ScalarNode, Value: OutputFormatDotenv})
		outputsNode.Content = append(outputsNode.Content, target)
		res.CreatedOutput = true
		res.OutputPath = outPath
	}

	if err := appendUnique(target, "include", includeKeys); err != nil {
		return err
	}
	return appendUnique(target, "secrets", secretKeys)
}

// appendUnique adds names to a string sequence under key, creating it when
// absent and skipping anything already listed. Re-listing a key would make the
// output declare it twice, which validateOutputs rejects — so an idempotent
// re-run has to be a no-op here rather than an error later.
func appendUnique(mapping *yaml.Node, key string, names []string) error {
	if len(names) == 0 {
		return nil
	}
	seq := mappingValue(mapping, key)
	if seq == nil {
		seq = &yaml.Node{Kind: yaml.SequenceNode}
		mapping.Content = append(mapping.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: key}, seq)
	}
	// Returning quietly here would let the scaffold report success having
	// wired nothing — the keys go into secrets.definitions:, no output selects
	// them, and the generated file is short every credential the user just
	// added. The config is hand-written and wrong; say so.
	if seq.Kind != yaml.SequenceNode {
		return fmt.Errorf(".wet.yaml: outputs %s must be a list, so wet cannot add %s to it",
			key, strings.Join(names, ", "))
	}
	present := make(map[string]bool, len(seq.Content))
	for _, item := range seq.Content {
		present[item.Value] = true
	}
	for _, name := range names {
		if present[name] {
			continue
		}
		present[name] = true
		seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: name})
	}
	return nil
}

// markEnabledGroups keeps the "Enabled groups:" comment on env: current, so
// the next run knows what is already scaffolded and the file says what it
// contains without cross-referencing the catalog.
func markEnabledGroups(root, envNode *yaml.Node, groups []string) {
	all := map[string]bool{}
	for _, g := range groups {
		all[g] = true
	}
	for _, g := range EnabledGroups(root) {
		all[g] = true
	}
	names := make([]string, 0, len(all))
	for g := range all {
		names = append(names, g)
	}
	sort.Strings(names)

	comment := enabledGroupsPrefix + strings.Join(names, ", ") + "\n\n" +
		"Only ordinary values live here — derived URLs, hosts, plain settings —\n" +
		"and they are committed as written. A provider's credentials are NOT in\n" +
		"this block: they are declared under secrets.definitions:, where each one\n" +
		"carries the dev fallback a vault-less clone boots on in its `default:`.\n" +
		"Keeping the two apart is what lets a secret stay scoped to the outputs\n" +
		"that name it, instead of being readable by every generated file."

	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "env" {
			root.Content[i].HeadComment = comment
			return
		}
	}
	envNode.HeadComment = comment
}

// EnabledGroups reads the groups a previous scaffold recorded.
func EnabledGroups(root *yaml.Node) []string {
	if root == nil || root.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "env" {
			continue
		}
		for _, line := range strings.Split(root.Content[i].HeadComment, "\n") {
			line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "#"))
			if !strings.HasPrefix(line, enabledGroupsPrefix) {
				continue
			}
			var out []string
			for _, name := range strings.Split(strings.TrimPrefix(line, enabledGroupsPrefix), ",") {
				if name = strings.TrimSpace(name); name != "" {
					out = append(out, name)
				}
			}
			return out
		}
	}
	return nil
}

// ReadYAMLDocument parses a file into a node tree, comments included.
func ReadYAMLDocument(path string) (*yaml.Node, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &doc, nil
}

// MarshalYAMLDocument renders a node tree back to bytes at wet's indentation.
func MarshalYAMLDocument(doc *yaml.Node) ([]byte, error) {
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return []byte(spaceOutYAMLBlocks(b.String())), nil
}

// spaceOutYAMLBlocks restores the blank line before each top-level key and
// each comment block.
//
// yaml.v3 preserves comments but not blank lines, so a round trip silently
// compacts a file whose whole job is to be read by a human — .wet.yaml is
// mostly annotation. Reinstating the separation is cosmetic to the parser and
// the difference between a config that still reads as documentation and a
// wall of keys.
func spaceOutYAMLBlocks(body string) string {
	lines := strings.Split(body, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		isComment := strings.HasPrefix(trimmed, "#")
		if len(out) > 0 && trimmed != "" {
			prev := out[len(out)-1]
			prevTrimmed := strings.TrimSpace(prev)
			prevIndent := len(prev) - len(strings.TrimLeft(prev, " "))

			startsComment := isComment && !strings.HasPrefix(strings.TrimSpace(prev), "#")
			// A top-level key only needs air after a nested block; two plain
			// scalars in a row belong together.
			endsNested := indent == 0 && !isComment && prevIndent > 0

			separate := startsComment || endsNested
			// Never split a comment from the key it documents, never follow a
			// mapping's own key with a gap, and never double a blank line.
			if prevTrimmed == "" || strings.HasPrefix(prevTrimmed, "#") || strings.HasSuffix(prevTrimmed, ":") {
				separate = false
			}
			if separate {
				out = append(out, "")
			}
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}
