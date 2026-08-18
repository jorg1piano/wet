# wet — worktree dev framework

A tiny, repo-agnostic CLI for running several git worktrees in parallel, each
with its own ports, env file, and optional lifecycle hooks. The CLI contains
**no** project-specific knowledge — everything is driven by a `.wet.yaml` at
your repo root.

Core commands:

```
wet setup                       # configure this checkout — main repo OR worktree
wet worktree create [branch]    # minimal: `git worktree add` + copy .env + open tmux window
wet worktree mount <branch>     # same, for a branch that already exists (local or on origin)
wet worktree setup              # worktree-only form of `wet setup`
wet worktree list               # interactive list — pick one to open in tmux or VS Code (alias: ls)
wet worktree pop                # remove the current worktree (run pre_remove hooks, tear down)
wet env populate <tpl> -o <out> # render a template with this checkout's values
wet pr create                   # push branch + open a GitHub PR (via gh)
wet install wet-skill           # install the Claude Code helper skill
```

`create` is intentionally minimal — it only forks the worktree and copies
`.env` (if present). Everything else (`.wet.yaml` parsing, port allocation,
`.env.worktree` rendering, hooks, state-file bookkeeping) happens in `setup`,
which you run from inside the new worktree, because the hooks in `.wet.yaml`
typically assume they're running in the worktree's shell.

`mount` is `create` for a branch that already exists: it never creates a
branch, and if only `origin/<branch>` is there it fetches and sets up a local
tracking branch first. Everything after the checkout is identical — copy
`.env`, open a tmux window, then `wet worktree setup` inside. Both take
`--no-tmux` when something else is managing sessions.

## Quick start

A fully annotated config lives at
[`cmd/wet.example.yaml`](cmd/wet.example.yaml) in this repo — and
`wet install wet-skill` drops the same file into *your* repo at
`.claude/skills/wet/wet.example.yaml`, which is the copy to reach for once wet
is installed. Copy it to `.wet.yaml` at your repo root and delete what you
don't need. The short version:

```yaml
project_name: myapp

copy:
  files:
    - .env
  dirs:
    - .vscode

port_stride: 10

ports:
  - { name: APP_PORT, base: 8080 }

env:
  COMPOSE_PROJECT_NAME: "{{.ProjectName}}-{{.BranchSafe}}"
```

Then from inside the repo:

```
wet worktree create feature/new-thing
cd ../worktree-feature/new-thing   # or let tmux open the new window for you
wet worktree setup
```

`create` forks `../worktree-feature/new-thing/` and copies `.env` into it.
`setup` then reads `.wet.yaml`, picks the lowest free port slot (1 on a fresh
install), copies the configured files and dirs, and writes a `.env.worktree`
holding `APP_PORT=8090` — slot N gets `base + N*port_stride`, so slot 1 of a
`base: 8080` port with `port_stride: 10` is 8090 — and
`COMPOSE_PROJECT_NAME=myapp-feature-new-thing`. It is idempotent — re-running it
on a tracked worktree updates the entry in place rather than appending a
duplicate.

To clean up, `cd` into the worktree and run `wet worktree pop`.

### Installing the Claude helper skill

If you use [Claude Code](https://claude.com/claude-code) and want in-editor
help writing or debugging `.wet.yaml`, install the bundled skill:

```
wet install wet-skill
```

It writes an embedded Claude Code skill to `.claude/skills/wet/SKILL.md` at your
repo root, which teaches Claude how to probe your project (env files, Docker
Compose services, ports, lifecycle needs) and produce a `.wet.yaml` that matches
it, and lets it answer schema questions and debug hook failures without leaving
the editor. Re-run with `--force` to overwrite. Both files behave the same in
every worktree and every clone, so they're worth checking into git.

It touches a few more things in your repo, so it's worth knowing before you run
it:

- `.claude/skills/wet/wet.example.yaml` — the annotated schema, alongside the
  skill, so Claude (and you) have it locally.
- `wet.op.json` and `scripts/wet-secrets*.sh` — the 1Password secrets kit,
  written into the project rather than the skill directory because you edit and
  commit them. Delete the ones you don't want; nothing else depends on them.
- `.gitignore` — appends `.wet-worktrees.json`, `.wet-worktrees.lock` and
  `.env.worktree` if they aren't already mentioned, and prints what it added.
  An existing file is appended to, never rewritten.
- `.claude/settings.json` — sets `env.CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1`,
  merging into the existing settings and leaving every other key alone. Already
  set is a no-op.

The skill's source lives at `cmd/install-wet-template.md` in the `wet` repo and
is embedded into the binary via `//go:embed`.

## Schema reference

The single annotated schema is [`cmd/wet.example.yaml`](cmd/wet.example.yaml) —
every key, with its defaults and validation rules, in one file. In your own repo
it's the copy `wet install wet-skill` leaves at
`.claude/skills/wet/wet.example.yaml`. Two things it
can't tell you, because they're about how the file is found rather than what's
in it:

- `.wet.yaml` is loaded from **the checkout being configured**, falling back to
  the main repo root. That's what makes a `.wet.yaml` change testable in the
  worktree where it's written.
- A missing config is a hard error, not a default — see
  [No config, no setup](#no-config-no-setup).

### Template fields

Both `env:` values and hook commands are Go `text/template` strings. The
following keys are injected automatically for every render:

| Key | Value | Example |
|---|---|---|
| `{{.ProjectName}}` | `project_name` from the config | `myapp` |
| `{{.Branch}}` | Raw git branch name | `feature/foo` |
| `{{.BranchSafe}}` | Docker-Compose-sanitized branch name | `feature-foo` |
| `{{.Slot}}` | Allocated slot index (1, 2, …) | `1` |
| `{{.APP_PORT}}`, `{{.DB_PORT}}`, … | One key per declared port with its resolved value | `8090` |
| `{{.Pools.devices}}`, `{{.Pools.fixtures}}`, … | Allocated item per declared pool | `emulator:pixel-a` |
| `{{.Handles.devices}}`, … | The address that item answers to *now* | `emulator-5554` |

Templates run with `missingkey=error`, so a typo fails at render rather than
producing an empty value. Branch sanitization follows
[Docker Compose's project-name rules](https://docs.docker.com/compose/how-tos/project-name/):
lowercased, non-`[a-z0-9_-]` replaced with `-`, collapsed dashes, trimmed.
Empty result falls back to `branch`.

### Hook environment

Hook commands run via `sh -c` with:

- **cwd** = the worktree root
- **stdin / stdout / stderr** = inherited from `wet` (so interactive hooks work)
- **env** = the caller's environment, plus:

| Var | Same as |
|---|---|
| `PROJECT_NAME` | `{{.ProjectName}}` |
| `BRANCH` | `{{.Branch}}` |
| `BRANCH_SAFE` | `{{.BranchSafe}}` |
| `SLOT` | `{{.Slot}}` |
| `APP_PORT`, `DB_PORT`, … | One per declared port |
| `POOL_DEVICES`, `POOL_FIXTURES`, … | One per declared pool (`POOL_<NAME>` in upper snake case) |
| `POOL_HANDLE_DEVICES`, … | The resolved handle for each pool |
| `COMPOSE_PROJECT_NAME`, … | One per `env:` entry, rendered, with `.env.local` overrides applied |

So hook commands can use either template interpolation (expanded before `sh`
runs) or shell interpolation (expanded by `sh`):

```yaml
hooks:
  post_create:
    # template: {{.APP_PORT}} becomes "8090" before sh sees it
    - curl http://localhost:{{.APP_PORT}}/health
    # shell: $APP_PORT is resolved by sh at run time
    - echo "worktree on $APP_PORT"
```

A nonzero exit aborts the current operation — subsequent hooks do **not**
run, and the caller bails out with the failing command printed. Pre-remove
hook failures abort the pop entirely (the worktree stays on disk).

Because that abort depends on the hook's exit code, `wet` warns when a
`post_create` hook contains `||`, a pipe, or a trailing `; true` — all three can
turn a failure into exit 0. The warning is advisory, and `pre_remove` is exempt;
see [Why hooks may not swallow a failure](docs/design.md#why-hooks-may-not-swallow-a-failure).

### Pools

`pools:` declares named lists of opaque strings that `wet` round-robins across
live worktrees. Use pools for any scarce, one-per-worktree resource where the
item identity matters but the mapping to worktrees doesn't — device IDs,
preloaded database snapshots, test-fixture directories that must not be shared.

**Allocation rule.** Pools are claimed in `wet setup`, not in `wet worktree
create` — like ports and slots, and under the same lock. Each declared pool
picks the lowest-indexed item not currently claimed by another worktree, so
released items are reclaimed before trailing ones. Claims release implicitly
when the owning worktree is popped; there is no explicit release step.

Because allocation happens in `setup`, the worktree and its branch are already
on disk when a pool comes up empty — an exhausted pool fails the setup, not the
checkout, and no state entry is recorded for the tree. Recover by freeing an
item (`wet worktree pop` another worktree) or adding one, then re-run `wet
worktree setup`; or `wet worktree pop` the new tree itself.

**Exhaustion.** When every item is claimed, `wet setup` fails with a message
listing each claim by branch and item. Grow the pool in `.wet.yaml` (or
`.wet.local.yaml`), or configure `pool_grow:` if wet can create the resource
itself.

**`pool_grow:`** maps a pool name to a strategy wet runs instead of failing on
exhaustion. `android-emulator` is the only strategy — it creates a new AVD,
boots it, records it as `emulator:<AVD_NAME>` in `.wet.local.yaml`, and hands it
back as the claim. Any other value is not a strategy wet knows, and the pool
just reports as exhausted.

```yaml
pool_grow:
  devices: android-emulator
```

**Template and hook access.** Pool picks are exposed as `{{.Pools.<name>}}`,
and as `POOL_<NAME>` in upper snake case (`devices` → `POOL_DEVICES`,
`test_fixtures` → `POOL_TEST_FIXTURES`) for shell hooks:

```yaml
env:
  FLUTTER_DEVICE: "{{.Pools.devices}}"
hooks:
  post_create:
    - ./scripts/restore-snapshot.sh {{.Pools.fixtures}}
  pre_remove:
    - ./scripts/release-device.sh "$POOL_DEVICES"
```

**Name rules.** Pool names must match `[a-z_][a-z0-9_]*` and must not
collide with reserved template fields (`ProjectName`, `Branch`,
`BranchSafe`, `Slot`, `Pools`, `Handles`). Items within a single pool must be
non-empty and unique; items may repeat across pools.

## One bootstrap — `wet setup`

`wet setup` configures the checkout you are standing in, whichever that is:

```
git clone … && cd repo && wet setup    # main checkout → slot 0
wet worktree create feature/x
cd ../worktree-feature/x && wet setup  # worktree → slot 1, 2, …
```

Main runs the same pipeline as a worktree — it is a working checkout too
([why](docs/design.md#why-the-main-checkout-runs-the-same-pipeline)) — differing
only where a difference is forced:

- **Slot is pinned to 0**, so main gets the declared `base:` ports (`4000`, not
  `4010`) and no worktree can be handed them.
- **`copy:` is skipped** — source and destination are the same directory.
- **Pools are claimed normally.** Main holds an item and keeps it, and the
  pool-exhausted message names it as a claimant like any other.
- **Destructive lifecycle stays worktree-only.** `pre_remove` hooks and
  `wet worktree pop` never run against main.

`wet worktree setup` remains as the worktree-only form; run in the main repo it
points you at `wet setup`.

## Env templates — `wet env populate`

`wet env populate` replaces the copied-`.env` habit with a checked-in template
that re-renders per worktree
([why](docs/design.md#why-templates-beat-copying-a-generated-env)).

```
wet env populate config/.env.tpl -o config/.env
wet env populate config/.env.tpl --dry-run     # print instead of writing
```

The template is Go `text/template` and sees the same fields hooks do:
`ProjectName`, `Branch`, `BranchSafe`, `Slot`, every declared port, every
`.Pools.<name>`, and every rendered `env:` entry.

```
port={{.GRPC_PORT}}
webBaseUrl=http://localhost:{{.APP_PORT}}
COMPOSE_PROJECT_NAME={{.COMPOSE_PROJECT_NAME}}
```

### Secrets come in through `--secrets-from`

wet talks to no secret store. Hand it a flat JSON object and any command that
can produce one works — `op`, `az`, `docker compose config`, a fixture file:

```bash
./scripts/wet-secrets.sh \
  | wet env populate config/.env.tpl -o config/.env --secrets-from -

wet env populate config/.env.tpl -o config/.env --secrets-from ./secrets.json
```

Keep the producer in a committed script rather than inline in a hook, so it can
be run on its own (`./scripts/wet-secrets.sh | jq keys`) when a render fails.
`wet install skill` writes a working 1Password one straight into your project —
no copying out of the skill directory, since these are files you edit:

| file | role |
| --- | --- |
| `wet.op.json` | the keys and where their values live, in named groups |
| `scripts/wet-secrets.sh` | reads the map, prints `{"KEY": "value"}` |
| `scripts/wet-secrets-provision.sh` | reads the map, creates the fields in the vault |
| `scripts/secrets-map.sh` | the map loader both scripts source |

Keep what you use and delete the rest. Re-running `wet install skill` restores
a deleted file and leaves an edited one alone unless you pass `--force`.

The map is `wet.<store>.json` — the middle segment names the secret store, so a
repo that later adds Azure keeps `wet.az.json` beside `wet.op.json`, each read
by its own producer. A producer handed the wrong one refuses rather than
resolving references with the wrong CLI. It stays out of `.wet.yaml` on
purpose: wet has no vault code path, and keeping store coordinates outside its
config is what makes that true ([why](docs/design.md#why-secrets-stay-outside-wetyaml)).

```json
{
  "vault": "Development",
  "item": "myapp",
  "groups": {
    "google": {
      "GOOGLE_CLIENT_ID": "google_client_id",
      "GOOGLE_CLIENT_SECRET": "google_client_secret"
    }
  }
}
```

A value is a field on the default item, `"Other Item/field"` for another item
in the same vault, or a full `"op://vault/item/field"`. Edit the map, not the
scripts — it is the single source of truth, so a key cannot exist in the vault
but be missing from the render. `OP_VAULT` / `OP_ITEM` (flags or env) override
the file's defaults; name one or more groups to act on a subset
(`./scripts/wet-secrets.sh core google`).

The producer resolves each key with `op read` and assembles the JSON with
`jq --arg`, so a secret holding a quote or a newline still comes out valid. The
provisioner creates any field the map names and the vault lacks — empty, or
prompted for with `--fill` — and never overwrites a field that already has a
value, so first-time setup is a command instead of a click-through.

Prefer the pipe form: a `secrets_from:` path leaves a plaintext copy of every
secret on disk, where `--secrets-from -` keeps them in one process's stdin. Wrap
it in `set -o pipefail` — see
[Prefer a pipe over a file on disk](docs/design.md#prefer-a-pipe-over-a-file-on-disk).

`--secrets-from` is repeatable and **later sources win** on a key collision, so
a vault and a running container compose in one command. Prefer it over merging
with `jq -s add`, which hides a dead producer
([why](docs/design.md#why---secrets-from-twice-rather-than-jq--s-add)). Only one
source may be `-`; naming stdin twice is an error, since the second read would
find an exhausted pipe.

When a file is unavoidable, a `secrets_from:` path in the config is resolved
against **the checkout being configured** — it's a fact about the repo, so it
means the same thing whichever directory `wet setup` ran from. (A
`--secrets-from` flag is a fact about the invocation, so it stays relative to
your cwd.) `~` and `$VAR` expand, so a committed config can name a per-machine
location without hardcoding anyone's home directory:

```yaml
render:
  - template: web/.env.tpl
    out:      web/.env.dev
    secrets_from: ["~/.config/myapp/secrets.json"]   # one file per workstation
```

### Rules

- **The source is always explicit.** wet never sniffs stdin
  ([why](docs/design.md#why-wet-never-sniffs-stdin)); with no `--secrets-from`,
  the template renders from wet's own values alone.
- **Values must be scalars.** Strings, numbers, and booleans are stringified;
  a nested object or array is rejected by key name rather than silently
  serialized into the env file.
- **Piped keys override `env:` entries** — with a warning naming the key — but
  may *not* override a declared port or a reserved field, which is a hard error
  ([why](docs/design.md#why-a-store-key-may-override-env-but-not-a-port)).
- **An unresolved `{{.KEY}}` is fatal**, never an empty value.
- **Output is written `0600`**, since the whole point of the secrets channel
  is that the result may hold secrets.
- **Rendered outputs are wet's to own** — re-rendering is how you repair a
  checkout, so it never needs a flag, and a hand-edit to the output does not
  survive one. Per-machine values belong in `.env.local` or `.wet.local.yaml`
  ([why](docs/design.md#rendered-outputs-are-wets-to-own)).

### Where it runs

The usual place is a `post_create` hook, which runs through `sh -c` so a
pipeline works verbatim. Hooks run *before* `setup` persists
`.wet-worktrees.json`, so there populate reads its values from the hook
environment (`$SLOT`, `$BRANCH`, `$APP_PORT`, …) rather than from the state
file; run by hand afterwards it reads the state file entry for the current
worktree. Either way the values are the ones that worktree was allocated.

## Monorepos — derive once, render many

**`env:` is the derivation layer.** Entries may reference each other, resolved
to a fixed point rather than in sorted order, so a value is built once and every
package's template consumes the result instead of rebuilding it — see
[the failure mode this avoids](docs/design.md#monorepos--the-failure-mode-is-re-derivation).

```yaml
ports:
  - { name: PHX_PORT, base: 4000 }

env:
  DEV_HOST:           "localhost"
  AUTH_BASE_URL:      "http://{{.DEV_HOST}}:{{.PHX_PORT}}"
  VIPPS_REDIRECT_URI: "{{.AUTH_BASE_URL}}/hydra/login/vipps/callback"

render:
  - template: web_elixir/.env.tpl
    out:      web_elixir/.env.dev
    secrets_from: [/tmp/vault.json]
  - template: mobile_elixir/.env.tpl
    out:      mobile_elixir/.env
```

### `render:` targets

Targets are declared rather than written as hook lines, so the whole set is
inspectable
([why](docs/design.md#why-render-targets-are-declared-not-written-as-hook-lines)):

```
wet env populate --all             # render every target
wet env populate --all --dry-run   # print every file, write nothing
```

`wet setup` renders them too, **before** hooks — a hook that boots the stack
finds the files already written.

Each target carries its own `secrets_from`, because packages don't need the same
secrets, and **one target's secrets cannot leak into another's output.** Two
targets writing the same `out` is rejected at load time.

### `.env.local` — per-machine overrides

Some values vary by workstation, not by worktree — a LAN IP for physical-device
testing, a personal staging hostname. Those go in a gitignored `.env.local` at
the repo root:

```
DEV_HOST=192.168.1.9
```

Keys there **override `env:` entries**, which makes everything in `env:` a
default, and the override propagates into every value derived from it — every
package's env file follows `DEV_HOST` without a per-machine edit to `.wet.yaml`.
The same overrides reach `.env.worktree`, hooks, and `pre_remove`, so teardown
addresses the same stack setup brought up.

Format is the usual `KEY=value`, `#` comments and blanks ignored, quotes
stripped. A line without `=` is an error rather than a silent skip — a dropped
override surfaces much later as a mystery wrong hostname.

## `.wet.local.yaml` — machine-local overrides

Some of the config isn't a fact about the repo, it's a fact about a workstation
— a `pools:` list of device IDs is the clearest case. Drop a gitignored
`.wet.local.yaml` beside the `.wet.yaml` **in the checkout that has
`main_branch:` checked out** — not necessarily git's "main working tree", and
[the difference matters](docs/design.md#why-wetlocalyaml-exists-and-where-it-is-read-from).
wet reads the overlay (and `.env.local`, and the files `copy:` seeds a worktree
with) from there, and writes grown pool items back there.

```yaml
pools:
  devices:
    - RFCR921L4DP                 # my Galaxy
    - 00008120-0008054914EBC01E   # my iPhone
pool_grow:
  devices: android-emulator
```

**Everything is optional.** No file at all is the normal case. A file names
only the keys it wants to change; anything it doesn't mention falls through to
`.wet.yaml` untouched.

**It overrides any key, not a special subset.** The merge happens inside the
config loader, before anything is unmarshalled, so the rest of wet just reads
"the config". Override `port_stride`, a single hook, or `ports:` outright — it
all works the same way.

**Merge rules**, deliberately boring:

- **Maps merge key by key** — overriding one `env:` entry leaves the rest alone.
- **Sequences replace wholesale** — a local `pools.devices` is *your* device
  list, not yours appended to a teammate's. Appending would hand you items that
  don't exist on this machine.
- **The merged result is validated as a whole**, and a local file that breaks
  the schema fails with its own name in the error.
- `wet setup` prints which top-level keys the local file overrode, so a
  surprising value is traceable.

**Pool growth writes here, never to `.wet.yaml`** — a grown AVD exists only on
this machine. Because sequences replace, the local file gets the full merged
list.

## Items and handles

A pool item is a durable name; a handle is how you address it right now. For
everything except emulators they are the same string and you can ignore the
distinction. For an emulator they cannot be: the item is `emulator:<AVD_NAME>`
(an AVD survives reboots, so it is what gets recorded and what teardown reasons
about), the handle is `emulator-5554` (what `flutter run -d` and `adb -s`
actually take). `pool_grow: android-emulator` records items in the same form.

Three rules follow from that, and they are the whole contract for emulator
pools:

- **A pool item names an AVD, never a runtime serial.** `emulator-5554` is
  assigned by adb at boot and means nothing while the emulator is stopped
  ([why that poisons a pool](docs/design.md#dont-resolve-emulator-serials-in-a-hook)).
- **The `emulator:` prefix is what hooks test for** to decide whether an item
  needs booting at all — a physical device is either plugged in or not, an AVD
  has to be started. Write `case "$POOL_DEVICES" in emulator:*)` rather than
  guessing from the item's shape.
- **`pool_grow: android-emulator` asks adb for a free console port**, rather
  than deriving one from the item — the item carries no port, and an emulator
  someone started outside wet holds a port just as firmly as one wet booted.

`wet setup` boots any claimed emulator that isn't running and resolves the
serial before rendering anything, so `{{.Handles.devices}}` is a device you can
target and `$POOL_HANDLE_DEVICES` is the same value in hooks. Don't reimplement
this in a hook —
[the obvious shell version breaks](docs/design.md#dont-resolve-emulator-serials-in-a-hook)
as soon as a second device is attached.

## Running several worktrees at once

Slot, port and pool allocation happens under a lock (`.wet-worktrees.lock`,
beside the state file), and the claim is written before any hook runs. The lock
never spans the hooks — concurrent stack boots are the point
([background](docs/design.md#why-allocation-holds-a-lock)). Two consequences
worth knowing:

- A failed setup leaves its claim behind. That is deliberate: the tree still
  exists, re-running setup finds its own entry and keeps the same slot, and
  `wet worktree pop` releases it.
- Anything a hook allocates from a shared namespace — a docker subnet is the
  usual one — is outside the lock and races the same way. Have the hook ask
  what's free and retry on collision rather than deriving a value from the
  slot: slots are recycled when a worktree is popped, and whatever the old one
  created is not.

## Pull requests

`wet pr create` pushes the current branch and opens a PR via the GitHub CLI
(`gh`). It's designed for the worktree workflow: finish work in a worktree,
run `wet pr create`, then `wet worktree pop` once merged.

```
wet pr create              # push + create PR with auto-generated title
wet pr create --draft      # create as draft
wet pr create --fill       # auto-fill title + body from commits (gh --fill)
wet pr create -t "My PR"   # explicit title
wet pr create -b develop   # override base branch

wet pr open                # open this branch's existing PR in the browser
```

**Base branch** resolves in order: `--base` flag → `.wet.yaml` `pr.base` →
`main_branch:` → `"main"`.

**PR body** — when run from a tracked worktree, the body auto-includes the slot
index, allocated ports, and pool items. Override with `pr.body_template` in
`.wet.yaml`; it sees `ProjectName`, `Branch`, `BranchSafe`, `Slot`, `Ports` and
`Pools`.

**Title** — defaults to the branch name cleaned up (`feature/add-login` →
`Add login`). Common prefixes (`feature/`, `fix/`, `chore/`, etc.) are stripped
automatically.

**Requirements**: `git` and `gh` (GitHub CLI, authenticated). Run `gh auth
status` to verify.

## Worktree state

Per-worktree bookkeeping lives in `.wet-worktrees.json` at the repo root.
Each entry records the branch, path, creation time, slot index, and resolved
ports. Legacy entries (created before `.wet.yaml` existed) omit `index` and
`ports` — they load cleanly and are treated as slot 0.

## No config, no setup

`wet setup` requires a `.wet.yaml`; a missing one is a hard error naming both
paths it looked in. There is deliberately no hardcoded fallback copy list —
that would be project knowledge baked into a CLI that claims to have none, and
it produces a worktree with no ports allocated, which fails later as a port
collision nobody traces back to setup.

(`wet worktree create` doesn't consult `.wet.yaml` at all — it only forks the
worktree and copies `.env`. Everything else happens in `wet setup`.)

## Examples

The smallest useful config is a `project_name:` and a `copy:` block — enough to
fork a worktree with the same VS Code settings and env file. With no ports and
no `env:`, nothing is rendered and no hooks run.

A full-stack one adds ports and hooks that point editor tooling at this
worktree's slot:

```yaml
project_name: myapp

copy:
  files: [web/.env, mobile/.env.dev]
  dirs:  [.vscode, web/.vscode, mobile/.vscode]

port_stride: 10

ports:
  - { name: APP_PORT,   base: 8080  }
  - { name: DB_PORT,    base: 5432  }
  - { name: DEBUG_PORT, base: 40000 }

env:
  COMPOSE_PROJECT_NAME: "{{.ProjectName}}-{{.BranchSafe}}"

hooks:
  post_create:
    - sed -i.bak 's/"port": 40000,/"port": {{.DEBUG_PORT}},/' web/.vscode/launch.json
    - rm -f web/.vscode/launch.json.bak
```

More shapes, every key, and the validation rules:
[`cmd/wet.example.yaml`](cmd/wet.example.yaml), or your installed copy at
`.claude/skills/wet/wet.example.yaml`. Design rationale for the rules
above: [`docs/design.md`](docs/design.md).
