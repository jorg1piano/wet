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

## Why generated env files are declared, not templated

wet used to generate env files by rendering checked-in `.env.tpl` templates
named by a `render:` block. That split the inventory across at least three
files: `.wet.yaml` held the ports and the derived values, each `.env.tpl` held
the output keys, and `render:` held the wiring between them. No single file
could answer the questions people actually ask — which env files does this repo
generate, which keys does each one hold, which of those are secrets, which come
from the store, and which vary because of port allocation.

Worse, a split inventory drifts. A template could reference a key nobody had
added to `.wet.yaml`; a secret could be mapped in the config and consumed by no
template; a render target could forget to name its secret section, and the
generated file would be quietly short a credential.

For a dotenv file the template was never buying anything. wet already knows
every value. The only remaining information is a mapping from wet's names to
output keys — which is data, and belongs in the config as data:

```yaml
outputs:
  - name: mobile
    out: mobile/.env
    format: dotenv
    values:
      port: { from: GRPC_PORT }
```

Declaring outputs rather than rendering templates also moves the whole class of
"is this config coherent" questions to load time. An unknown `include:`, a
secret the output never selected, a duplicate output key, two outputs writing
one path — all of that is now knowable from `.wet.yaml` alone, in every
command, before a secret store is contacted or a byte is written. Under
templates, the same mistakes surfaced as a missing key in a booting app.

The narrowness is deliberate: this is a typed encoder for declared outputs, not
a general-purpose text-template engine growing a second syntax. `format:` is
the seam where a second encoder goes when one is genuinely needed.

`render:` was not kept alongside `outputs:`. Two generation models would mean
two answers for validation, precedence, secret scope, and command behavior, and
would preserve exactly the split inventory the change exists to remove. Loading
a config that still has `render:` or `secrets.sections:` is therefore a hard
error naming the replacement — silently ignoring the old keys would be the
worst outcome of all: a config that still parses, generates nothing, and leaves
a stale `.env.dev` on disk looking current.

## Why outputs are declared, not written as hook lines

Declaring them is what makes the set inspectable — `wet env explain` and `wet
env pull --dry-run` can report every file wet would write for this slot, and
which keys each would carry, which opaque `sh -c` lines can't.

It is also what makes the write all-or-nothing. Every requested output is
resolved, validated and encoded before any of them is written, so a second
output that fails validation cannot leave the first one already replaced on
disk. A sequence of hook lines has no such moment: the first has already
written by the time the second discovers it can't.

Two outputs writing the same `out:` is rejected at load time, because the
result would depend on ordering and the loser's keys would vanish silently.

## Why `values:` keeps its declared order

A generated file is read by people, and `values:` is where the renames live, so
the order they were written in is the order that makes the file legible. wet
guarantees it: `include:`, then `secrets:`, then `values:` in declaration
order.

Keeping that guarantee took work in two places, because YAML mappings decode
into Go maps and Go maps have no order. `OutputValues` has its own
`UnmarshalYAML`/`MarshalYAML` pair that records the key sequence off the node
tree, and the `.wet.local.yaml` overlay is merged at the node level rather than
by round-tripping the whole document through `map[string]any` — an earlier
version did the latter, and it silently alphabetized every `values:` block on
the way through. That is the characteristic shape of this bug: nothing errors,
the file is correct, and it just isn't the file you wrote.

## Why secret scope belongs to the output

A secret definition says where a credential lives. It does not say who may read
it. That second question is answered by each output's `secrets:` list, and only
there.

The failure this prevents is concrete: a mobile app's `.env` is compiled into a
shipped client, and every value in it must be assumed public. If defining
`SECRET_KEY_BASE` made it visible to every generated file, the only thing
standing between a backend signing key and an App Store binary would be
somebody remembering not to mention it. Scope-by-declaration means the mobile
output cannot receive it even by accident, and the config says so in the place
a reviewer is already looking.

That is also why a secret can never be renamed. `include:` refuses a secret
name and points at `secrets:`; `values.<key>.from:` refuses one outright.
Allowing `oidcClientSecret: { from: GOOGLE_CLIENT_SECRET }` would put the
authorization decision in a rename, where it is invisible — the whole point is
that one list, per output, is the boundary.

The same rule runs in reverse for `wet env push`. Presence in
`secrets.definitions:` *plus* selection by the named output is the authorization
boundary for every write, and there is deliberately no flag that widens it. An
ordinary or renamed output key like mobile's `port` must never be inferrable as
a secret destination, so wet does not reverse-map `values.<key>.from` at all.

Internally this is enforced in one place. All secrets for a run are fetched
into a single map, and each output is resolved against a view holding only the
names it selected — never against the merged map. Merging is exactly how a
mobile `.env` ends up carrying a backend credential.

## Why three secret policies

Reading secrets automatically during setup is a real convenience and a real
hazard: it makes the behavior of a fresh clone ambiguous unless the config says
what "the store could not answer" means. Three answers cover the honest cases,
and nothing in between is meaningfully different.

**`fallback`** is the default, and the only policy under which a fresh clone
with no 1Password session still boots. It tries the store and falls back to the
declared `default:`. Its critical property is that it is never silent: every
pull prints which names came from the store and which fell back, by name and
never by value. A developer who believes Sentry and Google sign-in are
configured, when wet actually wrote empty strings, debugs the wrong thing for an
hour — so the summary is the feature, not decoration.

**`required`** exists for the opposite need: a project that must be certain the
real credential is what landed in the file. Under it a declared `default:`
deliberately does *not* substitute for a store failure. A default that papered
over a missing session would make `required` mean nothing.

**`disabled`** never contacts the store at all, so offline work and tests are
deterministic and prompt-free. Declared defaults must then satisfy every
selected secret — a secret with no default under `disabled` is an error, not an
empty string.

The policy is a config value with a per-run override (`wet env pull
--secrets=…`) rather than a flag alone, because "how strict is this repo about
secrets" is a fact about the repo, while "I am on a plane" is a fact about this
invocation.

An empty-string `default:` is a first-class value, distinct from having no
default. `SENTRY_DSN: { field: sentry_dsn, default: "" }` says "this provider
is deliberately unconfigured locally"; omitting `default:` says "there is no
safe stand-in, tell me loudly". Collapsing the two would silently turn every
missing credential into an empty one.

## Why `setup` and `pull` share one pipeline

`wet setup` owns lifecycle — registration, slot, ports, pools, handles,
`.env.worktree`, hooks. It owns no second way to write an env file: once the
lifecycle work is done it calls the same internal pull that `wet env pull`
calls, before hooks run so a hook that boots the stack finds its files already
written.

This is a fix for a bug that actually happened. The previous design had setup
render targets itself while `wet env populate --all` did its own thing, and the
two diverged — only one of them read a target's secrets — so the file setup
wrote and the file a re-render wrote were not the same file. Two entry points
for one repository-wide operation is a standing invitation to that divergence,
which is why `wet env populate` was removed rather than kept beside `pull`.

The converse holds too: `wet env pull` performs no lifecycle work at all. It
does not register a worktree, allocate or change a slot, ports or pool items,
write `.env.worktree`, or run hooks. That restraint is what makes it safe to
re-run at any time, which is what makes it usable as the repair command.

## Rendered outputs are wet's to own

In the main checkout as much as in a worktree. Re-generating is how you repair a
checkout, so it must not need a flag. Per-machine differences that once
justified hand-editing the output — a LAN IP, a staging hostname — belong in
`.env.local` or `.wet.local.yaml`, where a re-pull preserves them instead of
destroying them.

Every generated file says so in its first line (`# Generated by wet. Do not
edit — change outputs: in .wet.yaml.`), because the alternative is someone
patching a value into a file that the next `wet setup` silently reverts.

The write itself is atomic — a sibling temp file, created `0600` whatever the
final mode, chmodded, then renamed over the target. A truncate-and-write would
leave half an env file on disk if the process died mid-write, and the reader of
that file is a booting app. Creating the temp file `0600` first means a secret
is never briefly world-readable between create and chmod.

## Why generating beats copying a generated `.env`

Copying a generated `.env` into each worktree is the weak link in the flow: the
copy snapshots one machine's values, then drifts from the checked-in shape and
carries the *previous* worktree's ports. A declared output has neither problem —
the shape is reviewed like code, in `.wet.yaml`, and the values are always this
slot's.

## Why a value with a newline is rejected rather than escaped

dotenv dialects disagree about multi-line values: some accept `"a\nb"`, some
accept a real newline inside quotes, some accept neither. A value that
half-parses is worse than one that fails — it puts the tail of a private key on
a line the reader treats as the next assignment, and the error surfaces
somewhere else entirely. So wet refuses, and says to store PEM material
base64-encoded on a single line.

Relatedly, an unresolvable reference is fatal rather than an empty value:
`port=` in a strictly-typed loader is a silent boot failure a long way
downstream.

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

Four derivations of one fact, in three languages. Adding one generated file per
package doesn't fix it by itself — each one could still rebuild `AUTH_BASE_URL`
from the port. That's why `env:` is the derivation layer: entries may reference
each other, resolved to a fixed point rather than in sorted order, so every
package's output just names `AUTH_BASE_URL` instead of rebuilding it,
`docker-compose.yml` reads it from `.env.worktree` instead of interpolating it,
and the `sed` hook disappears.

Renaming lives in the output, not in the derivation: mobile's `port:
{ from: GRPC_PORT }` is a fact about what that app's loader calls the value, and
keeping it there means `env:` never grows a per-consumer alias.

## Don't resolve emulator serials in a hook

`wet setup` boots any claimed emulator that isn't running and resolves its
serial before generating anything. The obvious shell version —
`flutter emulators --launch` then `adb wait-for-device` — fails with "error:
more than one device/emulator" as soon as a second device is attached, which is
the normal state once you run worktrees in parallel. wet resolves by asking each
console its AVD name, which is the only method that survives a crowd.

## Why a second device is an extra claim, not a second pool

Some work needs two phones on one backend: a chat between two accounts, a
share flow. Two worktrees cannot do it. They are two stacks and two databases,
so the two accounts never see each other, which is the only thing the test is
for.

Two pools cannot do it either. `devices` and `devices_2` would be sized and
filled separately, and a worktree wanting one device would have to claim from
both or from neither.

The second device is another claim on the pool the first came from, recorded
in the worktree's state entry as `pool_extras`. The pool stays one list of
interchangeable devices, sized by how many devices the machine has.

State keeps the primary apart from the extras instead of holding one flat
list. `{{.Handles.devices}}`, `$POOL_HANDLE_DEVICES` and `from: Handles.devices`
each render one string, which is what `flutter run -d` takes. A flat list
breaks every config that renders a single device. Extras therefore reach
templates and hooks only through the list forms, `{{.AllHandles.devices}}` and
`$POOL_HANDLE_DEVICES_ALL`. A one-device worktree carries those too, one entry
long, so a template written for several devices is correct on all of them.

The code that reasons about claims rather than about the device reads both
together: the allocator, the exhaustion message, `reverse-tcp`, and pop.

## Why pop leaves a claimed emulator running

`wet mobile remove` kills the emulator it releases. `wet worktree pop` does not.

wet boots emulators because it has to. An AVD has no serial until something
starts it, and every rendered file needs that serial. Stopping one is a
separate decision that belongs to the project, and `pre_remove` is where it
lived before extras existed. It stays there.

`remove` is the exception. You named one device and asked for it gone, so it
goes, and `--keep-running` releases the claim while leaving the emulator up.

Pop still defers to the hook, so write the hook to loop over
`$POOL_HANDLE_DEVICES_ALL`. Killing `$POOL_HANDLE_DEVICES` alone leaves every
added emulator running.

## Why allocation holds a lock

Two `wet setup` runs started at the same moment used to hand out the same slot,
the same ports and the same device: the state file was read-modify-write, and
the claim was only written after the hooks, leaving an image-build-long window
in which a taken slot looked free. Allocation now happens under a lock
(`.wet-worktrees.lock`, beside the state file), and the claim is written before
the hooks run. The lock never spans the hooks — concurrent stack boots are the
point.

## Why the main checkout runs the same pipeline

Everything a worktree wants, the main checkout wants too — generated env files, a
claimed device, a recorded slot — because it is a working checkout as well. An
emulator that got deleted should be re-claimable there by re-running setup, not
hand-fixed. So main runs the same pipeline, differing only where a difference is
forced.

That is also why main claims pool items normally and keeps them, rather than
being exempted: it is the honest accounting. That device really is in use, and a
pool that hid main's claim would hand the same device to a worktree.

## Why `.wet.local.yaml` exists, and where it is read from

Some of the config isn't a fact about the repo, it's a fact about a workstation.
A `pools:` list of device IDs is the clearest case: `RFCR921L4DP` means nothing
on a teammate's machine, so a committed pool either gets hand-edited by everyone
or gets committed wrong. That's what makes an otherwise shareable `.wet.yaml`
unshareable.

The same reasoning puts the 1Password account and vault there. Which account you
are signed into is a fact about you; which credentials the project needs and
what each one is called is a fact about the project. So `secrets.definitions:`
is committed, while `secrets.account` and `secrets.vault` are the two keys the
overlay carries.

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

## Why the secrets map lives in `.wet.yaml`

The store coordinates and the credential map sit in `.wet.yaml`, and wet talks
to 1Password directly. That is a deliberate reversal of wet's older position,
which was that wet should own no vault code path at all and secrets should
arrive only as opaque values piped in from a producer script.

What changed the balance is that the old boundary bought purity at the cost of
a second source of truth. The key list had to live somewhere the producer
script could read, the generated files had to be described again in
`.wet.yaml`, and nothing connected the two: a key could exist in the vault and
be missing from the output, or a mapping could name a consumer that no file
corresponded to. The failure mode was always the same shape — two files
agreeing at the moment they were written and drifting afterwards.

`secrets:` is still optional. A repo that generates only ordinary values
declares `outputs:` with no `secrets:` list anywhere, and wet contacts no store.

What the block buys, beyond one file:

- Each credential is defined once, by logical name, and selected per output. The
  config already says which env file consumes which credential, so nothing has
  to restate it — `wet env explain` reads it straight off, and `wet env push`
  derives its authorization boundary from the same list.
- Definitions are storage-agnostic labels. `GOOGLE_CLIENT_ID` is one credential
  whether the API or the app reads it, so the vault holds no per-consumer
  duplicates and a rotation is one edit. The vault never learns which consumer
  reads what.
- Validation happens at load: an output selecting a secret that isn't defined,
  a secret colliding with a port, a definition with neither a resolvable
  reference nor a default — all errors at `wet setup` time, where the old
  arrangement surfaced them as an unresolvable template field much later.

The cost is real and worth stating: wet now shells out to `op`, so the binary
is in the path of a credential fetch. It holds no credential of its own — every
call is `op` with the user's existing session, and values live in memory for
the length of one pull — but the property "wet cannot leak a secret because
it never has one" is now "wet handles secrets for as long as it takes to write
them to a 0600 file".

## Why a secret may not share a name with a port or an `env:` entry

Both collisions are load errors rather than a precedence rule, because there is
no precedence that isn't a trap.

A secret named `APP_PORT` winning would hand two worktrees the same port —
slot allocation exists precisely so that cannot happen, and a value from a vault
is the last thing that should be able to undo it. A secret sharing a name with
an `env:` entry makes the output key ambiguous, and the ambiguity has a real
source: under the old schema a development fallback lived in `env:` while the
vault mapping lived elsewhere under the same name. The new schema removes the
need — the fallback goes in the definition's `default:`, where the credential
is already described — so the collision is a leftover, and wet says so.
