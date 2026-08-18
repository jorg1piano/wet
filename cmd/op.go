package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var opCmd = &cobra.Command{
	Use:   "op",
	Short: "Work with the 1Password vault named in .wet.yaml",
	Long: `Move secrets between this repo's env files and the 1Password vault
declared in the secrets: block of .wet.yaml.

The map lives in .wet.yaml, so one file says which keys exist, where their
values come from, and which render target consumes them. Sections are labels
for consumers ("backend", "mobile"), never for storage: two sections naming the
same field share one credential in the vault, and a rotation is one edit.`,
}

var opVaultCmd = &cobra.Command{
	Use:   "vault",
	Short: "Seed the vault from an env file, or write env files from the vault",
}

var (
	opFlags       opTargetFlags
	opSections    []string
	opSeedInput   string
	opSeedForce   bool
	opSeedDryRun  bool
	opReadOutput  string
	opReadDryRun  bool
	opNoRemember  bool
	opReadStdout  bool
	opSeedUnknown bool
)

var opVaultSeedCmd = &cobra.Command{
	Use:   "seed",
	Short: "Write values from an env file into the vault",
	Long: `Read KEY=value lines and store them in the vault fields the secrets:
block maps them to.

A field that already holds the same value is left alone, and one that holds a
different value is reported as a conflict and NOT overwritten — re-running
after a rotation must never silently undo it. --force overwrites conflicts.`,
	Args: cobra.NoArgs,
	RunE: runOPVaultSeed,
}

var opVaultReadCmd = &cobra.Command{
	Use:   "read",
	Short: "Write env files from the vault",
	Long: `Fetch every key in the selected sections and write them as env files.

--output names the file; each section supplies the directory, taken from the
render targets that name it. So one run writes backend/.env.development and
mobile/.env.development without either section knowing a path.`,
	Args: cobra.NoArgs,
	RunE: runOPVaultRead,
}

func init() {
	rootCmd.AddCommand(opCmd)
	opCmd.AddCommand(opVaultCmd)
	opVaultCmd.AddCommand(opVaultSeedCmd, opVaultReadCmd)

	for _, c := range []*cobra.Command{opVaultSeedCmd, opVaultReadCmd} {
		addOPTargetFlags(c, &opFlags)
		c.Flags().StringSliceVar(&opSections, "section", nil,
			"secrets sections to act on (default: all)")
		c.Flags().BoolVar(&opNoRemember, "no-remember", false,
			"don't write a picked account/vault to .wet.local.yaml")
	}
	opVaultSeedCmd.Flags().StringVar(&opSeedInput, "input", "", "env file to read (required)")
	opVaultSeedCmd.Flags().BoolVar(&opSeedForce, "force", false, "overwrite fields whose vault value differs")
	opVaultSeedCmd.Flags().BoolVar(&opSeedDryRun, "dry-run", false, "report what would be written, write nothing")
	opVaultSeedCmd.Flags().BoolVar(&opSeedUnknown, "allow-unmapped", false,
		"ignore keys in the input that the secrets: block does not map")
	_ = opVaultSeedCmd.MarkFlagRequired("input")

	opVaultReadCmd.Flags().StringVar(&opReadOutput, "output", "", "file name to write in each section's directory")
	opVaultReadCmd.Flags().BoolVar(&opReadDryRun, "dry-run", false, "report what would be written, write nothing")
	opVaultReadCmd.Flags().BoolVar(&opReadStdout, "stdout", false, "print one section to stdout instead of writing files")
}

// opContext is everything a subcommand needs after config load, flag
// resolution and any interactive picking: the config, where to talk to, and
// the client bound to the chosen account.
type opContext struct {
	cfg      *WetConfig
	repoRoot string
	target   opTarget
	client   opClient
	sections []string
}

func newOPContext() (*opContext, error) {
	if err := requireOP(); err != nil {
		return nil, err
	}
	repoRoot, err := getRepoRoot()
	if err != nil {
		return nil, err
	}
	cfg, err := LoadWetConfig(repoRoot)
	if err != nil {
		return nil, err
	}
	if cfg.Secrets == nil {
		return nil, errors.New(".wet.yaml has no secrets: block — nothing to read or seed")
	}

	target, picked, err := resolveOPTarget(cfg, opFlags)
	if err != nil {
		if errors.Is(err, errPickerCancelled) {
			return nil, errors.New("cancelled")
		}
		return nil, err
	}
	if !opNoRemember {
		if err := rememberOPTarget(repoRoot, picked); err != nil {
			return nil, err
		}
	}

	// The picked values apply to this run, including to references that did
	// not spell out a vault of their own.
	sec := *cfg.Secrets
	sec.Account, sec.Vault, sec.Item = target.account, target.vault, target.item
	cfg.Secrets = &sec

	sections, err := cfg.Secrets.SectionNames(opSections)
	if err != nil {
		return nil, err
	}
	return &opContext{
		cfg:      cfg,
		repoRoot: repoRoot,
		target:   target,
		client:   opClient{account: target.account},
		sections: sections,
	}, nil
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

	doc := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		if doc == nil {
			doc = map[string]any{}
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}

	secrets, _ := doc["secrets"].(map[string]any)
	if secrets == nil {
		secrets = map[string]any{}
	}
	if picked.account != "" {
		secrets["account"] = picked.account
	}
	if picked.vault != "" {
		secrets["vault"] = picked.vault
	}
	doc["secrets"] = secrets

	out, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Printf("  Remembered account/vault in %s\n", wetLocalConfigFileName)
	return nil
}

// sectionRefs resolves every selected section, and rejects a key that two
// sections point at different fields.
//
// Sharing a key across sections is the normal case — one Google client id read
// by both the API and the app — but sharing the NAME while disagreeing about
// the field is a config bug: whichever section ran last would win, differently
// per command.
func (o *opContext) sectionRefs() ([]SecretRef, error) {
	var all []SecretRef
	byKey := map[string]SecretRef{}
	for _, section := range o.sections {
		refs, err := o.cfg.Secrets.ResolveSection(section)
		if err != nil {
			return nil, err
		}
		for _, ref := range refs {
			if prev, dup := byKey[ref.Key]; dup {
				if prev.URI() != ref.URI() {
					return nil, fmt.Errorf(
						"%s maps to %s in section %q but %s in section %q — one key, one field",
						ref.Key, prev.URI(), prev.Section, ref.URI(), ref.Section)
				}
				continue
			}
			byKey[ref.Key] = ref
			all = append(all, ref)
		}
	}
	return all, nil
}

func runOPVaultSeed(cmd *cobra.Command, args []string) error {
	o, err := newOPContext()
	if err != nil {
		return err
	}
	values, order, err := ParseEnvFile(opSeedInput)
	if err != nil {
		return err
	}
	refs, err := o.sectionRefs()
	if err != nil {
		return err
	}

	byKey := make(map[string]SecretRef, len(refs))
	for _, ref := range refs {
		byKey[ref.Key] = ref
	}

	// An input key with nowhere to go is a mapping the user forgot, not
	// something to drop quietly — the whole point of seeding is that what is
	// in the file ends up in the vault.
	var unmapped []string
	for _, key := range order {
		if _, ok := byKey[key]; !ok {
			unmapped = append(unmapped, key)
		}
	}
	if len(unmapped) > 0 && !opSeedUnknown {
		return fmt.Errorf("%s has %d key(s) the secrets: block does not map: %s\n"+
			"add them to .wet.yaml, or pass --allow-unmapped to ignore them",
			opSeedInput, len(unmapped), strings.Join(unmapped, ", "))
	}
	for _, key := range unmapped {
		fmt.Printf("  ignored   %s (not in secrets:)\n", key)
	}

	// Group by item so each item is fetched once and edited once, whatever
	// mixture of sections and reference forms produced the list.
	type edit struct {
		fields map[string]string
		vault  string
	}
	edits := map[string]*edit{}
	var conflicts, wrote, same, missing []string

	for _, ref := range refs {
		value, present := values[ref.Key]
		if !present {
			missing = append(missing, ref.Key)
			continue
		}
		item, exists, err := o.client.item(ref.Vault, ref.Item)
		if err != nil {
			return err
		}
		if exists {
			if current, has := item.value(ref.Field); has && current != "" {
				if current == value {
					same = append(same, ref.Field)
					continue
				}
				if !opSeedForce {
					conflicts = append(conflicts, ref.Field)
					continue
				}
			}
		}
		key := ref.Vault + "\x00" + ref.Item
		if edits[key] == nil {
			edits[key] = &edit{fields: map[string]string{}, vault: ref.Vault}
		}
		edits[key].fields[ref.Field] = value
		wrote = append(wrote, ref.Field)
	}

	sort.Strings(wrote)
	for _, f := range wrote {
		fmt.Printf("  write     %s\n", f)
	}
	for _, f := range same {
		fmt.Printf("  unchanged %s (already set to this value)\n", f)
	}
	for _, k := range missing {
		fmt.Printf("  absent    %s (mapped, but not in %s)\n", k, opSeedInput)
	}
	for _, f := range conflicts {
		fmt.Printf("  CONFLICT  %s (vault holds a different value)\n", f)
	}

	if opSeedDryRun {
		fmt.Println("\n(dry run — nothing written)")
		return nil
	}
	for key, e := range edits {
		item := strings.SplitN(key, "\x00", 2)[1]
		if _, exists, err := o.client.item(e.vault, item); err != nil {
			return err
		} else if !exists {
			fmt.Printf("  creating item %q in vault %s\n", item, e.vault)
			if _, err := o.client.createItem(e.vault, item); err != nil {
				return err
			}
		}
		if err := o.client.setFields(e.vault, item, e.fields); err != nil {
			return err
		}
	}

	fmt.Printf("\n%d written, %d unchanged, %d conflicting\n", len(wrote), len(same), len(conflicts))
	if len(conflicts) > 0 {
		return fmt.Errorf("%d field(s) already hold a different value; rerun with --force to overwrite",
			len(conflicts))
	}
	return nil
}

func runOPVaultRead(cmd *cobra.Command, args []string) error {
	o, err := newOPContext()
	if err != nil {
		return err
	}
	if opReadStdout && len(o.sections) != 1 {
		return errors.New("--stdout writes one section: name it with --section")
	}
	if opReadOutput == "" && !opReadStdout {
		return errors.New("--output is required (the file name to write in each section's directory)")
	}
	if strings.ContainsRune(opReadOutput, filepath.Separator) && len(o.sections) != 1 {
		return fmt.Errorf("--output %q has a directory in it, which only makes sense for one section: name it with --section",
			opReadOutput)
	}

	// Fetch once per key, not once per section: a key shared by backend and
	// mobile is one credential and one round trip.
	refs, err := o.sectionRefs()
	if err != nil {
		return err
	}
	values := make(map[string]string, len(refs))
	for _, ref := range refs {
		if opReadDryRun {
			values[ref.Key] = ""
			continue
		}
		value, err := o.client.read(ref.URI())
		if err != nil {
			return err
		}
		values[ref.Key] = value
	}

	for _, section := range o.sections {
		sectionRefs, err := o.cfg.Secrets.ResolveSection(section)
		if err != nil {
			return err
		}
		if opReadStdout {
			fmt.Print(FormatEnvFile(sectionRefs, values, ""))
			continue
		}

		paths, err := o.outputPaths(section)
		if err != nil {
			return err
		}
		content := FormatEnvFile(sectionRefs, values,
			fmt.Sprintf("written by `wet op vault read` from section %q — do not commit", section))
		for _, path := range paths {
			if opReadDryRun {
				fmt.Printf("  would write %s (%d keys)\n", path, len(sectionRefs))
				continue
			}
			if err := WriteFile0600(path, []byte(content)); err != nil {
				return err
			}
			fmt.Printf("  wrote %s (%d keys)\n", path, len(sectionRefs))
		}
	}
	return nil
}

// outputPaths turns a section into the files to write: --output resolved
// against every directory a render target consumes that section in.
func (o *opContext) outputPaths(section string) ([]string, error) {
	if strings.ContainsRune(opReadOutput, filepath.Separator) {
		return []string{filepath.Join(o.repoRoot, opReadOutput)}, nil
	}
	dirs := o.cfg.SectionOutputDirs(section)
	if len(dirs) == 0 {
		return nil, fmt.Errorf("section %q is not used by any render: target, so there is no directory to write %s into\n"+
			"add `secrets: %s` to the target that consumes it, or pass --output with a path",
			section, opReadOutput, section)
	}
	paths := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		paths = append(paths, filepath.Join(o.repoRoot, dir, opReadOutput))
	}
	return paths, nil
}
