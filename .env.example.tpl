# .env.example.tpl — the template half of cmd/wet.example.yaml.
#
# Checked into git and rendered per checkout by the `render:` list in
# cmd/wet.example.yaml — via `wet setup`, or on demand:
#
#   wet env populate --all
#   wet env populate --all --dry-run      # show every file, write nothing
#
# Main repo renders it at slot 0 (the declared base ports); each worktree
# renders it at its own slot. That is the whole point of a template over a
# copied .env — the copy would carry whichever ports the source checkout had.

# ─────────────────────────────────────────────────────────────────────────
# Derived values — computed ONCE in .wet.yaml's env: block and consumed here.
#
# Note what this file does NOT do: rebuild the URL from {{"{{"}}.APP_PORT}} itself.
# In a monorepo every package's template would repeat that derivation, and
# they would drift. Derive in env:, consume here.
# ─────────────────────────────────────────────────────────────────────────
APP_URL={{.APP_URL}}
CALLBACK_URL={{.CALLBACK_URL}}

# ─────────────────────────────────────────────────────────────────────────
# Raw ports — one field per `ports:` entry in .wet.yaml.
# Slot 0 → 4000, slot 1 → 4010, … (port_stride: 10)
# ─────────────────────────────────────────────────────────────────────────
DATABASE_URL=postgres://postgres:postgres@localhost:{{.DB_PORT}}/app_dev
GRPC_ADDR=localhost:{{.GRPC_PORT}}

# Per-port stride overrides work the same way (DEBUG_PORT strides by 5).
DEBUG_PORT={{.DEBUG_PORT}}

# ─────────────────────────────────────────────────────────────────────────
# Identity — Branch is the raw name, BranchSafe is flattened for Docker.
# ─────────────────────────────────────────────────────────────────────────
COMPOSE_PROJECT_NAME={{.COMPOSE_PROJECT_NAME}}
WORKTREE_SLOT={{.Slot}}
WORKTREE_BRANCH={{.Branch}}

# ─────────────────────────────────────────────────────────────────────────
# Pools — the exclusive resource this checkout claimed.
# ─────────────────────────────────────────────────────────────────────────
FLUTTER_DEVICE={{.Pools.devices}}
TEST_FIXTURE={{.Pools.fixtures}}

# ─────────────────────────────────────────────────────────────────────────
# Secrets — NOT known to wet. These come from --secrets-from, so this file
# renders only when the hook supplies them:
#
#   op item get myapp --format json | jq 'from_entries' \
#     | wet env populate .env.example.tpl -o .env --secrets-from -
#
# An unresolved field is a hard error, never an empty value — so a missing
# secret fails the render instead of booting the app with SENTRY_DSN=.
#
# NOTE: `#` is a comment to the env file, NOT to the template engine. A
# commented-out line containing a field still renders — and still fails if
# the field is missing. To park a field without rendering it, wrap it in a Go
# template comment, as the block just below does.
# ─────────────────────────────────────────────────────────────────────────
{{/* Uncomment once the hook passes --secrets-from:
SENTRY_DSN={{.SENTRY_DSN}}
*/}}
