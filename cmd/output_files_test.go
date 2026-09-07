package cmd

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const fileOutputConfig = `project_name: files
secrets:
  store: op
  policy: required
  vault: Development
  item: Assets
  definitions:
    ARTIFACT: {field: arbitrary.bin}
    PASSWORD: {field: password}
    UNUSED: {field: unused}
outputs:
  - name: settings
    out: config/app.properties
    format: properties
    values:
      storeFile: {file: artifact}
      storePassword: {secret: PASSWORD}
      keyAlias: {literal: key}
      keyPassword: {secret: PASSWORD}
  - name: artifact
    out: private/arbitrary.bin
    format: file
    secret: ARTIFACT
    on_demand: true
`

type binaryStore struct {
	*fakeStore
	files     map[string][]byte
	fileReads map[string]int
	fileErr   error
}

func testBinaryStore() *binaryStore {
	data := make([]byte, 257)
	for i := 0; i < 256; i++ {
		data[i] = byte(i)
	}
	data[256] = '\n'
	return &binaryStore{
		fakeStore: newFakeStore(map[string]string{"op://Development/Assets/password": " leading 'password' \\=:#\nø"}),
		files:     map[string][]byte{"op://Development/Assets/arbitrary.bin": data},
		fileReads: map[string]int{},
	}
}

func (s *binaryStore) ReadFile(uri string) ([]byte, error) {
	s.fileReads[uri]++
	if s.fileErr != nil {
		return nil, s.fileErr
	}
	data, ok := s.files[uri]
	if !ok {
		return nil, errors.New("file missing")
	}
	return data, nil
}

func TestPullFileDependencyAndProperties(t *testing.T) {
	cfg := loadTestConfig(t, fileOutputConfig)
	store := testBinaryStore()
	tree, log, err := pullInto(t, PullRequest{Cfg: cfg, Reader: store, Only: []string{"settings"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := readOutput(t, tree, "private/arbitrary.bin"); !bytes.Equal([]byte(got), store.files["op://Development/Assets/arbitrary.bin"]) {
		t.Fatal("file bytes changed")
	}
	values, order, err := ParsePropertiesFile(filepath.Join(tree, "config/app.properties"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(order, []string{"storeFile", "storePassword", "keyAlias", "keyPassword"}) {
		t.Fatalf("order = %v", order)
	}
	wantPath, _ := filepath.Abs(filepath.Join(tree, "private/arbitrary.bin"))
	if values["storeFile"] != wantPath || values["keyAlias"] != "key" {
		t.Fatal("file path or literal did not resolve")
	}
	for _, key := range []string{"storePassword", "keyPassword"} {
		if values[key] != store.values["op://Development/Assets/password"] {
			t.Fatalf("%s did not preserve password", key)
		}
	}
	if len(store.reads) != 1 || store.reads["op://Development/Assets/password"] != 1 || len(store.fileReads) != 1 {
		t.Fatalf("unexpected reads: %v, files: %v", store.reads, store.fileReads)
	}
	if strings.Contains(log, "password'") || strings.Contains(log, "UNUSED") {
		t.Fatalf("unexpected log: %s", log)
	}
	for _, path := range []string{"private/arbitrary.bin", "config/app.properties"} {
		info, err := os.Stat(filepath.Join(tree, path))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o", path, info.Mode().Perm())
		}
	}
	if unused := cfg.UnusedSecrets(); !slices.Equal(unused, []string{"UNUSED"}) {
		t.Fatalf("unused = %v", unused)
	}
}

func TestPullFileSharedReferencesAndSelection(t *testing.T) {
	cfg := loadTestConfig(t, fileOutputConfig+`  - name: copy
    out: copy.bin
    format: file
    secret: ARTIFACT
  - name: public
    out: public.env
    format: dotenv
    values:
      PUBLIC: {literal: yes}
`)
	store := testBinaryStore()
	tree, _, err := pullInto(t, PullRequest{Cfg: cfg, Reader: store})
	if err != nil {
		t.Fatal(err)
	}
	if store.fileReads["op://Development/Assets/arbitrary.bin"] != 1 {
		t.Fatal("shared file fetched twice")
	}
	if readOutput(t, tree, "copy.bin") != readOutput(t, tree, "private/arbitrary.bin") {
		t.Fatal("shared files differ")
	}
	store = testBinaryStore()
	_, _, err = pullInto(t, PullRequest{Cfg: cfg, Reader: store, Only: []string{"public"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(store.reads)+len(store.fileReads) != 0 {
		t.Fatal("public output fetched secrets")
	}
}

func TestPullFilesFailBeforeReplacingOutputs(t *testing.T) {
	for _, failure := range []string{"missing file", "missing password", "encoding", "staging"} {
		t.Run(failure, func(t *testing.T) {
			cfg := loadTestConfig(t, fileOutputConfig)
			store := testBinaryStore()
			tree := t.TempDir()
			for _, path := range []string{"private/arbitrary.bin", "config/app.properties"} {
				full := filepath.Join(tree, path)
				if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte("old"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			switch failure {
			case "missing file":
				store.fileErr = errors.New("download failed")
			case "missing password":
				store.fail = errors.New("password missing")
			case "encoding":
				store.values["op://Development/Assets/password"] = string([]byte{0xff})
			case "staging":
				if err := os.RemoveAll(filepath.Join(tree, "config")); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(tree, "config"), []byte("block parent"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, _, err := pullInto(t, PullRequest{Cfg: cfg, Reader: store, TreePath: tree})
			if err == nil {
				t.Fatal("expected failure")
			}
			if readOutput(t, tree, "private/arbitrary.bin") != "old" {
				t.Fatal("failed pull replaced binary")
			}
			if failure != "staging" && readOutput(t, tree, "config/app.properties") != "old" {
				t.Fatal("failed pull replaced properties")
			}
			assertNoStagedLeftovers(t, tree)
		})
	}
}

func TestPullFileDryRunAndStream(t *testing.T) {
	cfg := loadTestConfig(t, fileOutputConfig)
	store := testBinaryStore()
	tree, _, err := pullInto(t, PullRequest{Cfg: cfg, Reader: store, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(tree)
	if len(entries) != 0 || len(store.fileReads) != 1 {
		t.Fatal("dry run must resolve files without creating anything")
	}
	var content bytes.Buffer
	store = testBinaryStore()
	_, _, err = pullInto(t, PullRequest{Cfg: cfg, Reader: store, Only: []string{"settings"}, Content: &content})
	if err == nil || !strings.Contains(err.Error(), "file dependencies") {
		t.Fatalf("err = %v", err)
	}
	if content.Len() != 0 || len(store.reads)+len(store.fileReads) != 0 {
		t.Fatal("invalid stream touched store or content")
	}
	_, _, err = pullInto(t, PullRequest{Cfg: cfg, Reader: store, Only: []string{"artifact"}, Content: &content})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content.Bytes(), store.files["op://Development/Assets/arbitrary.bin"]) {
		t.Fatal("stream changed binary bytes")
	}
}

func TestPullFilePolicies(t *testing.T) {
	for _, policy := range SecretPolicies {
		t.Run(policy, func(t *testing.T) {
			cfg := loadTestConfig(t, fileOutputConfig)
			def := cfg.Secrets.Definitions["ARTIFACT"]
			fallback := "development certificate\n"
			def.Default = &fallback
			cfg.Secrets.Definitions["ARTIFACT"] = def
			store := testBinaryStore()
			store.fileErr = errors.New("offline")
			tree, log, err := pullInto(t, PullRequest{Cfg: cfg, Reader: store, Only: []string{"artifact"}, Policy: policy})
			if policy == SecretPolicyRequired {
				if err == nil {
					t.Fatal("required used a default")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if readOutput(t, tree, "private/arbitrary.bin") != fallback {
				t.Fatal("fallback content changed")
			}
			if !strings.Contains(log, "default") {
				t.Fatal("fallback not reported")
			}
			if policy == SecretPolicyDisabled && len(store.fileReads) != 0 {
				t.Fatal("disabled read a file")
			}
		})
	}
	cfg := loadTestConfig(t, fileOutputConfig)
	_, _, err := pullInto(t, PullRequest{Cfg: cfg, Only: []string{"artifact"}, Policy: SecretPolicyDisabled})
	if err == nil || !strings.Contains(err.Error(), "no default") {
		t.Fatalf("err = %v", err)
	}
}

func TestFileProviderUnavailableLatchesAcrossReadKinds(t *testing.T) {
	store := testBinaryStore()
	store.fail = errStoreUnavailable
	reader := &latchingReader{inner: store}
	_, _ = reader.Read("op://Development/Assets/password")
	if _, err := reader.ReadFile("op://Development/Assets/arbitrary.bin"); !errors.Is(err, errStoreUnavailable) {
		t.Fatal(err)
	}
	if len(store.fileReads) != 0 {
		t.Fatal("binary read retried unavailable store")
	}
	store = testBinaryStore()
	store.fileErr = errStoreUnavailable
	reader = &latchingReader{inner: store}
	_, _ = reader.ReadFile("op://Development/Assets/arbitrary.bin")
	_, _ = reader.Read("op://Development/Assets/password")
	if len(store.reads) != 0 {
		t.Fatal("text read retried unavailable store")
	}
}

func TestOPFileReadPreservesAllBytes(t *testing.T) {
	dir := t.TempDir()
	data := testBinaryStore().files["op://Development/Assets/arbitrary.bin"]
	fixture := filepath.Join(dir, "fixture.bin")
	if err := os.WriteFile(fixture, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WET_TEST_FILE", fixture)
	script := `#!/bin/sh
test "$1" = --account && test "$2" = account-id || exit 11
shift 2
test "$#" = 3 && test "$1" = read && test "$2" = --no-newline && test "$3" = op://Development/Assets/arbitrary.bin || exit 12
cat "$WET_TEST_FILE"
`
	if err := os.WriteFile(filepath.Join(dir, "op"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg := loadTestConfig(t, fileOutputConfig)
	cfg.Secrets.Account = "account-id"
	tree, _, err := pullInto(t, PullRequest{Cfg: cfg, Only: []string{"artifact"}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal([]byte(readOutput(t, tree, "private/arbitrary.bin")), data) {
		t.Fatal("CLI path changed bytes")
	}
}

func TestValidateFileOutputs(t *testing.T) {
	for _, tc := range []struct{ from, to, want string }{
		{"secret: ARTIFACT", "secret: MISSING", "unknown secret"},
		{"secret: ARTIFACT", "", "needs secret:"},
		{"secret: ARTIFACT", "secret: ARTIFACT\n    include: [Branch]", "cannot declare"},
		{"file: artifact", "file: missing", "format: file output"},
		{"file: artifact", "file: settings", "format: file output"},
		{"file: artifact", "file: artifact, literal: x", "mutually exclusive"},
		{"secret: PASSWORD", "secret: MISSING", "unknown secret"},
		{"private/arbitrary.bin", "../outside.bin", "escape"},
		{"private/arbitrary.bin", ".wet.yaml", "wet's own files"},
		{"private/arbitrary.bin", "config/app.properties", "already written"},
		{"private/arbitrary.bin", "config", "parent directory"},
		{"private/arbitrary.bin", "config/app.properties/nested", "parent directory"},
		{"private/arbitrary.bin", ".", "checkout directory"},
	} {
		t.Run(tc.want+tc.to, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, wetConfigFileName), []byte(strings.Replace(fileOutputConfig, tc.from, tc.to, 1)), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadWetConfig(dir)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %s", err, tc.want)
			}
		})
	}
}

func TestFileOutputsCanBeEmptyAndRequireBinaryCapability(t *testing.T) {
	cfg := loadTestConfig(t, fileOutputConfig)
	store := testBinaryStore()
	store.files["op://Development/Assets/arbitrary.bin"] = []byte{}
	tree, _, err := pullInto(t, PullRequest{Cfg: cfg, Reader: store, Only: []string{"artifact"}})
	if err != nil {
		t.Fatal(err)
	}
	if readOutput(t, tree, "private/arbitrary.bin") != "" {
		t.Fatal("empty file gained bytes")
	}
	_, _, err = pullInto(t, PullRequest{Cfg: cfg, Reader: store.fakeStore, Only: []string{"artifact"}})
	if err == nil || !strings.Contains(err.Error(), "does not support file reads") {
		t.Fatalf("err = %v", err)
	}
	if len(store.reads) != 0 {
		t.Fatal("file output used the text reader")
	}
}

func TestDotenvCanReferenceFilesAndSelectSecretAliases(t *testing.T) {
	cfg := loadTestConfig(t, strings.Replace(fileOutputConfig, "format: properties", "format: dotenv", 1))
	store := testBinaryStore()
	store.values["op://Development/Assets/password"] = "a password with spaces"
	tree, log, err := pullInto(t, PullRequest{Cfg: cfg, Reader: store})
	if err != nil {
		t.Fatal(err)
	}
	values, _, err := ParseEnvFile(filepath.Join(tree, "config/app.properties"))
	if err != nil {
		t.Fatal(err)
	}
	if values["storePassword"] != store.values["op://Development/Assets/password"] {
		t.Fatal("dotenv alias changed password")
	}
	if _, err := os.Stat(values["storeFile"]); err != nil {
		t.Fatal("dotenv file path does not name the downloaded file")
	}
	if !strings.Contains(log, "Secrets for config/app.properties: 1 from 1Password") {
		t.Fatal("provenance counted one definition twice")
	}
}

func TestEnvFileProviderReadsBinaryOutputWithoutDotenv(t *testing.T) {
	main := t.TempDir()
	if out, err := exec.Command("git", "-C", main, "init", "-qb", "main").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	cfg := loadTestConfig(t, strings.Replace(fileOutputConfig, "private/arbitrary.bin", "./.env", 1))
	store := testBinaryStore()
	if err := PullOutputs(PullRequest{Cfg: cfg, TreePath: main, Only: []string{"artifact"}, Reader: store, Out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	reader, err := newEnvFileSecretReader(cfg, main)
	if err != nil {
		t.Fatal(err)
	}
	content, err := reader.ReadFile("envfile://ARTIFACT")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, store.files["op://Development/Assets/arbitrary.bin"]) {
		t.Fatal("local provider changed bytes")
	}
}

func TestWorktreePullCopiesProviderFilesFromMain(t *testing.T) {
	main := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", main}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git: %v\n%s", err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "user.name", "test")
	git("config", "user.email", "test@example.com")
	body := strings.Replace(fileOutputConfig, "  store: op", "  store: op\n  worktree_store: env_file", 1)
	if err := os.WriteFile(filepath.Join(main, wetConfigFileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", wetConfigFileName)
	git("commit", "-qm", "config")
	tree := filepath.Join(t.TempDir(), "worktree")
	git("worktree", "add", "-qb", "feature", tree)
	cfg, err := LoadWetConfig(main)
	if err != nil {
		t.Fatal(err)
	}
	store := testBinaryStore()
	if err := PullOutputs(PullRequest{Cfg: cfg, TreePath: main, Reader: store, Out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	if err := PullOutputs(PullRequest{Cfg: cfg, TreePath: tree, Out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	if readOutput(t, main, "private/arbitrary.bin") != readOutput(t, tree, "private/arbitrary.bin") {
		t.Fatal("worktree binary differs")
	}
	values, _, err := ParsePropertiesFile(filepath.Join(tree, "config/app.properties"))
	if err != nil {
		t.Fatal(err)
	}
	wantPath, _ := filepath.Abs(filepath.Join(tree, "private/arbitrary.bin"))
	if values["storeFile"] != wantPath {
		t.Fatal("properties still points at main checkout")
	}
	if values["storePassword"] != store.values["op://Development/Assets/password"] {
		t.Fatal("aliased password did not resolve from main properties")
	}
}
