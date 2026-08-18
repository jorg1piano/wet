#!/usr/bin/env bash
#
# wet secrets provisioner — 1Password.
#
# Creates the *fields* named in wet.op.json, so you never hand-click
# them into the UI. Values are left alone: a field that already has a value is
# never touched, a missing one is created empty (or prompted for with --fill),
# and you paste the real secret in later — from the provider's console or
# `op item edit`.
#
# It reads the same map as wet-secrets.sh and derives everything from the
# op:// references in it — vault, item, field label — so whether you keep one
# item per app or one item per service is a fact about the map, not about this
# script. Adding an OAuth provider is one group in the JSON; no code changes.
#
# Usage:
#   wet-secrets-provision.sh [--vault V] [--item I] [--account A]
#                            [--map wet.op.json] [--fill] [--dry-run] [group...]
#
# Examples:
#   # every key in the map
#   ./scripts/wet-secrets-provision.sh
#
#   # just one provider's fields
#   ./scripts/wet-secrets-provision.sh google
#
#   # a named vault and 1Password account
#   ./scripts/wet-secrets-provision.sh google apple \
#       --vault fantasy_basketball_development \
#       --account my.1password.com
#
#   # prompt for each value instead of creating the fields empty
#   ./scripts/wet-secrets-provision.sh apple --fill
#
#   # show what it would create, touching nothing
#   ./scripts/wet-secrets-provision.sh --dry-run
#
# Redirect URIs are deliberately NOT in the map. `{{.HYDRA_PUBLIC_URL}}/callback/google`
# is derived per worktree by wet's env: block — putting it in the vault would
# pin every checkout to one URL.

set -euo pipefail

OP_VAULT="${OP_VAULT:-Development}"
OP_ITEM="${OP_ITEM:-myapp}"
OP_ACCOUNT="${OP_ACCOUNT:-}"
SECRETS_MAP="${SECRETS_MAP:-wet.op.json}"
FILL=0
DRY_RUN=0

usage() { sed -n '2,41p' "$0" | sed 's/^# \{0,1\}//'; }

groups=()
while [ $# -gt 0 ]; do
  case "$1" in
    --vault)   OP_VAULT="${2:?--vault needs a value}"; shift 2 ;;
    --item)    OP_ITEM="${2:?--item needs a value}"; shift 2 ;;
    --account) OP_ACCOUNT="${2:?--account needs a value}"; shift 2 ;;
    --map)     SECRETS_MAP="${2:?--map needs a value}"; shift 2 ;;
    --fill)    FILL=1; shift ;;
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

# op://vault/item/field — anything else this script cannot provision, and
# guessing would create fields in the wrong place.
ref_part() {
  local ref="$1" n="$2" rest
  case "$ref" in op://*) rest="${ref#op://}" ;; *) return 1 ;; esac
  case "$rest" in */*/*) ;; *) return 1 ;; esac
  case "$n" in
    vault) echo "${rest%%/*}" ;;
    item)  rest="${rest#*/}"; echo "${rest%%/*}" ;;
    field) echo "${rest##*/}" ;;
  esac
}

# Group the selected entries by (vault, item) so each item is fetched and
# edited exactly once, however the map is laid out.
targets=()
for i in "${!SECRETS_KEYS[@]}"; do
  ref="${SECRETS_REFS[$i]}"
  if ! vault=$(ref_part "$ref" vault) || ! item=$(ref_part "$ref" item); then
    echo "$SECRETS_MAP: ${SECRETS_KEYS[$i]}: not an op://vault/item/field reference: $ref" >&2
    exit 1
  fi
  key="$vault|$item"
  found=0
  for t in ${targets[@]+"${targets[@]}"}; do
    [ "$t" = "$key" ] && { found=1; break; }
  done
  [ "$found" -eq 0 ] && targets+=("$key")
done

for target in "${targets[@]}"; do
  vault="${target%%|*}"; item="${target#*|}"

  op_args=(--vault "$vault")
  [ -n "$OP_ACCOUNT" ] && op_args+=(--account "$OP_ACCOUNT")

  echo "item \"$item\" in vault $vault"

  if [ "$DRY_RUN" -eq 1 ]; then
    item_json='{"fields":[]}'
  elif ! item_json=$(op item get "$item" "${op_args[@]}" --format json 2>/dev/null); then
    echo "  creating item"
    item_json=$(op item create --category "API Credential" --title "$item" \
      "${op_args[@]}" --format json)
  fi

  edits=()
  for i in "${!SECRETS_KEYS[@]}"; do
    ref="${SECRETS_REFS[$i]}"
    [ "$(ref_part "$ref" vault)|$(ref_part "$ref" item)" = "$target" ] || continue
    env_key="${SECRETS_KEYS[$i]}"
    label="$(ref_part "$ref" field)"

    # A field counts as present only when it holds a non-empty value; an
    # existing empty field is still a candidate for --fill.
    if jq -e --arg l "$label" \
         '[.fields[]? | select(.label == $l) | .value // ""] | any(. != "")' \
         >/dev/null <<<"$item_json"; then
      echo "  keep    $label (already set) -> $env_key"
      continue
    fi
    if [ "$FILL" -eq 1 ] && [ "$DRY_RUN" -eq 0 ]; then
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
    continue
  fi
  if [ "$DRY_RUN" -eq 1 ]; then
    echo "  (dry run — nothing written)"
    continue
  fi
  op item edit "$item" "${op_args[@]}" "${edits[@]}" --format json >/dev/null
done

# The .env template is checked in and hand-authored, so the script prints the
# lines rather than editing it: appending would duplicate on a re-run, and the
# template's ordering and comments are yours.
echo
echo "paste into your .env template, then fill the empty fields in 1Password:"
for key in "${SECRETS_KEYS[@]}"; do
  echo "  ${key}={{.${key}}}"
done
echo
echo "render with:"
echo "  ./scripts/wet-secrets.sh | wet env populate backend/.env.tpl -o backend/.env --secrets-from -"
