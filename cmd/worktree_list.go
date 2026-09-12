package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
)

var worktreeListCmd = &cobra.Command{
	Use:     "list",
	Args:    cobra.NoArgs,
	Short:   "List and manage worktrees",
	Long:    "Interactive list of configured checkouts. Open one in tmux or VS Code, or remove a worktree with its pre_remove hooks. Removal keeps the branch.",
	Aliases: []string{"ls"},
	RunE:    runWorktreeList,
}

func init() {
	worktreeCmd.AddCommand(worktreeListCmd)
}

// --- List item ---

type wtItem struct {
	branch string
	path   string
	age    string
}

func (w wtItem) Title() string       { return w.branch }
func (w wtItem) Description() string { return fmt.Sprintf("%s  (%s)", w.path, w.age) }
func (w wtItem) FilterValue() string { return w.branch }

// --- Actions ---

type action int

const (
	actionNone action = iota
	actionTmux
	actionCode
	actionRemove
)

// --- Key bindings ---

type listKeyMap struct {
	openTmux key.Binding
	openCode key.Binding
	remove   key.Binding
}

var listKeys = listKeyMap{
	openTmux: key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("enter", "open in tmux"),
	),
	openCode: key.NewBinding(
		key.WithKeys("c"),
		key.WithHelp("c", "open in VS Code"),
	),
	remove: key.NewBinding(
		key.WithKeys("x"),
		key.WithHelp("x", "remove worktree"),
	),
}

// --- Model ---

type listModel struct {
	list     list.Model
	action   action
	selected *wtItem
	quitting bool
	width    int
	height   int
}

var (
	listTitleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205")).MarginLeft(2)
	helpStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("241")).MarginLeft(4).MarginTop(1)
)

func (m listModel) Init() tea.Cmd {
	return nil
}

func (m listModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		// Don't intercept keys when filtering
		if m.list.FilterState() == list.Filtering {
			break
		}
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+c", "q"))):
			m.quitting = true
			return m, tea.Quit
		case key.Matches(msg, listKeys.openTmux):
			if i, ok := m.list.SelectedItem().(wtItem); ok {
				m.selected = &i
				m.action = actionTmux
			}
			return m, tea.Quit
		case key.Matches(msg, listKeys.openCode):
			if i, ok := m.list.SelectedItem().(wtItem); ok {
				m.selected = &i
				m.action = actionCode
			}
			return m, tea.Quit
		case key.Matches(msg, listKeys.remove):
			if i, ok := m.list.SelectedItem().(wtItem); ok {
				m.selected = &i
				m.action = actionRemove
			}
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		h, v := lipgloss.NewStyle().Margin(1, 2).GetFrameSize()
		m.list.SetSize(msg.Width-h, msg.Height-v-2) // extra room for help
	}

	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m listModel) View() string {
	if m.quitting {
		return ""
	}
	help := helpStyle.Render("enter: tmux  c: vscode  x: remove  q: quit")
	return lipgloss.NewStyle().Margin(1, 2).Render(m.list.View()) + "\n" + help
}

// --- Command runner ---

func runWorktreeList(cmd *cobra.Command, args []string) error {
	config, err := loadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	if len(config.Worktrees) == 0 {
		fmt.Println("  No worktrees found.")
		return nil
	}

	items := make([]list.Item, len(config.Worktrees))
	for i, wt := range config.Worktrees {
		items[i] = wtItem{
			branch: wt.Branch,
			path:   wt.Path,
			age:    timeAgo(wt.CreatedAt),
		}
	}

	delegate := list.NewDefaultDelegate()
	l := list.New(items, delegate, 0, 0)
	l.Title = "Worktrees"
	l.SetShowStatusBar(false)

	m := listModel{list: l}

	p := tea.NewProgram(m, tea.WithAltScreen())
	finalModel, err := p.Run()
	if err != nil {
		return err
	}

	fm := finalModel.(listModel)
	if fm.selected == nil {
		return nil
	}

	switch fm.action {
	case actionTmux:
		return openInTmux(fm.selected.branch, fm.selected.path)
	case actionCode:
		return openInCode(fm.selected.path)
	case actionRemove:
		return removeWorktreeByPath(fm.selected.branch, fm.selected.path)
	}

	return nil
}

func openInTmux(branch, path string) error {
	if os.Getenv("TMUX") == "" {
		return fmt.Errorf("not inside a tmux session")
	}
	tmuxCmd := exec.Command("tmux", "new-window", "-n", branch, "-c", path)
	if err := tmuxCmd.Run(); err != nil {
		return fmt.Errorf("failed to open tmux window: %w", err)
	}
	fmt.Printf("  Opened '%s' in new tmux window\n", branch)
	return nil
}

func openInCode(path string) error {
	codeCmd := exec.Command("code", path)
	if err := codeCmd.Start(); err != nil {
		return fmt.Errorf("failed to open VS Code: %w", err)
	}
	fmt.Printf("  Opened '%s' in VS Code\n", path)
	return nil
}

func removeWorktreeByPath(_ string, path string) error {
	return removeWorktree(path)
}

func timeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
