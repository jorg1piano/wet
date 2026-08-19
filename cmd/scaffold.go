package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// starterConfigTemplate is the body of a scaffolded .wet.yaml, taking the
// derived project name and the name of the outputs: entry `wet secrets setup`
// scaffolds into. The output name is substituted rather than written literally
// so the starter and the scaffolder cannot drift into naming two different
// files, which would leave a provider's env: half in one and its secrets half
// in another.
//
// It is deliberately NOT the bundled cmd/wet.example.yaml. That file
// illustrates the whole schema against a fictional project — it claims
// emulator: pool items and declares secrets whose vault nobody has — so a repo
// seeded with it verbatim fails its very next setup. The annotated example is
// for reading; this is for running, and every live line here resolves against
// a fresh checkout.
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

project_name: %[1]s

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

# The env files this repo generates, written by ` + "`wet setup`" + ` and regenerated on
# demand with ` + "`wet env pull`" + `. Each output lists the keys it carries, so this file
# alone answers what your app reads — there is no checked-in .env.tpl to keep
# in sync, and a key not listed here simply is not in the file.
#
# ` + "`wet secrets setup`" + ` appends to the output named %[2]q, so keep that name if
# you plan to scaffold provider keys. In a monorepo, point out: at the package
# that reads it (backend/.env.dev) and add one entry per package; missing
# parent directories are created.
#
# EVERY out: path must be gitignored. It is regenerated per worktree with that
# worktree's own ports, and it is where secrets land once you declare any — a
# committed one is both permanently wrong for everyone else and the leak this
# whole model exists to prevent. wet added the path below to .gitignore when it
# wrote this file; add the entry yourself for any output you add.
outputs:
  - name: %[2]s
    out: ` + starterOutputFile + `
    format: dotenv
    include:
      - DEV_HOST
      - PHX_PORT
      - AUTH_BASE_URL
      - HYDRA_PUBLIC_URL

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

	body := fmt.Sprintf(starterConfigTemplate, defaultProjectName(dir), defaultScaffoldOutput)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	if err := ignoreStarterOutputs(dir); err != nil {
		return "", err
	}
	return path, nil
}

// starterOutputFile is the out: path the starter declares. It is a constant
// because two things must agree on it — the config wet writes and the
// .gitignore entry that keeps it out of the repo — and a comment asking the
// user to keep them in sync is exactly the instruction that gets skipped.
const starterOutputFile = ".env.dev"

// ignoreStarterOutputs adds the starter's generated env file to .gitignore.
//
// This is not advice in a comment for a reason. The file holds this worktree's
// ports, and every secret once any is declared; committing it is wrong for
// every other checkout and leaks credentials to anyone with repo access. The
// starter is the one config nobody reviews before the first `wet setup`, so
// the guard has to be applied rather than described.
//
// A missing .gitignore is created; an entry already present is left alone, so
// re-running changes nothing. Matching is line-exact rather than by substring:
// a repo ignoring `.env.dev.sample` must not be read as already ignoring
// `.env.dev`.
func ignoreStarterOutputs(dir string) error {
	return ignoreGeneratedOutput(dir, starterOutputFile)
}

// ignoreGeneratedOutput adds one generated path to .gitignore. See
// ignoreStarterOutputs for why this is applied rather than described — and it
// applies to every path wet invents, not just the starter's: `wet secrets
// setup --output web` creates `web/.env.dev` carrying real credentials, and an
// output born gitignored is the only version of that guard that holds.
func ignoreGeneratedOutput(dir, out string) error {
	added, err := ensureGitignored(dir,
		"# Generated by wet — one per worktree, with this worktree's ports and\n"+
			"# whatever secrets its output declares. Never commit it.", out)
	if err != nil || len(added) == 0 {
		return err
	}
	// Worded to stand on its own: the caller prints its own narrative around
	// this call, and this line lands before it.
	fmt.Printf("  wet keeps generated env files out of git: added %s to .gitignore.\n", out)
	return nil
}

// ensureGitignored appends the entries a .gitignore is missing, under one
// header comment, and reports whether it wrote anything. Re-running is a
// no-op.
//
// Matching is LINE-EXACT rather than by substring: a repo ignoring
// `.env.dev.sample` must not be read as already ignoring `.env.dev`, and a
// `!.env.local` un-ignore or a mention inside a comment must not be read as
// ignoring `.env.local`. Substring matching gets that wrong in the direction
// that matters — it decides a secret-bearing file is already covered when it
// is not.
//
// One implementation because there was briefly two, and the weaker one was
// the one being handed the entries most worth getting right.
// It returns the entries it appended so the caller can name them: "added
// .gitignore entries" is not the same message as "added .env.local", and the
// user has to be able to see which secret-bearing file wet just covered.
func ensureGitignored(dir, header string, entries ...string) ([]string, error) {
	path := filepath.Join(dir, ".gitignore")
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	content := string(existing)
	present := map[string]bool{}
	for _, line := range strings.Split(content, "\n") {
		present[strings.TrimSpace(line)] = true
	}
	var missing []string
	for _, e := range entries {
		if !present[e] {
			missing = append(missing, e)
		}
	}
	if len(missing) == 0 {
		return nil, nil
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	if len(content) > 0 && !strings.HasSuffix(content, "\n") {
		fmt.Fprintln(f)
	}
	fmt.Fprintf(f, "\n%s\n%s\n", header, strings.Join(missing, "\n"))
	return missing, nil
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
