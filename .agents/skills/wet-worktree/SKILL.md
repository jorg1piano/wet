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

   Do not replace `herdr pane wait-output` with a hand-written sleep/poll loop, and do not park the orchestrator in a long `sleep`/poll Bash command that prevents it from handling the next user message. If the pane shows `Slot N start complete` and is back at a shell prompt, treat bootstrap as successful and continue to `herdr agent start`/`herdr agent prompt`; do not abandon a ready worktree because the watcher missed the line. If the pane is still booting, report that and leave the claim held. If it shows an error, stop before starting the agent.

   After bootstrap succeeds, starting and prompting the worktree agent is part of the same orchestration step. Do it before accepting or working on another user request. A created/opened/bootstrapped Herdr worktree with no `herdr agent start` and no `herdr agent prompt` means the user's work has not started. If another user message arrives that refers to updating that agent, but no agent exists yet, start the agent first and deliver both the original prompt and the update.

6. Start the agent in that pane and submit the user’s task verbatim:

   ```bash
   herdr agent start "<slug>" --kind <Claude-or-Codex> --pane "<pane_id>"
   herdr agent prompt "<slug>" "<user task verbatim>

   Work only in this worktree: $WT.
   This worktree has already been created for you. Do not create, mount, pop, or switch to another worktree.
   The repo root is $WT; if your shell cwd drifts into a subdirectory, cd back to $WT before running root-relative commands.
   When done, run the relevant checks/formatters and commit the branch. Do not report done with a dirty worktree."
   ```

If the user prompt depends on a design/MCP asset that the orchestrator cannot read because authentication is missing, do not delay or skip agent startup. Include the design URL/file names and the exact auth failure in the prompt, and tell the worktree agent to proceed once the user authorizes the design tool or to ask for the missing assets. Missing design auth is not a reason to leave a bootstrapped worktree idle.

After `herdr agent prompt`, verify the agent actually started: `herdr agent list` should show the branch as `working`, or `herdr pane read` should show Claude responding. If it is still `idle` with the prompt sitting in Claude's input area, send one Enter to the pane and verify again. Do not report that the work is running until the prompt has been submitted and the agent has begun responding.

7. Report back with branch, path, and that it is running in the background. If screenshots or other artifacts are part of the user request, include that requirement verbatim in the agent prompt and require absolute paths in the final report.

Default completion policy: if the orchestrator watches the agent through completion (for example because the user asked for screenshots/results back here), then after the worktree agent reports a clean committed branch, land and remove it: rebase on main, fast-forward main, stop the slot runtime, pop the worktree, and close its Herdr workspace. Leave the worktree open only when the user explicitly says to keep it for review or manual poking. This prevents finished worktrees with live helper processes from lingering after the branch has landed.

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

For multi-actor/device tasks, keep one worktree/backend/database and add devices to it. Do not create one worktree per actor. Use the deterministic WET command:

```bash
cd "$WT" && wet mobile ensure --count 4 --headless
cd "$WT" && wet mobile list
```

`ensure` counts the primary device from setup plus extras, reuses free predeclared pool items before growing new AVDs, boots/reverses every claimed device, and is safe to re-run after a half boot.

After restarting the machine, warm every claimed worktree from the main checkout:

```bash
wet slot warm --all --headless
```

Use `wet slot warm --all --headless --reset` only when you also want every claimed slot cleaned before startup.
