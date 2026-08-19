package cmd

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// secretsStoreOP is the only implemented value of secrets.store. It is a
// constant rather than a literal so adding a store is a compile error at every
// place that assumes 1Password, instead of a silent wrong CLI.
const secretsStoreOP = "op"

// SecretRef is one env key resolved to a place in the store.
type SecretRef struct {
	Key   string // env key, e.g. GOOGLE_CLIENT_ID
	Vault string
	Item  string
	Field string
}

// URI renders the reference in `op read` syntax.
func (r SecretRef) URI() string {
	return fmt.Sprintf("op://%s/%s/%s", r.Vault, r.Item, r.Field)
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

// safeUnquotedEnvValue is the set of values that may be written bare.
//
// It is an ALLOWLIST, and that is the whole point. The previous version
// denied a handful of characters it thought were special and let everything
// else through — so `AUTH_BASE_URL=http://host/cb?a=b&c=d` was emitted
// unquoted, and `set -a; . ./.env` (what a compose stack and every `just`
// recipe do) read the `&` as "background this command" and gave the app an
// EMPTY url. A denylist of shell metacharacters is a list nobody keeps
// complete; an allowlist fails safe by quoting one value too many.
var safeUnquotedEnvValue = regexp.MustCompile(`^[A-Za-z0-9_./:@%+,=-]*$`)

// quoteEnvValue quotes a value so that unquoteEnvValue — and the dotenv
// readers these files are written for, which all model themselves on `sh` —
// give back exactly what went in.
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
	if safeUnquotedEnvValue.MatchString(v) {
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
