package cmd

import (
	_ "embed"
	"fmt"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
)

// secretGroupCatalogYAML is the shipped group catalog. It is embedded rather
// than installed into the consuming repo on purpose: what "google" means is a
// fact about Google, not about the project, and a per-repo copy would be a
// second definition to keep in sync. Extending the catalog is a block in
// cmd/secret-groups.yaml, which every project picks up on upgrade.
//
//go:embed secret-groups.yaml
var secretGroupCatalogYAML string

// SecretGroupKey is one env key a group contributes.
type SecretGroupKey struct {
	// Default is the value written into env:. Empty means a real secret
	// belongs there and a fresh clone should still render.
	Default string `yaml:"default"`
	// Secret is the vault field this key lives in. Empty means the value is
	// derived or a safe dev default and never goes to the vault.
	Secret string `yaml:"secret"`
}

// SecretGroup is one selectable provider.
type SecretGroup struct {
	Description string                    `yaml:"description"`
	Env         map[string]SecretGroupKey `yaml:"env"`
	// Order is the key order as written in the catalog, so a scaffolded block
	// reads the way the catalog does rather than in map order.
	Order []string `yaml:"-"`
}

// LoadSecretGroups parses the embedded catalog.
func LoadSecretGroups() (map[string]SecretGroup, []string, error) {
	return parseSecretGroups(secretGroupCatalogYAML)
}

func parseSecretGroups(body string) (map[string]SecretGroup, []string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		return nil, nil, fmt.Errorf("parse secret group catalog: %w", err)
	}
	if len(doc.Content) == 0 {
		return nil, nil, fmt.Errorf("secret group catalog is empty")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("secret group catalog: top level must be a mapping")
	}

	groups := map[string]SecretGroup{}
	var order []string
	for i := 0; i+1 < len(root.Content); i += 2 {
		name := root.Content[i].Value
		var group SecretGroup
		if err := root.Content[i+1].Decode(&group); err != nil {
			return nil, nil, fmt.Errorf("secret group %q: %w", name, err)
		}
		// Key order comes from the node, not the decoded map, so the
		// scaffolded env: block matches the catalog's reading order.
		if envNode := mappingValue(root.Content[i+1], "env"); envNode != nil {
			for j := 0; j+1 < len(envNode.Content); j += 2 {
				group.Order = append(group.Order, envNode.Content[j].Value)
			}
		}
		if err := validateSecretGroup(name, group); err != nil {
			return nil, nil, err
		}
		groups[name] = group
		order = append(order, name)
	}
	return groups, order, nil
}

func validateSecretGroup(name string, g SecretGroup) error {
	if g.Description == "" {
		return fmt.Errorf("secret group %q: description is required (it is the picker's subtitle)", name)
	}
	if len(g.Env) == 0 {
		return fmt.Errorf("secret group %q: no env keys", name)
	}
	for key, spec := range g.Env {
		if isReservedTemplateField(key) {
			return fmt.Errorf("secret group %q: %s is reserved by wet", name, key)
		}
		if strings.Contains(spec.Default, "{{") {
			if _, err := template.New(key).Parse(spec.Default); err != nil {
				return fmt.Errorf("secret group %q: %s default: %w", name, key, err)
			}
			// A derived value is computed per worktree. Storing it would pin
			// every checkout to whichever URL happened to be seeded.
			if spec.Secret != "" {
				return fmt.Errorf("secret group %q: %s is derived from a template, so it must not also name a vault field (%s)",
					name, key, spec.Secret)
			}
		}
	}
	return nil
}

// mappingValue returns the value node for a key in a mapping node.
func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}
