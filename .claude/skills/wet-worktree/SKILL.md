---
name: wet-worktree
description: Create and manage wet worktrees with claimed slots. Use when the user asks to create a worktree, start work on X in parallel, spin up a branch, fix X in a new worktree, or implies new work should happen away from main.
---

# wet worktree companion

When the user asks to start a new worktree, assume the default is: **create a git worktree, claim a wet slot, reset/start the slot if configured, then work from that worktree path while managing it from the current/main checkout.**

## Rules

- Do not edit the main checkout for the requested feature/fix unless explicitly asked.
- Do not use tmux by default; pass `--no-tmux` and manage the worktree with `cd <path> && ...` commands.
- Run `wet setup` in the new worktree; that is what claims the slot, ports, env files, and pools.
- If `.wet.yaml` declares slot lifecycle hooks, run `wet slot reset` before `wet slot start`.
- Keep reporting the worktree path and slot-oriented commands clearly.

## New worktree flow

1. From the repo root, choose a short branch slug from the task:

   ```bash
   wet worktree create <slug> --no-tmux --json
   ```

2. Read the JSON output and use its `path` for all further commands.

3. Claim/configure the slot:

   ```bash
   cd <path> && wet setup --headless
   ```

4. If slot hooks exist in `.wet.yaml`, prepare the runtime:

   ```bash
   cd <path> && wet slot reset --headless
   cd <path> && wet slot start --headless
   ```

5. Do the user’s requested implementation from `<path>`, not from main.

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

If absent, do not invent lifecycle commands; rely on the repo’s existing `post_create`/`pre_remove` behavior.

## Useful commands

```bash
cd <path> && wet slot doctor --headless
cd <path> && wet slot stop --headless
cd <path> && wet worktree pop
```

If a slot is dirty or reset fails:

```bash
cd <path> && wet slot rebuild --headless
```

## Completion

When done, summarize:

- branch name
- worktree path
- slot number, if visible in `wet setup` output
- whether `wet slot reset/start` ran
- next command for the user, usually `cd <path>` or `wet worktree pop`
