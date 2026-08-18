package cmd

import (
	"fmt"
	"os"
	"os/exec"
)

// runWetHook executes a single already-rendered hook command.
//
// The caller drives a three-stage pipeline and owns the first two stages:
// RenderHookCommand expands the raw `.wet.yaml` string as a text/template
// (so `{{.APP_PORT}}` is a literal by the time it lands here), and
// BuildHookEnv produces `env` — the *complete* environment for the hook,
// seeded from os.Environ() and extended with PROJECT_NAME, BRANCH,
// BRANCH_SAFE, SLOT, one entry per resolved port, one POOL_<NAME> per pool
// pick, and every rendered `env:` entry. It is assigned to cmd.Env
// wholesale, which replaces rather than extends the child's environment —
// hence BuildHookEnv having to include os.Environ() itself.
//
// The command runs through `sh -c`, so hooks are real shell: pipes, &&,
// globs, and redirection all work. cwd is workdir (the worktree), so hooks
// can use paths relative to the tree they act on.
//
// Stdin, stdout, and stderr are inherited from the current process so
// interactive hooks (e.g. a device picker) can prompt, and output streams
// live instead of buffering until exit.
//
// The rendered command is echoed before running, and returned errors are
// left unwrapped: both call sites treat a nonzero exit as fatal and quote
// `rendered` in the failure report, which is why the caller keeps it in
// scope rather than having this function render the body itself.
func runWetHook(name, rendered, workdir string, env []string) error {
	fmt.Printf("  hook %s: %s\n", name, rendered)
	cmd := exec.Command("sh", "-c", rendered)
	cmd.Dir = workdir
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
