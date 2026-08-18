package cmd

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// wetSkillTemplate is the Claude Code skill body that `wet install wet-skill`
// writes into a user's repo. Embedded at build time from
// `cli/cmd/install-wet-template.md`, which is the single source of truth —
// edit the .md file, rebuild, and `wet install wet-skill` picks up the new
// content. The file lives inside this package (not at `cli/`) because
// `//go:embed` does not permit `..` in patterns.
//
//go:embed install-wet-template.md
var wetSkillTemplate string

// wetExampleConfig is the annotated `.wet.yaml` schema, shipped beside the
// skill so SKILL.md can link to a sibling file instead of restating the
// schema inline — one authored copy, no splicing, no drift. It lives in this
// package for the same `//go:embed` reason as the template above; there is
// deliberately no copy at the repo root.
//
//go:embed wet.example.yaml
var wetExampleConfig string

var installForce bool

var installCmd = &cobra.Command{
	Use:   "install {skill}",
	Short: "Install optional extras (e.g., the Claude Code helper skill)",
	Long: `Install optional extras into the current repo.

Known targets:
  skill        Drop a Claude Code skill into .claude/skills/wet/SKILL.md
               that teaches Claude how to write .wet.yaml configs AND how to
               boot the stack + spawn an agent team (worktree-aware).`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return cmd.Help()
		}
		return runInstall(cmd, args)
	},
}

func init() {
	installCmd.Flags().BoolVarP(&installForce, "force", "f", false, "overwrite existing files")
	rootCmd.AddCommand(installCmd)
}

func runInstall(cmd *cobra.Command, args []string) error {
	target := args[0]
	switch target {
	case "skill", "wet-skill":
		if err := installSkillFiles("wet", []skillFile{
			{name: "SKILL.md", content: wetSkillTemplate},
			{name: "wet.example.yaml", content: wetExampleConfig},
		}); err != nil {
			return err
		}
		if err := ensureGitignoreEntries(); err != nil {
			return err
		}
		return ensureClaudeSettings()
	default:
		return fmt.Errorf("unknown install target %q; known targets: skill", target)
	}
}

// ensureGitignoreEntries appends wet-specific entries to the repo's .gitignore
// if they're not already present.
func ensureGitignoreEntries() error {
	repoRoot, err := getRepoRoot()
	if err != nil {
		return err
	}
	gitignorePath := filepath.Join(repoRoot, ".gitignore")

	// The lock file is listed separately from the state file rather than as a
	// .wet-worktrees.* glob so the substring check below stays exact: a repo
	// installed before the lock existed gets just the missing entry appended.
	entries := []string{".wet-worktrees.json", ".wet-worktrees.lock", ".env.worktree"}

	existing, _ := os.ReadFile(gitignorePath)
	content := string(existing)

	var added []string
	for _, entry := range entries {
		if !strings.Contains(content, entry) {
			added = append(added, entry)
		}
	}

	if len(added) == 0 {
		return nil
	}

	f, err := os.OpenFile(gitignorePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open .gitignore: %w", err)
	}
	defer f.Close()

	// Ensure we start on a new line
	if len(content) > 0 && content[len(content)-1] != '\n' {
		fmt.Fprintln(f)
	}
	for _, entry := range added {
		fmt.Fprintln(f, entry)
	}

	fmt.Printf("  Added to .gitignore: %s\n", strings.Join(added, ", "))
	return nil
}

// ensureClaudeSettings ensures .claude/settings.json has the agent teams env var.
func ensureClaudeSettings() error {
	repoRoot, err := getRepoRoot()
	if err != nil {
		return err
	}
	settingsPath := filepath.Join(repoRoot, ".claude", "settings.json")

	// Load existing or start fresh
	settings := make(map[string]any)
	if data, err := os.ReadFile(settingsPath); err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("parse %s: %w", settingsPath, err)
		}
	}

	// Check if already set
	env, _ := settings["env"].(map[string]any)
	if env != nil {
		if v, ok := env["CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS"]; ok && v == "1" {
			return nil
		}
	}

	// Set it
	if env == nil {
		env = make(map[string]any)
		settings["env"] = env
	}
	env["CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS"] = "1"

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(settingsPath), err)
	}
	if err := os.WriteFile(settingsPath, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", settingsPath, err)
	}

	fmt.Println("  Enabled agent teams in .claude/settings.json")
	return nil
}

// skillFile is one embedded file to drop into a skill directory.
type skillFile struct {
	name    string
	content string
	// mode is the permission bits to write the file with. Zero means 0644.
	mode os.FileMode
}

// perm returns the mode to write f with, defaulting to 0644 so only files
// that must be executable have to say so.
func (f skillFile) perm() os.FileMode {
	if f.mode == 0 {
		return 0o644
	}
	return f.mode
}

// installSkillFiles writes embedded files into
// <repoRoot>/.claude/skills/<name>/. Each file is handled independently: one
// already matching the bundled copy is left alone and reported as up to date,
// and one that exists but differs is only overwritten with --force or an
// explicit yes. Per-file prompting (rather than one prompt for the set) keeps
// the answer meaningful — declining to clobber a hand-edited SKILL.md should
// not also withhold a schema file the user has never touched.
//
// The trailing "restart Claude Code" advice prints once, after the set, and
// only if something actually changed.
func installSkillFiles(name string, files []skillFile) error {
	repoRoot, err := getRepoRoot()
	if err != nil {
		return err
	}
	wrote, err := installFiles(filepath.Join(repoRoot, ".claude", "skills", name), files)
	if err != nil {
		return err
	}
	if wrote {
		fmt.Println("  Claude Code will load this skill on next session start.")
		fmt.Println("  Restart Claude Code or run /reload if you want it active immediately.")
	}
	return nil
}

// installProjectFiles writes embedded files into the repo root itself, for
// the ones that are working files rather than documentation: the user edits
// them, commits them, and deletes the ones they do not want. A skill
// directory is the wrong home for those — copying them out by hand is a
// manual step that drifts from the bundled copy.
func installProjectFiles(files []skillFile) error {
	repoRoot, err := getRepoRoot()
	if err != nil {
		return err
	}
	_, err = installFiles(repoRoot, files)
	return err
}

// installFiles writes each file into targetDir, reporting whether anything
// changed. A file name may carry a subdirectory ("scripts/foo.sh"), which is
// created on demand.
func installFiles(targetDir string, files []skillFile) (bool, error) {
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return false, fmt.Errorf("mkdir %s: %w", targetDir, err)
	}

	wrote := false
	for _, f := range files {
		targetPath := filepath.Join(targetDir, f.name)
		if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
			return false, fmt.Errorf("mkdir %s: %w", filepath.Dir(targetPath), err)
		}
		if existing, err := os.ReadFile(targetPath); err == nil {
			if string(existing) == f.content {
				// Content match is not enough: a copy restored from an
				// archive, or written by an older wet that had no mode
				// field, can be byte-identical yet non-executable. Repair
				// the bits rather than reporting a script that cannot run
				// as up to date.
				if err := os.Chmod(targetPath, f.perm()); err != nil {
					return false, fmt.Errorf("chmod %s: %w", targetPath, err)
				}
				fmt.Printf("  %s is already up to date.\n", targetPath)
				continue
			}
			if !installForce {
				ok, err := confirmOverwrite(targetPath)
				if err != nil {
					return false, err
				}
				if !ok {
					fmt.Printf("  Skipped: %s\n", targetPath)
					continue
				}
			}
		} else if !os.IsNotExist(err) {
			return false, fmt.Errorf("stat %s: %w", targetPath, err)
		}

		data := []byte(f.content)
		if err := os.WriteFile(targetPath, data, f.perm()); err != nil {
			return false, fmt.Errorf("write %s: %w", targetPath, err)
		}
		// WriteFile only applies the mode when it creates the file, so an
		// overwrite of an existing 0644 copy would leave the script
		// non-executable without this.
		if err := os.Chmod(targetPath, f.perm()); err != nil {
			return false, fmt.Errorf("chmod %s: %w", targetPath, err)
		}
		fmt.Printf("  Installed: %s (%d bytes)\n", targetPath, len(data))
		wrote = true
	}

	return wrote, nil
}

// confirmOverwrite asks before clobbering a file that differs from the
// bundled copy — the user may have edited it on purpose.
//
// Returns an error when there is nobody to ask: with stdin closed, redirected
// from /dev/null, or piped from a CI step, "no answer" is not a decision. The
// old code discarded Scanln's error, so EOF was indistinguishable from a typed
// "n" and `wet install wet-skill </dev/null` exited 0 having installed
// nothing — a green CI step that silently did no work. Ambiguity is now loud,
// and the message names the flag that resolves it.
func confirmOverwrite(targetPath string) (bool, error) {
	notInteractive := func() error {
		return fmt.Errorf(
			"%s exists and differs from the bundled version, but stdin is not interactive so there is nobody to ask; re-run with --force to overwrite it (or delete the file first)",
			targetPath,
		)
	}

	if !stdinIsTerminal() {
		return false, notInteractive()
	}

	fmt.Printf("  %s exists and differs from the bundled version.\n", targetPath)
	fmt.Print("  Overwrite? [y/N] ")
	var answer string
	if _, err := fmt.Scanln(&answer); err != nil {
		// A bare Enter yields "unexpected newline" — a deliberate "no", and
		// the documented default. Only an exhausted input means no answer
		// was ever possible.
		if errors.Is(err, io.EOF) {
			// The prompt was already printed (stdin looked like a terminal:
			// /dev/null is a character device too), so close the line before
			// the error lands.
			fmt.Println()
			return false, notInteractive()
		}
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", nil
}

// stdinIsTerminal reports whether stdin is a terminal a human could answer a
// prompt on. A redirect from a file or a pipe is not; note that /dev/null IS
// a character device and passes this check, which is why confirmOverwrite
// also treats EOF as "nobody to ask".
func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
