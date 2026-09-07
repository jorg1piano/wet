package cmd

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPropertiesEncodingAndJavaReader(t *testing.T) {
	pairs := []EnvPair{
		{Key: "storeFile", Value: `C:\Users\someone\signing key.jks`},
		{Key: "storePassword", Value: " leading and trailing \t\n\r\f\\'\"$=:# !ø😀 "},
		{Key: "keyAlias", Value: "key"},
		{Key: "keyPassword", Value: "'quotes remain literal'"},
		{Key: "a.b :#!\n", Value: "\x00"},
		{Key: "empty", Value: ""},
	}
	body, err := EncodeProperties(pairs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "keyAlias=key\n") || !strings.Contains(body, `\u00f8\ud83d\ude00`) {
		t.Fatal("missing plain or Unicode-escaped property")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "key.properties")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	values, order, err := ParsePropertiesFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range pairs {
		if order[i] != p.Key || values[p.Key] != p.Value {
			t.Fatalf("property %d failed round trip", i)
		}
	}
	// The JDK is an independent check of escaping and byte-stream encoding.
	java, err := exec.LookPath("java")
	if err != nil {
		t.Skip("Java unavailable; Go round trip passed")
	}
	if err := exec.Command(java, "-version").Run(); err != nil {
		t.Skip("Java runtime unavailable")
	}
	source := filepath.Join(dir, "CheckProperties.java")
	program := `import java.nio.file.*;
import java.util.*;
import java.nio.charset.StandardCharsets;
class CheckProperties {
  public static void main(String[] args) throws Exception {
    Properties p = new Properties();
    try (var input = Files.newInputStream(Path.of(args[0]))) { p.load(input); }
    for (int i = 1; i < args.length; i += 2) {
      String expected = new String(Base64.getDecoder().decode(args[i+1]), StandardCharsets.UTF_8);
      if (!expected.equals(p.getProperty(args[i]))) throw new Exception("Mismatch at property " + i);
    }
  }
}`
	if err := os.WriteFile(source, []byte(program), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{source, path}
	for _, p := range pairs {
		args = append(args, p.Key, base64.StdEncoding.EncodeToString([]byte(p.Value)))
	}
	if out, err := exec.Command(java, args...).CombinedOutput(); err != nil {
		t.Fatalf("Java properties reader: %v\n%s", err, out)
	}
}

func TestParsePropertiesSyntaxAndRedactedErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input.properties")
	body := " # comment\n! another\r\nfoo\\ bar : value\\\r\n  continued\nlatin=\xf8\nempty\ndupe=first\ndupe=last\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	values, order, err := ParsePropertiesFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if values["foo bar"] != "valuecontinued" || values["latin"] != "ø" || values["dupe"] != "last" || !slices.Equal(order, []string{"foo bar", "latin", "empty", "dupe"}) {
		t.Fatalf("parsed %v, order %v", values, order)
	}
	if err := os.WriteFile(path, []byte(`password=private-value\uZZZZ`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = ParsePropertiesFile(path)
	if err == nil || strings.Contains(err.Error(), "private-value") {
		t.Fatalf("err = %v", err)
	}
}

func TestEnvPushPropertiesUsesSecretBindings(t *testing.T) {
	resetEnvFlags(t)
	repo := opRepo(t)
	stubOP(t, `{"Assets":{"password":""}}`)
	if err := os.WriteFile(filepath.Join(repo, wetConfigFileName), []byte(fileOutputConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "input.properties")
	password := " leading \\password\nø"
	body, err := EncodeProperties([]EnvPair{{Key: "storePassword", Value: password}, {Key: "keyPassword", Value: password}, {Key: "storeFile", Value: "/local/file"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	envPushFrom = source
	if err := runEnvPush(envPushCmd, []string{"settings"}); err != nil {
		t.Fatal(err)
	}
	item, found, err := (opClient{}).item("Development", "Assets")
	if err != nil || !found {
		t.Fatalf("item: %v, found %v", err, found)
	}
	if value, _ := item.value("password"); value != password {
		t.Fatal("push changed property password")
	}
	if len(item.Fields) != 1 {
		t.Fatal("push stored a path or alias as a provider field")
	}
	if err := runEnvPush(envPushCmd, []string{"artifact"}); err == nil || !strings.Contains(err.Error(), "key/value outputs only") {
		t.Fatalf("file push err = %v", err)
	}
}

func TestEnvExplainFileDependencies(t *testing.T) {
	resetEnvFlags(t)
	repo := opRepo(t)
	if err := os.WriteFile(filepath.Join(repo, wetConfigFileName), []byte(fileOutputConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	text := captureStdout(t, func() {
		if err := runEnvExplain(envExplainCmd, []string{"settings"}); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"storeFile = path to artifact", "storePassword = secret PASSWORD", "Development/Assets/arbitrary.bin"} {
		if !strings.Contains(text, want) {
			t.Fatalf("explain missing %q:\n%s", want, text)
		}
	}
}
