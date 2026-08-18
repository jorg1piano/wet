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

// ScaffoldResult reports what a scaffold run changed, so the command can print
// it and the tests can assert on it without reparsing YAML.
type ScaffoldResult struct {
	AddedEnv     []string
	AddedSecrets []string
	KeptEnv      []string
	Groups       []string
}

// ScaffoldGroups adds the selected groups' keys to a .wet.yaml document.
//
// It edits the parsed node tree rather than re-marshalling a struct, because
// a .wet.yaml is mostly comments — the annotated schema is the documentation —
// and a struct round trip would delete every one of them.
//
// Existing keys are never touched. Re-running after you have filled in a real
// value must not reset it to the catalog's empty default, and that rule is
// what makes the command safe to run again when you add a provider.
func ScaffoldGroups(doc *yaml.Node, catalog map[string]SecretGroup, groups []string, section string) (*ScaffoldResult, error) {
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf(".wet.yaml: top level must be a mapping")
	}
	root := doc.Content[0]
	res := &ScaffoldResult{Groups: groups}

	envNode := mappingValue(root, "env")
	if envNode == nil {
		envNode = &yaml.Node{Kind: yaml.MappingNode}
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "env"}, envNode)
	}
	if envNode.Kind != yaml.MappingNode {
		return nil, fmt.Errorf(".wet.yaml: env: must be a mapping")
	}

	for _, name := range groups {
		group, ok := catalog[name]
		if !ok {
			return nil, fmt.Errorf("unknown group %q (known: %s)",
				name, strings.Join(sortedKeys(catalog), ", "))
		}
		first := true
		for _, key := range group.Order {
			spec := group.Env[key]
			if mappingValue(envNode, key) != nil {
				res.KeptEnv = append(res.KeptEnv, key)
			} else {
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
	}

	if err := scaffoldSecretsSection(root, catalog, groups, section, res); err != nil {
		return nil, err
	}
	markEnabledGroups(root, envNode, groups)
	return res, nil
}

// scaffoldSecretsSection adds the vault-backed keys of each group to one
// section of the secrets: block, creating the block if there is none.
//
// Only keys carrying `secret:` go in. A derived value like a redirect URI is
// computed per worktree, so putting it in the vault would hand every checkout
// the same URL — the failure this split exists to prevent.
func scaffoldSecretsSection(root *yaml.Node, catalog map[string]SecretGroup, groups []string, section string, res *ScaffoldResult) error {
	if section == "" {
		return nil
	}
	secretsNode := mappingValue(root, "secrets")
	if secretsNode == nil {
		secretsNode = &yaml.Node{Kind: yaml.MappingNode}
		secretsNode.Content = append(secretsNode.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "store"},
			&yaml.Node{Kind: yaml.ScalarNode, Value: secretsStoreOP})
		key := &yaml.Node{Kind: yaml.ScalarNode, Value: "secrets"}
		key.HeadComment = "Where the values above actually live. vault/item/account may be\n" +
			"omitted — `wet op vault` asks and remembers the answer in the\n" +
			"gitignored .wet.local.yaml."
		root.Content = append(root.Content, key, secretsNode)
	}
	if secretsNode.Kind != yaml.MappingNode {
		return fmt.Errorf(".wet.yaml: secrets: must be a mapping")
	}

	sectionsNode := mappingValue(secretsNode, "sections")
	if sectionsNode == nil {
		sectionsNode = &yaml.Node{Kind: yaml.MappingNode}
		secretsNode.Content = append(secretsNode.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "sections"}, sectionsNode)
	}
	target := mappingValue(sectionsNode, section)
	if target == nil {
		target = &yaml.Node{Kind: yaml.MappingNode}
		sectionsNode.Content = append(sectionsNode.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: section}, target)
	}

	for _, name := range groups {
		group := catalog[name]
		for _, key := range group.Order {
			spec := group.Env[key]
			if spec.Secret == "" || mappingValue(target, key) != nil {
				continue
			}
			target.Content = append(target.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: key},
				&yaml.Node{Kind: yaml.ScalarNode, Value: spec.Secret})
			res.AddedSecrets = append(res.AddedSecrets, key)
		}
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
		"A `secret` key is EMPTY here so a fresh clone renders and boots with\n" +
		"no vault at all. Real values arrive over the secrets channel, and a\n" +
		"supplied key wins over the default below — which is the whole reason\n" +
		"the defaults are allowed to be empty."

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
