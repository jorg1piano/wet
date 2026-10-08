# wet — worktree dev framework

A tiny, repo-agnostic CLI for running several git worktrees in parallel, each
with its own ports, env file, and optional lifecycle hooks. The CLI contains
**no** project-specific knowledge — everything is driven by a `.wet.yaml` at
your repo root.

Core commands:

```
wet setup                       # configure this checkout — main repo OR worktree
wet worktree create [branch]    # minimal: `git worktree add` + open tmux window
wet worktree mount <branch>     # same, for a branch that already exists (local or on origin)
wet worktree setup              # worktree-only form of `wet setup`
wet worktree list               # interactive list — pick one to open in tmux or VS Code (alias: ls)
wet worktree pop                # remove the current worktree, keep the branch (runs pre_remove hooks)
wet worktree cleanup --dry-run  # preview clearing every recorded allocation, keeping the checkouts
wet docker cleanup --dry-run    # find Compose stacks not referenced by panes or slot_pool
wet secrets setup [group…]      # scaffold a provider's keys into .wet.yaml
wet env pull [output]           # regenerate the env files declared under outputs:
wet env explain [output]        # what each output contains, and where its secrets live
wet env push <output> --from …  # write that output's declared secrets back to the store
wet mobile setup                # point this checkout's dev URLs at the device you test on
wet mobile open                 # put this worktree's emulator on screen, booting it if it is down
wet mobile add [item]           # claim one more device for this worktree
wet mobile ensure --count 4     # idempotently claim/boot enough devices for a 4-actor scenario
wet mobile list                 # what devices this worktree claims, and are they up (alias: ls)
wet mobile remove <item>        # release a device `add` claimed, and stop it
wet mobile reverse-tcp          # adb reverse this worktree's ports onto its devices
wet slot list                   # every claimed slot with its branch, ports and pools (alias: ls)
wet slot warm --all --headless   # run slot_start for every configured checkout
wet slot release <path>         # free the claim of a checkout removed without pop
wet port-cluster next           # claim a free port band for this clone
wet machine list                # every wet project on this machine, its ports and slots
wet pr create                   # push branch + open a GitHub PR (via gh)
wet install wet-skill           # install the Claude Code helper skill
```

`create` forks the worktree and opens the configured multiplexer window. It
reads configuration to choose the trunk and multiplexer, but does no copying,
port allocation, env rendering, hooks, or state-file bookkeeping. Those happen in
`setup`, which you run from inside the new worktree because hooks usually assume
they're running in that checkout.

`mount` is the same minimal checkout flow for a branch that already exists: it
never creates a branch, and if only `origin/<branch>` is there it fetches and
sets up a local tracking branch first. After the checkout it opens the
configured multiplexer window and prints the same `wet worktree setup` next-step
hint. Both commands take `--no-multiplexer` when something else is managing
sessions.

For scripts and orchestrators, `create --json` keeps stdout machine-readable
and reports the new branch, its absolute worktree path, and the ref it branched
from; progress continues on stderr:

```console
$ wet worktree create feature/new-thing --no-multiplexer --json
{"branch":"feature/new-thing","path":"/code/worktree-feature/new-thing","base":"main"}
```

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
cd ../worktree-feature/new-thing   # or let the multiplexer open it for you
wet worktree setup
```

`create` forks `../worktree-feature/new-thing/` and opens the configured
multiplexer window. `setup` then reads `.wet.yaml`, picks the lowest free port
slot (1 on a fresh install), copies the configured files and dirs, and writes a
`.env.worktree` holding `APP_PORT=8090` — slot N gets
`base + N*port_stride`, so slot 1 of a `base: 8080` port with
`port_stride: 10` is 8090 — and
`COMPOSE_PROJECT_NAME=myapp-feature-new-thing`. It is idempotent — re-running it
on a tracked worktree updates the entry in place rather than appending a
duplicate.

To clean up, `cd` into the worktree and run `wet worktree pop`. It runs the
configured `pre_remove` hooks and removes the checkout. Container teardown
requires a hook that stops the containers. The branch is kept.

`wet worktree list` lists configured checkouts from the state file. Its `x`
action runs the same removal hooks and keeps the branch. A checkout created
with `create` or `mount` appears after `setup`. The main checkout can be opened
from the list but cannot be removed.

`wet worktree cleanup` clears every allocation from the state file. It keeps
Git worktrees, branches, generated files, and running resources. Use
`--dry-run` to preview the count, then run `wet setup` in each checkout that
should claim resources again. `wet worktree clean` is an alias.

If Docker has stale Compose stacks from old worktrees, run
`wet docker cleanup --dry-run`. It keeps stacks referenced by Herdr panes,
falling back to tmux when Herdr is unavailable. With `slot_pool: true` it also keeps reusable slot
projects. Use `--verbose` only when you want the full kept-project list. Without
`--dry-run` it runs `docker compose -p <project> down --remove-orphans` and then
`docker rm -f` for any containers still listed for the stale project.

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
- `.gitignore` — appends `.wet-worktrees.json`, `.wet-worktrees.lock`,
  `.env.worktree`, `.wet.local.yaml` and `.env.local` if they aren't already
  listed, matching whole lines so `.env.local.example` isn't mistaken for
  `.env.local`. The last two are per-machine files you
  write by hand — `.wet.local.yaml` carries your 1Password sign-in address —
  so wet ignores them for you. An existing file is appended to, never
  rewritten.

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
following keys are injected automatically for every render. `outputs:` names
the same values, but by bare name rather than as a template — `include:
[APP_PORT]`, `from: Pools.devices`:

| Key | Value | Example |
|---|---|---|
| `{{.ProjectName}}` | `project_name` from the config | `myapp` |
| `{{.Branch}}` | Raw git branch name | `feature/foo` |
| `{{.BranchSafe}}` | Docker-Compose-sanitized branch name | `feature-foo` |
| `{{.Slot}}` | Allocated slot index (1, 2, …) | `1` |
| `{{.APP_PORT}}`, `{{.DB_PORT}}`, … | One key per declared port with its resolved value | `8090` |
| `{{.Pools.devices}}`, `{{.Pools.fixtures}}`, … | Allocated item per declared pool | `emulator:pixel-a` |
| `{{.Handles.devices}}`, … | The address that item answers to *now* | `emulator-5554` |
| `{{.AllPools.devices}}`, … | Every item this worktree claims from the pool, primary first | `[emulator:pixel-a emulator:pixel-b]` |
| `{{.AllHandles.devices}}`, … | The address each of those answers to now | `[emulator-5554 emulator-5556]` |

`Pools`/`Handles` are the single device — the primary, the one `wet setup`
claimed. `AllPools`/`AllHandles` are lists that additionally carry whatever
[`wet mobile add`](#several-devices-on-one-worktree) claimed. A worktree with
one device has one-entry lists, so a template written for several is right on
every worktree.

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
| `POOL_DEVICES_ALL`, … | Every claimed item, primary first, space-separated |
| `POOL_HANDLE_DEVICES_ALL`, … | Every claimed handle, same order |
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

`pools:` declares named lists of opaque strings that `wet` claims in declaration
order across live worktrees. Use pools for any scarce, one-per-worktree resource where the
item identity matters but the mapping to worktrees doesn't — device IDs,
preloaded database snapshots, test-fixture directories that must not be shared.

**Allocation rule.** Pools are claimed in `wet setup`, not in `wet worktree
create` — like ports and slots, and under the same lock. Each declared pool
picks the lowest-indexed item not currently claimed by another worktree, so
released items are reclaimed before trailing ones. Claims release implicitly
when the owning worktree is popped; there is no explicit release step.

A re-run of `wet setup` keeps the item this worktree already holds rather than
re-picking. Re-picking would silently move a running worktree onto a different
emulator, and the file it just regenerated would name a device nobody is
looking at.

A worktree may hold more than one item of a pool: `wet setup` claims the
primary, and [`wet mobile add`](#several-devices-on-one-worktree) claims the
rest. Every claim counts as claimed for allocation and for the exhaustion
message, and all of them release together when the worktree is popped.

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
exhaustion. `android-emulator` creates a new AVD, boots it, records it as
`emulator:<AVD_NAME>` in `.wet.local.yaml`, and hands it back as the claim.
It is the only supported strategy. An unknown strategy or an entry naming
an undeclared pool fails config validation.

```yaml
pool_grow:
  devices: android-emulator
```

A pool with a strategy may start empty — that is the bootstrap case, and wet
creates the first AVD on the first `wet setup`:

```yaml
pools:
  devices: []
pool_grow:
  devices: android-emulator
```

A new AVD clones the system image and device profile of an AVD the machine
already has. A machine with none — a fresh workstation, or one whose AVDs were
deleted while `.wet.local.yaml` still lists them — gets the newest installed
phone system image for its ABI (`arm64-v8a` on Apple silicon, `x86_64`
elsewhere). With no such image installed, wet downloads
`system-images;android-36;google_apis;<abi>` through `sdkmanager` first. The
SDK is `$ANDROID_HOME` (or `$ANDROID_SDK_ROOT`), else the directory Android
Studio installs to; `avdmanager`, `sdkmanager` and `emulator` are found on
`PATH` or under that root.

An empty pool *without* a strategy is rejected at load: nothing can ever fill
it, so it can only fail later at allocation, and failing at load names the file
to fix.

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

## Generated env files — `outputs:`

Every environment file wet generates is declared inline in `.wet.yaml` under
`outputs:`. There is no checked-in `.env.tpl` and no `render:` block: for a
dotenv file wet already knows every value, so the only thing left to say is
which values the file carries and under what key
([why the template layer is gone](docs/design.md#why-generated-env-files-are-declared-not-templated)).

```yaml
outputs:
  - name: backend                 # the CLI identifier: `wet env pull backend`
    out: backend/.env.dev         # relative to the checkout being configured
    format: dotenv
    include:                      # keep the wet name as the output key
      - AUTH_BASE_URL
      - DATABASE_URL
      - PHX_PORT
    secrets:                      # only these definitions are visible here
      - SECRET_KEY_BASE
      - GOOGLE_CLIENT_SECRET

  - name: mobile
    out: mobile/.env
    format: dotenv
    mode: "0644"                  # optional; default 0600
    values:                       # rename, or write a fixed value
      host:          { from: DEV_HOST }
      port:          { from: GRPC_PORT }
      webBaseUrl:    { from: AUTH_BASE_URL }
      oidcClientId:  { literal: myapp-mobile }

  - name: deploy-staging
    out: deploy/.env.staging
    format: dotenv
    on_demand: true               # not this checkout's file; see below
    values:
      APP_HOST:      { literal: staging.example.com }
    secrets:
      - SECRET_KEY_BASE
```

That one block answers the questions the old split between `.wet.yaml` and a
pile of `.env.tpl` files could not: which files does this repo generate, which
keys does each hold, and which of those keys are secrets.

**The three key sources.**

- `include:` names an ordinary value, a declared port, or a reserved field, and
  keeps its name as the output key.
- `secrets:` names entries of `secrets.definitions:` (below) and likewise keeps
  their names. Use `values.<key>.secret:` to select one under another key.
- `values:` maps an output key to exactly one source. `from:` reads an ordinary
  value, port, reserved field, or `Pools.<name>` / `Handles.<name>` entry.
  `literal:` writes a fixed scalar. `secret:` selects a secret definition.
  `file:` uses the absolute path of a downloaded file output.

`values:` keys are emitted in the order you wrote them, and the file as a whole
is `include:`, then `secrets:`, then `values:` — declaration order, so the
generated file reads the way the config does. That order is a real guarantee,
not an accident of the YAML library: `values:` decodes through a custom
unmarshaller and `.wet.local.yaml` is merged at the node level, so nothing
alphabetizes the keys on the way through
([why that had to be fixed](docs/design.md#why-values-keeps-its-declared-order)).

**Everything that can be checked from `.wet.yaml` alone is checked at load**,
in every command, long before a store is contacted or a byte is written: an
unknown `include:` name, a secret an output forgot to declare, `from:` and
`literal:` both set, the same output key claimed twice across the three
sources, two outputs writing the same `out:`, a duplicate `name:`, an `out:`
escaping the checkout, an unsupported `format:`, a `mode:` that isn't octal,
and an output declaring no keys at all. A misspelled include must not first
surface as a missing key in a booting app.

One consequence worth stating: the names an `include:` may use come from the
config — reserved fields, `ports:`, `env:` — and **not** from `.env.local`. A
key that exists only on your workstation cannot become an output's contract;
`.env.local` overrides declared values, it does not add new ones.

**Writing.** Each file is written atomically — a sibling temp file created
`0600`, chmodded to the final mode, then renamed over the target — so a process
that dies mid-write cannot leave half an env file for a booting app to read.
Missing parent directories of `out:` are created. Every dotenv file starts with

```
# Generated by wet. Do not edit — change outputs: in .wet.yaml.
```

because a hand-edit there does not survive the next pull. A value containing a
newline is rejected rather than escaped: dotenv dialects disagree about
multi-line values, and a value that half-parses puts the tail of a private key
on a line the reader treats as the next assignment. Base64-encode PEM material
into a single-line secret.

Generated files default to mode `0600` — they may carry secrets and they
outlive the command that wrote them. Set `mode:` explicitly if a consumer needs
otherwise.

### Provider files and properties

`format: file` downloads a secret definition as raw bytes. The provider
reference can name any file type. No extension checks, decoding, headers, or
newline changes apply to the downloaded content.

For example, this pulls a keystore and writes the properties that use it:

```yaml
secrets:
  store: op
  policy: required
  vault: Development
  item: Android signing
  definitions:
    KEYSTORE: {field: release.jks}
    STORE_PASSWORD: {field: store password}
    KEY_PASSWORD: {field: key password}

outputs:
  - name: keystore
    out: android/app/release.jks
    format: file
    secret: KEYSTORE
    on_demand: true

  - name: signing
    out: android/key.properties
    format: properties
    values:
      storeFile: {file: keystore}
      storePassword: {secret: STORE_PASSWORD}
      keyAlias: {literal: key}
      keyPassword: {secret: KEY_PASSWORD}
```

Run `wet env pull signing`. wet downloads `keystore` too, even though it is
`on_demand`. `storeFile` contains the absolute path to this checkout's copy.
A bare pull and setup use the same dependency handling. To make signing
optional during setup, mark the `signing` output `on_demand: true` as well.

For 1Password, `field:` names the attachment or Document item's filename.
A full `ref: op://vault/item/filename` works too. File reads use the native
[`op read` command](https://www.1password.dev/cli/reference/commands/read)
through the provider's binary read method. Scalar secrets retain batch reads.
With `worktree_store: env_file`, wet copies the corresponding file output from
main and resolves paths against the receiving worktree.

`format: properties` writes Java properties, including escaped backslashes,
leading spaces, newlines, and Unicode. Passwords are not shell-quoted.
The encoding follows
[Java's properties format](https://docs.oracle.com/en/java/javase/25/docs/api/java.base/java/util/Properties.html).
Both properties and dotenv outputs support `values.<key>.secret:` and `file:`.

File outputs use the same secret policies. A definition's `default:` is literal
file content, useful for development text files. It is never a local filename
or a base64 instruction. Without a default, a failed or disabled file read
fails the pull. An old download never substitutes for a failed read.

All downloads and key/value outputs resolve before staging. wet stages the
whole selection before replacing targets, and files default to mode `0600`.
Add the generated paths to `.gitignore`. `--dry-run` fetches and validates
without writing files. `--stdout` can stream one binary file, but rejects an
output with file dependencies because those paths require files on disk.
`env explain` includes file dependencies and provider destinations.
`env push` accepts dotenv and properties values. Uploading file content is not
part of `env push`.

### `wet env pull` — regenerate, and nothing else

```
wet env pull                       # every declared output that is not on_demand
wet env pull backend               # just that one
wet env pull --dry-run             # resolve, validate, write nothing
wet env pull --secrets=disabled    # override secrets.policy for this run
wet env pull deploy --stdout       # write the file to the stream, not to disk
```

Values come from this worktree's allocation — its slot, its strided ports, its
pool items — layered with `.env.local` and the resolved `env:` block. `pull`
regenerates files and nothing else: it does not register a worktree, allocate
or change a slot, ports or pool items, write `.env.worktree`, or run lifecycle
hooks. That is `wet setup`, which finishes by running this same pull. So a pull
is always safe to re-run, and re-running is how you repair a checkout
([why it needs no flag](docs/design.md#rendered-outputs-are-wets-to-own)).

Every requested output is resolved and validated **before any of them is
written**, so a second output that fails validation cannot leave the first one
already replaced on disk.

`--dry-run` still contacts the store, because whether a `required` secret
actually exists is exactly what a dry run is asked to find out. It never prints
a secret value.

#### `on_demand:` and `--stdout` — the outputs that are not this checkout's

A deploy environment's dotenv belongs in the same inventory as everything else
— it is the same question, which keys does that app run with and where does
each come from, and answering it in a second file is how the two drift apart.
But it is not a file this checkout wants. `wet setup` pulls with no selection,
so a plain entry would fetch a running system's credentials onto every
developer's disk when they configure a checkout.

`on_demand: true` withholds an output from that sweep; naming it is what
generates it. `wet env explain` still lists it, marked `on demand`, because it
is part of the inventory even when it is not part of the checkout.
Setup succeeds when every output is on demand. A bare `wet env pull` then
asks you to name an output.

`--stdout` writes the generated file to standard output instead of to disk, so
the credentials go from the store into whatever consumes them and never exist
as a file:

```
wet env pull deploy-staging --env staging --secrets required --stdout \
  | fly secrets import --app myapp-staging
```

It takes exactly one output — two dotenv files concatenated into one stream is
not a file either reader would accept — and every progress line goes to stderr,
so the stream carries the file and nothing else. It is mutually exclusive with
`--dry-run`: both mean "write nothing to disk", but one of them still produces
the file.

Pair it with `--secrets required`. Under the default `fallback` policy an
unreachable store writes each declared `default:`, and pushing a development
placeholder over a live credential is the mistake this path must never make.

### `wet env explain` — read the inventory

```
wet env explain
wet env explain backend
```

Reads the config only — `.wet.yaml` plus any `.wet.local.yaml` overlaying it.
No worktree, no store, no values. It prints each
output's keys and where each comes from, and for every selected secret the
vault/item/field it resolves to and whether it has a declared default.

### Secrets — `secrets.definitions:`

The `secrets:` block declares, once for the whole repo, what each credential is
called and where it lives. It holds no values.

```yaml
secrets:
  store: op                 # main-branch provider: op or env_file
  worktree_store: env_file  # optional worktree override; op is also allowed
  policy: fallback          # required | fallback | disabled — default fallback
  item: backend-development # the item the fields sit on — a project fact
  vault: Development        # the vault holding it — also a project fact
  # account: is NOT here: see .wet.local.yaml below
  definitions:
    SECRET_KEY_BASE:
      field: secret_key_base
      default: dev-only-secret-key-base
    SENTRY_DSN:
      field: sentry_dsn
      default: ""           # deliberately unconfigured locally
    STRIPE_KEY:
      field: api_key
      item: stripe          # per-secret override of the block's item
    LEGACY_TOKEN:
      ref: op://Shared/legacy/token   # or a full provider reference instead
```

Use `worktree_store: env_file` when main should bootstrap from 1Password but
worktrees should read local development secrets from the `.env` file in the
checkout of `main_branch`. That lets new worktrees populate generated env files
without prompting for 1Password again. `worktree_store` can name any supported
provider, including `op`, so there is no special limitation on worktrees. With
`env_file`, the `.env` key defaults to the definition name (`SECRET_KEY_BASE`),
leaving `field:` free to keep naming the 1Password field when `store: op`.

A definition is `field:` plus optional `vault:` / `item:` overrides, **or** a
full `ref:` — the two forms are mutually exclusive, because a half-overridden
reference is ambiguous. `default:` is a pointer in spirit: an explicit `""` is
a real, deliberate default ("this provider simply isn't configured locally")
and is not the same as having none, which is an error the developer must see.

**One definition per credential, shared by name.** `GOOGLE_CLIENT_ID` is one
credential whether the backend or the app reads it, so the store holds no
per-consumer duplicates and a rotation is one edit.

**Scope is per output.** `secrets:`, `values.<key>.secret:`, and a file output's
`secret:` explicitly select definitions. Defining a credential does not hand it to every generated file; an
output sees exactly the definitions it named
([why](docs/design.md#why-secret-scope-belongs-to-the-output)). Two rules fall
out of that and are enforced at load:

- `include:` refuses a secret name and tells you to use `secrets:`.
- `values.<key>.from:` refuses a secret name. Use `values.<key>.secret:` to
  select a secret under a different output key. This selection participates
  in pull, push, policy handling, and provenance reports.

A secret name must look like an env var (`[A-Z_][A-Z0-9_]*`), must not be a
reserved field, and must not collide with a declared port or an `env:` entry. A
store value shadowing an allocated port would hand two worktrees the same port;
a secret and an ordinary value sharing a name makes the output key ambiguous —
put the fallback in the definition's `default:` instead.

`.env.local` cannot override a secret. It overrides ordinary values, which is
its job; secret values reach a generated file only from the store or from a
declared `default:`.

### Scaffold the keys instead of typing them

`wet secrets setup` writes a provider's keys into `.wet.yaml` for you — across
all three blocks, wired together:

```bash
wet secrets setup                    # multi-select of the catalog
wet secrets setup google apple       # or name them
wet secrets setup google --output web
wet secrets groups                   # what each group contains
wet secrets setup --dry-run          # print the result instead of writing it
```

It splits each group's keys by what they are:

- **Credentials** (the ones with a store field) become
  `secrets.definitions.<KEY>: { field: …, default: … }`, carrying the catalog's
  development fallback in `default:`. If there is no `secrets:` block yet it
  creates one with `store: op` and `policy: fallback`.
- **Derived and plainly public values** — a redirect URI built from this
  worktree's ports — stay ordinary `env:` entries. Storing one in the vault
  would pin every checkout to a single URL.

The two are disjoint on purpose: a definition sharing a name with an `env:`
entry is a load error, so the scaffolder never writes a credential to both.

**Both halves are then wired into an output**, `backend` unless `--output` says
otherwise, appending to its `include:` and `secrets:` lists — and creating the
output (`out: <name>/.env.dev`, `format: dotenv`) if the file has none by that
name. Skipping this step is what would make a scaffolded group inert: the
definitions would exist, no generated file would select them, and `wet env
pull` would write exactly what it wrote before, which reads as wet ignoring the
provider you just added. The summary says explicitly when an output was
invented, since that is the one change you didn't ask for.

Nothing existing is ever touched. Re-running to add a provider leaves the
values you have filled in alone, doesn't re-list a name an output already
selects, and leaves a credential you had already written into `env:` as an
ordinary value rather than moving it out from under you. The result is run
through the real loader before it is saved — a scaffold that writes a config
wet then rejects would be worse than no scaffold at all.

`wet setup` offers the same picker when it scaffolds a starter config, so a
fresh clone doesn't need to know the command exists.

The catalog lives in the wet binary, not in your repo — what `google` means is
a fact about Google, not about your project, so a group gains keys when you
upgrade wet rather than when you remember to copy a file.

### Secret policy — `required`, `fallback`, `disabled`

Automatic secret loading must not make a fresh clone ambiguous
([why there are exactly three](docs/design.md#why-three-secret-policies)).
`secrets.policy:` sets it; `wet env pull --secrets=…` overrides it for one run.

| Policy | Behavior |
|---|---|
| `required` | Every selected secret must come from the store. A declared `default:` deliberately does **not** substitute — this mode exists so a project can be certain the real credential is what landed in the file. |
| `fallback` (default) | Try the store; on any failure — client missing, not signed in, item absent, field absent — use the declared `default:`. A secret with neither is an error. |
| `disabled` | Never contact the store. Declared defaults must satisfy every selected secret. For offline work and deterministic tests. |

`fallback` is the default because it is the only policy under which a fresh
clone with no store access still boots — and it is never silent. Each output
gets a provenance line, names only:

```
  Secrets for backend/.env.dev: 7 from 1Password, 3 defaults
    defaults: ADMIN_EMAILS, POSTMARK_API_KEY, SENTRY_DSN
```

A developer who believes Sentry and Google sign-in are configured, when wet
actually wrote empty strings, debugs the wrong thing for an hour.

Each distinct reference is fetched **once** per run, across every requested
output — one round trip, and more importantly one authorization prompt.

### Where the vault and the account come from

Both are in `.wet.yaml`, with one exception. The vault and the item are facts
about the *project* — the credentials sit where they sit, whoever clones the
repo — so they are committed alongside the definitions that resolve against
them. Which 1Password account you are signed into is a fact about *your
workstation*, so that one key is the one that normally lives in the gitignored
`.wet.local.yaml`:

```yaml
# .wet.local.yaml
secrets:
  account: my.1password.com
```

There is no special rule making the vault local. `.wet.local.yaml` overrides
*any* key, so a machine that reads a differently-named vault overrides `vault:`
there exactly as it would override `port_stride:` — but that is an override,
not where the value belongs.

#### More than one deploy environment

When the repo has staging, production and sandbox credentials, nothing about
the definitions changes — the same keys, the same fields, the same item, in a
different vault each. Say that with `vaults:` instead of `vault:` (they are
mutually exclusive), in `.wet.yaml` beside the definitions:

```yaml
secrets:
  vaults:
    - name: myapp_staging
      env: staging
    - name: myapp_production
      env: production
  default_env: staging
```

`default_env:` is the environment a plain `wet env pull` reads, and it is
required as soon as there is more than one vault — wet does not guess which
environment a command meant. A single run can say otherwise with `--env`, on
`pull`, `explain` and `push` alike:

```
wet env pull --env production
wet env explain --env production
wet env push backend --from backend/.env.prod --env production
```

An `--env` naming an environment nobody declared is an error, not a fallback to
the default — a typo must not push staging values over production credentials.
All three print the environment they chose before doing anything — and only
when the run actually resolves a secret, since an output selecting none reads
no vault for the choice to affect.

Two things stay outside the flag, on purpose:

- **`item:` does not vary by environment.** The same item name has to exist in
  every vault. Where it genuinely differs, give that definition its own `item:`
  or a full `ref:`.
- **A definition's own `vault:` or `ref:` is environment-invariant** — it names
  its own place and `--env` does not move it. `wet env explain` marks those
  destinations `same in every environment` so the exception is visible rather
  than inferred.

`wet setup`, `wet worktree create` and `wet mobile setup` have no `--env`: they
configure a checkout that then lives on, and an environment chosen by a
transient flag would leave no record of which vault filled the files. Put
`default_env:` in that checkout's own `.wet.local.yaml` instead — it is the
same choice, written down.

Because `.wet.local.yaml` layers per checkout, a worktree's own overlay can
carry `default_env:` alone and work against production while the rest of the
machine stays on staging.

No separate command sets any of this, and the provider is never named on the
command line — `secrets.store:` decides. Until a vault is named, `pull` under
`policy: fallback` writes each secret's declared `default:` and says so, and
`explain` reports the missing destination rather than failing.

### `wet env push` — declared secrets, back to the store

```
wet env push backend --from backend/.env.dev --dry-run
wet env push backend --from backend/.env.dev
```

**The named output is the authorization boundary.** wet writes a key only when
it is declared under `secrets.definitions:` and selected by that output's
`secrets:` list or `values.<key>.secret:`. Every other key in the file — ordinary configuration, unknown
names, a secret belonging to another output — is ignored and reported. There is
no flag that lets an undeclared key reach the store, and a destination mapping
cannot be invented on the command line.

That is why a mixed, generated file needs no permissive flag: `backend/.env.dev`
is a perfectly normal input.

**A value equal to that secret's declared `default:` is refused by default**,
reported as `fallback value — not pushed`. A file generated under the
`fallback` policy is full of development placeholders, and pushing an empty
string or a dev-only key over a real credential is the one mistake this command
must never make. `--include-defaults` overrides it, deliberately.

| Flag | Effect |
|---|---|
| `--from <file>` | the dotenv or properties file to read, matching the output format (required) |
| `--dry-run` | report every destination and status, write nothing |
| `--force` | overwrite a field holding a different non-empty value (a conflict is otherwise reported and left alone, so re-pushing a stale file cannot undo a rotation) |
| `--create-item` | create the destination item; a missing item is otherwise an error |
| `--reject-unmapped` | fail if the source holds any key this output does not select — what you want when importing a dedicated secrets-only file and a typo should be loud. It does not widen what wet may write. |
| `--include-defaults` | also push values equal to a declared `default:` |

Names, destinations and statuses are printed. Values never are.

### Where generation runs

`wet setup` runs this same pull itself, after `.env.worktree` exists and
**before** `post_create` hooks (and skips it entirely when the config declares
no `outputs:` — there is nothing to generate) — a hook that boots the stack finds its env
files already written. Setup owns the lifecycle; it owns no second way to write
an env file ([why that matters](docs/design.md#why-setup-and-pull-share-one-pipeline)).

You can also call `wet env pull` from a `post_create` hook. It reads the hook
environment (`$SLOT`, `$BRANCH`, `$APP_PORT`, etc.). Setup records the allocation
before hooks start. A pull run by hand reads that state entry. Both paths use
the values allocated to this checkout.

### Migrating from `render:`

`render:` and `secrets.sections:` are removed, and loading a config that still
has either is a hard error naming the replacement — not a silent skip, which
would leave a stale `.env.dev` on disk looking current. Migration is a
deliberate, reviewable edit:

1. For each `render:` target, add an `outputs:` entry with the target's `out:`
   as `out:` and `format: dotenv`.
2. Move each `KEY={{.FIELD}}` line out of the `.env.tpl`: same name on both
   sides becomes `include: [KEY]`, a rename becomes
   `values: { KEY: { from: FIELD } }`, and a constant becomes
   `values: { KEY: { literal: … } }`.
3. Turn each `secrets.sections.<name>` entry into a `secrets.definitions:`
   entry (`LOGICAL_NAME: { field: … }`), and list the names each output needs
   under that output's `secrets:`.
4. Move any development fallback out of `env:` and into the definition's
   `default:` — the two may no longer share a name.
5. Delete the `.env.tpl` files.

## Monorepos — derive once, emit many

**`env:` is the derivation layer.** Entries may reference each other, resolved
to a fixed point rather than in sorted order, so a value is built once and every
package's output consumes the result instead of rebuilding it — see
[the failure mode this avoids](docs/design.md#monorepos--the-failure-mode-is-re-derivation).

```yaml
ports:
  - { name: PHX_PORT,  base: 4000 }
  - { name: GRPC_PORT, base: 50051 }

env:
  DEV_HOST:           "localhost"
  AUTH_BASE_URL:      "http://{{.DEV_HOST}}:{{.PHX_PORT}}"
  VIPPS_REDIRECT_URI: "{{.AUTH_BASE_URL}}/hydra/login/vipps/callback"

outputs:
  - name: web
    out: web_elixir/.env.dev
    format: dotenv
    include: [AUTH_BASE_URL, VIPPS_REDIRECT_URI]
    secrets: [SECRET_KEY_BASE]

  - name: mobile
    out: mobile_elixir/.env
    format: dotenv
    values:
      host:       { from: DEV_HOST }
      port:       { from: GRPC_PORT }
      webBaseUrl: { from: AUTH_BASE_URL }
```

Outputs are declared rather than written as hook lines, so the whole set is
inspectable — `wet env explain` and `wet env pull --dry-run` can report every
file wet would write for this slot, which opaque `sh -c` lines cannot
([why](docs/design.md#why-outputs-are-declared-not-written-as-hook-lines)).

The mobile output carries no `secrets:`, so no backend credential can reach a
shipped client — not by accident and not by rename. It still gets this
worktree's allocated `GRPC_PORT`, under the key the app expects.

`wet setup` generates them too, **before** hooks, so a hook that boots the
stack finds the files already written. Two outputs writing the same `out:` is
rejected at load time.

### `.env.local` — per-machine overrides

Some values vary by workstation, not by worktree — a LAN IP for physical-device
testing, a personal staging hostname. Those go in a gitignored `.env.local` beside
the checkout that has your trunk branch checked out — the same place
`.wet.local.yaml` lives, and the same one every worktree reads it from:

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

It **overrides** declared values; it does not introduce new ones and it cannot
touch a secret. An `include:` may only name something `.wet.yaml` declares, so
a key that exists on one workstation never becomes an output's contract, and
secret values reach a generated file only from the store or a declared
`default:`.

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

**So does the port cluster.** `port_cluster:` is which band of ports this clone
drew on this machine — see
[Several clones at once](#several-clones-at-once--wet-port-cluster). Committing
it would hand the whole team one clone's answer.

## Mobile — `wet mobile setup` and `wet mobile reverse-tcp`

(For running two phones against one worktree, see
[Several devices on one worktree](#several-devices-on-one-worktree).)

`wet mobile setup` points this checkout at the device you are about to run on.
It finds the `env:` keys the committed `.wet.yaml` declares as `localhost` (the
`DEV_HOST` convention every URL derives from), lists the connected devices
(`adb`, `devicectl`, `simctl`), and rewires the checkout for the one you pick:

```
wet mobile setup                       # pick a device in a TUI
wet mobile setup --device RFCR…        # or name it (id, unique prefix, or name)
wet mobile setup --host 192.168.1.7    # skip LAN-IP autodetection
```

| picked | `.wet.local.yaml` | Android also gets |
| --- | --- | --- |
| real phone | `KEY: <this machine's LAN IP>` | `adb reverse` for this checkout's ports |
| emulator / simulator | the override removed, so the declared `localhost` wins | the same reverses — that is what makes its `localhost` this machine |

The override lands in the `.wet.local.yaml` of the checkout you run it in, so
one device follows one worktree's stack while every other checkout keeps its
own host — and every later `wet setup`, `wet worktree setup` and `wet env pull`
there renders for that device. The run finishes by regenerating this checkout's
outputs; restart the stack afterwards, since running services cached their
environment at boot. Re-run it when you switch devices, or when the machine's
IP changes.

If the machine's `.env.local` sets the same host key, remove that entry first.
`.env.local` overrides the YAML layers, so mobile setup reports the conflict
before making changes.

An Android app talks to `localhost`, and on a device that *is* the device. The
fix is `adb reverse tcp:P tcp:P` — and which `P` is exactly what the worktree
already knows: the ports it was allocated for its slot.

```
wet mobile reverse-tcp                 # every allocated port, onto this worktree's device
wet mobile reverse-tcp APP_PORT        # only the named ports
wet mobile reverse-tcp --dry-run       # print the adb commands, change nothing
wet mobile reverse-tcp --remove        # undo the same set
wet mobile reverse-tcp --headless      # boot a claimed emulator without a window
wet mobile reverse-tcp --device RFCR…  # target a serial wet knows nothing about
```

To have `wet setup` do it for you, make it a hook — it reads its ports from the
hook environment, so it needs no arguments and is right on every slot:

```yaml
hooks:
  post_create:
    - docker compose up -d
    - wet mobile reverse-tcp
```

Ports come from `.wet-worktrees.json` — or from that hook environment, which is
what lets it run before setup has written the state file.

The target is this worktree's own claims — the device `wet setup` picked and
every one `wet mobile add` added, in pool order with the primary first. A claim
counts as a device when it names an emulator (booted first if it isn't running,
`--headless` to boot without a window) or is an attached adb serial. A pool item
that is neither — a fixture snapshot — is never passed to `adb`.

`wet mobile setup` runs the same reverses itself, on the device you pick there,
so choosing a device and making it reach the stack are one step. An emulator
needs them (localhost on the device is only this machine once they land, so a
failure fails the command); a phone pointed at the LAN IP does not, so there
they are a bonus for anything still dialling localhost and a failure is only
reported. Reach for `wet mobile reverse-tcp` when the ports change without the
device changing, in a hook, or for a serial wet knows nothing about.

## Seeing the emulator with `wet mobile open`

A worktree's emulator is addressed by its serial from the moment `wet setup`
claims it, which is all `flutter run -d` and `adb -s` need. Looking at it is a
separate question: with eight AVDs up, the window you want is the one running
this worktree's AVD, and if the emulator is stopped there is no window at all.

```
wet mobile open                     # the primary device of the devices pool
wet mobile open emulator:pixel-b    # one claim, by item or by serial
wet mobile open --all               # every emulator this worktree claims
wet mobile open --restart           # reboot a headless emulator so it gets a window
```

A stopped emulator is booted with a window, the opposite of `--headless`, and
this worktree's ports are reversed onto it, so the app on it reaches this
worktree's stack straight away. One that is already up is only raised: its
state and its reverses are untouched.

The window is raised through macOS Accessibility, so the terminal running
`wet` needs to be allowed under System Settings > Privacy & Security >
Accessibility. Elsewhere the boot still happens and the raise reports that you
have to switch windows yourself.

A window cannot be attached to a live emulator, so one booted with
`--headless` has to be restarted to get one. `--restart` does that, and it is
opt-in because it costs whatever the emulator was holding.

The device named has to be one this worktree claims. Opening any other AVD
would put another worktree's device on screen, and print a serial this one must
not run against.

## Several devices on one worktree

Some work needs two phones on one backend: a chat between two accounts, a
share flow, anything where the second user is the point. That is not a second
worktree — same branch, same stack, same database — and it is not a second
pool, because the devices are interchangeable. It is this worktree holding one
more claim.

```
wet mobile add                      # the next free item of the devices pool
wet mobile add emulator:pixel-b     # or a specific pool item / any AVD
wet mobile add RFCR921L4DP          # or an attached adb serial
wet mobile add --headless           # boot the emulator without a window
wet mobile ensure --count 4         # claim/boot/reverse until this worktree has 4 devices
wet mobile list                     # what this worktree claims, and is it up
wet mobile list --all --json        # every worktree, for scripts
wet mobile remove emulator:pixel-b  # release it, and stop it
wet mobile open --all               # put every emulator it claims on screen
```

`add` claims one device, boots it if it is an emulator, `adb reverse`s this
worktree's ports onto it, and regenerates `.env.worktree` and the declared
outputs. So the device it prints is one you can `flutter run -d` against
immediately.

`ensure --count N` is the deterministic version for agent workflows and
multi-actor scenarios: it counts the primary plus extras, adds only the missing
claims, and boots/reverses every claimed device so retries heal half-booted
emulators. It reuses free predeclared pool items before `pool_grow` creates new
AVDs. It never shrinks the set; use `wet mobile remove` when releasing an extra
is the goal.

The claim is real, and that is the point: no other worktree's `wet setup` can
be handed a device you added, the exhaustion message names it, `wet worktree
pop` releases it with the rest, and a `wet setup` re-run boots it again after a
reboot along with the primary.

**What it does not change is what a single-device template renders.**
`{{.Handles.devices}}` and `$POOL_HANDLE_DEVICES` keep meaning the primary —
the one `wet setup` claimed — so a config that renders one `FLUTTER_DEVICE_ID`
is untouched. The whole set lives in the list forms:

```yaml
env:
  FLUTTER_DEVICE_ID: "{{.Handles.devices}}"                      # the primary, as before
  ALL_DEVICE_IDS: "{{range $i, $h := .AllHandles.devices}}{{if $i}},{{end}}{{$h}}{{end}}"
hooks:
  pre_remove:
    # kills every emulator this worktree booted, not only the first
    - 'for h in $POOL_HANDLE_DEVICES_ALL; do case "$h" in emulator-*) adb -s "$h" emu kill ;; esac; done; true'
```

`wet mobile reverse-tcp` reverses onto every claimed device, so the second
phone reaches the same stack the first one does with no extra step.

Only devices `add` claimed can be `remove`d. The primary belongs to the
worktree's lifecycle — `wet worktree pop` is what releases it — and removing it
would leave `{{.Handles.devices}}` rendering a device nothing claims.

**Teardown of an emulator stays a hook's job**, as it already was. `wet mobile
remove` stops the emulator it releases (that is the explicit undo of an
explicit command, and `--keep-running` opts out), but `wet worktree pop` runs
your `pre_remove` hooks and nothing else — so a hook that kills only
`$POOL_HANDLE_DEVICES` leaves the added emulators running. The loop above is
the version that doesn't.

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

## Several clones at once — `wet port-cluster`

Slots spread ports out inside **one** checkout. Nothing spreads two *clones* of
the same repo apart: both start at slot 0, both resolve to the declared bases,
and the second stack to boot dies with "port is already allocated".

A **port cluster** is the band of ports one clone owns across all of its
worktrees. `cluster_slots:` (default `10`) is how wide that band is;
`port_cluster:` says which band this clone took:

```
cluster 0 -> APP_PORT 4000 .. 4090     (slot 0 = 4000, slot 1 = 4010, ...)
cluster 1 -> APP_PORT 4100 .. 4190
cluster 2 -> APP_PORT 4200 .. 4290
```

`wet port-cluster next` binds every port in every candidate band, lowest
cluster first, and claims the first one where nothing answers:

```
wet port-cluster next             claim the lowest free cluster
wet port-cluster next --dry-run   report which one that is, change nothing
wet port-cluster next --force     re-probe even though this clone has a cluster
```

Each port shifts by **its own stride**, so a port declared `stride: 5` moves 50
per cluster while one on `stride: 10` moves 100. That keeps the bands disjoint
whatever mix of strides a config declares — a flat per-cluster offset would let
a wide-stride port walk into the next cluster at a high enough slot.

**Where the number lives.** In the `.wet.local.yaml` beside the trunk checkout,
which every worktree of the clone reads — a cluster belongs to the clone, not to
one worktree, or its worktrees would interleave with each other. It's
gitignored, so nobody inherits this machine's answer. `cluster_slots:` is the
opposite: it goes in the committed `.wet.yaml`, because every clone has to agree
on the width.

Generated files and recorded allocations still hold the old numbers after a
cluster claim. With the stacks down, re-run `wet setup` in each checkout to
update its recorded ports and generated files. `wet env pull` uses the
recorded ports, so it cannot move a checkout to another band.

With ports declared, worktree slots stay below `cluster_slots`. The default
of 10 allows main at slot 0 and worktrees at slots 1 through 9. Setup fails
when that band is full or a port would exceed 65535. Increase `cluster_slots`
consistently across clones if more slots are needed, then choose bands again
with the stacks down. A checkout with no ports has no port-band limit.

**A re-run keeps the band it has.** Once a clone has a `port_cluster:`, a bare
`next` reports it and changes nothing. The probe cannot tell "a neighbour is on
this band" from "my own stack is on this band", and walking away from a band
this clone is actively serving would strand every generated file and every
running container on it. `--force` re-probes anyway, and will also move *down*
to a lower band that has since freed up.

**What the probe can and can't see.** It finds anything currently serving —
including a loopback-only Postgres and a server bound to IPv4 only, both of
which a naive wildcard-bind check reads as free. A neighbouring stack that is
**down** looks free to a probe, so `next` also skips every band another wet
project on this machine owns (see below), up or not.

### Other projects on this machine — `wet machine`

Every wet project on the machine allocates under one lock, in
`~/.local/state/wet/` (or `$WET_STATE_DIR`). Beside it, `machine.json` lists
each project's main checkout and the shared emulator pool. Everything a
project claims stays in its own `.wet-worktrees.json` and `.wet.local.yaml`.

`wet setup` lists the project the first time it runs, after checking that its
band overlaps no project already listed:

- A project with no claims and no recorded `port_cluster:` is moved to the
  lowest free cluster, and the number is written to its `.wet.local.yaml`.
- A project that already holds slots, or has a recorded cluster, is never
  moved behind its back. Setup stops and names the other project; run
  `wet port-cluster next` there, then `wet setup` again.

```
wet machine list          every listed project, its band and slots, and the shared emulators
wet machine add PATH...   list projects without running setup in them
```

## Running several worktrees at once

Slot, port and pool allocation happens under one lock per machine
(`~/.local/state/wet/wet.lock`), and the claim is written before any hook runs. The lock
never spans the hooks — concurrent stack boots are the point
([background](docs/design.md#why-allocation-holds-a-lock)). Two consequences
worth knowing:

- A failed setup leaves its claim behind. That is deliberate: the tree still
  exists, re-running setup finds its own entry and keeps the same slot, and
  `wet worktree pop` releases it.
- A checkout removed without `pop` (`git worktree remove`, `rm -rf`, a
  multiplexer's own remove) no longer holds its claim forever. Every
  `wet setup` releases the claims of checkouts that are gone, in this project
  and in every other listed one, and prints each. No hooks run for them: the
  checkout is gone, and the slot's warm resources stay for its next user.
  `wet slot release PATH` does the same for one checkout right away.
- Anything a hook allocates from a shared namespace — a docker subnet is the
  usual one — is outside the lock and races the same way. Have the hook ask
  what's free and retry on collision rather than deriving a value from the
  slot: slots are recycled when a worktree is popped, and whatever the old one
  created is not.

## Slot lifecycle

`wet slot reset`, `start`, `stop`, `rebuild`, and `doctor` run the corresponding
`hooks.slot_reset`, `slot_start`, `slot_stop`, `slot_rebuild`, and `slot_doctor`
commands in the configured checkout. They use its recorded allocation and
hook environment. An undeclared hook is a reported no-op.

`wet slot list` prints every checkout in `.wet-worktrees.json` sorted by slot,
with its branch, ports, pool items and path. It marks the current checkout with
`*` and flags paths that are gone from disk. With ports declared, a last line
names the slots below `cluster_slots` that no checkout claims. That line reads
the state file only, so a stack wet does not track can still hold those ports.

`wet slot warm` runs `slot_start`. Add `--reset` to run `slot_reset` first,
`--all` to visit every recorded checkout, and `--headless` to boot claimed
emulators without windows. A failure is reported after the other checkouts
have been attempted. These commands require `wet setup` first.

With `slot_pool: true`, allocation can reuse busy ports when Docker reports
that this slot's own Compose project holds them. It skips ports held by other
projects or unknown listeners. Define `COMPOSE_PROJECT_NAME` using `Slot` for
reusable containers. Run `wet slot reset` explicitly before starting reused
resources, or call it from a `post_create` hook. Setup does not automatically
run the slot lifecycle hooks.

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
Each entry records the branch, path, creation time, slot index, resolved
ports, the pool item claimed per pool (`pools`), and any further items
`wet mobile add` claimed (`pool_extras`). Legacy entries (created before
`.wet.yaml` existed) omit `index` and `ports` — they load cleanly and are
treated as slot 0, and an entry written before `pool_extras` existed simply
has none.

## No config, no setup

When `.wet.yaml` is missing from both the checkout and the main repo, `wet setup`
writes a starter config at the main repo root and exits with an error. It asks
you to edit the project name and ports before retrying. With a terminal it also
offers the secret-group picker. It allocates nothing on this first run.
The CLI has no fallback copy list. The config must declare which files the
project needs and which ports setup should allocate.

`wet worktree create` works without a config. It copies no files and performs
no setup. With a valid config, it uses `main_branch` and `multiplexer`.

## Examples

The smallest useful config has `project_name:` and `copy:`. After creation,
setup copies the declared settings and files and writes `.env.worktree`.
Additional generated files require `outputs:`, and hooks run only when declared.

A full-stack one adds ports and a generated env file that points editor
tooling at this worktree's slot:

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

outputs:
  - name: web
    out: web/.env          # gitignored; carries this worktree's ports
    format: dotenv
    mode: "0644"           # no secrets here, and the toolchain reads it
    include: [APP_PORT, DB_PORT, DEBUG_PORT, COMPOSE_PROJECT_NAME]
```

Editor config then needs no wet involvement at all — point the debugger at the
generated env file and `launch.json` stops varying per worktree, so it can just
be committed:

```jsonc
// web/.vscode/launch.json
{"configurations": [{
  "name": "app (dev)", "request": "launch", "type": "node",
  "envFile": "${workspaceFolder}/web/.env"
}]}
```

More shapes, every key, and the validation rules:
[`cmd/wet.example.yaml`](cmd/wet.example.yaml), or your installed copy at
`.claude/skills/wet/wet.example.yaml`. Design rationale for the rules
above: [`docs/design.md`](docs/design.md).
