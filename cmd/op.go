package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var opCmd = &cobra.Command{
	Use:   "op",
	Short: "Point this machine at the 1Password account and vault named in .wet.yaml",
	Long: `Choose which 1Password account and vault this workstation reads secrets
from, and remember the answer in .wet.local.yaml.

The map itself lives in .wet.yaml: secrets.definitions: says which credentials
exist and which field each lives in, and an output's secrets: list says which
files may see them. That is one inventory, shared by everyone.

Which account you are signed into and which vault you keep the project's item
in are facts about this machine, so they are the only parts that live in the
gitignored overlay.

Secrets move with the env commands, not with this one:

  wet env pull                  read the store and write the declared files
  wet env push backend --from … write declared secrets back to the store`,
}

var opVaultCmd = &cobra.Command{
	Use:   "vault",
	Short: "Pick the account and vault, and remember them in .wet.local.yaml",
	Long: `Resolve the 1Password account and vault for this machine — from the
flags, from .wet.yaml/.wet.local.yaml, or by asking — and write the answer to
.wet.local.yaml so nothing asks again.`,
	Args: cobra.NoArgs,
	RunE: runOPVault,
}

var (
	opFlags      opTargetFlags
	opNoRemember bool
)

func init() {
	rootCmd.AddCommand(opCmd)
	opCmd.AddCommand(opVaultCmd)
	addOPTargetFlags(opVaultCmd, &opFlags)
	opVaultCmd.Flags().BoolVar(&opNoRemember, "no-remember", false,
		"don't write the picked account/vault to .wet.local.yaml")
}

func runOPVault(cmd *cobra.Command, args []string) error {
	if err := requireOP(); err != nil {
		return err
	}
	repoRoot, err := getRepoRoot()
	if err != nil {
		return err
	}
	cfg, err := LoadWetConfig(repoRoot)
	if err != nil {
		return err
	}
	if cfg.Secrets == nil {
		return errors.New(".wet.yaml has no secrets: block — there is no vault to point at")
	}
	target, picked, err := resolveOPTarget(cfg, opFlags)
	if err != nil {
		if errors.Is(err, errPickerCancelled) {
			return errors.New("cancelled")
		}
		return err
	}
	if !opNoRemember {
		// The overlay is one per MACHINE and is read from the checkout holding
		// the trunk, never from the current one (see LocalConfigDir). Writing
		// it beside a worktree would print "Remembered" while writing a file
		// nothing ever reads, and the next pull would still say "no vault".
		//
		// A value supplied on the command line is remembered too: the user
		// said which vault they want, and asking again next run — or worse,
		// reporting success having stored nothing — is the same surprise.
		remember := picked
		if remember.account == "" {
			remember.account = opFlags.account
		}
		if remember.vault == "" {
			remember.vault = opFlags.vault
		}
		if err := rememberOPTarget(LocalConfigDir(repoRoot, repoRoot), remember); err != nil {
			return err
		}
	}
	fmt.Printf("  Account: %s\n", orNone(target.account))
	fmt.Printf("  Vault:   %s\n", orNone(target.vault))
	fmt.Printf("  Item:    %s\n", orNone(target.item))
	return nil
}

// orNone renders an unset target field as something readable rather than as
// an empty column the user has to guess at.
func orNone(v string) string {
	if v == "" {
		return "(not set)"
	}
	return v
}

// rememberOPTarget records an interactively picked account/vault in
// .wet.local.yaml.
//
// Machine-local is the honest scope: which account you are signed into and
// which vault you use is a fact about this workstation, not about the project,
// so it must not land in the committed .wet.yaml. The overlay already exists
// for exactly this, and it means the picker asks once rather than every run.
func rememberOPTarget(repoRoot string, picked opTarget) error {
	if picked.account == "" && picked.vault == "" {
		return nil
	}
	path := filepath.Join(repoRoot, wetLocalConfigFileName)

	// Edited as a node tree, not re-marshalled from a map. The overlay is a
	// file a human writes and annotates, and a map round trip drops every
	// comment and alphabetizes the keys — including `outputs[].values`, whose
	// order the loader goes to real trouble to preserve. A writer that undoes
	// what the reader protects is the kind of disagreement nobody notices
	// until the generated file comes out in the wrong order.
	var doc yaml.Node
	if raw, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	root := documentRoot(&doc)
	if root == nil {
		if len(doc.Content) > 0 {
			return fmt.Errorf("%s: top level must be a mapping", path)
		}
		root = &yaml.Node{Kind: yaml.MappingNode}
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}
	}

	secrets := mappingValue(root, "secrets")
	if secrets == nil {
		secrets = &yaml.Node{Kind: yaml.MappingNode}
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "secrets"}, secrets)
	}
	if secrets.Kind != yaml.MappingNode {
		return fmt.Errorf("%s: secrets: must be a mapping", path)
	}
	for _, e := range []struct{ key, value string }{
		{"account", picked.account},
		{"vault", picked.vault},
	} {
		if e.value == "" {
			continue
		}
		mergeYAMLNode(secrets,
			&yaml.Node{Kind: yaml.ScalarNode, Value: e.key},
			&yaml.Node{Kind: yaml.ScalarNode, Value: e.value, Tag: "!!str"})
	}

	out, err := MarshalYAMLDocument(&doc)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Printf("  Remembered account/vault in %s\n", wetLocalConfigFileName)
	return nil
}
