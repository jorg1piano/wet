#!/usr/bin/env bash
#
# wet secrets provisioner — 1Password.
#
# Creates the *fields* an OAuth provider needs, so you never hand-click them
# into the UI. Values are left alone: a field that already has a value is never
# touched, a missing one is created empty (or prompted for with --fill), and you
# paste the real secret in later — from the provider's console or `op item edit`.
#
# Usage:
#   wet-secrets-provision.sh [--vault V] [--account A] [--fill] <provider>...
#   wet-secrets-provision.sh --list
#
# Examples:
#   # one provider, default vault
#   ./scripts/wet-secrets-provision.sh google
#
#   # several providers into a named vault
#   ./scripts/wet-secrets-provision.sh google apple \
#       --vault fantasy_basketball_development
#
#   # pick the 1Password account when you are signed in to more than one
#   ./scripts/wet-secrets-provision.sh google \
#       --vault fantasy_basketball_development \
#       --account my.1password.com
#
#   # prompt for each value instead of creating the fields empty
#   ./scripts/wet-secrets-provision.sh apple --fill
#
#   # show the providers and the op:// references they map to
#   ./scripts/wet-secrets-provision.sh --list
#
# One item per service, the way a vault is normally laid out — a "Google OAuth"
# item with a `client_id` field, not one myapp item holding GOOGLE_CLIENT_ID.
# The env key lives only in the mapping below, which is also what the producer
# (wet-secrets.sh) needs: it prints the matching op:// references with --list.
#
# Redirect URIs are deliberately NOT here. `{{.HYDRA_PUBLIC_URL}}/callback/google`
# is derived per worktree by wet's env: block — putting it in the vault would
# pin every checkout to one URL.
#
# To add a provider: append one PROVIDERS line. Nothing else changes.

set -euo pipefail

OP_VAULT="${OP_VAULT:-Development}"
OP_ACCOUNT="${OP_ACCOUNT:-}"
FILL=0

# provider|item title|ENV_KEY=field_label ENV_KEY=field_label ...
PROVIDERS=(
  "google|Google OAuth|GOOGLE_CLIENT_ID=client_id GOOGLE_CLIENT_SECRET=client_secret"
  "apple|Apple Sign In|APPLE_SERVICE_ID=service_id APPLE_TEAM_ID=team_id APPLE_KEY_ID=key_id APPLE_PRIVATE_KEY_PEM_BASE64=private_key_pem_base64"
)

usage() { sed -n '2,42p' "$0" | sed 's/^# \{0,1\}//'; }

entry_for() {
  local want="$1" entry
  for entry in "${PROVIDERS[@]}"; do
    [ "${entry%%|*}" = "$want" ] && { printf '%s\n' "$entry"; return 0; }
  done
  return 1
}

list_providers() {
  local entry rest item pairs pair
  for entry in "${PROVIDERS[@]}"; do
    rest="${entry#*|}"; item="${rest%%|*}"; pairs="${rest#*|}"
    echo "${entry%%|*}  ->  item \"$item\""
    for pair in $pairs; do
      echo "    ${pair%%=*} = op://${OP_VAULT}/${item}/${pair#*=}"
    done
  done
}

selected=()
while [ $# -gt 0 ]; do
  case "$1" in
    --vault)   OP_VAULT="${2:?--vault needs a value}"; shift 2 ;;
    --account) OP_ACCOUNT="${2:?--account needs a value}"; shift 2 ;;
    --fill)    FILL=1; shift ;;
    --list)    list_providers; exit 0 ;;
    -h|--help) usage; exit 0 ;;
    -*)        echo "$0: unknown flag: $1" >&2; exit 2 ;;
    *)         selected+=("$1"); shift ;;
  esac
done

if [ "${#selected[@]}" -eq 0 ]; then
  echo "$0: name at least one provider (see --list)" >&2
  exit 2
fi

for bin in op jq; do
  command -v "$bin" >/dev/null 2>&1 || { echo "$0: $bin is not installed" >&2; exit 1; }
done

# Validate every provider before touching the vault, so a typo in the second
# argument doesn't leave the first provider's item half-provisioned.
entries=()
for provider in "${selected[@]}"; do
  if ! entry=$(entry_for "$provider"); then
    echo "$0: unknown provider: $provider (known: $(printf '%s ' "${PROVIDERS[@]%%|*}"))" >&2
    exit 2
  fi
  entries+=("$entry")
done

op_args=(--vault "$OP_VAULT")
[ -n "$OP_ACCOUNT" ] && op_args+=(--account "$OP_ACCOUNT")

provision() {
  local item="$1" pairs="$2" item_json pair env_key label value
  local edits=()

  if ! item_json=$(op item get "$item" "${op_args[@]}" --format json 2>/dev/null); then
    echo "creating item \"$item\" in vault $OP_VAULT"
    item_json=$(op item create --category "API Credential" --title "$item" \
      "${op_args[@]}" --format json)
  else
    echo "item \"$item\" exists"
  fi

  for pair in $pairs; do
    env_key="${pair%%=*}"; label="${pair#*=}"
    # A field counts as present only when it holds a non-empty value; an
    # existing empty field is still a candidate for --fill.
    if jq -e --arg l "$label" \
         '[.fields[]? | select(.label == $l) | .value // ""] | any(. != "")' \
         >/dev/null <<<"$item_json"; then
      echo "  keep    $label (already set) -> $env_key"
      continue
    fi
    if [ "$FILL" -eq 1 ]; then
      printf '  value for %s (%s), empty to skip: ' "$label" "$env_key" >&2
      IFS= read -rs value; echo >&2
      if [ -n "$value" ]; then
        edits+=("$label[password]=$value")
        echo "  set     $label -> $env_key"
        continue
      fi
    fi
    edits+=("$label[password]=")
    echo "  create  $label (empty) -> $env_key"
  done

  if [ "${#edits[@]}" -eq 0 ]; then
    echo "  nothing to do"
    return 0
  fi
  op item edit "$item" "${op_args[@]}" "${edits[@]}" --format json >/dev/null
}

for entry in "${entries[@]}"; do
  rest="${entry#*|}"
  provision "${rest%%|*}" "${rest#*|}"
done

# The two files that consume these fields are checked in and hand-authored, so
# the script prints the lines rather than editing them: appending would
# duplicate on a re-run, and the template's ordering and comments are yours.
echo
echo "1. paste into SECRETS=() in scripts/wet-secrets.sh:"
for entry in "${entries[@]}"; do
  rest="${entry#*|}"; item="${rest%%|*}"
  for pair in ${rest#*|}; do
    echo "  \"${pair%%=*}=op://${OP_VAULT}/${item}/${pair#*=}\""
  done
done

echo
echo "2. paste into your .env template (redirect URIs derive from wet's env: block):"
for entry in "${entries[@]}"; do
  provider="${entry%%|*}"; rest="${entry#*|}"
  for pair in ${rest#*|}; do
    key="${pair%%=*}"
    echo "  ${key}={{.${key}}}"
  done
  echo "  $(echo "$provider" | tr '[:lower:]' '[:upper:]')_REDIRECT_URI={{.HYDRA_PUBLIC_URL}}/callback/${provider}"
done

echo
echo "then render:  ./scripts/wet-secrets.sh | wet env populate backend/.env.tpl -o backend/.env --secrets-from -"
