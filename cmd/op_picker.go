package cmd

import (
	"errors"
	"fmt"
	"os"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// errPickerCancelled distinguishes "the user pressed q" from a real failure,
// so a cancelled picker exits quietly instead of printing a stack of context.
var errPickerCancelled = errors.New("cancelled")

// pickerItem is one choice: a value plus the human label shown for it.
type pickerItem struct {
	value string
	desc  string
}

func (p pickerItem) Title() string       { return p.value }
func (p pickerItem) Description() string { return p.desc }
func (p pickerItem) FilterValue() string { return p.value + " " + p.desc }

type pickerModel struct {
	list      list.Model
	chosen    string
	cancelled bool
}

func (m pickerModel) Init() tea.Cmd { return nil }

func (m pickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.list.SetSize(msg.Width, msg.Height-2)
		return m, nil
	case tea.KeyMsg:
		// While filtering, these keys are text, not commands.
		if m.list.FilterState() == list.Filtering {
			break
		}
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			m.cancelled = true
			return m, tea.Quit
		case "enter":
			if it, ok := m.list.SelectedItem().(pickerItem); ok {
				m.chosen = it.value
				return m, tea.Quit
			}
		}
	}
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m pickerModel) View() string {
	if m.cancelled || m.chosen != "" {
		return ""
	}
	return m.list.View()
}

// pickOne runs a single-select TUI and returns the chosen value.
//
// It refuses rather than hangs when there is nobody to ask: a hook, a CI step
// or a piped invocation has no terminal, and a picker waiting on a closed
// stdin would look like wet hanging. The error names the flag that would have
// made the run non-interactive.
func pickOne(title, flag string, items []pickerItem) (string, error) {
	if len(items) == 0 {
		return "", fmt.Errorf("nothing to choose from for %s", title)
	}
	if len(items) == 1 {
		return items[0].value, nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return "", fmt.Errorf("%s is not set and there is no terminal to ask: pass %s", title, flag)
	}

	listItems := make([]list.Item, len(items))
	for i, it := range items {
		listItems[i] = it
	}
	l := list.New(listItems, list.NewDefaultDelegate(), 0, 0)
	l.Title = title
	l.SetShowStatusBar(false)
	l.Styles.Title = lipgloss.NewStyle().Bold(true).Padding(0, 1)

	final, err := tea.NewProgram(pickerModel{list: l}, tea.WithAltScreen()).Run()
	if err != nil {
		return "", fmt.Errorf("picker: %w", err)
	}
	m := final.(pickerModel)
	if m.cancelled || m.chosen == "" {
		return "", errPickerCancelled
	}
	return m.chosen, nil
}

// resolveOPTarget fills in account, vault and item — from flags, then the
// config, then by asking — and reports which of them the user picked so the
// caller can offer to remember them.
//
// Asking beats defaulting here: a wrong vault is not a failed run, it is
// secrets written to the wrong place.
func resolveOPTarget(cfg *WetConfig, flags opTargetFlags) (resolved opTarget, picked opTarget, err error) {
	sec := cfg.Secrets
	resolved.account = firstNonEmpty(flags.account, sec.Account)
	resolved.vault = firstNonEmpty(flags.vault, sec.Vault)
	resolved.item = firstNonEmpty(flags.item, sec.Item)

	client := opClient{account: resolved.account}

	if resolved.account == "" {
		accounts, err := client.accounts()
		if err != nil {
			return resolved, picked, err
		}
		items := make([]pickerItem, 0, len(accounts))
		for _, a := range accounts {
			items = append(items, pickerItem{value: a.URL, desc: a.label()})
		}
		// One account is not a choice — signing in once and being asked about
		// it forever is friction with no decision behind it.
		if len(accounts) > 1 {
			choice, err := pickOne("1Password account", "--account", items)
			if err != nil {
				return resolved, picked, err
			}
			resolved.account = choice
			picked.account = choice
			client = opClient{account: choice}
		}
	}

	if resolved.vault == "" {
		vaults, err := client.vaults()
		if err != nil {
			return resolved, picked, err
		}
		items := make([]pickerItem, 0, len(vaults))
		for _, v := range vaults {
			items = append(items, pickerItem{value: v.Name, desc: v.ID})
		}
		choice, err := pickOne("Vault", "--vault", items)
		if err != nil {
			return resolved, picked, err
		}
		resolved.vault = choice
		picked.vault = choice
	}

	if resolved.item == "" {
		return resolved, picked, errors.New("no item: set secrets.item in .wet.yaml or pass --item")
	}
	return resolved, picked, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// opTarget is where a run reads and writes: one account, vault and item.
type opTarget struct {
	account string
	vault   string
	item    string
}

// opTargetFlags are the per-invocation overrides, shared by the subcommands.
type opTargetFlags struct {
	account string
	vault   string
	item    string
}

func addOPTargetFlags(cmd *cobra.Command, f *opTargetFlags) {
	cmd.Flags().StringVar(&f.account, "account", "", "1Password account (asked for when several are signed in)")
	cmd.Flags().StringVar(&f.vault, "vault", "", "vault name (asked for when unset)")
	cmd.Flags().StringVar(&f.item, "item", "", "item title holding the fields")
}
