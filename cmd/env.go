package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

var (
	envPullDryRun  bool
	envPullSecrets string
)

var envCmd = &cobra.Command{
	Use:   "env",
	Short: "Generate, inspect, and push the environment files declared in .wet.yaml",
	Long: `Work with the outputs: declared in .wet.yaml.

An output is a generated environment file: which ordinary values it carries,
which declared secrets it may see, and where it is written. There is no
checked-in template — .wet.yaml is the whole inventory, so one file answers
what is generated, what each file holds, and which of those keys are secrets.

The secret store is never named on the command line: secrets.store decides,
and .wet.local.yaml supplies the machine's account and vault.`,
}

var envPullCmd = &cobra.Command{
	Use:   "pull [output]",
	Short: "Regenerate declared environment files for this worktree",
	Long: `Regenerate the files declared under outputs: in .wet.yaml.

With no argument every declared output is regenerated; with a name, only that
one:

  wet env pull
  wet env pull backend

Values come from this worktree's allocation — its slot, its strided ports, its
pool items — layered with .env.local overrides and the resolved env: block.
Secrets come from the store named by secrets.store, and only the ones an
output's secrets: list selects reach that output.

pull regenerates files and nothing else. It does not register a worktree,
allocate or change a slot, ports or pool items, write .env.worktree, or run
lifecycle hooks — that is ` + "`wet setup`" + `, which finishes by running this same
pull. So a pull is always safe to re-run.

--secrets overrides secrets.policy for one run:

  required   every selected secret must come from the store
  fallback   use a declared default when the store cannot answer, and say so
  disabled   never contact the store; declared defaults must suffice

--dry-run resolves and validates everything and writes nothing. It still
contacts the store, because whether a required secret exists is exactly what a
dry run is asked to find out. It never prints a secret value.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runEnvPull,
}

var envExplainCmd = &cobra.Command{
	Use:   "explain [output]",
	Short: "Show what each declared output contains and where its secrets live",
	Long: `Report the declared outputs: their keys, their sources, and the store
destination of every secret they select.

It reads .wet.yaml only — no worktree, no store, no values. Names and
provenance are printed; secret values never are.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runEnvExplain,
}

func init() {
	envPullCmd.Flags().BoolVar(&envPullDryRun, "dry-run", false,
		"resolve and validate everything, write nothing")
	envPullCmd.Flags().StringVar(&envPullSecrets, "secrets", "",
		"override secrets.policy for this run (required|fallback|disabled)")
	envCmd.AddCommand(envPullCmd, envExplainCmd, envPushCmd)
	rootCmd.AddCommand(envCmd)
}

// loadConfigForTree is the config load every env subcommand starts from: the
// worktree's own .wet.yaml when it has one, else the main repo's.
func loadConfigForTree() (*WetConfig, string, error) {
	treePath, err := getRepoRoot()
	if err != nil {
		return nil, "", err
	}
	repoRoot, err := getMainRepoRoot()
	if err != nil {
		return nil, "", err
	}
	cfg, _, err := LoadWetConfigForWorktree(treePath, repoRoot)
	if err != nil {
		if err == ErrConfigMissing {
			return nil, "", fmt.Errorf("`wet env` requires a .wet.yaml (none at %s or %s)", treePath, repoRoot)
		}
		return nil, "", err
	}
	return cfg, treePath, nil
}

func runEnvPull(cmd *cobra.Command, args []string) error {
	cfg, treePath, err := loadConfigForTree()
	if err != nil {
		return err
	}
	data, err := buildWorktreeData(cfg, treePath)
	if err != nil {
		return err
	}
	return PullOutputs(PullRequest{
		Cfg:      cfg,
		TreePath: treePath,
		Data:     data,
		Only:     args,
		Policy:   envPullSecrets,
		DryRun:   envPullDryRun,
	})
}

// valueSourceLabel says where an ordinary value comes from, so `explain`
// answers the question people actually open it for: is this key per-worktree,
// or the same everywhere?
func valueSourceLabel(cfg *WetConfig, name string) string {
	for _, p := range cfg.Ports {
		if p.Name == name {
			return "allocated port"
		}
	}
	if _, ok := cfg.Env[name]; ok {
		return "env:"
	}
	if prefix, _, dotted := strings.Cut(name, "."); dotted && (prefix == "Pools" || prefix == "Handles") {
		return "pool"
	}
	return "wet field"
}

func runEnvExplain(cmd *cobra.Command, args []string) error {
	cfg, _, err := loadConfigForTree()
	if err != nil {
		return err
	}
	selected, err := cfg.SelectOutputs(args)
	if err != nil {
		return err
	}
	for i, o := range selected {
		if i > 0 {
			fmt.Println()
		}
		fmt.Printf("%s — %s (%s)\n", o.Name, o.Out, o.Format)
		ordinary := len(o.Include) + len(o.Values.Keys)
		fmt.Printf("  ordinary: %d\n", ordinary)
		// Sources are named plainly rather than as {{.NAME}}. There is no
		// template any more, and showing one would invite a `from:` written
		// as a Go template expression — which is not a thing wet accepts.
		for _, name := range o.Include {
			fmt.Printf("    %s = %s\n", name, valueSourceLabel(cfg, name))
		}
		for _, key := range o.Values.Keys {
			v := o.Values.By[key]
			if v.Literal != nil {
				fmt.Printf("    %s = literal\n", key)
				continue
			}
			fmt.Printf("    %s = from %s (%s)\n", key, v.From, valueSourceLabel(cfg, v.From))
		}
		if len(o.Secrets) == 0 {
			fmt.Printf("  secrets: 0\n")
			continue
		}
		fmt.Printf("  secrets: %d (policy %s)\n", len(o.Secrets), cfg.Secrets.PolicyOrDefault())
		for _, name := range o.Secrets {
			def := cfg.Secrets.Definitions[name]
			fallback := "no default"
			if def.HasDefault() {
				fallback = "has default"
			}
			// A missing vault or item is a machine-local gap, not a broken
			// config, and explain is the command you run to find out what is
			// missing — so it reports the gap instead of failing on it.
			ref, err := cfg.Secrets.ResolveSecretRef(name)
			if err != nil {
				fmt.Printf("    %s -> (no destination: run `wet op vault`) (%s)\n", name, fallback)
				continue
			}
			fmt.Printf("    %s -> %s/%s/%s (%s)\n", name, ref.Vault, ref.Item, ref.Field, fallback)
		}
	}
	return nil
}

// buildWorktreeData assembles the ordinary value set for an env command.
//
// The environment wins over the state file, and has to: post_create hooks run
// BEFORE `wet worktree setup` persists .wet-worktrees.json, so a hook-invoked
// pull would find no entry for its own worktree. BuildHookEnv has already
// exported everything needed (SLOT, BRANCH, ports, POOL_*), so in that context
// the environment is both authoritative and the only source available.
//
// Outside a hook (a hand-run pull after setup), the state file is the record
// of what this worktree was allocated.
func buildWorktreeData(cfg *WetConfig, worktreePath string) (map[string]any, error) {
	if slot, ok := slotFromEnv(); ok {
		return dataFromEnv(cfg, worktreePath, slot)
	}
	state, err := loadConfig()
	if err != nil {
		return nil, fmt.Errorf("load wet state: %w", err)
	}
	wt := worktreeStateForPath(state, worktreePath)
	if wt == nil {
		return nil, fmt.Errorf("no wet state for %s — run `wet setup` first "+
			"(it works in the main repo and in worktrees), or invoke `wet env pull` "+
			"from a .wet.yaml hook", worktreePath)
	}
	ports := make([]ResolvedPort, 0, len(wt.Ports))
	for _, p := range cfg.Ports {
		if v, ok := wt.Ports[p.Name]; ok {
			ports = append(ports, ResolvedPort{Name: p.Name, Value: v})
		}
	}
	return withRenderedEnv(cfg, worktreePath, BuildTemplateData(cfg, wt.Branch, wt.Index, ports, wt.Pools))
}

func slotFromEnv() (int, bool) {
	raw, ok := os.LookupEnv("SLOT")
	if !ok {
		return 0, false
	}
	slot, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, false
	}
	return slot, true
}

// dataFromEnv reconstructs template data from the variables BuildHookEnv
// exported. Ports and pools are read back by declared name so the values
// always match the ones the hook itself sees as $APP_PORT / $POOL_DEVICES.
func dataFromEnv(cfg *WetConfig, treePath string, slot int) (map[string]any, error) {
	branch := os.Getenv("BRANCH")
	if branch == "" {
		return nil, fmt.Errorf("SLOT is set but BRANCH is not — refusing to guess the worktree's identity")
	}
	ports := make([]ResolvedPort, 0, len(cfg.Ports))
	for _, p := range cfg.Ports {
		raw, ok := os.LookupEnv(p.Name)
		if !ok {
			return nil, fmt.Errorf("port %s declared in .wet.yaml but missing from the hook environment", p.Name)
		}
		v, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("port %s=%q is not a number", p.Name, raw)
		}
		ports = append(ports, ResolvedPort{Name: p.Name, Value: v})
	}
	pools := map[string]string{}
	for name := range cfg.Pools {
		if v, ok := os.LookupEnv("POOL_" + strings.ToUpper(name)); ok {
			pools[name] = v
		}
	}
	return withRenderedEnv(cfg, treePath, BuildTemplateData(cfg, branch, slot, ports, pools))
}

// withRenderedEnv layers .env.local and the resolved cfg.Env values onto the
// base template data, so a template can reference {{.AUTH_BASE_URL}} exactly
// as .env.worktree and hooks do — derived once in `env:`, consumed everywhere.
func withRenderedEnv(cfg *WetConfig, treePath string, data map[string]any) (map[string]any, error) {
	repoRoot, err := getMainRepoRoot()
	if err != nil {
		return nil, err
	}
	// Resolved through LocalConfigDir, exactly as `wet setup` does. Reading it
	// from git's main working tree instead makes a pull revert the overrides
	// setup applied — the two paths must see the same file or the whole
	// "pull regenerates what setup wrote" contract is false.
	local, err := LoadEnvLocalForTree(treePath, repoRoot)
	if err != nil {
		return nil, err
	}
	if _, err := ResolveEnvValues(cfg, data, local); err != nil {
		return nil, err
	}
	return data, nil
}

// worktreeStateForPath finds the state entry whose Path matches worktreePath,
// comparing resolved absolute paths so a symlinked or relative entry still
// matches. Returns nil when the worktree isn't tracked.
func worktreeStateForPath(state *WorktreeConfig, worktreePath string) *WorktreeState {
	want := resolvePath(worktreePath)
	for i := range state.Worktrees {
		if resolvePath(state.Worktrees[i].Path) == want {
			return &state.Worktrees[i]
		}
	}
	return nil
}

// resolvePath makes a path absolute and resolves its symlinks, including when
// the path does not exist yet: it resolves the deepest ancestor that does and
// re-appends the rest.
//
// Plain EvalSymlinks fails outright on a missing leaf. A caller that falls back
// to the unresolved path is then comparing one resolved path against one
// unresolved — which on macOS means /private/var/... against /var/... for the
// same directory, so an equality or containment test silently answers
// "different". For samePath that would mean truncating a file it meant to
// protect.
func resolvePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	return resolveExisting(abs)
}

func resolveExisting(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p // reached the root; nothing left to resolve
	}
	return filepath.Join(resolveExisting(parent), filepath.Base(p))
}
