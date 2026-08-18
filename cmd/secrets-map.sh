#!/usr/bin/env bash
#
# Shared loader for a wet secrets map. Sourced by wet-secrets.sh and
# wet-secrets-provision.sh.
#
# The map is `wet.<provider>.json` — the middle segment names the store, so a
# repo can hold wet.op.json next to wet.az.json and each producer reads its
# own. The format is JSON because both scripts already depend on jq; a
# hand-rolled format would mean a hand-rolled parser in every producer.
#
# secrets_map_load <file> <provider> [group...]
#   Fills the parallel arrays SECRETS_KEYS / SECRETS_REFS / SECRETS_GROUPS
#   with the entries of the named groups, or every entry when no group is
#   named. Order follows the file, so output is stable across runs.
#
#   Reference forms, in the JSON values:
#     "field"                  -> op://<vault>/<item>/field   (the defaults)
#     "Other Item/field"       -> op://<vault>/Other Item/field
#     "op://vault/item/field"  -> used verbatim
#
#   <vault>/<item> come from OP_VAULT/OP_ITEM when set (flag or environment),
#   otherwise from the map's own "vault"/"item" keys. That order matters: the
#   committed file names the shared default, the environment overrides it.

secrets_map_load() {
  local file="$1" provider="$2"; shift 2
  local wanted=()
  if [ $# -gt 0 ]; then wanted=("$@"); fi   # `a && b` would trip set -e

  [ -r "$file" ] || { echo "secrets map not readable: $file" >&2; return 1; }

  # The filename is the provider declaration, so a producer reading the wrong
  # map fails here rather than resolving op:// references with `az`.
  local base="${file##*/}"
  case "$base" in
    wet."$provider".json) ;;
    *) echo "$file: not a wet.$provider.json map (this producer speaks $provider)" >&2
       return 1 ;;
  esac

  if ! jq -e . "$file" >/dev/null 2>&1; then
    echo "$file: not valid JSON" >&2
    return 1
  fi

  local vault item
  vault="${OP_VAULT:-$(jq -r '.vault // empty' "$file")}"
  item="${OP_ITEM:-$(jq -r '.item // empty' "$file")}"
  if [ -z "$vault" ] || [ -z "$item" ]; then
    echo "$file: needs \"vault\" and \"item\" (or OP_VAULT/OP_ITEM set)" >&2
    return 1
  fi

  local known
  known="$(jq -r '.groups | keys_unsorted[]' "$file")"
  local want found group
  for want in ${wanted[@]+"${wanted[@]}"}; do
    found=0
    while IFS= read -r group; do
      [ "$want" = "$group" ] && { found=1; break; }
    done <<<"$known"
    if [ "$found" -eq 0 ]; then
      echo "$file: unknown group: $want (known: $(echo "$known" | tr '\n' ' '))" >&2
      return 1
    fi
  done

  # Selection and reference expansion happen in jq, where the values are typed
  # data rather than words the shell might split. Only the key, ref and group
  # cross back, tab-separated — none of which can contain a tab.
  # Built with jq -n so an empty selection is [] and not [""] — printf pipes
  # one empty line when the array is empty, which would select no groups.
  local want_json='[]'
  if [ "${#wanted[@]}" -gt 0 ]; then
    want_json="$(printf '%s\n' "${wanted[@]}" | jq -R . | jq -s .)"
  fi

  local rows
  rows="$(
    jq -r --arg vault "$vault" --arg item "$item" --argjson want "$want_json" '
      .groups
      | to_entries[]
      | select(($want | length) == 0 or (.key | IN($want[])))
      | .key as $group
      | .value
      | to_entries[]
      | [ .key,
          ( if   (.value | startswith("op://")) then .value
            elif (.value | test("/"))          then "op://\($vault)/\(.value)"
            else                                    "op://\($vault)/\($item)/\(.value)"
            end ),
          $group ]
      | @tsv
    ' "$file"
  )" || return 1

  SECRETS_KEYS=(); SECRETS_REFS=(); SECRETS_GROUPS=()
  local key ref
  while IFS=$'\t' read -r key ref group; do
    [ -n "$key" ] || continue
    if [ -z "$ref" ]; then
      echo "$file: $key has an empty reference" >&2
      return 1
    fi
    SECRETS_KEYS+=("$key")
    SECRETS_REFS+=("$ref")
    SECRETS_GROUPS+=("$group")
  done <<<"$rows"

  if [ "${#SECRETS_KEYS[@]}" -eq 0 ]; then
    echo "$file: no entries selected" >&2
    return 1
  fi
}

secrets_map_groups() {
  jq -r '.groups | keys_unsorted[]' "$1"
}
