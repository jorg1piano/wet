package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// scaffoldStarterConfig writes a minimal `.wet.yaml` at dir and returns its
// path. It refuses to touch an existing file: the only caller reaches here
// because the config was missing, so finding one means something changed
// underneath us and overwriting it would destroy the user's work.
//
// The starter is deliberately NOT a copy of the bundled cmd/wet.example.yaml.
// That file illustrates the whole schema against a fictional project — it
// renders `.env.example.tpl` and claims `emulator:` pool items — so a repo
// seeded with it verbatim fails its very next `wet setup` on a render target
// that does not exist. The annotated example is for reading; this is for
// running. It declares the one required key and nothing else, so `wet setup`
// succeeds immediately after it is written.
func scaffoldStarterConfig(dir string) (string, error) {
	path := filepath.Join(dir, wetConfigFileName)
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("%s already exists — refusing to overwrite it", path)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("stat %s: %w", path, err)
	}

	body := fmt.Sprintf(`# %s — see the annotated schema for every key and what it does:
#   .claude/skills/wet/%s  (after `+"`wet install wet-skill`"+`)
#   https://github.com/devda/wet/blob/main/cmd/%s

project_name: %s

# Ports each get their own slot per worktree: slot N is base + N*port_stride.
# Uncomment and name the ports your project listens on.
#
# port_stride: 10
# ports:
#   - name: APP_PORT
#     base: 8080
`, wetConfigFileName, exampleConfigFileName, exampleConfigFileName, defaultProjectName(dir))

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
