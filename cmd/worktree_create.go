package cmd

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
)

var worktreeCreateCmd = &cobra.Command{
	Use:   "create [branch-name]",
	Short: "Create a new git worktree (minimal)",
	Long: `Create a new git worktree at ../worktree-<branch>, copy .env from the
main repo root if present, and open a new tmux window when inside tmux.

This command is intentionally minimal: it performs no .wet.yaml loading,
port allocation, template rendering, or hook execution. Run
` + "`wet worktree setup`" + ` from inside the new worktree to perform the full
configuration step.

Pass ` + "`--no-tmux`" + ` to skip opening a tmux window — useful when a
higher-level orchestrator (e.g. the Claude ` + "`/team`" + ` flow) manages
sessions itself and doesn't want a stray window spawned.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runWorktreeCreate,
}

var worktreeCreateNoTmux bool

func init() {
	worktreeCreateCmd.Flags().BoolVar(&worktreeCreateNoTmux, "no-tmux", false, "don't open a new tmux window after creating the worktree")
	worktreeCmd.AddCommand(worktreeCreateCmd)
}

// --- Bubble Tea model for branch name input ---

type createModel struct {
	textInput textinput.Model
	done      bool
	cancelled bool
}

func newCreateModel() createModel {
	ti := textinput.New()
	ti.Placeholder = "feature/my-branch"
	ti.Focus()
	ti.CharLimit = 100
	ti.Width = 50
	return createModel{textInput: ti}
}

func (m createModel) Init() tea.Cmd {
	return textinput.Blink
}

func (m createModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			m.cancelled = true
			return m, tea.Quit
		case "enter":
			if strings.TrimSpace(m.textInput.Value()) != "" {
				m.done = true
				return m, tea.Quit
			}
		}
	}
	var cmd tea.Cmd
	m.textInput, cmd = m.textInput.Update(msg)
	return m, cmd
}

var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
	promptStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
)

func (m createModel) View() string {
	if m.cancelled {
		return "\n  Cancelled.\n\n"
	}
	if m.done {
		return ""
	}
	return fmt.Sprintf(
		"\n  %s\n\n  %s\n\n  %s\n\n",
		titleStyle.Render("Create Worktree"),
		promptStyle.Render("Branch name:"),
		m.textInput.View(),
	)
}

// --- Core logic ---

func runWorktreeCreate(cmd *cobra.Command, args []string) error {
	var branchName string

	if len(args) > 0 {
		branchName = args[0]
	} else {
		// Interactive prompt
		m := newCreateModel()
		p := tea.NewProgram(m)
		finalModel, err := p.Run()
		if err != nil {
			return fmt.Errorf("TUI error: %w", err)
		}
		fm := finalModel.(createModel)
		if fm.cancelled {
			return nil
		}
		branchName = strings.TrimSpace(fm.textInput.Value())
	}

	if branchName == "" {
		return fmt.Errorf("branch name cannot be empty")
	}

	return createWorktree(branchName, worktreeCreateNoTmux)
}

// createWorktree is the minimal worktree-create path. It:
//  1. Resolves the main repo root (works from inside a worktree too).
//  2. Runs `git worktree add -b <branch> ../worktree-<branch>`.
//  3. Copies `.env` from the main repo root if present (single-file copy,
//     no config lookup, no templating).
//  4. Opens a tmux window named <branch> when inside tmux (unless noTmux).
//
// No .wet.yaml load, no port allocation, no state file write, no hooks. Run
// `wet worktree setup` from inside the new worktree to configure the rest.
func createWorktree(branchName string, noTmux bool) error {
	repoRoot, err := getMainRepoRoot()
	if err != nil {
		return err
	}

	worktreePath := worktreePathFor(repoRoot, branchName)

	// Branch off the configured trunk, not off whatever the main checkout
	// happens to have checked out. A repo whose default branch is stale (or
	// simply parked on another feature branch) otherwise hands every new
	// worktree that state, and the first sign of it is a PR full of someone
	// else's commits.
	base := baseRefForNewBranch(mainBranchName())
	fmt.Printf("  Branching %s off %s\n", branchName, base)

	// --no-track: branching off origin/<main> would otherwise set the new
	// branch's upstream to the trunk, so a plain `git push` from the worktree
	// pushes the feature work straight onto it. `wet pr create` sets the
	// upstream to the branch's own remote ref when it pushes.
	gitCmd := exec.Command("git", "worktree", "add", "--no-track", "-b", branchName, worktreePath, base)
	gitCmd.Stdout = os.Stdout
	gitCmd.Stderr = os.Stderr
	if err := gitCmd.Run(); err != nil {
		return fmt.Errorf("failed to create worktree: %w", err)
	}

	return postWorktreeAdd(repoRoot, worktreePath, branchName, "Created", noTmux)
}

// baseRefForNewBranch picks the ref a new worktree's branch starts from: the
// most up-to-date copy of the trunk this machine actually has.
//
// Neither side of local-vs-remote is right on its own, and picking wrong is
// expensive in both directions. A repo whose main checkout is rarely the one
// anyone works in leaves the local trunk months behind, and branching off it
// hands every worktree that stale state. But committing to origin/<trunk>
// instead means work that is committed and not yet pushed — a fix to
// .wet.yaml itself, say — is invisible to the very next worktree, which then
// runs the old config and fails in ways the fix already addressed.
//
// So: use the local branch when it already contains everything origin has
// (ahead, or equal), and origin otherwise (behind, or diverged). Whichever
// wins, its name is printed, because "which trunk did this fork from" is the
// first thing you want to know when a fresh worktree misbehaves.
func baseRefForNewBranch(mainBranch string) string {
	hasRef := func(ref string) bool {
		return exec.Command("git", "rev-parse", "--verify", "--quiet", ref).Run() == nil
	}
	local, remote := "refs/heads/"+mainBranch, "refs/remotes/origin/"+mainBranch
	switch {
	case hasRef(local) && hasRef(remote):
		// Is every commit on origin also on local? Then local is the superset.
		if exec.Command("git", "merge-base", "--is-ancestor", remote, local).Run() == nil {
			return mainBranch
		}
		return "origin/" + mainBranch
	case hasRef(remote):
		return "origin/" + mainBranch
	case hasRef(local):
		return mainBranch
	}
	return "HEAD"
}

// worktreePathFor returns the conventional worktree location:
// `<repoRoot>/../worktree-<branch>`. Shared by `create` and `mount`.
func worktreePathFor(repoRoot, branchName string) string {
	return filepath.Join(repoRoot, "..", fmt.Sprintf("worktree-%s", branchName))
}

// postWorktreeAdd runs the shared steps after `git worktree add` succeeds,
// for both `create` and `mount`: copy `.env` from the main repo root (if
// present), open a tmux window when inside tmux (unless noTmux), and print
// the next-step hint. `label` is the past-tense verb used in the success
// line ("Created" or "Mounted"). `noTmux` suppresses the tmux window — used
// by orchestrators (e.g. the Claude `/team` flow) that manage sessions
// themselves.
func postWorktreeAdd(repoRoot, worktreePath, branchName, label string, noTmux bool) error {
	srcEnv := filepath.Join(repoRoot, ".env")
	if _, statErr := os.Stat(srcEnv); statErr == nil {
		dstEnv := filepath.Join(worktreePath, ".env")
		if err := copyFile(srcEnv, dstEnv); err != nil {
			return fmt.Errorf("copy .env: %w", err)
		}
		fmt.Printf("  Copied: .env\n")
	}

	fmt.Printf("\n  %s worktree '%s' at %s\n", label, branchName, worktreePath)

	if !noTmux && os.Getenv("TMUX") != "" {
		tmuxCmd := exec.Command("tmux", "new-window", "-n", branchName, "-c", worktreePath)
		if err := tmuxCmd.Run(); err != nil {
			fmt.Printf("  Warning: could not open tmux window: %v\n", err)
		} else {
			fmt.Printf("  Opened in new tmux window '%s'\n", branchName)
		}
	}

	fmt.Printf("\n  Next: cd %s && wet worktree setup\n", worktreePath)
	return nil
}

// printSetupHookFailure writes a multi-line recovery block to stderr when a
// post_create hook returns a nonzero exit during `wet worktree setup`. The
// worktree already exists on disk (it was created by `wet worktree create`
// or `git worktree add` earlier) and the state file was partially updated
// depending on how far setup got, so the user's recovery path is either to
// fix the cause and re-run setup, or to finish setup by hand.
func printSetupHookFailure(branch, path string, slot int, hook, rendered string, hookErr error, isMain ...bool) {
	what, retry := "worktree", "from inside the worktree"
	if len(isMain) > 0 && isMain[0] {
		what, retry = "main checkout", "from the repo root"
	}
	fmt.Fprintf(os.Stderr, `
Hook failed while setting up %s '%s'.

  Path:      %s
  Branch:    %s
  Slot:      %d
  Hook:      %s
  Command:   %s
  Error:     %v

Nothing was removed. Fix the cause and re-run `+"`wet setup`"+` %s, or
finish setup manually (inspect .env.worktree in the tree root and run the
remaining post_create commands from .wet.yaml by hand).

`, what, branch, path, branch, slot, hook, rendered, hookErr, retry)
}

// copyFromConfig copies the files and directories declared in cfg.Copy.
// Missing sources are silently skipped — the `.wet.yaml` author may list
// optional files (e.g., `.claude/settings.local.json`) that not every
// developer has locally.
func copyFromConfig(cfg *WetConfig, srcRoot, dstRoot string) []string {
	var copied []string
	for _, f := range cfg.Copy.Files {
		if ok := copyOneFile(srcRoot, dstRoot, f); ok {
			copied = append(copied, f)
		}
	}
	for _, dir := range cfg.Copy.Dirs {
		if ok := copyOneDir(srcRoot, dstRoot, dir); ok {
			copied = append(copied, dir+"/")
		}
	}
	return copied
}

func copyOneFile(srcRoot, dstRoot, rel string) bool {
	src := filepath.Join(srcRoot, rel)
	if _, err := os.Stat(src); err != nil {
		return false
	}
	dst := filepath.Join(dstRoot, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false
	}
	return copyFile(src, dst) == nil
}

func copyOneDir(srcRoot, dstRoot, rel string) bool {
	src := filepath.Join(srcRoot, rel)
	if _, err := os.Stat(src); err != nil {
		return false
	}
	dst := filepath.Join(dstRoot, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false
	}
	return copyDir(src, dst) == nil
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)

		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}
