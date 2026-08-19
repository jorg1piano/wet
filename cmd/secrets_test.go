package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
// Every value wet writes must survive a round trip through the dotenv encoder
// and back, or a credential silently arrives at the app mangled. The awkward
// ones are the point: spaces, quotes, `$`, and `#` all mean something to one
// dialect or another.
func TestEncodeDotenvRoundTrip(t *testing.T) {
	pairs := []EnvPair{
		{"PLAIN", "abc123"},
		{"SPACED", "two words"},
		{"QUOTE", `a"b'c`},
		{"DOLLAR", "$NOT_EXPANDED"},
		{"HASH", "no # comment"},
		{"EMPTY", ""},
	}
	body, err := EncodeDotenv(pairs)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// An empty value stays an empty value, not a missing key: `KEY=` is how a
	// consumer learns the key exists and is deliberately unset.
	if !strings.Contains(body, "\nEMPTY=\n") {
		t.Errorf("empty value should render as KEY=, got:\n%s", body)
	}
	// One assignment per line, whatever the values hold.
	if lines := strings.Count(strings.TrimSpace(body), "\n") + 1; lines != len(pairs)+1 {
		t.Errorf("file has %d lines, want %d (header + one per key):\n%s", lines, len(pairs)+1, body)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, _, err := ParseEnvFile(path)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	for _, p := range pairs {
		if got[p.Key] != p.Value {
			t.Errorf("%s round-tripped to %q, want %q", p.Key, got[p.Key], p.Value)
		}
	}
}

// A newline is refused rather than escaped: dotenv dialects disagree about
// multi-line values, and one that half-parses puts the tail of a private key
// on a line the reader treats as the next assignment.
func TestEncodeDotenvRejectsNewlines(t *testing.T) {
	_, err := EncodeDotenv([]EnvPair{{"PEM", "-----BEGIN KEY-----\nabc\n-----END KEY-----"}})
	if err == nil || !strings.Contains(err.Error(), "PEM") {
		t.Fatalf("err = %v, want it to name the offending key", err)
	}
	if !strings.Contains(err.Error(), "base64") {
		t.Errorf("err = %v, want it to say what to do instead", err)
	}
}
