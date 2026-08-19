package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// offerSecretGroups opens the provider multi-select against a freshly
// scaffolded .wet.yaml and writes the chosen groups into it.
//
// It reuses the same catalog, scaffolder and validation as `wet secrets
// setup`; the only thing that differs is when it runs. Keeping one
// implementation matters more here than usual, because the two entry points
// would otherwise drift into writing different env: blocks for the same group.
//
// Every failure short of a broken repo is non-fatal. The caller has already
// written a working config and is about to tell the user to go and edit it —
// losing the picker is a worse experience, not a broken one, so a declined or
// unavailable prompt leaves a pointer to `wet secrets setup` and returns nil.
func offerSecretGroups(configPath string) error {
	if !stdinIsTerminal() {
		fmt.Println("  Add provider keys (Google, Apple, Stripe, …) with `wet secrets setup`.")
		return nil
	}

	catalog, order, err := LoadSecretGroups()
	if err != nil {
		// A broken embedded catalog is a bug in wet, not in the user's repo,
		// and it must not take the scaffold down with it.
		fmt.Printf("  Could not load the provider catalog (%v).\n", err)
		fmt.Println("  Add provider keys later with `wet secrets setup`.")
		return nil
	}

	doc, err := ReadYAMLDocument(configPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", configPath, err)
	}

	fmt.Println("\n  Which provider keys should this project have?")
	groups, err := pickGroups(catalog, order, EnabledGroups(doc.Content[0]))
	if err != nil {
		if errors.Is(err, errPickerCancelled) {
			fmt.Println("  Skipped — add them later with `wet secrets setup`.")
			return nil
		}
		return err
	}
	if len(groups) == 0 {
		fmt.Println("  None selected — add them later with `wet secrets setup`.")
		return nil
	}

	res, err := ScaffoldGroups(doc, catalog, groups,
		ScaffoldOptions{Output: defaultScaffoldOutput})
	if err != nil {
		return err
	}
	out, err := MarshalYAMLDocument(doc)
	if err != nil {
		return err
	}
	// A scaffold that writes a config wet then rejects would be worse than no
	// scaffold at all — and here it would also strand the starter we just
	// wrote, so refuse to save rather than half-apply.
	if err := validateScaffolded(out, configPath); err != nil {
		return err
	}
	if err := os.WriteFile(configPath, out, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", configPath, err)
	}
	// Same reason as in `wet secrets setup`: a file wet has promised to
	// generate, holding whatever credentials it selects, is born gitignored.
	if res.CreatedOutput {
		if err := ignoreGeneratedOutput(filepath.Dir(configPath), res.OutputPath); err != nil {
			return err
		}
	}

	fmt.Printf("  Added %d env key(s) for: %s\n", len(res.AddedEnv), strings.Join(groups, ", "))
	if len(res.AddedSecrets) > 0 {
		fmt.Printf("  Declared %d credential(s) under secrets.definitions:, each with the dev\n", len(res.AddedSecrets))
		fmt.Println("  fallback it boots on until the store can answer.")
		fmt.Println()
		printSecretDestinationSteps("  ", res.Output)
		fmt.Println()
	}
	if res.CreatedOutput {
		fmt.Printf("  Generating %s from the %q output — move it if that is not\n", res.OutputPath, res.Output)
		fmt.Println("  where your app reads its env file from.")
	}
	return nil
}
