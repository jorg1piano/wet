package cmd

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"text/template"

	"github.com/spf13/cobra"
)

var (
	prTitle  string
	prBase   string
	prDraft  bool
	prFill   bool
	prNoOpen bool
)

var prCmd = &cobra.Command{
	Use:   "pr",
	Short: "Pull request helpers",
}

var prCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Push the current branch and create a GitHub pull request",
	Long: `Push the current branch to origin and open a GitHub pull request via gh.

Works from any branch, but is especially useful inside wet worktrees: when
run from a worktree tracked in .wet-worktrees.json, the PR body automatically
includes the worktree's allocated ports, pool items, and slot index.

Requires: git, gh (GitHub CLI, authenticated).

If .wet.yaml has a pr.base field, it's used as the base branch; otherwise
defaults to 'main'. The --base flag overrides both.`,
	RunE: runPrCreate,
}

func init() {
	prCreateCmd.Flags().StringVarP(&prTitle, "title", "t", "", "PR title (default: branch name cleaned up)")
	prCreateCmd.Flags().StringVarP(&prBase, "base", "b", "", "base branch (default: from .wet.yaml or 'main')")
	prCreateCmd.Flags().BoolVarP(&prDraft, "draft", "d", false, "create as draft PR")
	prCreateCmd.Flags().BoolVar(&prFill, "fill", false, "auto-fill title and body from commits (gh --fill)")
	prCreateCmd.Flags().BoolVar(&prNoOpen, "no-open", false, "don't open the PR in the browser after creating")
	prCmd.AddCommand(prCreateCmd)
	prCmd.AddCommand(prOpenCmd)
	rootCmd.AddCommand(prCmd)
}

func runPrCreate(cmd *cobra.Command, args []string) error {
	// 1. Get current branch
	branch, err := currentBranch()
	if err != nil {
		return err
	}
	if branch == mainBranchName() || branch == "main" || branch == "master" {
		return fmt.Errorf("refusing to create PR from %s — switch to a feature branch first", branch)
	}

	// 2. Resolve base branch
	base := resolveBaseBranch()

	// 3. Push to origin
	fmt.Printf("  Pushing %s to origin...\n", branch)
	pushCmd := exec.Command("git", "push", "-u", "origin", branch)
	pushCmd.Stdout = os.Stdout
	pushCmd.Stderr = os.Stderr
	if err := pushCmd.Run(); err != nil {
		return fmt.Errorf("git push failed: %w", err)
	}

	// 4. Build title
	title := prTitle
	if title == "" && !prFill {
		title = branchToTitle(branch)
	}

	// 5. Build body (include worktree context if available)
	body := buildPrBody(branch)

	// 6. Build gh pr create args
	ghArgs := []string{"pr", "create", "--base", base}
	if prFill {
		ghArgs = append(ghArgs, "--fill")
	} else {
		if title != "" {
			ghArgs = append(ghArgs, "--title", title)
		}
		if body != "" {
			ghArgs = append(ghArgs, "--body", body)
		}
	}
	if prDraft {
		ghArgs = append(ghArgs, "--draft")
	}

	// 7. Run gh
	fmt.Printf("  Creating PR: %s → %s\n", branch, base)
	ghCmd := exec.Command("gh", ghArgs...)
	ghCmd.Stdout = os.Stdout
	ghCmd.Stderr = os.Stderr
	ghCmd.Stdin = os.Stdin
	if err := ghCmd.Run(); err != nil {
		return fmt.Errorf("gh pr create failed: %w", err)
	}

	// 8. Open in browser (default: yes, unless --no-open)
	if !prNoOpen {
		fmt.Println("  Opening PR in browser...")
		openCmd := exec.Command("gh", "pr", "view", "--web")
		openCmd.Stdout = os.Stdout
		openCmd.Stderr = os.Stderr
		_ = openCmd.Run() // best-effort, don't fail the command if browser can't open
	}

	return nil
}

var prOpenCmd = &cobra.Command{
	Use:   "open",
	Short: "Open the PR for the current branch in the browser",
	RunE: func(cmd *cobra.Command, args []string) error {
		openCmd := exec.Command("gh", "pr", "view", "--web")
		openCmd.Stdout = os.Stdout
		openCmd.Stderr = os.Stderr
		if err := openCmd.Run(); err != nil {
			return fmt.Errorf("gh pr view --web failed (is there a PR for this branch?): %w", err)
		}
		return nil
	},
}

func currentBranch() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("failed to get current branch: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func resolveBaseBranch() string {
	// CLI flag takes precedence
	if prBase != "" {
		return prBase
	}
	// Then .wet.yaml pr.base, which defaults to the repo's main branch:
	// a repo whose trunk is `development` shouldn't have to say so twice.
	repoRoot, err := getRepoRoot()
	if err == nil {
		cfg, err := LoadWetConfig(repoRoot)
		if err == nil {
			if cfg.PR.Base != "" {
				return cfg.PR.Base
			}
			return cfg.MainBranchName()
		}
	}
	return DefaultMainBranch
}

// mainBranchName reads `main_branch:` from the repo's config, falling back to
// the default when there is no config to read (wet works in a repo without
// one for everything that doesn't need per-worktree state).
func mainBranchName() string {
	repoRoot, err := getRepoRoot()
	if err != nil {
		return DefaultMainBranch
	}
	cfg, err := LoadWetConfig(repoRoot)
	if err != nil {
		return DefaultMainBranch
	}
	return cfg.MainBranchName()
}

// branchToTitle converts "feature/add-login" to "Add login"
func branchToTitle(branch string) string {
	// Strip common prefixes
	for _, prefix := range []string{"feature/", "feat/", "fix/", "bugfix/", "chore/", "docs/", "refactor/", "hotfix/"} {
		if strings.HasPrefix(branch, prefix) {
			branch = strings.TrimPrefix(branch, prefix)
			break
		}
	}
	// Replace separators with spaces
	title := strings.NewReplacer("-", " ", "_", " ", "/", " ").Replace(branch)
	// Capitalize first letter
	if len(title) > 0 {
		title = strings.ToUpper(title[:1]) + title[1:]
	}
	return title
}

// buildPrBody generates a PR body with worktree context if available.
func buildPrBody(branch string) string {
	config, err := loadConfig()
	if err != nil {
		return ""
	}

	// Find this branch in worktree state
	var wt *WorktreeState
	for i := range config.Worktrees {
		if config.Worktrees[i].Branch == branch {
			wt = &config.Worktrees[i]
			break
		}
	}

	// Try loading .wet.yaml for a body template
	repoRoot, _ := getRepoRoot()
	var wetCfg *WetConfig
	if repoRoot != "" {
		wetCfg, _ = LoadWetConfig(repoRoot)
	}

	// If there's a custom body template in .wet.yaml, render it
	if wetCfg != nil && wetCfg.PR.BodyTemplate != "" && wt != nil {
		data := map[string]any{
			"Branch":      branch,
			"BranchSafe":  SanitizeBranch(branch),
			"Slot":        wt.Index,
			"Ports":       wt.Ports,
			"Pools":       wt.Pools,
			"ProjectName": wetCfg.ProjectName,
		}
		tmpl, err := template.New("pr-body").Parse(wetCfg.PR.BodyTemplate)
		if err == nil {
			var buf bytes.Buffer
			if err := tmpl.Execute(&buf, data); err == nil {
				return buf.String()
			}
		}
	}

	// Default body: include worktree info if we have it
	if wt == nil {
		return ""
	}

	var sb strings.Builder
	if len(wt.Ports) > 0 || len(wt.Pools) > 0 {
		sb.WriteString("## Worktree context\n\n")
		sb.WriteString(fmt.Sprintf("- **Slot**: %d\n", wt.Index))
		for name, val := range wt.Ports {
			sb.WriteString(fmt.Sprintf("- **%s**: %d\n", name, val))
		}
		for name, val := range wt.Pools {
			sb.WriteString(fmt.Sprintf("- **Pool %s**: %s\n", name, val))
		}
		sb.WriteString("\n---\n")
	}
	return sb.String()
}
