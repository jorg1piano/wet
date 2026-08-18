# Design notes

Why wet behaves the way it does. The [README](../README.md) states the rules;
this file holds the reasoning behind the ones that are easy to mistake for
arbitrary.

## Why hooks may not swallow a failure

Aborting on a nonzero exit is only as good as the exit code the hook hands
back. `||`, a pipe, and a trailing `; true` each turn a failing command into
exit 0, and a half-configured worktree then goes on to boot as if nothing
happened — which is why wet warns about all three.

If a hook needs a fallback, put it in a script or recipe that tests the one
condition it means to recover from and lets every other failure propagate; don't
write `a || b`, which recovers from all of them indiscriminately. The warning is
advisory (`a || exit 1` is a legitimate guard and does propagate) and
`pre_remove` is exempt, since teardown is best-effort by design.

## Prefer a pipe over a file on disk

A `secrets_from:` path means a plaintext copy of every secret sits in the
filesystem. Piping a store's output into `--secrets-from -` keeps them in one
process's stdin instead, so the only materialized copy is the env file the app
needs:

```bash
op inject -i secrets.tpl \
  | jq -Rn '[inputs | select(test("^[A-Z_][A-Z0-9_]*=")) | split("=") | {(.[0]): (.[1:] | join("="))}] | add' \
  | wet env populate web/.env.tpl -o web/.env.dev --secrets-from -
```

Run it from a `post_create` hook: `wet env populate` reads `$SLOT` / `$BRANCH` /
the ports out of the hook environment and renders exactly what a `render:`
target would. A pipe can't be a `render:` target — those render *before* hooks,
with nothing on stdin — so `render:` is for targets whose inputs need no
producer.

Use `set -o pipefail` around it. Without it a store that dies mid-pipe is
invisible: wet renders from the partial object and fails on the first missing
key, blaming the template rather than the vault.

## Why `--secrets-from` twice, rather than `jq -s add`

`--secrets-from` is repeatable, and later sources win on a key collision.
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
pipeline, and a source that fails to read aborts the render. With `jq -s add`, a
producer that dies still yields valid JSON from the survivors, so the key it was
supposed to supply just goes missing.

## Rendered outputs are wet's to own

In the main checkout as much as in a worktree. Re-rendering is how you repair a
checkout, so it must not need a flag. Per-machine differences that once
justified hand-editing the output — a LAN IP, a staging hostname — belong in
`.env.local` or `.wet.local.yaml`, where a re-render preserves them instead of
destroying them.

## Monorepos — the failure mode is re-derivation

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
`AUTH_BASE_URL` from `{{.PHX_PORT}}`. That's why `env:` is the derivation layer:
entries may reference each other, resolved to a fixed point rather than in
sorted order, so every package's template consumes `{{.AUTH_BASE_URL}}` instead
of rebuilding it, `docker-compose.yml` reads it from `.env.worktree` instead of
interpolating it, and the `sed` hook disappears.

## Don't resolve emulator serials in a hook

`wet setup` boots any claimed emulator that isn't running and resolves its
serial before rendering anything. The obvious shell version —
`flutter emulators --launch` then `adb wait-for-device` — fails with "error:
more than one device/emulator" as soon as a second device is attached, which is
the normal state once you run worktrees in parallel. wet resolves by asking each
console its AVD name, which is the only method that survives a crowd.

## Why allocation holds a lock

Two `wet setup` runs started at the same moment used to hand out the same slot,
the same ports and the same device: the state file was read-modify-write, and
the claim was only written after the hooks, leaving an image-build-long window
in which a taken slot looked free. Allocation now happens under a lock
(`.wet-worktrees.lock`, beside the state file), and the claim is written before
the hooks run. The lock never spans the hooks — concurrent stack boots are the
point.

## Why the main checkout runs the same pipeline

Everything a worktree wants, the main checkout wants too — rendered env files, a
claimed device, a recorded slot — because it is a working checkout as well. An
emulator that got deleted should be re-claimable there by re-running setup, not
hand-fixed. So main runs the same pipeline, differing only where a difference is
forced.

That is also why main claims pool items normally and keeps them, rather than
being exempted: it is the honest accounting. That device really is in use, and a
pool that hid main's claim would hand the same device to a worktree.

## Why templates beat copying a generated `.env`

Copying a generated `.env` into each worktree is the weak link in the flow: the
copy snapshots one machine's values, then drifts from the checked-in shape and
carries the *previous* worktree's ports. A checked-in template that re-renders
per worktree has neither problem — the shape is reviewed like code, and the
values are always this slot's.

## Why a store key may override `env:` but not a port

A store key that shares a name with an `env:` entry overrides it, and wet prints
a warning naming the key. That combination is deliberate: overriding a default
is a real use, but the same mechanism is how a store carrying staging URLs
silently replaces the ports wet just allocated. If the store owns a key `env:`
also derives, filter it out where you produce the JSON.

A key colliding with a *declared port* is a hard error instead, because a vault
key named `APP_PORT` winning would hand two worktrees the same port.

## Why wet never sniffs stdin

The secrets source is always explicit. A hook inherits its parent's stdin, so an
inherited-but-empty pipe (a script, CI, an agent harness) is indistinguishable
from one carrying JSON — and guessing wrong blocks forever on a read that never
delivers. With no `--secrets-from`, the template renders from wet's own values
alone.

Relatedly, an unresolved `{{.KEY}}` is fatal rather than an empty value: `port=`
in a strictly-typed loader is a silent boot failure a long way downstream.

## Why `.wet.local.yaml` exists, and where it is read from

Some of the config isn't a fact about the repo, it's a fact about a workstation.
A `pools:` list of device IDs is the clearest case: `RFCR921L4DP` means nothing
on a teammate's machine, so a committed pool either gets hand-edited by everyone
or gets committed wrong. That's what makes an otherwise shareable `.wet.yaml`
unshareable.

It is read from the checkout that has `main_branch:` checked out — not
necessarily git's "main working tree". A machine often carries several checkouts
of one repo, and the one git calls main is just the one cloned first; it can sit
on an unrelated branch, or be a stale clone nobody works in. wet reads the
overlay (and `.env.local`, and the files `copy:` seeds a worktree with) from the
trunk's checkout, and writes grown pool items back there. Shared allocation
state stays in git's main tree, which is the one path every worktree agrees on.

Pool growth writes to the overlay and never to `.wet.yaml`: an AVD the grower
just created exists only on this machine, and recording it in the committed
config would dirty a tracked file and hand teammates a device they don't have.

## Why `render:` targets are declared, not written as hook lines

Declaring them is what makes the set inspectable — `wet env populate --all
--dry-run` can report every file it would write for this slot, which opaque
`sh -c` lines can't.

Each target renders against its own copy of the data, so one target's secrets
cannot leak into another's output: a template referencing a key it wasn't given
fails rather than silently picking up a sibling's value. Two targets writing the
same `out` is rejected at load time, because the result would depend on ordering
and the loser's secrets would vanish silently.
