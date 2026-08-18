package cmd

import (
	_ "embed"
	"encoding/json"
	"fmt"
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
		if err := installSkillFile("wet", wetSkillTemplate); err != nil {
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

// installSkillFile writes an embedded skill template to
// <repoRoot>/.claude/skills/<name>/SKILL.md. Refuses to overwrite an
// existing file unless --force was passed.
// ensureGitignoreEntries appends wet-specific entries to the repo's .gitignore
// if they're not already present.
func ensureGitignoreEntries() error {
	repoRoot, err := getRepoRoot()
	if err != nil {
		return err
	}
	gitignorePath := filepath.Join(repoRoot, ".gitignore")

	entries := []string{".wet-worktrees.json", ".env.worktree"}

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

func installSkillFile(name, content string) error {
	repoRoot, err := getRepoRoot()
	if err != nil {
		return err
	}
	targetDir := filepath.Join(repoRoot, ".claude", "skills", name)
	targetPath := filepath.Join(targetDir, "SKILL.md")

	if existing, err := os.ReadFile(targetPath); err == nil {
		if string(existing) == content {
			fmt.Printf("  %s is already up to date.\n", targetPath)
			return nil
		}
		if !installForce {
			fmt.Printf("  %s exists and differs from the bundled version.\n", targetPath)
			fmt.Print("  Overwrite? [y/N] ")
			var answer string
			fmt.Scanln(&answer)
			answer = strings.ToLower(strings.TrimSpace(answer))
			if answer != "y" && answer != "yes" {
				fmt.Println("  Skipped.")
				return nil
			}
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", targetPath, err)
	}

	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", targetDir, err)
	}
	data := []byte(content)
	if err := os.WriteFile(targetPath, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", targetPath, err)
	}

	fmt.Printf("  Installed: %s (%d bytes)\n", targetPath, len(data))
	fmt.Println("  Claude Code will load this skill on next session start.")
	fmt.Println("  Restart Claude Code or run /reload if you want it active immediately.")
	return nil
}
