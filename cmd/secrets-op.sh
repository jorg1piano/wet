#!/usr/bin/env bash
#
# wet secrets producer — 1Password.
#
# Prints one flat JSON object on stdout: {"KEY": "value", ...}. That is exactly
# what `wet env populate --secrets-from -` consumes, so the whole integration
# is one pipe:
#
#   ./scripts/wet-secrets.sh | wet env populate backend/.env.tpl -o backend/.env --secrets-from -
#
# Which keys it produces is NOT hardcoded here — it reads wet.op.json, the same
# map wet-secrets-provision.sh uses to create the fields. One list, two
# consumers: a key can never exist in the vault but be missing from the render.
# The middle segment of the filename is the store, so a repo that later adds
# Azure keeps wet.az.json beside it and this script stays 1Password-only.
#
# The vault is a parameter, not a hardcoded name, so the same committed script
# works against a personal vault locally and a shared one in CI. Nothing here is
# 1Password-specific beyond the `op read` line — swap it for `az keyvault secret
# show` or `pass` and the rest still holds.
#
# Usage:
#   wet-secrets.sh [--vault V] [--item I] [--account A] [--map wet.op.json] [group...]
#
# Examples:
#   # every key in the map
#   ./scripts/wet-secrets.sh
#
#   # only what the OAuth providers need
#   ./scripts/wet-secrets.sh core google apple
#
#   # a shared vault and a named account
#   ./scripts/wet-secrets.sh \
#       --vault fantasy_basketball_development \
#       --account my.1password.com
#
#   # what it would fetch, without fetching it
#   ./scripts/wet-secrets.sh --dry-run

set -euo pipefail

OP_VAULT="${OP_VAULT:-Development}"
OP_ITEM="${OP_ITEM:-myapp}"
OP_ACCOUNT="${OP_ACCOUNT:-}"
SECRETS_MAP="${SECRETS_MAP:-wet.op.json}"
DRY_RUN=0

usage() { sed -n '2,34p' "$0" | sed 's/^# \{0,1\}//'; }

groups=()
while [ $# -gt 0 ]; do
  case "$1" in
    --vault)   OP_VAULT="${2:?--vault needs a value}"; shift 2 ;;
    --item)    OP_ITEM="${2:?--item needs a value}"; shift 2 ;;
    --account) OP_ACCOUNT="${2:?--account needs a value}"; shift 2 ;;
    --map)     SECRETS_MAP="${2:?--map needs a value}"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    -*)        echo "$0: unknown flag: $1" >&2; exit 2 ;;
    *)         groups+=("$1"); shift ;;
  esac
done

for bin in op jq; do
  command -v "$bin" >/dev/null 2>&1 || { echo "$0: $bin is not installed" >&2; exit 1; }
done

# shellcheck source=secrets-map.sh
. "$(dirname "$0")/secrets-map.sh"
secrets_map_load "$SECRETS_MAP" op ${groups[@]+"${groups[@]}"}

if [ "$DRY_RUN" -eq 1 ]; then
  for i in "${!SECRETS_KEYS[@]}"; do
    echo "${SECRETS_KEYS[$i]} = ${SECRETS_REFS[$i]}"
  done
  exit 0
fi

op_args=(read --no-newline)
if [ -n "$OP_ACCOUNT" ]; then
  op_args+=(--account "$OP_ACCOUNT")
fi

# One `op read` per key, assembled with `jq --arg`, rather than one `op inject`
# over a JSON template: jq escapes quotes, backslashes and newlines correctly,
# where op inject splices the raw secret into the template and a value holding
# a `"` (or a PEM key) would produce invalid JSON. op keeps the session warm,
# so the extra round trips cost milliseconds.
args=()
filter="{}"
for i in "${!SECRETS_KEYS[@]}"; do
  # `op read` exits non-zero on a missing field, and set -e stops here — a
  # silently absent secret would otherwise render an empty env value.
  value="$(op "${op_args[@]}" "${SECRETS_REFS[$i]}")"
  args+=(--arg "k$i" "${SECRETS_KEYS[$i]}" --arg "v$i" "$value")
  filter="$filter + {(\$k$i): \$v$i}"
done

jq -n "${args[@]}" "$filter"
