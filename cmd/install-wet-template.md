---
name: wet
description: "wet parallel-worktree helper: write/edit .wet.yaml configs AND boot the stack + spawn an agent team. Detects worktree context automatically. Triggers on: wet, wet.yaml, worktree create, parallel worktrees, port allocator, worktree hooks, wet pr, pull request, start team, spawn team, boot team, wet team."
allowed-tools: [Bash, Read, Glob]
context: inherit
---

# wet — parallel-worktree helper

This skill has two modes. Activate the right one based on what the user is doing:

1. **Config mode** — the user is writing, editing, or debugging `.wet.yaml`. Follow the "Config guide" sections below.
2. **Team mode** — the user wants to boot the dev stack and spawn an agent team. Follow the "Team guide" sections below.

If the user says something ambiguous, ask which they want.

---

# Config guide

Use this when the user is writing a fresh `.wet.yaml`, editing an existing one, asking what a field does, debugging a hook failure, or migrating a project to `wet`.

## Quick schema orientation

A `.wet.yaml` has these top-level keys, all optional except `project_name`:

- `project_name` — required. Docker-compose-safe name for generated compose project names.
- `main_branch` — trunk new worktrees are cut from, and the default PR base. Defaults to `main`.
- `copy` — files and directories copied from the main repo into each new worktree.
- `port_stride` — default port stride (how far ports advance per slot). Defaults to `10`.
- `ports` — named port declarations. Each port gets `base + slot * stride`.
- `pools` — named lists of opaque strings round-robined across worktrees.
- `pool_grow` — optional per-pool auto-expand strategy. Currently supports `android-emulator`: when a pool is exhausted, creates a new AVD, cold boots it, and appends the new serial to `.wet.yaml`.
- `env` — Go `text/template` strings, resolved to a fixed point; the derivation layer every template reads.
- `render` — template → output pairs, rendered by `wet setup` and `wet env populate --all`.
- `hooks` — shell commands run around worktree lifecycle events (`post_create`, `pre_remove`).
- `pr` — configures `wet pr create` behavior (base branch, body template).

## Read the schema of record first

`wet.example.yaml` sits next to this file, in the same
`.claude/skills/wet/` directory. It is a real local file, not a URL: open it
with Read before writing or editing any `.wet.yaml`. It is the fully annotated
schema — every key, its validation rule, and why it exists. This skill covers
how to *use* the schema; that file defines it. If the two ever disagree,
`wet.example.yaml` wins.

## Template fields reference

Every `env:` value, `render:` template, and hook command is rendered with this
data. Rendering uses `missingkey=error`: a reference to a field that does not
exist fails the operation loudly rather than producing an empty string.

| Key | Value | Example |
|---|---|---|
| `{{.ProjectName}}` | `project_name` from config | `myapp` |
| `{{.Branch}}` | Raw git branch name | `feature/foo` |
| `{{.BranchSafe}}` | Branch with slashes flattened, safe for Docker names | `feature-foo` |
| `{{.Slot}}` | Allocated slot (main = 0, worktrees from 1) | `1` |
| `{{.APP_PORT}}`, … | One key per declared port | `8090` |
| `{{.Pools.devices}}`, … | The item this worktree claimed from pool `devices` | `emulator:wet-myapp-1` |
| `{{.Handles.devices}}`, … | The address that item answers to right now | `emulator-5554` |

`Pools` vs `Handles`: the pool item is the durable name the lifecycle reasons
about; the handle is what a command must target now. For an emulator the item
is `emulator:<AVD_NAME>` and the handle is the live adb serial; for everything
else the handle equals the item. Use `{{.Handles.x}}` in commands, `{{.Pools.x}}`
when you mean the claim itself.

`env:` entries are also available as fields to `render:` templates and hooks,
so a derived value is written once and consumed everywhere.

## How to populate a fresh `.wet.yaml`

When a user says *"write a .wet.yaml for my project"*, follow this recipe:

### Step 1. Project name
Ask: **"What's the project name?"** Must match `[a-z0-9][a-z0-9_-]*`.

### Step 2. Files and directories to copy
Ask: **"What files/dirs are not in git but needed per-worktree?"** Probe: `.env*`, `.vscode/`, `.idea/`, `.claude/settings*.json`, `config/local.*`. Check `.gitignore` for candidates.

Split the answers in **three**:

- **Identical in every worktree** (`.claude/settings.json`, most of `.vscode/`) — `copy:` them.
- **Holds a port, hostname, device id, or secret** — a checked-in `.tpl` plus a
  `render:` target, never a copy. A copy carries the previous worktree's ports.
  Editor config is the subtle case: `.vscode/launch.json` *looks* static, but a
  debugger attaches to this slot's port. Don't render it — point it at the
  rendered `.env` (`--dart-define-from-file` / `envFile`) and it becomes static
  for real, so `copy:` or commit it. See below.
- **Genuinely machine-local** (a LAN IP, a personal API key) — `.env.local`,
  which is gitignored and overrides `env:` entries.

### Step 3. Ports
Ask: **"What ports does your project listen on?"** Probe: `docker-compose.yml` ports, `Dockerfile` EXPOSE, Go `:NNNN` listen strings, `package.json` scripts. Name each port `UPPER_SNAKE_CASE`.

### Step 4. Compose project name
If Docker Compose is used, add: `env: { COMPOSE_PROJECT_NAME: "{{.ProjectName}}-{{.BranchSafe}}" }`

### Step 5. Pools (only if needed)
Ask: **"Any scarce one-per-worktree resources?"** (device IDs, DB snapshots, test fixtures). Most projects skip this.

### Step 6. Hooks
Ask: **"What should happen on worktree create/destroy?"** Common: render env templates (`wet env populate`), start/stop Docker, run migrations, forward adb ports.

If you reach for `sed` to patch a port into a config file, check first whether that file could just be a template rendered by `wet env populate` — that's the same job without the in-place edit.

### Step 7. PR config
If the project uses GitHub, optionally add `pr: { base: main }`.

### Step 8. Gitignore the machine-local state files

wet writes two files at the **main repo root** that must never be committed:

- `.wet-worktrees.json` — the allocation state (slots, ports, pool claims,
  absolute worktree paths). Per-machine, not per-repo.
- `.wet-worktrees.lock` — the empty flock file guarding allocation. Pure
  scratch; it appears the first time anyone runs `wet worktree create` and
  otherwise just clutters `git status`.

Make sure the repo's `.gitignore` covers both. A single entry does it:

```gitignore
.wet-worktrees.*
```

If the `.gitignore` already lists `.wet-worktrees.json` on its own, add
`.wet-worktrees.lock` beside it rather than leaving the lock file untracked.
Also gitignore any per-worktree file the config generates at the repo root —
typically `.env.worktree`, plus `.env.local` and `.wet.local.yaml` if the
project uses those overlays.

## Common patterns

### Docker Compose with per-worktree ports

**IMPORTANT: env file layering.** Docker Compose uses `--env-file` for
compose-level variable interpolation (`${APP_PORT:-8080}` in the YAML).
`.env.worktree` has port overrides but NOT database credentials or secrets.
Those typically live in a separate `.env` file (e.g. `web/.env`, `backend/.env`,
or `.env` at the repo root). You MUST pass BOTH files so compose has access
to everything:

```yaml
hooks:
  post_create:
    # --env-file order matters: first file provides base vars (DB creds etc.),
    # second file overrides ports. Later values win on duplicate keys.
    - "docker compose -f web/docker-compose.yml --env-file web/.env --env-file .env.worktree -p {{.ProjectName}}-{{.BranchSafe}} up -d"
  pre_remove:
    - "docker compose -p {{.ProjectName}}-{{.BranchSafe}} down"
```

**When helping the user write this hook**, always ask: **"Where does your
docker-compose.yml live, and where are your DB credentials / secrets stored?"**
Common patterns:
- `web/.env` or `backend/.env` — credentials in a subdirectory alongside the compose file
- `.env` at the repo root — credentials at the top level
- `docker-compose.yml` uses `env_file:` inside the YAML for container env — this is SEPARATE from `--env-file` on the CLI. The container-level `env_file:` paths are relative to the compose file's directory and load independently.

If the project has NO separate credentials file (everything is in env vars or
hardcoded), a single `--env-file .env.worktree` suffices. But always check —
missing DB passwords is the #1 failure mode for worktree Docker boot.

In the docker-compose.yml itself, parameterize host ports with defaults:
```yaml
services:
  app:
    ports:
      - "${APP_PORT:-8080}:8080"
  db:
    ports:
      - "${DB_PORT:-5432}:5432"
```

### Point editor config at the rendered env file

The first move for `launch.json` and friends is **not** to render them. Ask
whether the tool can read an env file — most can — and if so, keep the editor
config plain and committed:

```jsonc
// .vscode/launch.json — identical in every worktree, so just commit it
{"configurations": [{
  "name": "app (dev)", "request": "launch", "type": "dart",
  "args": ["--dart-define-from-file", ".env"]   // VS Code node/python: "envFile"
}]}
```

`.env` is a `render:` target, so it already carries this worktree's ports.
launch.json then holds nothing that varies, and needs no template, no
gitignore juggling, and no `--assume-unchanged`.

Tell the user to leave host, port, and device **out** of the JSON. A pinned
port is right in exactly one worktree; a committed device id points at
hardware their colleagues don't have. Device selection belongs in the editor's
device picker or a `just` recipe that reads the worktree's allocation.

### Render a config file the tool can't parameterize

Only when the value must sit inside the JSON and no env-file indirection
exists:

```yaml
render:
  - template: config/thing.json.tpl
    out: config/thing.json
```

Two preconditions to check with the user:

1. **The output must be gitignored.** `wet setup` renders in the main checkout
   too (slot 0), so a *tracked* output is overwritten and left dirty on every
   run.
2. **The template must reach the worktree.** Templates are read from the
   worktree. Watch the `.gitignore` trap: git cannot re-include a file under an
   excluded directory, so `.vscode/` + `!.vscode/*.tpl` silently leaves the
   template ignored — it needs `.vscode/*` + `!.vscode/*.tpl`, or keep the
   directory in `copy: dirs:` and accept an untracked template.

Missing parent directories of `out:` are created. (Not for a bare
`wet env populate -o <path>`: that path is unvalidated user input, so it fails
fast rather than building directory trees from a typo.)

### Patching a tracked file with sed (last resort)

Only when the file cannot be generated from a template — e.g. it is huge,
hand-maintained, and only one line varies. Patching a tracked file makes it
show as modified, and users will commit per-worktree ports back to the shared
branch, so it needs `git update-index --assume-unchanged` as the last step:

```yaml
hooks:
  post_create:
    - sed -i.bak 's/"port": 40000/"port": {{.DEBUG_PORT}}/' .vscode/launch.json
    - rm -f .vscode/launch.json.bak
    - git update-index --assume-unchanged .vscode/launch.json
```

Be honest with the user about the cost: `--assume-unchanged` is permanent and
indiscriminate. It hides *every* later change to that file, including edits
they make deliberately and expect to commit. `.gitignore` and
`.git/info/exclude` are no help — both only apply to untracked files. Undo with
`git update-index --no-assume-unchanged <file>`.

**If you find yourself writing a sed hook, first ask whether the file could be
rendered instead.** Usually it can, and then none of this applies.

### Create a PR from a worktree
`wet pr create` pushes the branch and opens a GitHub PR. Title auto-generated from branch name. Flags: `--draft`, `--fill`, `-t "title"`, `-b base`.

## Env templates (`wet env populate`)

Prefer this over listing a generated `.env` in `copy:`. A copy snapshots one
machine's values and then drifts; a checked-in template re-renders with each
worktree's own ports.

```
wet env populate config/.env.tpl -o config/.env
wet env populate config/.env.tpl --dry-run      # print, don't write
```

The template sees every field in the template-fields table above, plus every
rendered `env:` entry:

```
port={{.GRPC_PORT}}
webBaseUrl=http://localhost:{{.APP_PORT}}
COMPOSE_PROJECT_NAME={{.COMPOSE_PROJECT_NAME}}
```

**Secrets arrive through `--secrets-from`.** wet talks to no secret store —
hand it a flat JSON object from whatever holds yours:

```bash
./scripts/wet-secrets.sh \
  | wet env populate config/.env.tpl -o config/.env --secrets-from -

wet env populate config/.env.tpl -o config/.env --secrets-from ./secrets.json
```

**Don't hand-write the keys — scaffold them.** `wet secrets setup` (no args
for a picker, or `wet secrets setup google apple`) writes a provider's `env:`
defaults and its `secrets:` entries into `.wet.yaml`. `wet secrets groups`
lists what wet knows. Stored keys are written empty on purpose, so a fresh
clone renders without a vault; derived values like a redirect URI are computed
per worktree and never stored. Re-running is safe: existing values are left
alone. Reach for it before writing any of the block below by hand.

**For 1Password, don't write a producer at all — declare it.** A `secrets:`
block in `.wet.yaml` gives wet the vault and the key map, and two commands move
values across:

```yaml
render:
  - template: backend/.env.tpl
    out: backend/.env
    secrets: backend
  - template: mobile/.env.tpl
    out: mobile/.env
    secrets: mobile

secrets:
  store: op
  vault: fantasy_basketball_development
  item: fantasy
  sections:
    backend:
      SECRET_KEY_BASE: secret_key_base
      GOOGLE_CLIENT_ID: google_client_id
    mobile:
      GOOGLE_CLIENT_ID: google_client_id
      APPLE_KEY_ID: apple_key_id
```

```bash
wet op vault seed --input .env.development    # env file -> vault
wet op vault read --output .env.development   # vault -> one file per section
```

Rules worth stating to a user before writing this block:

- **Sections name consumers, never storage.** `backend` and `mobile` are labels
  a render target claims with `secrets:`. Two sections may map the same field —
  one credential, two readers — and the vault stays free of per-consumer
  duplicates. Never suggest `backend_google_client_id` style field names; the
  section already carries that meaning, in the config where it belongs.
- **No paths in the secrets block.** `read` writes into the directories of the
  render targets that name the section. A section that no target claims has
  nowhere to go, and wet says so.
- **`seed` never overwrites** a field holding a different value without
  `--force`, so re-seeding a stale file cannot undo a rotation.
- **`vault` / `account` may be omitted** — wet asks and remembers the answer in
  the gitignored `.wet.local.yaml`. Do not put an account in `.wet.yaml` unless
  the whole team signs into the same one.
- A value is a field on `item`, `"Other Item/field"`, or a full
  `"op://vault/item/field"`.

For any other store, `--secrets-from` is unchanged and is the answer: any
command emitting a flat JSON object works, and wet talks to nothing itself.

Repeatable, later sources winning on a collision — so a vault and a running
container compose without a shell-side merge:

```bash
docker compose config --format json | jq '.services.db.environment' > /tmp/db.json
./scripts/wet-secrets.sh \
  | wet env populate config/.env.tpl -o config/.env \
      --secrets-from /tmp/db.json --secrets-from -
```

Prefer this over `{ a; b; } | jq -s add`: precedence stays visible, and a
source that fails to read aborts the render instead of silently dropping the
keys it owed.

- The source is always explicit; wet never sniffs stdin. A hook inherits its
  parent's stdin, so guessing would block forever on an empty inherited pipe.
  With no `--secrets-from`, the template renders from wet's values alone.
- Scalars only — a nested object or array is rejected by key name.
- Piped keys beat `env:` entries, but overriding a declared port or a reserved
  field is an error (it would defeat slot allocation).
- An unresolved `{{.KEY}}` fails the render; it never becomes an empty value.
- Output is written `0600`.
- Rendered outputs are wet's to own everywhere, main included — re-rendering
  is a repair. Machine-specific values go in `.env.local` / `.wet.local.yaml`,
  never hand-edited into the generated file.

Inside a `post_create` hook it reads `$SLOT` / `$BRANCH` / `$APP_PORT` from the
hook environment, because hooks run *before* `.wet-worktrees.json` is written.
Run by hand afterwards, it reads the state file. Same values either way.

## `wet setup` — one bootstrap for main and worktrees

`wet setup` configures whichever checkout it is run in. A worktree gets slot
N; the main repo gets slot 0 — the declared `base:` ports, its own pool claim,
its own rendered env files — skipping only `copy:` (no source to copy from).
This is why a fresh clone and a fresh worktree bootstrap with the same command,
and why a main checkout whose emulator was deleted re-claims one by re-running
setup instead of being hand-fixed.

Destructive lifecycle stays worktree-only: `pre_remove` and `wet worktree pop`
never run against main. When writing a `.wet.yaml`, assume every `post_create`
hook will also run in the main checkout, and keep anything irreversible out of
it.

## Monorepos — derive once, render many

Several packages, several env files, all descending from the same ports. Do
NOT give each template its own copy of the derivation — they drift. Derive in
`env:` (entries may reference each other, resolved to a fixed point, so order
doesn't matter), then list one `render:` target per package:

```yaml
env:
  DEV_HOST:      "localhost"
  AUTH_BASE_URL: "http://{{.DEV_HOST}}:{{.PHX_PORT}}"
  CALLBACK_URL:  "{{.AUTH_BASE_URL}}/callback"

render:
  - template: web/.env.tpl
    out:      web/.env.dev
    secrets_from: [/tmp/vault.json]
  - template: mobile/.env.tpl
    out:      mobile/.env
```

- `wet setup` renders every target before hooks run; `wet env populate --all`
  (add `--dry-run`) does it on demand.
- Each target renders against its own data copy — one target's secrets cannot
  leak into another's output.
- Two targets writing the same `out` is rejected at load time.
- A gitignored `.env.local` at the repo root (`KEY=value`) overrides `env:`
  entries per machine, and the override propagates into everything derived
  from it. Put a LAN IP for physical-device testing there, never in
  `.wet.yaml`.

When probing a project, look for the same fact being rebuilt in several
places — a compose `environment:` block, a Justfile shell snippet, a
hand-maintained `.env` — and collapse them into one `env:` entry.

## `.wet.local.yaml` — machine-local overrides

A gitignored overlay beside `.wet.yaml`. Optional file, optional keys: it names
only what it changes, everything else falls through.

Put anything machine-specific there — device IDs above all, since a committed
`pools:` list is what makes a `.wet.yaml` unshareable:

```yaml
pools:
  devices: [RFCR921L4DP, "emulator:wet-myapp-1"]
pool_grow:
  devices: android-emulator
```

- Any top-level key is overridable (`port_stride`, `ports`, `hooks`, …) — the
  merge happens in the loader, so nothing downstream knows the file exists.
- Maps merge key by key; **sequences replace wholesale** (a local pool is your
  list, not yours appended to someone else's).
- The merged result is validated as a whole; errors name the local file.
- `pool_grow` records new AVDs here, never in the committed config.

**When writing a `.wet.yaml`, keep `pools:` out of it** unless every item is
portable, and tell the user to put their device IDs in `.wet.local.yaml`.
Emulator items are always `emulator:<AVD_NAME>`, never an `emulator-NNNN`
serial — the serial is transient and unbootable once the emulator stops.

Also: `pool_grow: android-emulator` grows AVDs, so it belongs with the
machine-local pool, and a pool mixing physical devices with emulators is fine
— growth ignores non-emulator items.

## Validation rules

- `project_name`: `[a-z0-9][a-z0-9_-]*`
- Port `name`: `[A-Z_][A-Z0-9_]*`, must not collide with reserved fields (`ProjectName`, `Branch`, `BranchSafe`, `Slot`, `Pools`, `Handles`)
- Pool `name`: `[a-z_][a-z0-9_]*`, same reserved-name check
- Copy paths: no `..`, no leading `/`
- `env:` and hook strings: parsed as Go templates at load time (syntax errors fail early)
- Slot 0 reserved for main; worktrees start at 1

## Hook environment quick-ref

Hooks get every value twice: as a template field expanded before `sh -c` runs,
and as a real environment variable the command (or a script it calls) can read.

| Template | Shell env var | Example |
|---|---|---|
| `{{.ProjectName}}` | `$PROJECT_NAME` | `myapp` |
| `{{.Branch}}` | `$BRANCH` | `feature/foo` |
| `{{.BranchSafe}}` | `$BRANCH_SAFE` | `feature-foo` |
| `{{.Slot}}` | `$SLOT` | `1` |
| `{{.APP_PORT}}` | `$APP_PORT` | `8090` |
| `{{.Pools.devices}}` | `$POOL_DEVICES` | `emulator:wet-myapp-1` |
| `{{.Handles.devices}}` | `$POOL_HANDLE_DEVICES` | `emulator-5554` |
| every `env:` key | `$<KEY>` | `$COMPOSE_PROJECT_NAME` |

Pool names uppercase into the var name: pool `test_fixtures` becomes
`$POOL_TEST_FIXTURES` / `$POOL_HANDLE_TEST_FIXTURES`.

## Debugging

- **Hook failure on create**: recovery block printed with worktree path, failing hook, and exact command. Worktree left on disk, not recorded in state. Fix cause → `git worktree remove --force` → `git branch -D` → retry.
- **Hook failure on pop**: worktree NOT removed. Fix and retry.
- **Port collision**: bump the `base:` or use per-port `stride:` override.

---

# Team guide

Use this when the user wants to boot the dev stack and spawn an agent team. Works from the main repo or from an wet worktree.

## Step 0 — Detect context

```bash
REPO_ROOT=$(git rev-parse --show-toplevel 2>/dev/null)
if [ -f "$REPO_ROOT/.env.worktree" ]; then
  echo "=== WORKTREE MODE ==="
  echo "WORKTREE_ROOT=$REPO_ROOT"
  cat "$REPO_ROOT/.env.worktree"
else
  echo "=== MAIN MODE ==="
fi
# What agents are available?
ls "$REPO_ROOT"/*/.claude/agents/*.md "$REPO_ROOT"/.claude/agents/*.md 2>/dev/null || true
```

**Worktree mode** (`.env.worktree` exists): source it for `APP_PORT`, `DB_PORT`, `COMPOSE_PROJECT_NAME`, `FLUTTER_DEVICE_ID`, etc. **CRITICAL: all commands and all spawned teammates must use `$REPO_ROOT` (the worktree root), never the main repo path.** The worktree is a full copy — `mobile/`, `web/`, etc. all exist inside it.

**Main mode**: use defaults from `.wet.yaml` port bases, or fall back to common defaults (8080, 5432).

## Step 1 — Discover what this project has

Not every project has a backend, a mobile app, or Docker. Detect what exists before doing anything:

```bash
# Backend: look for docker-compose or a Go/Node/Python server
ls "$REPO_ROOT"/**/docker-compose.yml "$REPO_ROOT"/docker-compose.yml 2>/dev/null || true
# Mobile: look for a Flutter project
ls "$REPO_ROOT"/**/pubspec.yaml 2>/dev/null || true
# Running services
docker compose ps --format json 2>/dev/null || true
pgrep -fl 'flutter run' 2>&1 || true
which flutter >/dev/null 2>&1 && flutter devices --machine 2>/dev/null || true
```

Only include components that actually exist in the pre-flight report:

```
Team pre-flight (<worktree: branch slot N  OR  main>)
  backend     ✓ up (:$APP_PORT)   or   ✗ down          or   — (no backend found)
  mobile      ✓ running           or   ✗ not running   or   — (no Flutter project)
  devices     <list>              or   n/a
```

## Step 2 — Boot what's missing

Only boot components that exist in this project. Skip sections that don't apply.

**Backend** — only if a docker-compose.yml exists:
- Worktree: find the compose file AND the credentials env file (e.g. `web/.env`, `backend/.env`, or `.env`). Pass both: `docker compose -f <compose-file> --env-file <creds-env> --env-file .env.worktree -p $COMPOSE_PROJECT_NAME up -d`. The creds file provides DB passwords etc., `.env.worktree` overrides the ports. **Missing this is the #1 failure mode** — always check for a separate `.env` file near the compose file.
- Main: `cd <compose-dir> && docker compose up -d` (auto-loads the local `.env`).
- Poll port ~15s. Never `down -v`, `prune`, `rmi`, or `--build` unless asked.

**Mobile** — only if a `pubspec.yaml` exists. **Always launch the app** — don't wait to be asked:
- Use the device assigned to this worktree (`$FLUTTER_DEVICE_ID` from `.env.worktree`, or the pool-allocated device ID). Never prompt for device selection — the pool handles it.
- Android: `adb reverse tcp:$APP_PORT tcp:$APP_PORT` first.
- Launch with `flutter run -d $FLUTTER_DEVICE_ID` in background (`run_in_background: true`).
- Confirm the app is running on the device before proceeding to Step 3.

## Step 3 — Discover and spawn teammates

Scan for agent definitions:
```bash
ls "$REPO_ROOT"/*/.claude/agents/*.md "$REPO_ROOT"/.claude/agents/*.md 2>/dev/null
```

Spawn one teammate per agent definition found. **Include the worktree root path (`$REPO_ROOT`) in each spawn prompt** so teammates work in the worktree, not the main repo. Include worktree context (ports, device, status) in each spawn prompt. Each teammate reads its own CLAUDE.md, confirms in one sentence, then stands by.

**Rules:**
- Don't invent teammates beyond what agent definitions exist.
- Don't inline agent definitions.
- One team per session.
- **Do not auto-start work.** Wait for the user.

## Step 4 — Report

Only include lines for components that exist. A mobile-only project should NOT show a backend line:

```
Team — ready
  context     <worktree: branch slot N  OR  main repo>
  ✓ backend   <status>   :$APP_PORT              ← only if backend exists
  ✓ mobile    <status>   device: $DEVICE_ID       ← only if Flutter project exists
  ✓ team      N teammates: <names>
  > next      Tell the lead what to work on
```

## Guardrails

- **Never** destroy state (no `-v`, no `prune`, no `--build`).
- **Never** silently pick a device when multiple are connected (unless worktree pre-assigned one).
- **Never** start Flutter in the foreground.
- **Never** loop on boot failures — show logs and stop.
- **Never** duplicate agent definitions inline.
- **Never** run commands or edit files in the main repo when in worktree mode. All paths must be relative to `$REPO_ROOT` (the worktree root).

---

# Further reading

- `wet.example.yaml` — beside this file; the annotated schema of record.
- `wet op vault --help` — the 1Password path, driven by the `secrets:` block in
  `.wet.yaml` rather than a per-repo `op` pipeline.
- `wet --help`, `wet worktree --help`, `wet env --help` — the installed CLI's
  own reference, always matching the installed version.
