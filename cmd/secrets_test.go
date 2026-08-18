package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveSectionRefForms pins the three reference forms. The short forms
// are what keep the config free of per-consumer duplication, so a regression
// here would push every entry back to a full op:// URI.
func TestResolveSectionRefForms(t *testing.T) {
	sec := &SecretsConfig{
		Store: secretsStoreOP,
		Vault: "fbd",
		Item:  "fantasy",
		Sections: map[string]map[string]string{
			"backend": {
				"BARE":  "secret_key_base",
				"ITEM":  "Google OAuth/client_id",
				"FULL":  "op://other/Thing/field",
				"SPACE": "Apple Sign In/key_id",
			},
		},
	}
	refs, err := sec.ResolveSection("backend")
	if err != nil {
		t.Fatalf("ResolveSection: %v", err)
	}
	got := map[string]string{}
	for _, r := range refs {
		got[r.Key] = r.URI()
	}
	want := map[string]string{
		"BARE":  "op://fbd/fantasy/secret_key_base",
		"ITEM":  "op://fbd/Google OAuth/client_id",
		"FULL":  "op://other/Thing/field",
		"SPACE": "op://fbd/Apple Sign In/key_id",
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q", k, got[k], w)
		}
	}

	// Order must be stable: the refs drive the env file's line order, and a
	// map-ordered result would rewrite the file on every run.
	first, _ := sec.ResolveSection("backend")
	second, _ := sec.ResolveSection("backend")
	for i := range first {
		if first[i].Key != second[i].Key {
			t.Fatalf("ResolveSection order is not stable at %d: %s vs %s", i, first[i].Key, second[i].Key)
		}
	}
}

// TestResolveSectionRejectsMalformed covers the references that would
// otherwise resolve to a plausible-but-wrong place in the vault.
func TestResolveSectionRejectsMalformed(t *testing.T) {
	cases := map[string]string{
		"too few op:// parts": "op://vault/item",
		"empty op:// field":   "op://vault/item/",
		"trailing slash":      "Item/",
		"leading slash":       "/field",
		"three segments":      "a/b/c",
	}
	for name, ref := range cases {
		t.Run(name, func(t *testing.T) {
			sec := &SecretsConfig{Vault: "v", Item: "i",
				Sections: map[string]map[string]string{"s": {"K": ref}}}
			if _, err := sec.ResolveSection("s"); err == nil {
				t.Errorf("%q was accepted, want an error", ref)
			}
		})
	}

	// A bare field with no vault anywhere cannot be resolved, and guessing
	// would write into whichever vault happened to be default.
	sec := &SecretsConfig{Item: "i", Sections: map[string]map[string]string{"s": {"K": "f"}}}
	if _, err := sec.ResolveSection("s"); err == nil || !strings.Contains(err.Error(), "vault") {
		t.Errorf("missing vault error = %v, want it to name the vault", err)
	}
}

// TestSectionOutputDirs is the heart of the section->path mapping: the vault
// says nothing about directories, the render targets do.
func TestSectionOutputDirs(t *testing.T) {
	cfg := &WetConfig{Render: []RenderTarget{
		{Template: "backend/.env.tpl", Out: "backend/.env", Secrets: "backend"},
		{Template: "backend/.env.test.tpl", Out: "backend/.env.test", Secrets: "backend"},
		{Template: "mobile/.env.tpl", Out: "mobile/.env", Secrets: "mobile"},
		{Template: "web/.env.tpl", Out: "web/.env"},
	}}
	if got := cfg.SectionOutputDirs("backend"); len(got) != 1 || got[0] != "backend" {
		t.Errorf("backend dirs = %v, want [backend] deduplicated", got)
	}
	if got := cfg.SectionOutputDirs("mobile"); len(got) != 1 || got[0] != "mobile" {
		t.Errorf("mobile dirs = %v, want [mobile]", got)
	}
	if got := cfg.SectionOutputDirs("nope"); len(got) != 0 {
		t.Errorf("unknown section dirs = %v, want none", got)
	}
}

// TestValidateSecrets covers the config-level mistakes that would otherwise
// surface much later, as a template field that will not resolve.
func TestValidateSecrets(t *testing.T) {
	base := func() *WetConfig {
		return &WetConfig{
			ProjectName: "app",
			Secrets: &SecretsConfig{
				Store:    secretsStoreOP,
				Vault:    "v",
				Item:     "i",
				Sections: map[string]map[string]string{"backend": {"K": "f"}},
			},
			Render: []RenderTarget{{Template: "a.tpl", Out: "a", Secrets: "backend"}},
		}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	t.Run("unknown section on a render target", func(t *testing.T) {
		cfg := base()
		cfg.Render[0].Secrets = "backnd"
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "backnd") {
			t.Fatalf("err = %v, want it to name the typo", err)
		}
		if !strings.Contains(err.Error(), "backend") {
			t.Errorf("err = %v, want it to list the known sections", err)
		}
	})

	t.Run("section named with no secrets block", func(t *testing.T) {
		cfg := base()
		cfg.Secrets = nil
		if err := cfg.Validate(); err == nil {
			t.Error("a render target naming a section with no secrets: block was accepted")
		}
	})

	t.Run("unknown store", func(t *testing.T) {
		cfg := base()
		cfg.Secrets.Store = "vault"
		if err := cfg.Validate(); err == nil {
			t.Error("secrets.store: vault was accepted, but only op is implemented")
		}
	})

	t.Run("reserved key would shadow wet's own data", func(t *testing.T) {
		cfg := base()
		cfg.Secrets.Sections["backend"]["Branch"] = "branch"
		if err := cfg.Validate(); err == nil {
			t.Error("a secret named Branch was accepted; it would shadow wet's field")
		}
	})

	t.Run("empty section", func(t *testing.T) {
		cfg := base()
		cfg.Secrets.Sections["mobile"] = map[string]string{}
		if err := cfg.Validate(); err == nil {
			t.Error("an empty section was accepted")
		}
	})
}

func TestParseEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	body := "# comment\n\nexport A=1\nB = two words \nC='quoted'\nD=\"dq\"\nE=\nA=last\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	values, order, err := ParseEnvFile(path)
	if err != nil {
		t.Fatalf("ParseEnvFile: %v", err)
	}
	want := map[string]string{"A": "last", "B": "two words", "C": "quoted", "D": "dq", "E": ""}
	for k, w := range want {
		if values[k] != w {
			t.Errorf("%s = %q, want %q", k, values[k], w)
		}
	}
	// A repeated key is listed once, at its first appearance, so seeding
	// reports it once.
	count := 0
	for _, k := range order {
		if k == "A" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("A appears %d times in order, want 1", count)
	}

	// A junk line is an error, not a skip: a seed that silently ignored half
	// its input would look like it worked.
	bad := filepath.Join(dir, "bad.env")
	if err := os.WriteFile(bad, []byte("A=1\nthis is not an assignment\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ParseEnvFile(bad); err == nil {
		t.Error("a non-assignment line was accepted")
	}
}

// TestFormatEnvFileRoundTrip is the guard that matters for `read`: whatever a
// vault holds — quotes, spaces, a PEM key's newlines — must come back out of
// the written file unchanged.
func TestFormatEnvFileRoundTrip(t *testing.T) {
	values := map[string]string{
		"PLAIN":  "abc123",
		"SPACED": "two words",
		"QUOTE":  `a"b'c`,
		"DOLLAR": "$NOT_EXPANDED",
		"HASH":   "no # comment",
		// A PEM key is the case that breaks a line-based reader if the
		// writer emits a literal newline.
		"PEM": "-----BEGIN KEY-----\nabc\ndef\n-----END KEY-----",
	}
	refs := []SecretRef{
		{Key: "PLAIN"}, {Key: "SPACED"}, {Key: "QUOTE"}, {Key: "DOLLAR"}, {Key: "HASH"}, {Key: "PEM"},
	}
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := WriteFile0600(path, []byte(FormatEnvFile(refs, values, "header"))); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600 — the file holds secrets", info.Mode().Perm())
	}
	// One assignment per line, whatever the values hold.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(strings.TrimSpace(string(raw)), "\n") + 1; lines != len(refs)+1 {
		t.Errorf("file has %d lines, want %d (header + one per key):\n%s", lines, len(refs)+1, raw)
	}

	got, _, err := ParseEnvFile(path)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	for k, w := range values {
		if got[k] != w {
			t.Errorf("%s round-tripped to %q, want %q", k, got[k], w)
		}
	}
}
