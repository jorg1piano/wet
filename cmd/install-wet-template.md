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

A `.wet.yaml` has eight top-level keys, all optional except `project_name`:

- `project_name` — required. Docker-compose-safe name for generated compose project names.
- `copy` — files and directories copied from the main repo into each new worktree.
- `port_stride` — default port stride (how far ports advance per slot). Defaults to `10`.
- `ports` — named port declarations. Each port gets `base + slot * stride`.
- `pools` — named lists of opaque strings round-robined across worktrees.
- `pool_grow` — optional per-pool auto-expand strategy. Currently supports `android-emulator`: when a pool is exhausted, creates a new AVD, cold boots it, and appends the new serial to `.wet.yaml`.
- `env` — Go `text/template` strings rendered into `.env.worktree`.
- `hooks` — shell commands run around worktree lifecycle events (`post_create`, `pre_remove`).
- `pr` — configures `wet pr create` behavior (base branch, body template).

Full reference: `cli/README.md` in the wet repo.

## The schema in full

```yaml
# .wet.yaml — paste at the repo root, edit per your project.

project_name: myapp         # required; must match [a-z0-9][a-z0-9_-]*.

copy:                       # optional. Missing files silently skipped.
  files:                    # prefer an env template over copying a generated
    - config/local.toml     # .env — see "Env templates" below.
  dirs:
    - .vscode
    - .idea

port_stride: 10             # default stride. Slot N → base + N*stride.

ports:                      # named port declarations.
  - name: APP_PORT          # must match [A-Z_][A-Z0-9_]*.
    base: 8080
  - name: DB_PORT
    base: 5432
  - name: DEBUG_PORT
    base: 40000
    stride: 5               # optional per-port override

pools:                      # optional; round-robin allocated.
  devices:
    - emulator-5554
    - emulator-5556
  fixtures:
    - fresh-snapshot-a
    - fresh-snapshot-b

pool_grow:                  # optional; auto-expand strategy per pool.
  devices: android-emulator # creates a new AVD + cold boots when exhausted

env:                        # KEY=value → .env.worktree
  COMPOSE_PROJECT_NAME: "{{.ProjectName}}-{{.BranchSafe}}"

hooks:
  post_create:
    - echo "created {{.Branch}} at slot {{.Slot}}"
    - wet env populate config/.env.tpl -o config/.env   # see "Env templates"
    - ./scripts/setup.sh
  pre_remove:
    - docker compose -p {{.ProjectName}}-{{.BranchSafe}} down

pr:                         # optional; configures `wet pr create`.
  base: main
  body_template: |
    Worktree slot {{.Slot}}
```

## Template fields reference

| Key | Value | Example |
|---|---|---|
| `{{.ProjectName}}` | `project_name` from config | `myapp` |
| `{{.Branch}}` | Raw git branch name | `feature/foo` |
| `{{.BranchSafe}}` | Docker-Compose-sanitized branch | `feature-foo` |
| `{{.Slot}}` | Allocated slot (1, 2, …) | `1` |
| `{{.APP_PORT}}`, … | One key per declared port | `8090` |
| `{{.Pools.devices}}`, … | One key per declared pool | `emulator-5554` |

## How to populate a fresh `.wet.yaml`

When a user says *"write a .wet.yaml for my project"*, follow this recipe:

### Step 1. Project name
Ask: **"What's the project name?"** Must match `[a-z0-9][a-z0-9_-]*`.

### Step 2. Files and directories to copy
Ask: **"What files/dirs are not in git but needed per-worktree?"** Probe: `.env*`, `.vscode/`, `.idea/`, `.claude/settings*.json`, `config/local.*`. Check `.gitignore` for candidates.

Split the answers in two. Editor and tool settings (`.vscode/`, `.claude/`) are identical in every worktree — `copy:` them. Anything holding a port, a hostname, or a secret differs per worktree — make it a checked-in `.env.tpl` rendered by a `wet env populate` hook instead, or the copy will carry the previous worktree's ports.

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

### Patch a config file with sed
```yaml
hooks:
  post_create:
    - sed -i.bak 's/"port": 40000/"port": {{.DEBUG_PORT}}/' .vscode/launch.json
    - rm -f .vscode/launch.json.bak
    # IMPORTANT: hide patched tracked files from git status in this worktree
    - git update-index --assume-unchanged .vscode/launch.json
```

**IMPORTANT: hiding patched tracked files from git.** When a `post_create`
hook patches a **tracked** file (like `.vscode/launch.json`), the worktree's
`git status` will show it as modified. Users will accidentally commit the
per-worktree port values back to the shared branch. Fix this with
`git update-index --assume-unchanged <file>` as the last hook step.

Key points:
- `.gitignore` does NOT work for tracked files — only for untracked ones.
- `.git/info/exclude` also only works for untracked files.
- `--assume-unchanged` tells git to stop checking the file in this worktree's
  index. It's per-worktree, so it doesn't affect the main repo.
- To undo (e.g., if the user intentionally wants to commit a port change):
  `git update-index --no-assume-unchanged <file>`

**When helping the user write sed hooks**, always add a matching
`git update-index --assume-unchanged` line for every tracked file that
gets patched. Group them into one command:
```yaml
    - git update-index --assume-unchanged web/.vscode/launch.json mobile/.vscode/launch.json
```

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

The template sees every field in the table above, plus every rendered `env:`
entry:

```
port={{.GRPC_PORT}}
webBaseUrl=http://localhost:{{.APP_PORT}}
COMPOSE_PROJECT_NAME={{.COMPOSE_PROJECT_NAME}}
```

**Secrets arrive through `--secrets-from`.** wet talks to no secret store —
hand it a flat JSON object from whatever holds yours:

```bash
op item get myapp --format json | jq 'from_entries' \
  | wet env populate config/.env.tpl -o config/.env --secrets-from -

wet env populate config/.env.tpl -o config/.env --secrets-from ./secrets.json
```

Repeatable, later sources winning on a collision — so a vault and a running
container compose without a shell-side merge:

```bash
docker compose config --format json | jq '.services.db.environment' > /tmp/db.json
op item get myapp --format json | jq 'from_entries' \
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
- Port `name`: `[A-Z_][A-Z0-9_]*`, must not collide with reserved fields (`ProjectName`, `Branch`, `BranchSafe`, `Slot`, `Pools`)
- Pool `name`: `[a-z_][a-z0-9_]*`, same reserved-name check
- Copy paths: no `..`, no leading `/`
- `env:` and hook strings: parsed as Go templates at load time (syntax errors fail early)
- Slot 0 reserved for main; worktrees start at 1

## Hook environment quick-ref

| Template | Shell env var | Example |
|---|---|---|
| `{{.ProjectName}}` | `$PROJECT_NAME` | `myapp` |
| `{{.Branch}}` | `$BRANCH` | `feature/foo` |
| `{{.BranchSafe}}` | `$BRANCH_SAFE` | `feature-foo` |
| `{{.Slot}}` | `$SLOT` | `1` |
| `{{.APP_PORT}}` | `$APP_PORT` | `8090` |
| `{{.Pools.devices}}` | `$POOL_DEVICES` | `emulator-5554` |

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

Full CLI + schema reference: `cli/README.md` in the wet repo.
