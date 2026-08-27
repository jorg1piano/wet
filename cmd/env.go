package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

var (
	envPullDryRun  bool
	envPullStdout  bool
	envPullSecrets string
	// envSelectEnv backs the --env flag on pull, explain and push. One
	// variable for all three because one command runs per process, and the
	// flag means exactly the same thing in each: which secrets.vaults: entry
	// this run reads from.
	envSelectEnv string
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
and .wet.yaml names the vault and item the definitions resolve against.`,
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

--env selects a vault when .wet.yaml declares per-environment vaults under
secrets.vaults:. Without it the run uses secrets.default_env:

  wet env pull --env production

--dry-run resolves and validates everything and writes nothing. It still
contacts the store, because whether a required secret exists is exactly what a
dry run is asked to find out. It never prints a secret value.

--stdout writes the generated file to standard output instead of to disk, so a
deploy environment's credentials can be piped straight into whatever consumes
them and never land on a developer's filesystem:

  wet env pull fly-staging --stdout | fly secrets import --app my-app

It takes exactly one output, and every progress line goes to stderr so the
stream carries the file and nothing else.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runEnvPull,
}

var envExplainCmd = &cobra.Command{
	Use:   "explain [output]",
	Short: "Show what each declared output contains and where its secrets live",
	Long: `Report the declared outputs: their keys, their sources, and the store
destination of every secret they select.

It reads .wet.yaml only — no worktree, no store, no values. Names and
provenance are printed; secret values never are.

--env explains the destinations as another environment's vault, exactly as a
pull with the same flag would resolve them.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runEnvExplain,
}

func init() {
	envPullCmd.Flags().BoolVar(&envPullDryRun, "dry-run", false,
		"resolve and validate everything, write nothing")
	envPullCmd.Flags().StringVar(&envPullSecrets, "secrets", "",
		"override secrets.policy for this run (required|fallback|disabled)")
	envPullCmd.Flags().BoolVar(&envPullStdout, "stdout", false,
		"write the generated file to stdout instead of to disk (one output)")
	for _, c := range []*cobra.Command{envPullCmd, envExplainCmd, envPushCmd} {
		c.Flags().StringVar(&envSelectEnv, "env", "",
			"which secrets.vaults: environment to read (default secrets.default_env)")
	}
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
	// Both mean "write nothing to disk", but one of them still produces the
	// file. Picking either silently would break the other one's promise.
	if envPullStdout && envPullDryRun {
		return errors.New("--stdout and --dry-run are mutually exclusive: one writes the file to the stream, the other writes it nowhere")
	}
	cfg, treePath, err := loadConfigForTree()
	if err != nil {
		return err
	}
	data, err := buildWorktreeData(cfg, treePath)
	if err != nil {
		return err
	}
	req := PullRequest{
		Cfg:      cfg,
		TreePath: treePath,
		Data:     data,
		Only:     args,
		Policy:   envPullSecrets,
		Env:      envSelectEnv,
		DryRun:   envPullDryRun,
	}
	if envPullStdout {
		// Provenance and warnings move to stderr so the stream is the file and
		// nothing else: a "Wrote …" line inside a dotenv is a syntax error to
		// whatever reads it.
		req.Content = cmd.OutOrStdout()
		req.Out = cmd.ErrOrStderr()
	}
	return PullOutputs(req)
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
	selected, err := cfg.SelectEveryOutput(args)
	if err != nil {
		return err
	}
	env, err := cfg.Secrets.SelectEnv(envSelectEnv)
	if err != nil {
		return err
	}
	// Printed once, before the outputs: every destination below is a vault
	// this choice picked, and a reader comparing two `explain` runs has no
	// other way to see which one they are looking at.
	//
	// Only when it picked one, though — the same rule a pull follows.
	// `wet env explain mobile` on an output selecting no secrets resolves no
	// vault at all, and a banner over that listing claims an influence the
	// environment did not have.
	explainsASecret := false
	for _, o := range selected {
		explainsASecret = explainsASecret || len(o.Secrets) > 0
	}
	if env != "" && explainsASecret {
		fmt.Printf("environment: %s (of %s)\n\n", env, strings.Join(cfg.Secrets.VaultEnvs(), ", "))
	}
	for i, o := range selected {
		if i > 0 {
			fmt.Println()
		}
		// Stated on the header line because it changes what the reader can
		// expect on disk: this file is not there after a setup.
		demand := ""
		if o.OnDemand {
			demand = " — on demand, generated only when named"
		}
		fmt.Printf("%s — %s (%s)%s\n", o.Name, o.Out, o.Format, demand)
		// Literals are counted apart from ordinary values because they are a
		// different kind of fact: an ordinary key varies per worktree and can
		// go missing when a name is renamed, a literal is a constant that
		// cannot. A client output that is mostly literals would otherwise
		// report a large "ordinary" count and imply an environment coupling
		// it does not have.
		literals := 0
		for _, key := range o.Values.Keys {
			if o.Values.By[key].Literal != nil {
				literals++
			}
		}
		fmt.Printf("  ordinary: %d\n", len(o.Include)+len(o.Values.Keys)-literals)
		if literals > 0 {
			fmt.Printf("  literals: %d\n", literals)
		}
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
			// A missing vault or item is a gap in the config, not a broken
			// config, and explain is the command you run to find out what is
			// missing — so it reports the gap instead of failing on it.
			ref, err := cfg.Secrets.ResolveSecretRef(name, env)
			if err != nil {
				// The resolver's own error is printed rather than one
				// summary for all of them. A missing item and a missing
				// vault are different keys with different fixes, and a
				// message that named only the vault sent half the readers to
				// the wrong line. The errors already say which key is
				// missing.
				fmt.Printf("    %s -> (no destination: %s) (%s)\n", name, whyNoDestination(name, err), fallback)
				continue
			}
			// A definition carrying its own ref: or vault: resolves to the
			// same place whatever --env says. Under a vaults: config that is
			// the one thing a reader cannot infer from this listing, and the
			// mistake it prevents is assuming `--env production` moved every
			// secret when it moved all but this one.
			fixed := ""
			if env != "" && (def.Ref != "" || def.Vault != "") {
				fixed = ", same in every environment"
			}
			fmt.Printf("    %s -> %s/%s/%s (%s%s)\n", name, ref.Vault, ref.Item, ref.Field, fallback, fixed)
		}
	}

	// Only when explaining the whole config: with one output named, "unused"
	// would mean "unused by this output", which is not a problem at all.
	if len(args) == 0 {
		reportUnusedSecrets(os.Stdout, cfg)
	}
	return nil
}

// reportUnusedSecrets prints definitions no output selects. Non-fatal: a
// credential used only by a deployment is legitimate — so this names them and
// leaves the judgement to the reader.
func reportUnusedSecrets(w io.Writer, cfg *WetConfig) {
	unused := cfg.UnusedSecrets()
	if len(unused) == 0 {
		return
	}
	fmt.Fprintf(w, "\nunused: %d secret(s) no output selects — %s\n",
		len(unused), strings.Join(unused, ", "))
	fmt.Fprintln(w, "  A definition nothing selects is inert: it is never fetched and never")
	fmt.Fprintln(w, "  written, which reads as wet ignoring a provider you configured.")
}

// worktreeAllocation is what one checkout was actually granted: its slot,
// branch, resolved ports and claimed pool items. Every command that needs to
// act on "this worktree's ports" reads it from here, so `wet env pull` and
// `wet mobile reverse-tcp` can never disagree about which ports a worktree owns.
type worktreeAllocation struct {
	Branch string
	Slot   int
	Ports  []ResolvedPort
	Pools  map[string]string
	// Extras are the claims beyond the primary pick per pool — what
	// `wet mobile add` recorded. See WorktreeState.PoolExtras.
	Extras map[string][]string
}

// Port returns the resolved value of a declared port by name.
func (a worktreeAllocation) Port(name string) (int, bool) {
	for _, p := range a.Ports {
		if p.Name == name {
			return p.Value, true
		}
	}
	return 0, false
}

// readWorktreeAllocation reports what `worktreePath` was allocated.
//
// The environment wins over the state file, and has to: post_create hooks run
// BEFORE `wet worktree setup` persists .wet-worktrees.json, so a hook-invoked
// command would find no entry for its own worktree. BuildHookEnv has already
// exported everything needed (SLOT, BRANCH, ports, POOL_*), so in that context
// the environment is both authoritative and the only source available.
//
// Outside a hook (a hand-run command after setup), the state file is the
// record of what this worktree was allocated.
func readWorktreeAllocation(cfg *WetConfig, worktreePath string) (worktreeAllocation, error) {
	if slot, ok := slotFromEnv(); ok {
		return allocationFromEnv(cfg, slot)
	}
	state, err := loadConfig()
	if err != nil {
		return worktreeAllocation{}, fmt.Errorf("load wet state: %w", err)
	}
	wt := worktreeStateForPath(state, worktreePath)
	if wt == nil {
		return worktreeAllocation{}, fmt.Errorf("no wet state for %s — run `wet setup` first "+
			"(it works in the main repo and in worktrees), or invoke it "+
			"from a .wet.yaml hook", worktreePath)
	}
	ports := make([]ResolvedPort, 0, len(wt.Ports))
	for _, p := range cfg.Ports {
		if v, ok := wt.Ports[p.Name]; ok {
			ports = append(ports, ResolvedPort{Name: p.Name, Value: v})
		}
	}
	return worktreeAllocation{Branch: wt.Branch, Slot: wt.Index, Ports: ports, Pools: wt.Pools, Extras: wt.PoolExtras}, nil
}

// buildWorktreeData assembles the ordinary value set for an env command.
func buildWorktreeData(cfg *WetConfig, worktreePath string) (map[string]any, error) {
	alloc, err := readWorktreeAllocation(cfg, worktreePath)
	if err != nil {
		return nil, err
	}
	return withRenderedEnv(cfg, worktreePath, allocationTemplateData(cfg, alloc))
}

// allocationTemplateData is the template data for a render that must not
// boot anything — a pull, a device command, a pre_remove hook. Handles are
// peeked: a running emulator renders as its live serial, exactly as the
// setup that booted it rendered it, and a stopped one as its item.
func allocationTemplateData(cfg *WetConfig, alloc worktreeAllocation) map[string]any {
	data := BuildTemplateData(cfg, alloc.Branch, alloc.Slot, alloc.Ports, alloc.Pools)
	WithPoolHandles(data, peekPrimaryHandles(alloc.Pools))
	return WithPoolExtras(data, alloc.Extras, peekExtraHandles(alloc.Extras))
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

// allocationFromEnv reconstructs the allocation from the variables
// BuildHookEnv exported. Ports and pools are read back by declared name so the
// values always match the ones the hook itself sees as $APP_PORT /
// $POOL_DEVICES.
func allocationFromEnv(cfg *WetConfig, slot int) (worktreeAllocation, error) {
	branch := os.Getenv("BRANCH")
	if branch == "" {
		return worktreeAllocation{}, fmt.Errorf("SLOT is set but BRANCH is not — refusing to guess the worktree's identity")
	}
	ports := make([]ResolvedPort, 0, len(cfg.Ports))
	for _, p := range cfg.Ports {
		raw, ok := os.LookupEnv(p.Name)
		if !ok {
			return worktreeAllocation{}, fmt.Errorf("port %s declared in .wet.yaml but missing from the hook environment", p.Name)
		}
		v, err := strconv.Atoi(raw)
		if err != nil {
			return worktreeAllocation{}, fmt.Errorf("port %s=%q is not a number", p.Name, raw)
		}
		ports = append(ports, ResolvedPort{Name: p.Name, Value: v})
	}
	pools := map[string]string{}
	extras := map[string][]string{}
	for name := range cfg.Pools {
		if v, ok := os.LookupEnv("POOL_" + strings.ToUpper(name)); ok {
			pools[name] = v
		}
		// POOL_X_ALL is every claim with the primary first; what follows it
		// is the extras. Read back rather than trusted to match POOL_X: a
		// hook environment assembled by hand may set one without the other.
		all := strings.Fields(os.Getenv("POOL_" + strings.ToUpper(name) + "_ALL"))
		if len(all) > 1 {
			extras[name] = all[1:]
		}
	}
	return worktreeAllocation{Branch: branch, Slot: slot, Ports: ports, Pools: pools, Extras: extras}, nil
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

// whyNoDestination trims the resolver's `secrets.definitions.<NAME>: ` prefix
// off an error explain is about to print.
//
// explain lists one line per secret and has already printed the name, so the
// prefix repeats it — and a config missing its vault is missing it for EVERY
// secret, so the listing became ten near-identical long lines with the one
// word that differs buried in the middle. The rest of the message is the part
// worth reading, and it is the resolver's wording either way.
func whyNoDestination(name string, err error) string {
	return strings.TrimPrefix(err.Error(), "secrets.definitions."+name+": ")
}
