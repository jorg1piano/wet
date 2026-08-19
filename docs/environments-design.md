# Design: per-deploy-environment values

Status: **rejected.** Recorded so the idea is not re-proposed.

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
| **local**    | wet `env:` — derived from this slot and ports  | wet `secrets:` → rendered `.env`      |

Nothing in that table is a new wet feature. The only missing arrow is the one
into the platform's secret store: something must read the vault and push the
values. That belongs in the consuming repo as a `just` recipe, not in
`.wet.yaml` — wet does not deploy, and a block named for deployment would imply
it does.

## What was worth keeping

One prerequisite from the rejected design stands on its own and should still be
fixed:

**Unknown config keys are silently ignored.** `LoadWetConfigWithLocal` decodes
with plain `yaml.Unmarshal` and no `KnownFields(true)`. An unknown top-level key
is dropped without comment — and so is a typo in a *known* key. Write
`pool-grow` for `pool_grow`, or `renders:` for `render:`, and wet loads the file
cleanly and quietly does nothing.

That is a live footgun regardless of this feature, and the failure it produces
is the worst kind: a config that looks right, loads without error, and behaves
as though half of it were not there. Unknown keys should fail at load, naming
the key and the file it came from — which requires checking both halves of a
merged `.wet.yaml` + `.wet.local.yaml` before they are marshalled back together
and lose their provenance.
