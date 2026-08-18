package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// secretsStoreOP is the only implemented value of secrets.store. It is a
// constant rather than a literal so adding a store is a compile error at every
// place that assumes 1Password, instead of a silent wrong CLI.
const secretsStoreOP = "op"

// SecretRef is one env key resolved to a place in the store.
type SecretRef struct {
	Key     string // env key, e.g. GOOGLE_CLIENT_ID
	Vault   string
	Item    string
	Field   string
	Section string
}

// URI renders the reference in `op read` syntax.
func (r SecretRef) URI() string {
	return fmt.Sprintf("op://%s/%s/%s", r.Vault, r.Item, r.Field)
}

// ResolveSection turns one section's key -> reference map into concrete
// SecretRefs against the block's vault and item.
//
// Three reference forms, cheapest first: a bare "field" on the default item,
// "Other Item/field" for another item in the same vault, and a full
// "op://vault/item/field" that overrides both. The short forms are what keep
// per-consumer duplication out of the config — a section names the field, not
// where the file that consumes it lives.
func (s *SecretsConfig) ResolveSection(section string) ([]SecretRef, error) {
	keys, ok := s.Sections[section]
	if !ok {
		return nil, fmt.Errorf("unknown secrets section %q (known: %s)",
			section, strings.Join(sortedKeys(s.Sections), ", "))
	}
	refs := make([]SecretRef, 0, len(keys))
	for _, key := range sortedKeys(keys) {
		ref, err := s.resolveRef(section, key, keys[key])
		if err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

func (s *SecretsConfig) resolveRef(section, key, raw string) (SecretRef, error) {
	ref := SecretRef{Key: key, Section: section, Vault: s.Vault, Item: s.Item}
	switch {
	case strings.HasPrefix(raw, "op://"):
		parts := strings.SplitN(strings.TrimPrefix(raw, "op://"), "/", 3)
		if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
			return ref, fmt.Errorf("secrets.sections.%s.%s: %q is not op://vault/item/field", section, key, raw)
		}
		ref.Vault, ref.Item, ref.Field = parts[0], parts[1], parts[2]
	case strings.Contains(raw, "/"):
		item, field, _ := strings.Cut(raw, "/")
		if item == "" || field == "" || strings.Contains(field, "/") {
			return ref, fmt.Errorf("secrets.sections.%s.%s: %q is not \"Item/field\"", section, key, raw)
		}
		ref.Item, ref.Field = item, field
	default:
		ref.Field = raw
	}
	if ref.Vault == "" {
		return ref, errors.New("no vault: set secrets.vault or pass --vault")
	}
	if ref.Item == "" {
		return ref, errors.New("no item: set secrets.item or pass --item")
	}
	return ref, nil
}

// SectionNames returns the sections to act on: the ones named, or all of them
// in a stable order.
func (s *SecretsConfig) SectionNames(only []string) ([]string, error) {
	if len(only) == 0 {
		return sortedKeys(s.Sections), nil
	}
	for _, name := range only {
		if _, ok := s.Sections[name]; !ok {
			return nil, fmt.Errorf("unknown secrets section %q (known: %s)",
				name, strings.Join(sortedKeys(s.Sections), ", "))
		}
	}
	out := append([]string(nil), only...)
	sort.Strings(out)
	return out, nil
}

// SectionOutputDirs reports the directories a section's secrets are consumed
// in, taken from the render targets that name it. That is the whole reason a
// section never carries a path: the config already says where the target
// writes, and duplicating it would let the two disagree.
//
// Order follows the render list, deduplicated — two targets in one directory
// are one place to write.
func (c *WetConfig) SectionOutputDirs(section string) []string {
	var dirs []string
	seen := map[string]bool{}
	for _, r := range c.Render {
		if r.Secrets != section {
			continue
		}
		dir := filepath.Dir(r.Out)
		if !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

// ParseEnvFile reads KEY=value lines. It is deliberately small: `export`
// prefixes, `#` comments, blank lines and surrounding quotes, and nothing
// else. A line it cannot parse is an error rather than a skip — a seed that
// silently ignored half its input would look like a success.
func ParseEnvFile(path string) (map[string]string, []string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	out := map[string]string{}
	var order []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		text = strings.TrimPrefix(text, "export ")
		key, value, found := strings.Cut(text, "=")
		if !found {
			return nil, nil, fmt.Errorf("%s:%d: not a KEY=value line: %s", path, line, sc.Text())
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, nil, fmt.Errorf("%s:%d: empty key", path, line)
		}
		value = unquoteEnvValue(strings.TrimSpace(value))
		if _, dup := out[key]; !dup {
			order = append(order, key)
		}
		out[key] = value
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	return out, order, nil
}

// FormatEnvFile renders refs and their values as an env file. Values are
// quoted when they hold anything a reader would otherwise mangle, and a value
// spanning lines (a PEM key, say) is escaped so the file keeps exactly one
// assignment per line.
func FormatEnvFile(refs []SecretRef, values map[string]string, header string) string {
	var b strings.Builder
	if header != "" {
		b.WriteString("# " + header + "\n")
	}
	for _, ref := range refs {
		b.WriteString(ref.Key)
		b.WriteString("=")
		b.WriteString(quoteEnvValue(values[ref.Key]))
		b.WriteString("\n")
	}
	return b.String()
}

// quoteEnvValue quotes a value so that unquoteEnvValue — and the dotenv
// readers these files are written for — give back exactly what went in.
//
// Single quotes for the common case, because their contents are literal: no
// expansion of $VAR, no escape sequences to get wrong. A value containing a
// single quote cannot use them (shell's '\” splice is not understood by most
// dotenv parsers), so it falls back to double quotes with the four characters
// that are special inside them escaped.
func quoteEnvValue(v string) string {
	if v == "" {
		return ""
	}
	if !strings.ContainsAny(v, " \t\n\"'$`\\#") {
		return v
	}
	// A newline has to go through the double-quoted form as \n: the file is
	// read line by line, so a literal newline inside single quotes would make
	// the rest of the value look like the next assignment.
	if !strings.Contains(v, "'") && !strings.ContainsAny(v, "\n\r") {
		return "'" + v + "'"
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`", "\n", `\n`, "\r", `\r`)
	return `"` + r.Replace(v) + `"`
}

// unquoteEnvValue is quoteEnvValue's inverse. Single-quoted content is
// literal; double-quoted content has its escapes resolved. An unquoted value
// is returned as-is, which is what every dotenv reader does.
func unquoteEnvValue(v string) string {
	if len(v) < 2 {
		return v
	}
	switch {
	case v[0] == '\'' && v[len(v)-1] == '\'':
		return v[1 : len(v)-1]
	case v[0] == '"' && v[len(v)-1] == '"':
		body := v[1 : len(v)-1]
		var b strings.Builder
		for i := 0; i < len(body); i++ {
			if body[i] == '\\' && i+1 < len(body) {
				switch body[i+1] {
				case '\\', '"', '$', '`':
					b.WriteByte(body[i+1])
					i++
					continue
				case 'n':
					b.WriteByte('\n')
					i++
					continue
				case 'r':
					b.WriteByte('\r')
					i++
					continue
				}
			}
			b.WriteByte(body[i])
		}
		return b.String()
	}
	return v
}

// WriteFile0600 writes a file that may hold secrets. The mode is applied on
// overwrite too — os.WriteFile only sets it when creating.
func WriteFile0600(path string, content []byte) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}
