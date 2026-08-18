package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// starterConfigTemplate is the body of a scaffolded .wet.yaml, with one %s for
// the derived project name.
//
// It is deliberately NOT the bundled cmd/wet.example.yaml. That file
// illustrates the whole schema against a fictional project — it renders
// .env.example.tpl and claims emulator: pool items — so a repo seeded with it
// verbatim fails its very next setup on a render target that does not exist.
// The annotated example is for reading; this is for running, and every live
// line here resolves against a fresh checkout.
//
// The env: block is not arbitrary house style. The shipped secret catalog
// already depends on it: the google group writes
// GOOGLE_REDIRECT_URI: '{{.HYDRA_PUBLIC_URL}}/callback/google', so a config
// without HYDRA_PUBLIC_URL fails to render the moment anyone scaffolds that
// group. Shipping the base URLs here is what makes the catalog usable.
//
// Backticks are spliced in rather than written inline, because this is a raw
// string literal.
const starterConfigTemplate = `# .wet.yaml — see the annotated schema for every key and what it does:
#   .claude/skills/wet/` + exampleConfigFileName + `  (after ` + "`wet install wet-skill`" + `)
#   https://github.com/devda/wet/blob/main/cmd/` + exampleConfigFileName + `

project_name: %s

# Each port advances by port_stride per worktree: slot N gets base + N*stride,
# so slot 1 of PHX_PORT is 4010. Rename or drop the ones you do not listen on —
# but anything referencing a port below has to keep it.
port_stride: 10

ports:
  - name: PHX_PORT
    base: 4000
  - name: HYDRA_PUBLIC_PORT
    base: 4444

env:
  # The address a client dials to reach this worktree's stack. Override
  # in a gitignored .env.local (DEV_HOST=192.168.x.y) when testing from a
  # physical phone on the LAN — every URL below follows it, and so does
  # docker-compose.yml, which reads ${DEV_HOST} from .env.worktree.
  DEV_HOST: "localhost"

  COMPOSE_PROJECT_NAME: "{{.ProjectName}}-{{.BranchSafe}}"

  # Derived once here so compose, the hooks, and the mobile Justfile all
  # read the same values out of .env.worktree instead of each rebuilding
  # them from PHX_PORT.
  AUTH_BASE_URL: "http://{{.DEV_HOST}}:{{.PHX_PORT}}"
  HYDRA_PUBLIC_URL: "http://{{.DEV_HOST}}:{{.HYDRA_PUBLIC_PORT}}"

# Uncomment for a Flutter project with emulators. Both halves are needed:
# FLUTTER_DEVICE_ID reads the pool, so declaring it without the pool fails the
# next render. A pool of emulator: items also makes setup boot an AVD, which is
# why this stays off until you ask for it.
#
# pools:
#   devices:
#     - emulator:pixel-a
#     - emulator:pixel-b
#
# env:
#   # The live adb serial / device id, not the pool item: an emulator item
#   # names an AVD, and flutter run -d needs the serial the running emulator
#   # answers to. wet boots the AVD during setup and resolves it here.
#   # {{.Pools.devices}} still holds the durable item, which is what
#   # pre_remove reasons about.
#   FLUTTER_DEVICE_ID: "{{.Handles.devices}}"
`

// scaffoldStarterConfig writes a starter .wet.yaml at dir and returns its
// path. It refuses to touch an existing file: the only caller reaches here
// because the config was missing, so finding one means something changed
// underneath us and overwriting it would destroy the user's work.
func scaffoldStarterConfig(dir string) (string, error) {
	path := filepath.Join(dir, wetConfigFileName)
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("%s already exists — refusing to overwrite it", path)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("stat %s: %w", path, err)
	}

	body := fmt.Sprintf(starterConfigTemplate, defaultProjectName(dir))
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

var projectNameStripRE = regexp.MustCompile(`[^a-z0-9_-]+`)

// defaultProjectName derives a valid project_name from the checkout's
// directory name, so the scaffolded config is usable without an edit in the
// common case. project_name feeds COMPOSE_PROJECT_NAME, so it has to satisfy
// projectNameRE; anything that cannot be coerced into that shape falls back to
// a placeholder rather than writing a config that fails its own validation.
func defaultProjectName(dir string) string {
	name := projectNameStripRE.ReplaceAllString(strings.ToLower(filepath.Base(dir)), "-")
	name = strings.Trim(name, "-_")
	if !projectNameRE.MatchString(name) {
		return "myapp"
	}
	return name
}
