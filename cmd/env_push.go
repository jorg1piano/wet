package cmd

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

var (
	envPushFrom            string
	envPushDryRun          bool
	envPushForce           bool
	envPushCreateItem      bool
	envPushRejectUnmapped  bool
	envPushIncludeDefaults bool
)

var envPushCmd = &cobra.Command{
	Use:   "push <output> --from <dotenv-file>",
	Short: "Write an output's declared secrets from a dotenv file into the store",
	Long: `Read a dotenv file and store the values of the secrets the named output
selects.

The named output is the authorization boundary. wet writes a key only when it
is both declared under secrets.definitions: and listed in that output's
secrets:. Every other key in the file — ordinary configuration, unknown names,
secrets belonging to another output — is ignored and reported. There is no flag
that lets an undeclared key reach the store; a destination mapping cannot be
invented on the command line.

That is why a mixed, generated file is a fine input and needs no permissive
flag:

  wet env push backend --from backend/.env.dev --dry-run
  wet env push backend --from backend/.env.dev

A value equal to that secret's declared default: is refused by default. A file
generated under the fallback policy is full of development placeholders, and
pushing an empty string or a dev-only key over a real credential is the one
mistake this command must never make. --include-defaults overrides it.

An existing destination holding a different non-empty value is reported as a
conflict and left alone; --force overwrites. A missing item is an error unless
--create-item.

--reject-unmapped is the opposite opt-in: it fails when the source contains any
key this output does not select, which is what you want when importing a
dedicated secrets-only file and a typo should be loud. It does not widen what
wet may write.

Names, destinations and statuses are printed. Values never are.`,
	Args: cobra.ExactArgs(1),
	RunE: runEnvPush,
}

func init() {
	envPushCmd.Flags().StringVar(&envPushFrom, "from", "", "dotenv file to read (required)")
	envPushCmd.Flags().BoolVar(&envPushDryRun, "dry-run", false, "report what would be written, write nothing")
	envPushCmd.Flags().BoolVar(&envPushForce, "force", false, "overwrite a destination holding a different value")
	envPushCmd.Flags().BoolVar(&envPushCreateItem, "create-item", false, "create the destination item if it does not exist")
	envPushCmd.Flags().BoolVar(&envPushRejectUnmapped, "reject-unmapped", false,
		"fail if the source holds any key this output does not select")
	envPushCmd.Flags().BoolVar(&envPushIncludeDefaults, "include-defaults", false,
		"also push values equal to a secret's declared default")
	_ = envPushCmd.MarkFlagRequired("from")
}

// pushStatus is what happened to one selected secret, for the report.
type pushStatus struct {
	name   string
	ref    SecretRef
	status string
}

func runEnvPush(cmd *cobra.Command, args []string) error {
	cfg, _, err := loadConfigForTree()
	if err != nil {
		return err
	}
	selected, err := cfg.SelectOutputs(args[:1])
	if err != nil {
		return err
	}
	output := selected[0]
	if len(output.Secrets) == 0 {
		return fmt.Errorf("output %q selects no secrets, so there is nothing to push", output.Name)
	}
	if cfg.Secrets == nil {
		return errors.New(".wet.yaml has no secrets: block — nothing to push")
	}
	if cfg.Secrets.Store != secretsStoreOP {
		return fmt.Errorf("secrets.store %q is not supported (known: %s)", cfg.Secrets.Store, secretsStoreOP)
	}
	if err := requireOP(); err != nil {
		return err
	}

	values, order, err := ParseEnvFile(envPushFrom)
	if err != nil {
		return err
	}

	// The output's selection, not the whole definitions block: a secret
	// another output owns is as unwritable here as an undeclared key.
	selectable := make(map[string]bool, len(output.Secrets))
	for _, name := range output.Secrets {
		selectable[name] = true
	}
	var unmapped []string
	for _, key := range order {
		if !selectable[key] {
			unmapped = append(unmapped, key)
		}
	}
	if envPushRejectUnmapped && len(unmapped) > 0 {
		return fmt.Errorf("%s holds %d key(s) the %s output does not select: %s\n"+
			"drop them from the file, or omit --reject-unmapped to have them ignored",
			envPushFrom, len(unmapped), output.Name, strings.Join(unmapped, ", "))
	}

	client := opClient{account: cfg.Secrets.Account}
	var report []pushStatus
	// edits groups by item so each destination item is written once, whatever
	// mixture of per-secret vault/item overrides produced the list.
	type edit struct {
		vault  string
		item   string
		fields map[string]string
	}
	edits := map[string]*edit{}
	// Fetch each destination item once. Several definitions normally share an
	// item, and re-reading it once per field is both slow and capable of showing
	// an inconsistent snapshot if the item changes during validation.
	type itemState struct {
		item   opItem
		exists bool
	}
	items := map[string]itemState{}
	conflicts := 0

	for _, name := range output.Secrets {
		def := cfg.Secrets.Definitions[name]
		ref, err := cfg.Secrets.ResolveSecretRef(name)
		if err != nil {
			return err
		}
		value, present := values[name]
		if !present {
			report = append(report, pushStatus{name, ref, "absent from " + envPushFrom})
			continue
		}
		if def.HasDefault() && value == def.DefaultValue() && !envPushIncludeDefaults {
			report = append(report, pushStatus{name, ref, "fallback value — not pushed"})
			continue
		}

		itemKey := ref.Vault + "\x00" + ref.Item
		state, fetched := items[itemKey]
		if !fetched {
			item, exists, err := client.item(ref.Vault, ref.Item)
			if err != nil {
				return err
			}
			state = itemState{item: item, exists: exists}
			items[itemKey] = state
		}
		if !state.exists && !envPushCreateItem {
			return fmt.Errorf("item %q does not exist in vault %s (needed by %s)\n"+
				"pass --create-item to create it", ref.Item, ref.Vault, name)
		}
		if state.exists {
			if current, has := state.item.value(ref.Field); has && current != "" {
				if current == value {
					report = append(report, pushStatus{name, ref, "unchanged"})
					continue
				}
				if !envPushForce {
					report = append(report, pushStatus{name, ref, "CONFLICT — store holds a different value"})
					conflicts++
					continue
				}
			}
		}
		key := itemKey
		if edits[key] == nil {
			edits[key] = &edit{vault: ref.Vault, item: ref.Item, fields: map[string]string{}}
		}
		// Two definitions may legitimately point at one field — one credential
		// under two names. Two DIFFERENT values for it is a config bug, and
		// letting the map's last write win would push whichever name sorted
		// later, differently between runs.
		if prev, dup := edits[key].fields[ref.Field]; dup && prev != value {
			return fmt.Errorf("%s and another selected secret both map to %s/%s/%s but the file gives them "+
				"different values — one field holds one credential", name, ref.Vault, ref.Item, ref.Field)
		}
		edits[key].fields[ref.Field] = value
		what := "would write"
		if !envPushDryRun {
			what = "write"
		}
		if !state.exists {
			what += " (creating item)"
		}
		report = append(report, pushStatus{name, ref, what})
	}

	for _, r := range report {
		fmt.Printf("  %s -> %s/%s/%s: %s\n", r.name, r.ref.Vault, r.ref.Item, r.ref.Field, r.status)
	}
	sort.Strings(unmapped)
	for _, key := range unmapped {
		fmt.Printf("  %s: ignored (not a secret this output selects)\n", key)
	}

	if envPushDryRun {
		fmt.Println("\n(dry run — nothing written)")
		// A dry run exists to preview conflicts, so reporting them and then
		// exiting 0 is the worst possible answer: `--dry-run && push` reads
		// as a safety gate and walks straight into the failure.
		if conflicts > 0 {
			return fmt.Errorf("%d field(s) would conflict; rerun with --force to overwrite them", conflicts)
		}
		return nil
	}
	// Conflict detection is part of validation. Do not apply the unrelated
	// non-conflicting edits and then return an error: a failed push must leave
	// the store unchanged unless --force explicitly authorizes overwrites.
	if conflicts > 0 {
		return fmt.Errorf("%d field(s) already hold a different value; rerun with --force to overwrite", conflicts)
	}
	// Every conflict and every missing item was detected above, so the writes
	// below are the only step that can partially apply.
	for _, key := range sortedKeys(edits) {
		e := edits[key]
		if _, exists, err := client.item(e.vault, e.item); err != nil {
			return err
		} else if !exists {
			fmt.Printf("  creating item %q in vault %s\n", e.item, e.vault)
			if _, err := client.createItem(e.vault, e.item); err != nil {
				return err
			}
		}
		if err := client.setFields(e.vault, e.item, e.fields); err != nil {
			return err
		}
	}
	return nil
}
