# Design: per-deploy-environment values

Status: **rejected**, and now partly **superseded**. Recorded so the idea is not
re-proposed.

> **Written against an older schema.** This proposal predates the inline
> `outputs:` model. It talks about a per-target `environment:` key on `render:`,
> and `render:` no longer exists — generated env files are declared inline under
> `outputs:`, and secrets are `secrets.definitions:` selected per output. Read
> the mechanics as historical.
>
> The *conclusion* survives the schema change intact, and so does its reasoning:
> there is still no `environments:` block, `outputs:` entries still carry no
> `environment:` key, and wet still configures local checkouts only. If anything
> the case is stronger now — `.wet.yaml` is the single inventory of what this
> checkout generates, and a block describing what some *other* machine runs
> would be the one thing in the file that isn't about this checkout.
>
> One piece has since been half-fixed and one is now designed but unbuilt; both
> are called out in "What was worth keeping" below.

## The question

A repo needs values that differ per deployed environment: a public hostname,
an error-reporting environment name, an OIDC issuer, redirect URIs, the vault
item holding that environment's secrets. Where do they live?

The proposal was a new `environments:` block in `.wet.yaml`, holding named sets
of literal values, selected by a per-target `environment:` key on `render:`.

## Why it was rejected

**Deployed config already has two homes, and both are closer to where the
values are consumed.** A typical PaaS deployment models environments as
separate apps with separate config files, so:

- Non-secret per-environment values belong in that per-app config — committed,
  diffable, reviewed in the same change as the code that reads them.
- Secret or environment-varying values belong in the platform's secret store,
  loaded from whatever vault the team already uses.

An `environments:` block would be a third home for those same values, and the
one furthest from the thing that reads them. Two sources of truth for a
hostname is how a staging redirect URI ends up pointing at production.

**It also mis-scoped wet.** wet configures local worktrees. Its `env:` block
derives values from *this checkout's* slot and ports — that is the whole point
of the tool. What runs in a deployed app is the deploy pipeline's concern, and
the deploy platform already has a notion of environment. Teaching wet a second,
parallel one buys nothing and has to be kept in sync with the real one by hand.

This is the same rule that keeps every other project-specific concept out of
wet: the tool contains no project knowledge, and `.wet.yaml` is the entire
contract.

## What to do instead

|              | non-secret                                    | secret                                |
| ------------ | --------------------------------------------- | ------------------------------------- |
| **deployed** | the per-app config of the deploy platform      | the platform's secret store, from a vault |
| **local**    | wet `env:` — derived from this slot and ports  | `secrets.definitions:`, selected by an `outputs:` entry |

Nothing in that table is a new wet feature. The only missing arrow is the one
into the platform's secret store: something must read the vault and push the
values. That belongs in the consuming repo as a `just` recipe, not in
`.wet.yaml` — wet does not deploy, and a block named for deployment would imply
it does.

The inline-outputs design does sketch one narrow extension in this area: a
`deployments:` block emitting a file of `op://vault/item/field` *references*
(not values) from the same `secrets.definitions:`, to replace a hand-maintained
`secrets.tpl` that today repeats both the variable names and the field names.
That is not implemented, and it is not the rejected idea: it adds no second
home for values, because it emits no values — it is the canonical definition
list rendered in a different notation. If it is ever built, it must stay that.

## What this does NOT rule out: `secrets.vaults:`

wet now accepts a list of per-environment vaults in place of a single
`vault:`, with `default_env:` choosing between them and `--env` overriding for
one run. That is not the rejected idea returning under another name, and the
distinction is the whole argument above:

- It holds **no values** — only which vault a lookup reads from. The keys, the
  fields and the item are unchanged across environments; `secrets.definitions:`
  stays the single list.
- It is **committed**, for exactly the reason `vault:` and `item:` are: which
  vault holds a given environment's credentials is a fact about the project,
  the same for everyone who clones the repo. `.wet.local.yaml` can override it
  like any other key, but it is not that file's business.
- It configures **this checkout**, not a deployed app. `wet env pull --env
  production` fills a local file from production credentials; nothing about it
  describes what some other machine runs.

An `environments:` block of literal hostnames and issuers would still be a
second home for values, and is still rejected.

## What was worth keeping

Two prerequisites from the rejected design stand on their own:

**Unknown config keys are still silently ignored — mostly.**
`LoadWetConfigWithLocal` decodes with plain `yaml.Unmarshal` and no
`KnownFields(true)`. An unknown top-level key is dropped without comment — and
so is a typo in a *known* key. Write `pool-grow` for `pool_grow`, or `output:`
for `outputs:`, and wet loads the file cleanly and quietly does nothing.

`rejectRemovedKeys` has since closed exactly two holes: `render:` and
`secrets.sections:` are named explicitly and fail the load with a migration
message, because a config that still parses, generates nothing, and leaves a
stale `.env.dev` on disk looking current is the worst possible outcome of a
schema removal. That is the whole argument for general strictness, applied to
two keys.

The general footgun remains, and the failure it produces is the worst kind: a
config that looks right, loads without error, and behaves as though half of it
were not there. Unknown keys should fail at load, naming the key and the file it
came from — which requires checking both halves of a merged `.wet.yaml` +
`.wet.local.yaml` before they are marshalled back together and lose their
provenance.

**A generated output should be able to say it is client-facing.** The rejected
design worried about values crossing a boundary they shouldn't; `outputs:` now
makes the local half of that boundary explicit, since an output sees only the
secrets it names. What it cannot yet do is *assert* that a given output must see
none — the inline-outputs design proposes an optional `audience: client`
classification, under which selecting any secret is an error rather than a thing
a reviewer has to notice. Not implemented. Until it is, "the mobile output has
no `secrets:` list" is a convention that holds only as long as everyone reading
the diff is awake.
