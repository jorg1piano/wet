package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestEmbeddedSkillTemplate smoke-checks the //go:embed directive in
// install.go — confirms the template file made it into the binary, starts
// with the expected Claude Code frontmatter, and still contains a mention
// of `project_name` (the most distinctive schema keyword, so it catches
// accidental truncation or wrong-file embeds).
func TestEmbeddedSkillTemplate(t *testing.T) {
	if wetSkillTemplate == "" {
		t.Fatal("wetSkillTemplate is empty — //go:embed did not capture the file")
	}
	if !strings.HasPrefix(wetSkillTemplate, "---\nname: wet") {
		t.Errorf("wetSkillTemplate missing expected frontmatter prefix; got first 80 bytes: %q",
			wetSkillTemplate[:min(80, len(wetSkillTemplate))])
	}
	if !strings.Contains(wetSkillTemplate, "project_name") {
		t.Error("wetSkillTemplate does not contain 'project_name' — schema body may be missing")
	}
}

// TestEmbeddedExampleConfig smoke-checks the second //go:embed in install.go.
// The example config is what SKILL.md links to, so an empty or wrong-file
// embed would ship a skill pointing at nothing.
func TestEmbeddedExampleConfig(t *testing.T) {
	if wetExampleConfig == "" {
		t.Fatal("wetExampleConfig is empty — //go:embed did not capture the file")
	}
	if !strings.Contains(wetExampleConfig, "project_name:") {
		t.Error("wetExampleConfig does not contain 'project_name:' — wrong file or truncated")
	}
}

// TestInstallSkillFiles covers the three states a target file can be in:
// absent (written), byte-identical (left alone, no prompt), and different
// (kept unless --force). The differing case matters most: the schema file is
// written beside a SKILL.md the user may have edited, and neither decision
// may leak into the other file.
func TestInstallSkillFiles(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	// getRepoRoot shells out in the process's cwd. t.Chdir restores it even on
	// failure, and makes the test refuse to run in parallel rather than
	// corrupting the cwd that config_test.go's example-config tests read.
	t.Chdir(repo)
	// macOS resolves TempDir through /private; ask git for the same spelling
	// of the root that installSkillFiles will use.
	root, err := getRepoRoot()
	if err != nil {
		t.Fatal(err)
	}

	files := []skillFile{
		{name: "SKILL.md", content: "skill body\n"},
		{name: "wet.example.yaml", content: "project_name: myapp\n"},
	}
	path := func(name string) string {
		return filepath.Join(root, ".claude", "skills", "wet", name)
	}
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(path(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(b)
	}
	// installForce is package state; save and restore it so the value can't
	// leak into another test through ordering.
	prevForce := installForce
	t.Cleanup(func() { installForce = prevForce })

	// 1. Both absent: both get written.
	installForce = false
	if err := installSkillFiles("wet", files); err != nil {
		t.Fatalf("first install: %v", err)
	}
	for _, f := range files {
		if got := read(f.name); got != f.content {
			t.Errorf("%s = %q, want %q", f.name, got, f.content)
		}
	}

	// 2. Both identical: no prompt (which under `go test` would be an EOF
	// error), nothing rewritten.
	if err := installSkillFiles("wet", files); err != nil {
		t.Fatalf("re-install of identical files must be a silent no-op: %v", err)
	}

	// 3. SKILL.md hand-edited, schema file untouched. Under `go test` stdin
	// is not answerable, so the prompt must fail loudly instead of reporting
	// a successful install that wrote nothing.
	if err := os.WriteFile(path("SKILL.md"), []byte("hand edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = installSkillFiles("wet", files)
	if err == nil {
		t.Fatal("unanswerable prompt reported success; want an error naming --force")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("error does not name the flag that resolves it: %v", err)
	}
	if got := read("SKILL.md"); got != "hand edited\n" {
		t.Errorf("declined overwrite still clobbered SKILL.md: %q", got)
	}
	if got := read("wet.example.yaml"); got != files[1].content {
		t.Errorf("wet.example.yaml changed unexpectedly: %q", got)
	}

	// 4. --force overwrites the differing file without prompting.
	installForce = true
	if err := installSkillFiles("wet", files); err != nil {
		t.Fatalf("forced install: %v", err)
	}
	if got := read("SKILL.md"); got != files[0].content {
		t.Errorf("--force did not restore SKILL.md: %q", got)
	}
}

// TestConfirmOverwriteNonInteractive pins the bug that `wet install wet-skill
// </dev/null` used to exit 0 having installed nothing: fmt.Scanln's error was
// discarded, so "no answer" looked exactly like a typed "n".
//
// Both non-interactive shapes are covered, because they fail different checks:
// a redirect from a regular file or a pipe is not a character device at all,
// while /dev/null IS one and only reveals itself at EOF. The interactive
// "typed y" path needs a real tty and is not testable here.
func TestConfirmOverwriteNonInteractive(t *testing.T) {
	prev := os.Stdin
	t.Cleanup(func() { os.Stdin = prev })

	cases := map[string]func(t *testing.T) *os.File{
		"redirected from a file": func(t *testing.T) *os.File {
			p := filepath.Join(t.TempDir(), "answers")
			if err := os.WriteFile(p, []byte("y\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			f, err := os.Open(p)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = f.Close() })
			return f
		},
		"pipe": func(t *testing.T) *os.File {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			_ = w.Close() // an exhausted pipe: EOF on first read
			t.Cleanup(func() { _ = r.Close() })
			return r
		},
		"dev null": func(t *testing.T) *os.File {
			f, err := os.Open(os.DevNull)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = f.Close() })
			return f
		},
	}

	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			os.Stdin = mk(t)
			ok, err := confirmOverwrite("/repo/.claude/skills/wet/SKILL.md")
			if ok {
				t.Error("confirmOverwrite approved an overwrite nobody consented to")
			}
			if err == nil {
				t.Fatal("no error; a missing answer must not read as a decision")
			}
			if !strings.Contains(err.Error(), "--force") {
				t.Errorf("error does not name --force: %v", err)
			}
			if !strings.Contains(err.Error(), "SKILL.md") {
				t.Errorf("error does not name the file at stake: %v", err)
			}
		})
	}
}

// TestEnsureGitignoreEntries pins the machine-local files wet writes at the
// repo root. The lock file is the one that regressed: it is created lazily by
// the first allocation, long after install ran, so a missing entry shows up as
// unexplained `git status` noise rather than as an install failure.
//
// The second case covers a repo installed before the lock entry existed —
// appending must add only what is missing, and must not re-add the entry that
// is already there.
func TestEnsureGitignoreEntries(t *testing.T) {
	want := []string{".wet-worktrees.json", ".wet-worktrees.lock", ".env.worktree"}

	cases := map[string]string{
		"no gitignore":    "",
		"partial":         ".wet-worktrees.json\n",
		"missing newline": "node_modules/\n.wet-worktrees.json",
	}
	for name, initial := range cases {
		t.Run(name, func(t *testing.T) {
			repo := t.TempDir()
			if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
				t.Fatalf("git init: %v\n%s", err, out)
			}
			t.Chdir(repo)
			root, err := getRepoRoot()
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, ".gitignore")
			if initial != "" {
				if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			if err := ensureGitignoreEntries(); err != nil {
				t.Fatalf("ensureGitignoreEntries: %v", err)
			}
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read .gitignore: %v", err)
			}
			lines := strings.Split(strings.TrimSpace(string(body)), "\n")
			counts := map[string]int{}
			for _, l := range lines {
				counts[strings.TrimSpace(l)]++
			}
			for _, entry := range want {
				switch counts[entry] {
				case 1:
					// exactly what we want
				case 0:
					t.Errorf("%s not ignored; .gitignore is:\n%s", entry, body)
				default:
					t.Errorf("%s listed %d times; append must not duplicate:\n%s", entry, counts[entry], body)
				}
			}
			// A second run is a no-op: install is re-runnable.
			if err := ensureGitignoreEntries(); err != nil {
				t.Fatalf("second ensureGitignoreEntries: %v", err)
			}
			again, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(again) != string(body) {
				t.Errorf("not idempotent:\nfirst:\n%s\nsecond:\n%s", body, again)
			}
		})
	}
}
