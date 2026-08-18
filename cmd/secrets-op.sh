#!/usr/bin/env bash
#
# wet secrets producer — 1Password.
#
# Prints one flat JSON object on stdout: {"KEY": "value", ...}. That is exactly
# what `wet env populate --secrets-from -` consumes, so the whole integration
# is one pipe:
#
#   ./scripts/wet-secrets.sh | wet env populate web/.env.tpl -o web/.env --secrets-from -
#
# Copy this file into your repo (scripts/wet-secrets.sh), edit the SECRETS map
# below to name the keys your templates reference, commit it, and call it from
# a `post_create` hook. Nothing here is wet-specific beyond the output shape —
# swap the `op read` line for `az keyvault secret show`, `pass`, or anything
# else and the rest still holds.
#
# The vault is a parameter, not a hardcoded name, so the same committed script
# works against a personal vault locally and a shared one in CI:
#
#   OP_VAULT=Shared ./scripts/wet-secrets.sh
#   ./scripts/wet-secrets.sh --vault Shared --item myapp-staging

set -euo pipefail

OP_VAULT="${OP_VAULT:-Development}"
OP_ITEM="${OP_ITEM:-myapp}"
OP_ACCOUNT="${OP_ACCOUNT:-}"

while [ $# -gt 0 ]; do
  case "$1" in
    --vault)   OP_VAULT="${2:?--vault needs a value}"; shift 2 ;;
    --item)    OP_ITEM="${2:?--item needs a value}"; shift 2 ;;
    --account) OP_ACCOUNT="${2:?--account needs a value}"; shift 2 ;;
    -h|--help) sed -n '2,22p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "$0: unknown argument: $1" >&2; exit 2 ;;
  esac
done

# The keys this repo's templates reference, each mapped to a secret reference.
# `op://<vault>/<item>/<field>` — `op item get <item> --format json` lists the
# field names if you don't know them. Add a line per key; a template key with
# no line here fails the render loudly, which is the point.
SECRETS=(
  "DB_PASSWORD=op://${OP_VAULT}/${OP_ITEM}/db_password"
  "STRIPE_SECRET_KEY=op://${OP_VAULT}/${OP_ITEM}/stripe_secret_key"
)

for bin in op jq; do
  command -v "$bin" >/dev/null 2>&1 || {
    echo "$0: $bin is not installed" >&2
    exit 1
  }
done

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
i=0
for entry in "${SECRETS[@]}"; do
  key="${entry%%=*}"
  ref="${entry#*=}"
  if [ "$key" = "$entry" ] || [ -z "$key" ] || [ -z "$ref" ]; then
    echo "$0: malformed SECRETS entry: $entry (want KEY=op://vault/item/field)" >&2
    exit 1
  fi
  # `op read` exits non-zero on a missing field, and set -e stops here — a
  # silently absent secret would otherwise render an empty env value.
  value="$(op "${op_args[@]}" "$ref")"
  args+=(--arg "k$i" "$key" --arg "v$i" "$value")
  filter="$filter + {(\$k$i): \$v$i}"
  i=$((i + 1))
done

if [ "$i" -eq 0 ]; then
  echo "$0: SECRETS is empty — nothing to produce" >&2
  exit 1
fi

jq -n "${args[@]}" "$filter"
