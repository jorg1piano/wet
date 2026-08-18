# wet — worktree dev framework

A tiny, repo-agnostic CLI for running several git worktrees in parallel, each
with its own ports, env file, and optional lifecycle hooks. The CLI contains
**no** project-specific knowledge — everything is driven by a `.wet.yaml` at
your repo root.

Core commands:

```
wet setup                       # configure this checkout — main repo OR worktree
wet worktree create [branch]    # minimal: `git worktree add` + copy .env + open tmux window
wet worktree setup              # worktree-only form of `wet setup`
wet worktree pop                # remove the current worktree (run pre_remove hooks, tear down)
wet env populate <tpl> -o <out> # render a template with this checkout's values
wet pr create                   # push branch + open a GitHub PR (via gh)
wet install wet-skill           # install the Claude Code helper skill
```

`create` is intentionally minimal — it only forks the worktree and copies
`.env` (if present). Everything else (`.wet.yaml` parsing, port allocation,
`.env.worktree` rendering, hooks, state-file bookkeeping) happens in
`setup`, which you run from inside the new worktree. This split lets you
`cd` into the fresh worktree (or let tmux open it for you) and run setup
there — which is almost always what you want, because the hooks in
`.wet.yaml` typically assume they're running in the worktree's shell.

## Quick start

A fully annotated config lives at [`.wet.example.yaml`](.wet.example.yaml) —
copy it and delete what you don't need. The short version:

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
`setup` then reads `.wet.yaml`, picks the lowest free port slot (1 on a
fresh install), copies the configured files and dirs (e.g. `.vscode/`),
and writes a `.env.worktree` file with:

```
APP_PORT=8090
COMPOSE_PROJECT_NAME=myapp-feature-new-thing
```

`setup` is idempotent — re-running it on a worktree that's already
tracked updates the entry in place rather than appending a duplicate.

To clean up, `cd` into the worktree and run `wet worktree pop`.

### Installing the Claude helper skill

If you use [Claude Code](https://claude.com/claude-code) and want in-editor
help writing or debugging `.wet.yaml`, install the bundled skill:

```
wet install wet-skill
```

That writes an embedded Claude Code skill to
`.claude/skills/wet/SKILL.md` at your repo root. The skill teaches Claude
how to probe your project (for env files, Docker Compose services, ports,
lifecycle needs) and produce a `.wet.yaml` that matches it. It also covers
the schema, template fields, common hook patterns, and the validation
rules, so Claude can answer schema questions and debug hook failures
without leaving the editor.

Re-run with `--force` to overwrite an existing install. The skill file is
committed-source-worthy — it behaves the same in every worktree and every
clone — so feel free to check it into git.

The canonical source of the skill body lives at
`cli/cmd/install-wet-template.md` in the `wet` repo and is embedded into
the binary at build time via `//go:embed`.

## Schema reference

`.wet.yaml` is loaded from the checkout being configured, falling back to the
main repo root. A missing config is an error — see
[No config, no setup](#no-config-no-setup).

```yaml
project_name: myapp         # required; must match [a-z0-9][a-z0-9_-]*
                            # embeds into COMPOSE_PROJECT_NAME, so it has to
                            # satisfy Docker Compose's naming rules.

main_branch: development    # optional; defaults to "main". The trunk: what
                            # new worktrees branch off (origin/<branch> when
                            # it exists, so a stale main checkout can't leak
                            # its state into one), what `wet pr create`
                            # targets, and the branch it refuses to PR from.

copy:                       # optional
  files:                    # exact files copied into each new worktree,
    - .env                  # relative to the repo root. Missing files are
    - config/local.toml     # silently skipped so optional files don't break
                            # the flow.
  dirs:                     # directories recursively copied.
    - .vscode

port_stride: 10             # default stride for ports (see `ports` below).
                            # Worktree N gets port = base + N*stride.
                            # Optional; defaults to 10.

ports:                      # any number of named port declarations.
  - name: APP_PORT          # must match [A-Z_][A-Z0-9_]*. Must not collide
    base: 8080              # with the reserved template fields
                            # (ProjectName, Branch, BranchSafe, Slot).
  - name: DB_PORT
    base: 5432
  - name: DEBUG_PORT        # per-port stride override — this port advances
    base: 40000             # by 5 per worktree instead of the default 10.
    stride: 5

env:                        # KEY=value entries written to `.env.worktree`.
  COMPOSE_PROJECT_NAME: "{{.ProjectName}}-{{.BranchSafe}}"
  LITERAL: "hello"          # Values are Go text/template strings with
                            # missingkey=error, so typos fail at load time.

pools:                      # optional; named pools of opaque strings that
  devices:                  # get round-robin allocated across worktrees.
    - emulator-5554         # Pool names must match [a-z_][a-z0-9_]* and must
    - emulator-5556         # not collide with reserved template fields
  fixtures:                 # (ProjectName, Branch, BranchSafe, Slot, Pools).
    - fresh-snapshot-a      # Each worktree claims the lowest-indexed item
    - fresh-snapshot-b      # not already held by another worktree.

pr:                         # optional; configures `wet pr create` behavior.
  base: main                # base branch for PRs (default: `main_branch`)
  body_template: |          # Go text/template for the PR body. Available
    ## Summary              # fields: ProjectName, Branch, BranchSafe, Slot,
                            # Ports (map), Pools (map). If omitted, wet
    Worktree slot {{.Slot}} # generates a default body with worktree context.

hooks:                      # optional shell commands run around lifecycle
  post_create:              # events. Each entry is a single string passed
    - echo "created {{.Branch}} at slot {{.Slot}}"
    - ./scripts/setup.sh    # to `sh -c` from the worktree root.
  pre_remove:
    - docker compose -p {{.ProjectName}}-{{.BranchSafe}} down
```

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

Branch sanitization follows
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
| `COMPOSE_PROJECT_NAME`, … | One per `env:` entry, rendered |

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

That abort is only as good as the exit code the hook hands back, so `wet`
warns when a `post_create` hook contains `||`, a pipe, or a trailing
`; true` — each turns a failing command into exit 0, and a half-configured
worktree then goes on to boot as if nothing happened. If a hook needs a
fallback, put it in a script or recipe that tests the one condition it means
to recover from and lets every other failure propagate; don't write
`a || b`, which recovers from all of them indiscriminately. The warning is
advisory (`a || exit 1` is a legitimate guard and does propagate) and
`pre_remove` is exempt, since teardown is best-effort by design.

### Pools

`pools:` declares named lists of opaque strings that `wet` round-robins
across live worktrees. Use pools for any scarce, one-per-worktree resource
where the item identity matters but the mapping to worktrees doesn't — for
example, physical/emulated device IDs, preloaded database snapshots, or
test-fixture directories that must not be shared.

```yaml
pools:
  devices:
    - emulator-5554
    - emulator-5556
    - emulator-5558
  fixtures:
    - snap-alpha
    - snap-beta
```

**Allocation rule.** On `wet worktree create`, each declared pool picks the
lowest-indexed item not currently claimed by another worktree. Released
slots are reclaimed before trailing items: if worktrees hold `item-a` and
`item-c`, the next create picks `item-b`, not `item-d`. Allocation happens
**before** `git worktree add`, so an exhausted pool aborts cleanly with no
leftover worktree on disk. Claims release implicitly when the owning
worktree is popped — there is no explicit release step.

**Exhaustion.** When every item in a pool is claimed, `wet worktree create`
fails with a message listing each claim by branch and item, and suggests
popping a worktree to free one. Grow the pool in `.wet.yaml` if the
natural state of the project is to need more.

**Template access.** Pool picks are exposed as `{{.Pools.<name>}}`:

```yaml
env:
  FLUTTER_DEVICE: "{{.Pools.devices}}"
hooks:
  post_create:
    - ./scripts/restore-snapshot.sh {{.Pools.fixtures}}
```

**Hook env.** Each pool pick is also exported as `POOL_<NAME>` in upper
snake case (`devices` → `POOL_DEVICES`, `test_fixtures` → `POOL_TEST_FIXTURES`),
so shell hooks can reference them without template interpolation:

```yaml
hooks:
  pre_remove:
    - ./scripts/release-device.sh "$POOL_DEVICES"
```

**Name rules.** Pool names must match `[a-z_][a-z0-9_]*` and must not
collide with reserved template fields (`ProjectName`, `Branch`,
`BranchSafe`, `Slot`, `Pools`). Items within a single pool must be
non-empty and unique; items may repeat across pools.

## One bootstrap — `wet setup`

`wet setup` configures the checkout you are standing in, whichever that is:

```
git clone … && cd repo && wet setup    # main checkout → slot 0
wet worktree create feature/x
cd ../worktree-feature/x && wet setup  # worktree → slot 1, 2, …
```

Everything a worktree wants, the main checkout wants too — rendered env files,
a claimed device, a recorded slot — because it is a working checkout as well.
An emulator that got deleted should be re-claimable there by re-running setup,
not hand-fixed. So main runs the same pipeline, differing only where a
difference is forced:

- **Slot is pinned to 0.** `NextFreeSlot` already reserves it, so main gets the
  declared `base:` ports (`4000`, not `4010`) and no worktree can be handed
  them.
- **`copy:` is skipped** — source and destination are the same directory.
- **Pools are claimed normally.** Main holds an item and keeps it, which is the
  honest accounting: that device really is in use. The pool-exhausted message
  names main as a claimant like any other.
- **Destructive lifecycle stays worktree-only.** `pre_remove` hooks and
  `wet worktree pop` never run against main.

`wet worktree setup` remains as the worktree-only form; run in the main repo it
points you at `wet setup`.

## Env templates — `wet env populate`

Copying a generated `.env` into each worktree is the weak link in the flow: the
copy snapshots one machine's values, then drifts from the checked-in shape and
carries the *previous* worktree's ports. `wet env populate` replaces it with a
checked-in template that re-renders per worktree.

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
op item get myapp --format json | jq 'from_entries' \
  | wet env populate config/.env.tpl -o config/.env --secrets-from -

wet env populate config/.env.tpl -o config/.env --secrets-from ./secrets.json
```

### Prefer a pipe over a file on disk

A `secrets_from:` path means a plaintext copy of every secret sits in the
filesystem. Piping a store's output into `--secrets-from -` keeps them in one
process's stdin instead, so the only materialized copy is the env file the app
needs:

```bash
op inject -i secrets.tpl \
  | jq -Rn '[inputs | select(test("^[A-Z_][A-Z0-9_]*=")) | split("=") | {(.[0]): (.[1:] | join("="))}] | add' \
  | wet env populate web/.env.tpl -o web/.env.dev --secrets-from -
```

Run it from a `post_create` hook: `wet env populate` reads `$SLOT` / `$BRANCH`
/ the ports out of the hook environment and renders exactly what a `render:`
target would. A pipe can't be a `render:` target — those render *before* hooks,
with nothing on stdin — so `render:` is for targets whose inputs need no
producer.

Use `set -o pipefail` around it. Without it a store that dies mid-pipe is
invisible: wet renders from the partial object and fails on the first missing
key, blaming the template rather than the vault.

### Where a `secrets_from:` path points

When a file is unavoidable, the path is resolved against **the checkout being
configured** — a `secrets_from:` entry is a fact about the repo, so it means
the same thing whichever directory `wet setup` was invoked from. (A
`--secrets-from` flag is a fact about the invocation, so it stays relative to
your cwd.)

`~` and `$VAR` expand, so a committed config can name a per-machine location
without hardcoding anyone's home directory:

```yaml
render:
  - template: web/.env.tpl
    out:      web/.env.dev
    secrets_from: ["~/.config/myapp/secrets.json"]   # one file per workstation
```

**A store key that shares a name with an `env:` entry overrides it**, and wet
prints a warning naming the key. That combination is deliberate: overriding a
default is a real use, but the same mechanism is how a store carrying staging
URLs silently replaces the ports wet just allocated. If the store owns a key
`env:` also derives, filter it out where you produce the JSON.

### Several sources at once

`--secrets-from` is repeatable, and **later sources win** on a key collision.
That's how a vault and a running container compose:

```bash
docker compose config --format json | jq '.services.db.environment' > /tmp/db.json

op item get myapp --format json | jq 'from_entries' \
  | wet env populate config/.env.tpl -o config/.env \
      --secrets-from /tmp/db.json \
      --secrets-from -
```

You can merge in the shell instead — `{ producer_a; producer_b; } | jq -s add`
does the same left-to-right merge — but the flag is worth preferring for two
reasons: precedence stays visible in the command rather than buried in a
pipeline, and a source that fails to read aborts the render. With `jq -s add`,
a producer that dies still yields valid JSON from the survivors, so the key it
was supposed to supply just goes missing.

Only one source may be `-`; naming stdin twice is an error, since the second
read would find an exhausted pipe.

Rules:

- **The source is always explicit.** wet never sniffs stdin to decide whether
  something was piped in. A hook inherits its parent's stdin, so an
  inherited-but-empty pipe (a script, CI, an agent harness) is
  indistinguishable from one carrying JSON — and guessing wrong blocks forever
  on a read that never delivers. With no `--secrets-from`, the template
  renders from wet's own values alone.
- **Values must be scalars.** Strings, numbers, and booleans are stringified;
  a nested object or array is rejected by key name rather than silently
  serialized into the env file.
- **Piped keys override `env:` entries**, but may *not* override a declared
  port or a reserved field. A vault key named `APP_PORT` winning would hand
  two worktrees the same port, so it's a hard error instead.
- **An unresolved `{{.KEY}}` is fatal.** Never an empty value — `port=` in a
  strictly-typed loader is a silent boot failure downstream.
- **Output is written `0600`**, since the whole point of the secrets channel
  is that the result may hold secrets.
- **Rendered outputs are wet's to own**, in the main checkout as much as in a
  worktree. Re-rendering is how you repair a checkout, so it must not need a
  flag. Per-machine differences that once justified hand-editing the output —
  a LAN IP, a staging hostname — belong in `.env.local` or `.wet.local.yaml`,
  where a re-render preserves them instead of destroying them.

### Where it runs

The usual place is a `post_create` hook, which runs through `sh -c` so the
pipeline works verbatim:

```yaml
hooks:
  post_create:
    - op item get myapp --format json | jq 'from_entries' | wet env populate config/.env.tpl -o config/.env --secrets-from -
```

Hooks run *before* `setup` persists `.wet-worktrees.json`, so in that context
populate reads its values from the environment `BuildHookEnv` exported
(`$SLOT`, `$BRANCH`, `$APP_PORT`, `$POOL_DEVICES`, …) rather than from the
state file. Run by hand after setup, it reads the state file entry for the
current worktree instead. Either way the values are the ones that worktree was
actually allocated.

## Monorepos — derive once, render many

A monorepo has several env files (`web_elixir/.env.dev`, `mobile_elixir/.env`)
whose values all descend from the same ports. The failure mode is that each one
re-derives them:

```
PHX_PORT ──┬─→ docker-compose.yml   builds AUTH_BASE_URL
           ├─→ Justfile             builds HYDRA_CORS_ORIGINS in shell
           ├─→ web/.env.dev         declares AUTH_BASE_URL again
           └─→ mobile/.env          hardcodes localhost:4000, patched by sed
```

Four derivations of one fact, in three languages. Adding one `wet env populate`
call per package doesn't fix it — each template would still rebuild
`AUTH_BASE_URL` from `{{.PHX_PORT}}`.

**`env:` is the derivation layer.** Entries may reference each other, resolved
to a fixed point rather than in sorted order, so a value can be built from
other values:

```yaml
ports:
  - { name: PHX_PORT,          base: 4000 }
  - { name: HYDRA_PUBLIC_PORT, base: 4444 }
  - { name: GRPC_PORT,         base: 50051 }

env:
  DEV_HOST:           "localhost"
  AUTH_BASE_URL:      "http://{{.DEV_HOST}}:{{.PHX_PORT}}"
  HYDRA_PUBLIC_URL:   "http://{{.DEV_HOST}}:{{.HYDRA_PUBLIC_PORT}}"
  VIPPS_REDIRECT_URI: "{{.AUTH_BASE_URL}}/hydra/login/vipps/callback"

render:
  - template: web_elixir/.env.tpl
    out:      web_elixir/.env.dev
    secrets_from: [/tmp/vault.json]
  - template: mobile_elixir/.env.tpl
    out:      mobile_elixir/.env
```

Every package's template then consumes `{{.AUTH_BASE_URL}}` instead of
rebuilding it, `docker-compose.yml` reads it from `.env.worktree` instead of
interpolating it, and the `sed` hook disappears.

### `render:` targets

Declared rather than written as hook lines, because that's what makes the set
inspectable:

```
wet env populate --all             # render every target
wet env populate --all --dry-run   # print every file, write nothing
```

`wet setup` renders them too, **before** hooks — a hook that boots the stack
finds the files already written.

Each target carries its own `secrets_from`, because packages don't need the
same secrets (a backend needs the vault, a mobile app usually needs nothing),
and each renders against its own copy of the data — **one target's secrets
cannot leak into another's output.** A template referencing a key it wasn't
given fails; it never silently picks up a sibling's value.

Two targets writing the same `out` is rejected at load time: the result would
depend on ordering and the loser's secrets would vanish silently.

### `.env.local` — per-machine overrides

Some values vary by workstation, not by worktree — a LAN IP for physical-device
testing, a personal staging hostname. Those belong in a gitignored `.env.local`
at the repo root:

```
DEV_HOST=192.168.1.9
```

Keys there **override `env:` entries**, which makes everything in `env:` a
default, and the override propagates into every value derived from it —
`AUTH_BASE_URL`, `VIPPS_REDIRECT_URI`, and both packages' env files all follow
`DEV_HOST` without a single per-machine edit to `.wet.yaml`. The same overrides
reach `.env.worktree`, hooks, and `pre_remove` (so teardown addresses the same
stack setup brought up).

Format is the usual `KEY=value`, `#` comments and blanks ignored, quotes
stripped. A line without `=` is an error rather than a silent skip — a dropped
override surfaces much later as a mystery wrong hostname.

## `.wet.local.yaml` — machine-local overrides

Some of the config isn't a fact about the repo, it's a fact about a
workstation. A `pools:` list of device IDs is the clearest case: `RFCR921L4DP`
means nothing on a teammate's machine, so a committed pool either gets
hand-edited by everyone or gets committed wrong. That's what makes an otherwise
shareable `.wet.yaml` unshareable.

Drop a gitignored `.wet.local.yaml` beside the `.wet.yaml` **in the checkout
that has `main_branch:` checked out** — not necessarily git's "main working
tree". A machine often carries several checkouts of one repo, and the one git
calls main is just the one cloned first; it can sit on an unrelated branch, or
be a stale clone nobody works in. wet reads the overlay (and `.env.local`, and
the files `copy:` seeds a worktree with) from the trunk's checkout, and writes
grown pool items back there. Shared allocation state stays in git's main tree,
which is the one path every worktree agrees on.

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
"the config" and never learns the file exists. Override `port_stride` because
4010 is taken on your machine, or a single hook, or `ports:` outright — it all
works the same way:

```yaml
port_stride: 100        # slot 2 → base + 200
```

**Merge rules**, deliberately boring:

- **Maps merge key by key** — overriding one `env:` entry leaves the rest alone.
- **Sequences replace wholesale** — a local `pools.devices` is *your* device
  list, not yours appended to a teammate's. Appending would hand you items that
  don't exist on this machine.
- **The merged result is validated as a whole**, and a local file that breaks
  the schema fails with its own name in the error.
- `wet setup` prints which top-level keys the local file overrode, so a
  surprising value is traceable.

**Pool growth writes here, never to `.wet.yaml`.** An AVD the grower just
created exists only on this machine; recording it in the committed config would
dirty a tracked file and hand teammates a device they don't have. Because
sequences replace, the local file gets the full merged list.

### Emulator pool items

A pool item names an AVD — `emulator:<AVD_NAME>` — never a runtime serial. An
`emulator-5554` serial is assigned by adb at boot and means nothing while the
emulator is stopped: recording it leaves the pool full of items that resolve to
dead devices after a reboot, with no way to start them. Hooks test for the
`emulator:` prefix to decide whether something needs booting, and resolve the
serial at claim time. `pool_grow: android-emulator` follows the same
convention, and asks adb for a free console port rather than parsing one out of
the item.

## Items and handles

A pool item is a durable name; a handle is how you address it right now. For
everything except emulators they are the same string, and you can ignore the
distinction. For an emulator they cannot be:

- the item is `emulator:<AVD_NAME>` — an AVD survives reboots, a serial does
  not, so the item is what gets recorded and what teardown reasons about
  ("is this one mine to kill?");
- the handle is `emulator-5554` — what `flutter run -d`, `adb -s` and a
  screenshot actually take.

`wet setup` boots any claimed emulator that isn't running and resolves the
serial before rendering anything, so `{{.Handles.devices}}` is a device you can
target and `$POOL_HANDLE_DEVICES` is the same value in hooks.

**Don't reimplement this in a hook.** The obvious shell version —
`flutter emulators --launch` then `adb wait-for-device` — fails with "error:
more than one device/emulator" as soon as a second device is attached, which is
the normal state once you run worktrees in parallel. wet resolves by asking
each console its AVD name, which is the only method that survives a crowd.

## Running several worktrees at once

Two `wet setup` runs started at the same moment used to hand out the same slot,
the same ports and the same device: the state file was read-modify-write, and
the claim was only written after the hooks, leaving an image-build-long window
in which a taken slot looked free. Allocation now happens under a lock
(`.wet-worktrees.lock`, beside the state file), and the claim is written before
the hooks run. The lock never spans the hooks — concurrent stack boots are the
point.

Two consequences worth knowing:

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
```

**Base branch** resolves in order: `--base` flag → `.wet.yaml` `pr.base` → `"main"`.

**PR body** — when run from a tracked worktree, the body auto-includes the
slot index, allocated ports, and pool items. Override with `pr.body_template`
in `.wet.yaml`:

```yaml
pr:
  base: main
  body_template: |
    ## Summary

    Worktree slot {{.Slot}}, ports: {{range $k, $v := .Ports}}{{$k}}={{$v}} {{end}}
```

**Title** — defaults to the branch name cleaned up (`feature/add-login` →
`Add login`). Common prefixes (`feature/`, `fix/`, `chore/`, etc.) are
stripped automatically.

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

### Minimal — no ports, no Docker

```yaml
project_name: pure-frontend
copy:
  files:
    - .env.local
  dirs:
    - .vscode
```

Enough to fork a worktree with the same VS Code settings and env file. No
`.env.worktree` is written (no ports or `env:` to render), no hooks run.

### A full-stack config

```yaml
project_name: myapp

copy:
  files:
    - web/.env
    - mobile/.env.dev
    - .claude/settings.json
    - .claude/settings.local.json
  dirs:
    - .vscode
    - web/.vscode
    - mobile/.vscode

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
    - sed -i.bak -E 's/"port=[0-9]+"/"port={{.APP_PORT}}"/g' mobile/.vscode/launch.json
    - rm -f web/.vscode/launch.json.bak mobile/.vscode/launch.json.bak
```

Three ports, one env entry, two `sed`-based launch.json rewrites so the
Delve attach port and the Flutter `--dart-define` port both point at the
worktree's slot.
