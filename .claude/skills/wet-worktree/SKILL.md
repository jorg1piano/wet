---
name: wet-worktree
description: Create and manage wet worktrees with claimed slots, preferably in a separate Herdr worktree workspace. Use when the user asks to create a worktree, start work on X in parallel, spin up a branch, fix X in a new worktree, or implies new work should happen away from main.
---

# wet worktree companion

When the user asks to start a new worktree, assume the default is: **create a git worktree, claim a wet slot, open it as its own Herdr worktree workspace when Herdr is available, and run the implementation there — not inline in main.**

## Rules

- Do not edit the main checkout for the requested feature/fix unless explicitly asked.
- Use `wet worktree create <slug> --no-multiplexer --json` from the repo root.
- Run `wet setup` in the new worktree; that claims the slot, ports, env files, and pools.
- If slot lifecycle hooks exist, run `wet slot reset --headless` before `wet slot start --headless`.
- If Herdr is available, open the worktree with `herdr worktree open` and start the agent there; do not continue the feature work inline in the main pane.
- Do not run `wet setup`, `wet worktree setup`, `just setup`, or slot hooks in the orchestrator/main pane. Open the Herdr worktree workspace first; then bootstrap inside its pane.
- Do not inspect implementation files, edit files, run tests, or otherwise do the user's feature work from the orchestrator/main pane. The orchestrator creates the worktree, opens the workspace, runs lifecycle commands in that pane, starts the agent in that pane, submits the prompt, and watches/reports.

## Preferred Herdr flow

1. Create the worktree:

   ```bash
   wet worktree create <slug> --no-multiplexer --json
   ```

2. Parse the JSON `path` as `$WT`.

3. Open it as a Herdr worktree workspace, nested under the repo. Prefer `--cwd` + `--path`; `--path` without `--cwd` can resolve against the wrong Herdr source workspace when the controlling pane is not the repo workspace. Do not pass both `--path` and `--branch`; Herdr accepts exactly one selector.

   ```bash
   herdr worktree open --cwd "$REPO_ROOT" --path "$WT" --label "<slug>" --no-focus
   ```

4. Parse the returned `workspace_id`, then find its pane:

   ```bash
   herdr pane list --workspace "<workspace_id>"
   ```

5. Run setup/reset/start in that pane so progress is visible there. This is mandatory; running setup inline from the main/orchestrator pane creates a claimed worktree without a nested Herdr workspace and hides long-running boot output from the user.

   Do not continue to implementation after bootstrap. Start the agent in this pane first; all code changes, screenshots, tests, and commits belong to that worktree agent, not the orchestrator.

   ```bash
   herdr pane run "<pane_id>" 'wet setup --headless && wet slot reset --headless && wet slot start --headless'
   herdr pane wait-output "<pane_id>" --regex '(?i)slot .* start complete|worktree configured|hook failed|slot hook failed' --timeout 900000
   ```

6. Start the agent in that pane and submit the user’s task verbatim:

   ```bash
   herdr agent start "<slug>" --kind <Claude-or-Codex> --pane "<pane_id>"
   herdr agent prompt "<slug>" "<user task verbatim>

   Work only in this worktree: $WT.
   The repo root is $WT; if your shell cwd drifts into a subdirectory, cd back to $WT before running root-relative commands.
   When done, run the relevant checks/formatters and commit the branch. Do not report done with a dirty worktree."
   ```

7. Report back with branch, path, and that it is running in the background. If screenshots or other artifacts are part of the user request, include that requirement verbatim in the agent prompt and require absolute paths in the final report.

## Fallback when Herdr is unavailable

If `herdr` is not on PATH, create/configure the worktree and then work from that path:

```bash
cd "$WT" && wet setup --headless
cd "$WT" && wet slot reset --headless
cd "$WT" && wet slot start --headless
```

Do not use tmux; `--no-multiplexer` already prevented wet from opening one.

## Detecting slot hooks

Check `.wet.yaml` for any of:

```yaml
hooks:
  slot_reset:
  slot_start:
  slot_stop:
  slot_rebuild:
  slot_doctor:
```

If absent, skip `wet slot reset/start` and rely on the repo’s existing `post_create` behavior.

## Useful commands

```bash
cd "$WT" && wet slot doctor --headless
cd "$WT" && wet slot stop --headless
cd "$WT" && wet worktree pop
```

If a slot is dirty or reset fails:

```bash
cd "$WT" && wet slot rebuild --headless
```

After restarting the machine, warm every claimed worktree from the main checkout:

```bash
wet slot warm --all --headless
```

Use `wet slot warm --all --headless --reset` only when you also want every claimed slot cleaned before startup.
