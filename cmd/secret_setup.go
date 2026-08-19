package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var secretsCmd = &cobra.Command{
	Use:   "secrets",
	Short: "Scaffold the env:, secrets:, and outputs: blocks of .wet.yaml",
	Long: `Add a provider's keys to .wet.yaml: its derived env: values, the
secrets.definitions: entries for the credentials that come from the store,
and the outputs: entry that consumes them.

The catalog of providers is built into wet — nothing is installed into your
repo — so a group gains keys when you upgrade wet, not when you remember to
copy a file.`,
}

var secretsSetupCmd = &cobra.Command{
	Use:   "setup [group...]",
	Short: "Pick provider groups and write them into .wet.yaml",
	Long: `Scaffold one or more provider groups into .wet.yaml.

With no arguments it opens a multi-select of the catalog, pre-checked with
whatever is already scaffolded.

A credential becomes a secrets.definitions: entry naming its store field, with
the catalog's dev fallback in "default:" — under the default "fallback" policy
that is what lets a clone with no vault still render and boot, while the real
value from the store wins whenever there is one. A derived value like a
redirect URI is computed per worktree, so it stays an ordinary env: entry and
never reaches the store.

Both halves are wired into the --output entry of outputs:, since a credential
no generated file selects is one wet will never write.

Existing keys are never touched, so it is safe to re-run when you add a
provider — a value you have filled in stays filled in.`,
	RunE: runSecretsSetup,
}

var secretsGroupsCmd = &cobra.Command{
	Use:   "groups",
	Short: "List the provider groups wet knows",
	Args:  cobra.NoArgs,
	RunE:  runSecretsGroups,
}

// defaultScaffoldOutput is the outputs: entry scaffolded keys are wired into
// unless --output says otherwise. Named once so `wet secrets setup` and the
// scaffold offer made by `wet setup` cannot disagree about which file they
// added keys to — two names would leave half the provider in an output nobody
// generates.
const defaultScaffoldOutput = "backend"

var (
	secretsSetupOutput string
	secretsSetupDryRun bool
)

func init() {
	rootCmd.AddCommand(secretsCmd)
	secretsCmd.AddCommand(secretsSetupCmd, secretsGroupsCmd)
	secretsSetupCmd.Flags().StringVar(&secretsSetupOutput, "output", defaultScaffoldOutput,
		"outputs: entry the scaffolded keys are added to (created if it does not exist)")
	secretsSetupCmd.Flags().BoolVar(&secretsSetupDryRun, "dry-run", false,
		"print the resulting .wet.yaml instead of writing it")
}

func runSecretsGroups(cmd *cobra.Command, args []string) error {
	catalog, order, err := LoadSecretGroups()
	if err != nil {
		return err
	}
	for _, name := range order {
		g := catalog[name]
		fmt.Printf("%-10s %s\n", name, g.Description)
		for _, key := range g.Order {
			if field := g.Env[key].Secret; field != "" {
				fmt.Printf("    %-30s vault: %s\n", key, field)
			} else {
				fmt.Printf("    %-30s %s\n", key, describeDefault(g.Env[key].Default))
			}
		}
	}
	return nil
}

func describeDefault(def string) string {
	if strings.Contains(def, "{{") {
		return "derived: " + def
	}
	if def == "" {
		return "(empty)"
	}
	return "default: " + def
}

func runSecretsSetup(cmd *cobra.Command, args []string) error {
	catalog, order, err := LoadSecretGroups()
	if err != nil {
		return err
	}
	repoRoot, err := getRepoRoot()
	if err != nil {
		return err
	}
	path := filepath.Join(repoRoot, wetConfigFileName)
	doc, err := ReadYAMLDocument(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no %s here — run `wet install skill` and write one first", wetConfigFileName)
		}
		return err
	}
	if len(doc.Content) == 0 {
		return fmt.Errorf("%s is empty", path)
	}
	already := EnabledGroups(doc.Content[0])

	groups := args
	if len(groups) == 0 {
		groups, err = pickGroups(catalog, order, already)
		if err != nil {
			if errors.Is(err, errPickerCancelled) {
				fmt.Println("Cancelled — nothing written.")
				return nil
			}
			return err
		}
	}
	for _, name := range groups {
		if _, ok := catalog[name]; !ok {
			return fmt.Errorf("unknown group %q (known: %s)", name, strings.Join(order, ", "))
		}
	}
	if len(groups) == 0 {
		fmt.Println("No groups selected — nothing written.")
		return nil
	}

	res, err := ScaffoldGroups(doc, catalog, groups, ScaffoldOptions{Output: secretsSetupOutput})
	if err != nil {
		return err
	}
	out, err := MarshalYAMLDocument(doc)
	if err != nil {
		return err
	}

	// The result has to load — a scaffold that writes a config wet then
	// rejects would be worse than no scaffold at all.
	if err := validateScaffolded(out, path); err != nil {
		return err
	}

	if secretsSetupDryRun {
		fmt.Print(string(out))
		return nil
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	// An invented output is a file wet has just promised to generate, holding
	// whatever credentials it selects. It has to be born gitignored: telling
	// the user to do it is the version of this guard that gets skipped.
	if res.CreatedOutput {
		if err := ignoreGeneratedOutput(filepath.Dir(path), res.OutputPath); err != nil {
			return err
		}
	}

	printScaffoldSummary(res, groups)
	return nil
}

// printScaffoldSummary reports what landed where. It names the created output
// explicitly: an invented file is the one change the user did not ask for, and
// finding out later that wet generates backend/.env.dev is worse than being
// told now.
func printScaffoldSummary(res *ScaffoldResult, groups []string) {
	fmt.Printf("Scaffolded %s into %s\n", strings.Join(groups, ", "), wetConfigFileName)
	if len(res.AddedEnv) > 0 {
		fmt.Printf("  env:     +%d key(s) (%s)\n", len(res.AddedEnv), strings.Join(res.AddedEnv, ", "))
	}
	if len(res.AddedSecrets) > 0 {
		fmt.Printf("  secrets: +%d definition(s) (%s)\n", len(res.AddedSecrets), strings.Join(res.AddedSecrets, ", "))
	}
	if len(res.KeptEnv) > 0 {
		fmt.Printf("  kept:    %d existing key(s) untouched (%s)\n", len(res.KeptEnv), strings.Join(res.KeptEnv, ", "))
	}
	if res.CreatedOutput {
		fmt.Printf("  outputs: created %q, writing %s/.env.dev — change its out: if that is not where it belongs\n",
			res.Output, res.Output)
	} else {
		fmt.Printf("  outputs: wired into %q\n", res.Output)
	}
	if len(res.AddedSecrets) > 0 {
		fmt.Printf("\nNext: `wet op vault` to point this machine at the right vault, then\n")
		fmt.Printf("`wet env pull %s` to write the file, or `wet env push %s --from <dotenv>`\n", res.Output, res.Output)
		fmt.Println("to store real values you already have.")
	}
}

// validateScaffolded runs the freshly written bytes through the real loader
// AND resolves the env: block against a real worktree's data.
//
// Loading is not enough. Validate() only PARSES an env: template, so a
// catalog value like `{{.HYDRA_PUBLIC_URL}}/callback/google` scaffolded onto a
// config that never declared HYDRA_PUBLIC_URL passes cleanly, gets written —
// and then every `wet setup` and `wet env pull` in that repo fails on an
// unresolvable key. The scaffolder would have bricked the config it was asked
// to improve, so the resolution has to happen before the file is replaced.
func validateScaffolded(out []byte, path string) error {
	dir, err := os.MkdirTemp("", "wet-scaffold")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, wetConfigFileName), out, 0o600); err != nil {
		return err
	}
	cfg, err := LoadWetConfigWithLocal(dir, "")
	if err != nil {
		return fmt.Errorf("the scaffolded config does not load (%s left unchanged): %w", path, err)
	}
	// Slot 0 with no .env.local: the values a main checkout would resolve.
	pools := make(map[string]string, len(cfg.Pools))
	for name, items := range cfg.Pools {
		if len(items) > 0 {
			pools[name] = items[0]
		}
	}
	data := BuildTemplateData(cfg, cfg.MainBranchName(), 0, cfg.ResolvePorts(0), pools)
	if _, err := ResolveEnvValues(cfg, data, nil); err != nil {
		return fmt.Errorf("the scaffolded config would not resolve (%s left unchanged): %w\n"+
			"  A group's derived values reference keys your config does not declare yet.\n"+
			"  Add them under env:, or scaffold the group that provides them first", path, err)
	}
	return nil
}

// --- multi-select ---

type groupItem struct {
	name     string
	desc     string
	keys     int
	selected bool
	existing bool
}

func (g groupItem) Title() string {
	mark := "[ ]"
	if g.selected {
		mark = "[x]"
	}
	if g.existing {
		return fmt.Sprintf("%s %s (already scaffolded)", mark, g.name)
	}
	return fmt.Sprintf("%s %s", mark, g.name)
}
func (g groupItem) Description() string { return fmt.Sprintf("%s — %d keys", g.desc, g.keys) }
func (g groupItem) FilterValue() string { return g.name + " " + g.desc }

type multiSelectModel struct {
	list      list.Model
	cancelled bool
	done      bool
}

func (m multiSelectModel) Init() tea.Cmd { return nil }

func (m multiSelectModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.list.SetSize(msg.Width, msg.Height-3)
		return m, nil
	case tea.KeyMsg:
		if m.list.FilterState() == list.Filtering {
			break
		}
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			m.cancelled = true
			return m, tea.Quit
		case " ", "x":
			idx := m.list.Index()
			if it, ok := m.list.SelectedItem().(groupItem); ok {
				it.selected = !it.selected
				m.list.SetItem(idx, it)
			}
			return m, nil
		case "enter":
			m.done = true
			return m, tea.Quit
		}
	}
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m multiSelectModel) View() string {
	if m.cancelled || m.done {
		return ""
	}
	help := lipgloss.NewStyle().Faint(true).
		Render("  space toggles · enter confirms · q cancels")
	return m.list.View() + "\n" + help
}

// pickGroups runs the multi-select, pre-checking what is already scaffolded.
//
// Pre-checking matters: the list is the file's state, not a blank form, so
// confirming without touching anything is a no-op rather than a way to
// accidentally drop a group.
func pickGroups(catalog map[string]SecretGroup, order, already []string) ([]string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return nil, errors.New("no terminal to ask in: name the groups as arguments (see `wet secrets groups`)")
	}
	enabled := map[string]bool{}
	for _, g := range already {
		enabled[g] = true
	}
	items := make([]list.Item, 0, len(order))
	for _, name := range order {
		g := catalog[name]
		items = append(items, groupItem{
			name: name, desc: g.Description, keys: len(g.Env),
			selected: enabled[name], existing: enabled[name],
		})
	}
	l := list.New(items, list.NewDefaultDelegate(), 0, 0)
	l.Title = "Which providers does this project use?"
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(true)
	l.Styles.Title = lipgloss.NewStyle().Bold(true).Padding(0, 1)

	final, err := tea.NewProgram(multiSelectModel{list: l}, tea.WithAltScreen()).Run()
	if err != nil {
		return nil, fmt.Errorf("picker: %w", err)
	}
	m := final.(multiSelectModel)
	if m.cancelled {
		return nil, errPickerCancelled
	}
	var chosen []string
	for _, it := range m.list.Items() {
		g := it.(groupItem)
		// An already-scaffolded group is re-listed only if still checked;
		// unchecking it does not remove anything, since deleting keys the app
		// may already read is not a thing a scaffold should do silently.
		if g.selected {
			chosen = append(chosen, g.name)
		}
	}
	sort.Strings(chosen)
	return chosen, nil
}
