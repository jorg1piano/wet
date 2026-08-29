package cmd

import (
	"encoding/json"
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

Pass ` + "`--no-multiplexer`" + ` to skip opening a configured terminal
multiplexer window — useful when a higher-level orchestrator manages sessions
itself. ` + "`--no-tmux`" + ` is kept as a backward-compatible alias.

Pass ` + "`--json`" + ` to write the created branch, its worktree path, and
the base ref it branched from as JSON. Progress is written to stderr in this
mode so stdout remains machine-readable.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runWorktreeCreate,
}

var (
	worktreeCreateNoTmux bool
	worktreeCreateJSON   bool
)

func init() {
	worktreeCreateCmd.Flags().BoolVar(&worktreeCreateNoTmux, "no-multiplexer", false, "don't open a new terminal multiplexer window after creating the worktree")
	worktreeCreateCmd.Flags().BoolVar(&worktreeCreateNoTmux, "no-tmux", false, "deprecated alias for --no-multiplexer")
	worktreeCreateCmd.Flags().BoolVar(&worktreeCreateJSON, "json", false, "output the created worktree as JSON")
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
		programOptions := []tea.ProgramOption{}
		if worktreeCreateJSON {
			// Keep stdout reserved for the final JSON document.
			programOptions = append(programOptions, tea.WithOutput(cmd.ErrOrStderr()))
		}
		p := tea.NewProgram(m, programOptions...)
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

	if !worktreeCreateJSON {
		return createWorktree(branchName, worktreeCreateNoTmux)
	}

	result, err := createWorktreeWithWriters(
		branchName,
		worktreeCreateNoTmux,
		cmd.ErrOrStderr(),
		cmd.ErrOrStderr(),
	)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(cmd.OutOrStdout()).Encode(result); err != nil {
		return fmt.Errorf("write JSON output: %w", err)
	}
	return nil
}

// worktreeCreateResult is the machine-readable result of `worktree create`.
// Base records the exact ref chosen by baseRefForNewBranch, which may differ
// from the configured trunk name when origin is ahead of the local checkout.
type worktreeCreateResult struct {
	Branch string `json:"branch"`
	Path   string `json:"path"`
	Base   string `json:"base"`
}

// createWorktree is the minimal worktree-create path. It:
//  1. Resolves the main repo root (works from inside a worktree too).
//  2. Runs `git worktree add -b <branch> ../worktree-<branch>`.
//  3. Copies `.env` from the main repo root if present (single-file copy,
//     no config lookup, no templating).
//  4. Opens a configured multiplexer window (unless noTmux).
//
// No .wet.yaml load, no port allocation, no state file write, no hooks. Run
// `wet worktree setup` from inside the new worktree to configure the rest.
func createWorktree(branchName string, noTmux bool) error {
	_, err := createWorktreeWithWriters(branchName, noTmux, os.Stdout, os.Stderr)
	return err
}

// createWorktreeWithWriters performs createWorktree while directing progress
// and subprocess output to caller-selected streams. JSON mode uses stderr for
// both so its stdout can contain exactly one JSON document.
func createWorktreeWithWriters(branchName string, noTmux bool, out, errOut io.Writer) (worktreeCreateResult, error) {
	repoRoot, err := getMainRepoRoot()
	if err != nil {
		return worktreeCreateResult{}, err
	}

	worktreePath := worktreePathFor(repoRoot, branchName)

	// Branch off the configured trunk, not off whatever the main checkout
	// happens to have checked out. A repo whose default branch is stale (or
	// simply parked on another feature branch) otherwise hands every new
	// worktree that state, and the first sign of it is a PR full of someone
	// else's commits.
	base := baseRefForNewBranch(mainBranchName())
	fmt.Fprintf(out, "  Branching %s off %s\n", branchName, base)

	// --no-track: branching off origin/<main> would otherwise set the new
	// branch's upstream to the trunk, so a plain `git push` from the worktree
	// pushes the feature work straight onto it. `wet pr create` sets the
	// upstream to the branch's own remote ref when it pushes.
	gitCmd := exec.Command("git", "worktree", "add", "--no-track", "-b", branchName, worktreePath, base)
	gitCmd.Stdout = out
	gitCmd.Stderr = errOut
	if err := gitCmd.Run(); err != nil {
		return worktreeCreateResult{}, fmt.Errorf("failed to create worktree: %w", err)
	}

	if err := postWorktreeAddWithWriter(repoRoot, worktreePath, branchName, "Created", noTmux, out); err != nil {
		return worktreeCreateResult{}, err
	}

	return worktreeCreateResult{
		Branch: branchName,
		Path:   worktreePath,
		Base:   base,
	}, nil
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
// present), open a configured multiplexer window (unless noTmux), and print
// the next-step hint. `label` is the past-tense verb used in the success
// line ("Created" or "Mounted"). `noTmux` suppresses that window — used by
// orchestrators that manage sessions themselves.
func postWorktreeAdd(repoRoot, worktreePath, branchName, label string, noTmux bool) error {
	return postWorktreeAddWithWriter(repoRoot, worktreePath, branchName, label, noTmux, os.Stdout)
}

func postWorktreeAddWithWriter(repoRoot, worktreePath, branchName, label string, noTmux bool, out io.Writer) error {
	srcEnv := filepath.Join(repoRoot, ".env")
	if _, statErr := os.Stat(srcEnv); statErr == nil {
		dstEnv := filepath.Join(worktreePath, ".env")
		if err := copyFile(srcEnv, dstEnv); err != nil {
			return fmt.Errorf("copy .env: %w", err)
		}
		fmt.Fprintln(out, "  Copied: .env")
	}

	fmt.Fprintf(out, "\n  %s worktree '%s' at %s\n", label, branchName, worktreePath)

	if !noTmux {
		openMultiplexerWindow(repoRoot, worktreePath, branchName, out)
	}

	fmt.Fprintf(out, "\n  Next: cd %s && wet worktree setup\n", worktreePath)
	return nil
}

func openMultiplexerWindow(repoRoot, worktreePath, branchName string, out io.Writer) {
	multiplexer := "tmux"
	if cfg, err := LoadWetConfig(repoRoot); err == nil && cfg.Multiplexer != "" {
		multiplexer = cfg.Multiplexer
	}
	switch multiplexer {
	case "none":
		return
	case "tmux":
		if os.Getenv("TMUX") == "" {
			return
		}
		tmuxCmd := exec.Command("tmux", "new-window", "-n", branchName, "-c", worktreePath)
		if err := tmuxCmd.Run(); err != nil {
			fmt.Fprintf(out, "  Warning: could not open tmux window: %v\n", err)
		} else {
			fmt.Fprintf(out, "  Opened in new tmux window '%s'\n", branchName)
		}
	default:
		// LoadWetConfig validates known values; this fallback keeps create/mount
		// non-fatal if config loading was unavailable above.
		return
	}
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
// copyFromConfig copies the declared files and dirs, returning what it copied.
// The second return is a reason the copy was skipped entirely, for the caller
// to surface — a silent no-op reads as "nothing was declared".
func copyFromConfig(cfg *WetConfig, srcRoot, dstRoot string) ([]string, string) {
	// Copying a checkout onto itself destroys it: copyFile truncates the
	// destination before reading the source, so with src == dst every file
	// becomes zero bytes — including files wet only copies and never renders.
	//
	// This is reachable without the main checkout being involved. LocalConfigDir
	// resolves to whichever checkout holds main_branch, so a worktree standing
	// on that branch (e.g. `main_branch: development` in a checkout of
	// development) gets srcRoot == dstRoot even under `wet worktree setup`,
	// where isMain is false. Guarding on "which command ran" cannot catch that;
	// only comparing the resolved paths can.
	if samePath(srcRoot, dstRoot) {
		return nil, fmt.Sprintf(
			"source and destination are the same checkout (%s)", srcRoot)
	}
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
	return copied, ""
}

// samePath reports whether two paths denote the same file or directory.
//
// It guards a truncating copy, so it fails CLOSED: anything it cannot resolve
// is reported as "same", which costs a skipped copy, while a wrong "different"
// costs the file's contents. That asymmetry is the whole reason this exists.
//
// os.SameFile is the real predicate when both paths exist — it compares inode
// identity, so it also catches hardlinks and bind mounts that string equality
// misses. When one side is absent (the normal case: dst not yet created) that
// is a genuine "different", and only then do we fall back to comparing
// symlink-resolved paths, which keeps /tmp and /private/tmp from looking like
// distinct trees.
func samePath(a, b string) bool {
	fa, errA := os.Stat(a)
	fb, errB := os.Stat(b)
	switch {
	case errA == nil && errB == nil:
		return os.SameFile(fa, fb)
	case os.IsNotExist(errA) || os.IsNotExist(errB):
		// At least one side does not exist yet, so they cannot be the same
		// file. Fall through to the path comparison below, which still catches
		// a dst whose parent is a symlink to src's parent.
	default:
		// Permission denied, ELOOP, ENOTDIR: the path exists in some form we
		// cannot inspect. Refuse rather than truncate on a guess.
		return true
	}

	return resolvePath(a) == resolvePath(b)
}

// containsPath reports whether dir contains other, so a directory copy cannot
// be asked to write into its own source. filepath.Walk reads each directory's
// entries as it enters it, so a dst nested under src is copied again as Walk
// reaches it — deeper each time.
func containsPath(dir, other string) bool {
	rd, ro := resolvePath(dir), resolvePath(other)
	return strings.HasPrefix(ro, rd+string(os.PathSeparator))
}

// copyOneFile needs no self-copy guard of its own: every file write in this
// file goes through copyFile, which refuses one. copyOneDir does need its own,
// because containment is a directory-only hazard that copyFile cannot see.
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
	// Equality is not enough for a directory: a dst nested inside src (or the
	// reverse) makes Walk copy the tree into itself, growing as it goes.
	if samePath(src, dst) || containsPath(src, dst) || containsPath(dst, src) {
		return false
	}
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
	// os.Create below truncates, so a src == dst call would erase the file
	// rather than copy it. Callers guard too; this is the last line of defence
	// for any future caller that forgets.
	if samePath(src, dst) {
		return fmt.Errorf("refusing to copy %s onto itself", src)
	}
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
