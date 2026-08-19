package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// stubOP puts a fake `op` at the front of PATH. It is a real script rather
// than an interface seam because that is what the production path uses — a
// mock of opClient would pass while the arguments wet actually passes to op
// were wrong.
//
// The stub keeps its "vault" in a JSON file so a seed and a later read see the
// same state, which is what makes the round-trip test meaningful.
func stubOP(t *testing.T, initial string) (binDir, statePath string) {
	t.Helper()
	binDir = t.TempDir()
	statePath = filepath.Join(binDir, "vault.json")
	if err := os.WriteFile(statePath, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `#!/usr/bin/env bash
set -euo pipefail
state="` + statePath + `"
args=("$@")
# Drop a leading --account pair the way the real op accepts it anywhere.
if [ "${args[0]}" = "--account" ]; then args=("${args[@]:2}"); fi

case "${args[0]} ${args[1]}" in
  "account list") jq -n '[{url:"my.1password.com",email:"a@b.c",account_uuid:"U"}]' ;;
  "vault list")   jq -n '[{id:"V1",name:"fbd"}]' ;;
  "item get")
      title="${args[2]}"
      jq -e --arg t "$title" '.[$t] // empty' "$state" >/dev/null || {
        echo "\"$title\" isn't an item in any vault" >&2; exit 1; }
      jq --arg t "$title" '{id:$t,title:$t,fields:(.[$t]|to_entries|map({id:.key,label:.key,value:.value}))}' "$state"
      ;;
  "item create")
      title=""
      for ((i=0;i<${#args[@]};i++)); do [ "${args[$i]}" = "--title" ] && title="${args[$((i+1))]}"; done
      tmp="$(mktemp)"; jq --arg t "$title" '.[$t] = (.[$t] // {})' "$state" > "$tmp"; mv "$tmp" "$state"
      jq -n --arg t "$title" '{id:$t,title:$t,fields:[]}'
      ;;
  "item edit")
      title="${args[2]}"
      tmp="$(mktemp)"; cp "$state" "$tmp"
      for a in "${args[@]:3}"; do
        case "$a" in
          *"[password]="*)
            label="${a%%[*}"; value="${a#*=}"
            out="$(mktemp)"
            jq --arg t "$title" --arg l "$label" --arg v "$value" '.[$t][$l] = $v' "$tmp" > "$out"
            mv "$out" "$tmp" ;;
        esac
      done
      mv "$tmp" "$state"
      echo '{}'
      ;;
  "read --no-newline")
      ref="${args[2]}"; rest="${ref#op://}"
      item="$(echo "$rest" | cut -d/ -f2)"; field="$(echo "$rest" | cut -d/ -f3-)"
      jq -er --arg t "$item" --arg f "$field" '.[$t][$f] // empty' "$state" | tr -d '\n'
      ;;
  *) echo "stub op: unhandled: ${args[*]}" >&2; exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "op"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return binDir, statePath
}

// opRepo builds a repo whose .wet.yaml declares two outputs sharing one secret
// definition, which is the arrangement the whole design turns on: one
// credential in the store, and per-output selection deciding who sees it.
func opRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	cfg := `project_name: app
port_stride: 10
ports:
  - name: APP_PORT
    base: 4000
  - name: GRPC_PORT
    base: 50051
env:
  DEV_HOST: localhost
  AUTH_BASE_URL: "http://{{.DEV_HOST}}:{{.APP_PORT}}"
secrets:
  store: op
  policy: fallback
  vault: fbd
  item: fantasy
  account: my.1password.com
  definitions:
    SECRET_KEY_BASE:
      field: secret_key_base
      default: dev-only
    GOOGLE_CLIENT_ID:
      field: google_client_id
      default: ""
    APPLE_KEY_ID:
      field: apple_key_id
      default: ""
outputs:
  - name: backend
    out: backend/.env.dev
    format: dotenv
    include: [AUTH_BASE_URL]
    secrets: [SECRET_KEY_BASE, GOOGLE_CLIENT_ID]
  - name: mobile
    out: mobile/.env
    format: dotenv
    values:
      host:
        from: DEV_HOST
      port:
        from: GRPC_PORT
      clientId:
        literal: app-mobile
`
	if err := os.WriteFile(filepath.Join(repo, ".wet.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	return repo
}

// worktreeEnv makes the commands resolvable without a .wet-worktrees.json:
// buildWorktreeData prefers the hook environment, which is exactly the shape
// BuildHookEnv exports.
func worktreeEnv(t *testing.T, slot int) {
	t.Helper()
	t.Setenv("SLOT", strconv.Itoa(slot))
	t.Setenv("BRANCH", "feature/x")
	t.Setenv("APP_PORT", strconv.Itoa(4000+slot*10))
	t.Setenv("GRPC_PORT", strconv.Itoa(50051+slot*10))
}

// resetEnvFlags clears the package-level flag state cobra would otherwise
// carry between tests in one process.
func resetEnvFlags(t *testing.T) {
	t.Helper()
	restore := func() {
		envPullDryRun, envPullSecrets = false, ""
		envPushFrom = ""
		envPushDryRun, envPushForce, envPushCreateItem = false, false, false
		envPushRejectUnmapped, envPushIncludeDefaults = false, false
		opFlags, opNoRemember = opTargetFlags{}, false
	}
	restore()
	opNoRemember = true
	t.Cleanup(restore)
}

// requireJQ skips when the `op` stub's dependencies are absent — but only on
// a developer machine. In CI a skip is indistinguishable from a pass, and
// these are the only tests covering the command that writes to a credential
// store, so an image that quietly lost `jq` would report a green suite with
// the whole push surface untested. Set WET_REQUIRE_OP_TESTS=1 there.
func requireJQ(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"jq", "bash"} {
		if _, err := exec.LookPath(tool); err == nil {
			continue
		}
		if os.Getenv("WET_REQUIRE_OP_TESTS") != "" {
			t.Fatalf("%s is not installed, and WET_REQUIRE_OP_TESTS is set — "+
				"the secret-store tests must not be skipped here", tool)
		}
		t.Skipf("%s not available", tool)
	}
}

// TestEnvPushThenPull is the round trip the commands exist for: real values go
// into the store from a dotenv file, and come back out in the generated file
// of the output that selected them — and only that output.
func TestEnvPushThenPull(t *testing.T) {
	requireJQ(t)
	stubOP(t, `{"fantasy":{}}`)
	repo := opRepo(t)
	resetEnvFlags(t)
	worktreeEnv(t, 1)

	src := filepath.Join(repo, "import.env")
	if err := os.WriteFile(src, []byte(
		"SECRET_KEY_BASE=real-key\nGOOGLE_CLIENT_ID=goog-123\nAUTH_BASE_URL=http://ignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envPushFrom = src
	envPushCreateItem = true
	if err := runEnvPush(nil, []string{"backend"}); err != nil {
		t.Fatalf("push: %v", err)
	}

	if err := runEnvPull(nil, nil); err != nil {
		t.Fatalf("pull: %v", err)
	}
	backend := readFileString(t, filepath.Join(repo, "backend/.env.dev"))
	if !strings.Contains(backend, "SECRET_KEY_BASE=real-key") {
		t.Errorf("backend missing the pushed secret:\n%s", backend)
	}
	if !strings.Contains(backend, "GOOGLE_CLIENT_ID=goog-123") {
		t.Errorf("backend missing the pushed client id:\n%s", backend)
	}
	// Slot 1's strided port, derived in env: and consumed by include:.
	if !strings.Contains(backend, "AUTH_BASE_URL=http://localhost:4010") {
		t.Errorf("backend did not carry this worktree's port:\n%s", backend)
	}

	mobile := readFileString(t, filepath.Join(repo, "mobile/.env"))
	for _, leaked := range []string{"SECRET_KEY_BASE", "GOOGLE_CLIENT_ID", "real-key", "goog-123"} {
		if strings.Contains(mobile, leaked) {
			t.Errorf("mobile output leaked %q — it selects no secrets:\n%s", leaked, mobile)
		}
	}
	if !strings.Contains(mobile, "port=50061") {
		t.Errorf("mobile did not rename GRPC_PORT to port:\n%s", mobile)
	}
	if !strings.Contains(mobile, "clientId=app-mobile") {
		t.Errorf("mobile lost its literal:\n%s", mobile)
	}
}

// A generated file is the normal push input, and it is full of ordinary
// configuration and of fallback placeholders. Neither may reach the store: an
// ordinary key has no destination at all, and a placeholder would overwrite a
// real credential with a dev-only string.
func TestEnvPushIgnoresOrdinaryKeysAndFallbacks(t *testing.T) {
	requireJQ(t)
	_, state := stubOP(t, `{"fantasy":{}}`)
	repo := opRepo(t)
	resetEnvFlags(t)
	worktreeEnv(t, 1)

	src := filepath.Join(repo, "generated.env")
	if err := os.WriteFile(src, []byte(
		// SECRET_KEY_BASE holds exactly the declared default, GOOGLE_CLIENT_ID
		// the declared empty default, and AUTH_BASE_URL is not a secret at all.
		"AUTH_BASE_URL=http://localhost:4010\nSECRET_KEY_BASE=dev-only\nGOOGLE_CLIENT_ID=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envPushFrom = src
	envPushCreateItem = true
	if err := runEnvPush(nil, []string{"backend"}); err != nil {
		t.Fatalf("push: %v", err)
	}

	got := readFileString(t, state)
	for _, leaked := range []string{"dev-only", "AUTH_BASE_URL", "http://localhost"} {
		if strings.Contains(got, leaked) {
			t.Errorf("store received %q, which must never be pushed:\n%s", leaked, got)
		}
	}
}

// --reject-unmapped is the opt-in for a dedicated secrets file, where a typo
// should be loud rather than silently ignored.
func TestEnvPushRejectUnmapped(t *testing.T) {
	requireJQ(t)
	stubOP(t, `{"fantasy":{}}`)
	repo := opRepo(t)
	resetEnvFlags(t)
	worktreeEnv(t, 1)

	src := filepath.Join(repo, "import.env")
	if err := os.WriteFile(src, []byte("SECRET_KEY_BASE=real\nAPPLE_KEY_ID=nope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envPushFrom, envPushRejectUnmapped = src, true
	err := runEnvPush(nil, []string{"backend"})
	if err == nil {
		t.Fatal("expected --reject-unmapped to fail on a key the output does not select")
	}
	// APPLE_KEY_ID is a defined secret — just not one this output selects.
	// Being defined must not make it pushable here.
	if !strings.Contains(err.Error(), "APPLE_KEY_ID") {
		t.Errorf("error should name the offending key, got: %v", err)
	}
}

// Re-running a push must never undo a rotation done directly in the store.
func TestEnvPushNeverClobbers(t *testing.T) {
	requireJQ(t)
	_, state := stubOP(t, `{"fantasy":{"secret_key_base":"rotated-in-vault"}}`)
	repo := opRepo(t)
	resetEnvFlags(t)
	worktreeEnv(t, 1)

	src := filepath.Join(repo, "import.env")
	if err := os.WriteFile(src, []byte("SECRET_KEY_BASE=stale-local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envPushFrom = src
	if err := runEnvPush(nil, []string{"backend"}); err == nil {
		t.Fatal("expected a conflict error")
	}
	if got := readFileString(t, state); !strings.Contains(got, "rotated-in-vault") {
		t.Errorf("push overwrote a rotated value:\n%s", got)
	}

	envPushForce = true
	if err := runEnvPush(nil, []string{"backend"}); err != nil {
		t.Fatalf("--force push: %v", err)
	}
	if got := readFileString(t, state); !strings.Contains(got, "stale-local") {
		t.Errorf("--force did not overwrite:\n%s", got)
	}
}

// A missing destination item is an error rather than a silent create: the
// usual cause is a wrong `item:`, and creating it would leave the real item
// untouched while the run reported success.
func TestEnvPushRequiresCreateItem(t *testing.T) {
	requireJQ(t)
	stubOP(t, `{}`)
	repo := opRepo(t)
	resetEnvFlags(t)
	worktreeEnv(t, 1)

	src := filepath.Join(repo, "import.env")
	if err := os.WriteFile(src, []byte("SECRET_KEY_BASE=real\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envPushFrom = src
	err := runEnvPush(nil, []string{"backend"})
	if err == nil || !strings.Contains(err.Error(), "--create-item") {
		t.Fatalf("expected a --create-item hint, got: %v", err)
	}
}

// `wet env pull <name>` regenerates only the named file. Anything else is a
// surprise edit to a file the user did not ask about.
func TestEnvPullOnlyTouchesTheNamedOutput(t *testing.T) {
	requireJQ(t)
	stubOP(t, `{"fantasy":{"secret_key_base":"v","google_client_id":"g"}}`)
	repo := opRepo(t)
	resetEnvFlags(t)
	worktreeEnv(t, 1)

	if err := runEnvPull(nil, nil); err != nil {
		t.Fatalf("pull all: %v", err)
	}
	mobilePath := filepath.Join(repo, "mobile/.env")
	if err := os.WriteFile(mobilePath, []byte("SENTINEL\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runEnvPull(nil, []string{"backend"}); err != nil {
		t.Fatalf("pull backend: %v", err)
	}
	if got := readFileString(t, mobilePath); got != "SENTINEL\n" {
		t.Errorf("pulling backend rewrote mobile/.env:\n%s", got)
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

// --dry-run exists to preview conflicts, so it must report them AND fail.
// Exiting 0 would make `push --dry-run && push` read as a safety gate that
// walks straight into the failure it was supposed to catch.
func TestEnvPushDryRunWritesNothingAndFailsOnConflict(t *testing.T) {
	requireJQ(t)
	_, state := stubOP(t, `{"fantasy":{"secret_key_base":"rotated-in-vault"}}`)
	repo := opRepo(t)
	resetEnvFlags(t)
	worktreeEnv(t, 1)

	src := filepath.Join(repo, "import.env")
	if err := os.WriteFile(src, []byte("SECRET_KEY_BASE=stale\nGOOGLE_CLIENT_ID=goog\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envPushFrom, envPushDryRun = src, true
	err := runEnvPush(nil, []string{"backend"})
	if err == nil {
		t.Fatal("dry run reported success despite a conflict")
	}
	got := readFileString(t, state)
	if strings.Contains(got, "stale") || strings.Contains(got, "goog") {
		t.Errorf("dry run wrote to the store:\n%s", got)
	}
	if !strings.Contains(got, "rotated-in-vault") {
		t.Errorf("dry run disturbed the store:\n%s", got)
	}
}

// A generated file's placeholders are refused by default; --include-defaults
// is the deliberate override, and an untested flag is one that can break
// while the suite stays green.
func TestEnvPushIncludeDefaults(t *testing.T) {
	requireJQ(t)
	_, state := stubOP(t, `{"fantasy":{}}`)
	repo := opRepo(t)
	resetEnvFlags(t)
	worktreeEnv(t, 1)

	src := filepath.Join(repo, "generated.env")
	if err := os.WriteFile(src, []byte("SECRET_KEY_BASE=dev-only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envPushFrom, envPushCreateItem = src, true
	if err := runEnvPush(nil, []string{"backend"}); err != nil {
		t.Fatalf("push: %v", err)
	}
	if strings.Contains(readFileString(t, state), "dev-only") {
		t.Fatal("the fallback value was pushed without --include-defaults")
	}

	envPushIncludeDefaults = true
	if err := runEnvPush(nil, []string{"backend"}); err != nil {
		t.Fatalf("push --include-defaults: %v", err)
	}
	if !strings.Contains(readFileString(t, state), "dev-only") {
		t.Fatal("--include-defaults did not push the value")
	}
}

// An output that selects no secrets has nothing to push, and saying so beats
// a run that reports success having written nothing.
func TestEnvPushRefusesASecretlessOutput(t *testing.T) {
	requireJQ(t)
	stubOP(t, `{"fantasy":{}}`)
	repo := opRepo(t)
	resetEnvFlags(t)
	worktreeEnv(t, 1)

	src := filepath.Join(repo, "import.env")
	if err := os.WriteFile(src, []byte("SECRET_KEY_BASE=real\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envPushFrom = src
	err := runEnvPush(nil, []string{"mobile"})
	if err == nil || !strings.Contains(err.Error(), "mobile") {
		t.Fatalf("err = %v, want it to name the secretless output", err)
	}
}

// The one command whose contract is "values never are printed" must keep it on
// the failure path too. `op item edit` takes the value in argv, and wet's
// generic runner reports the command it ran — so a failed edit used to print
// the real credential to the terminal and into any CI log.
func TestEnvPushFailureNeverPrintsTheValue(t *testing.T) {
	requireJQ(t)
	// A stub that refuses every `item edit` is what an expired session or a
	// revoked vault looks like.
	binDir, _ := stubOP(t, `{"fantasy":{}}`)
	failing := "#!/usr/bin/env bash\n" +
		"for a in \"$@\"; do if [ \"$a\" = \"edit\" ]; then echo 'session expired' >&2; exit 1; fi; done\n" +
		"exec \"" + filepath.Join(binDir, "op") + "\" \"$@\"\n"
	shim := t.TempDir()
	if err := os.WriteFile(filepath.Join(shim, "op"), []byte(failing), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))

	repo := opRepo(t)
	resetEnvFlags(t)
	worktreeEnv(t, 1)
	const secret = "sup3r-s3cret-value"
	src := filepath.Join(repo, "import.env")
	if err := os.WriteFile(src, []byte("SECRET_KEY_BASE="+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envPushFrom, envPushCreateItem = src, true

	err := runEnvPush(nil, []string{"backend"})
	if err == nil {
		t.Fatal("expected the failing store write to be reported")
	}
	msg := err.Error()
	if strings.Contains(msg, secret) || strings.Contains(msg, "[password]=") {
		t.Fatalf("the error disclosed the credential: %s", msg)
	}
	// It still has to be diagnosable: the destination and the store's own
	// reason are what the user needs.
	for _, want := range []string{"secret_key_base", "session expired"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should mention %q, got: %s", want, msg)
		}
	}
}

// Two definitions may legitimately name one field — one credential under two
// names. Two DIFFERENT values for it is a config bug, and letting the map's
// last write win would push whichever name sorted later, differently between
// runs.
func TestEnvPushRejectsTwoValuesForOneField(t *testing.T) {
	requireJQ(t)
	stubOP(t, `{"fantasy":{}}`)
	repo := t.TempDir()
	if out, err := execCommand("git", "-C", repo, "init", "-q"); err != nil {
		t.Skipf("git unavailable: %v\n%s", err, out)
	}
	cfg := `project_name: app
port_stride: 10
ports:
  - name: APP_PORT
    base: 4000
secrets:
  store: op
  vault: fbd
  item: fantasy
  account: my.1password.com
  definitions:
    ALIAS_ONE:
      field: shared_field
      default: ""
    ALIAS_TWO:
      field: shared_field
      default: ""
outputs:
  - name: backend
    out: backend/.env.dev
    format: dotenv
    secrets: [ALIAS_ONE, ALIAS_TWO]
`
	if err := os.WriteFile(filepath.Join(repo, ".wet.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	resetEnvFlags(t)
	worktreeEnv(t, 1)

	src := filepath.Join(repo, "import.env")
	if err := os.WriteFile(src, []byte("ALIAS_ONE=first\nALIAS_TWO=second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envPushFrom, envPushCreateItem = src, true
	err := runEnvPush(nil, []string{"backend"})
	if err == nil {
		t.Fatal("two values for one destination field were accepted")
	}
	if !strings.Contains(err.Error(), "shared_field") {
		t.Errorf("err should name the contested field, got: %v", err)
	}

	// The same value under both names is the legitimate case and must work.
	if err := os.WriteFile(src, []byte("ALIAS_ONE=same\nALIAS_TWO=same\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runEnvPush(nil, []string{"backend"}); err != nil {
		t.Fatalf("one credential under two names should push: %v", err)
	}
}

// `wet op vault` writes the overlay to the checkout the LOADER reads it from —
// the one holding the trunk, not the current worktree. Writing it beside a
// worktree printed "Remembered" while writing a file nothing ever reads, and
// the next pull still said "no vault".
func TestOPVaultRemembersWhereTheLoaderReads(t *testing.T) {
	requireJQ(t)
	stubOP(t, `{"fantasy":{}}`)
	repo := opRepo(t)
	resetEnvFlags(t)
	opNoRemember = false
	// A value on the command line is remembered too: the user said which vault
	// they want, and asking again next run is the same surprise.
	opFlags = opTargetFlags{account: "my.1password.com", vault: "picked-vault"}

	if err := runOPVault(nil, nil); err != nil {
		t.Fatalf("op vault: %v", err)
	}
	// The assertion that matters: the loader must SEE it. Reading the file
	// directly would pass even if it landed in a directory nothing reads.
	cfg, err := LoadWetConfigWithLocal(repo, LocalConfigDir(repo, repo))
	if err != nil {
		t.Fatalf("load after remember: %v", err)
	}
	if cfg.Secrets.Vault != "picked-vault" {
		t.Errorf("the loader does not see the remembered vault: %q", cfg.Secrets.Vault)
	}
}

// The design makes this the authorization boundary in so many words:
// "Presence in secrets.definitions plus selection by the named output is the
// authorization boundary for every push." It had no guard, so a regression
// that let any declared secret through — or any key in the file — would have
// been silent.
func TestEnvPushWritesOnlyWhatTheOutputSelects(t *testing.T) {
	requireJQ(t)
	_, state := stubOP(t, `{"fantasy":{}}`)
	repo := opRepo(t)
	resetEnvFlags(t)
	worktreeEnv(t, 1)

	src := filepath.Join(repo, "mixed.env")
	if err := os.WriteFile(src, []byte(
		// Selected by backend:
		"SECRET_KEY_BASE=real-key\n"+
			// A DEFINED secret that backend does not select (mobile's, in the
			// fixture's terms). Being defined must not make it pushable here.
			"APPLE_KEY_ID=apple-should-not-move\n"+
			// An ordinary value, and a name wet has never heard of.
			"AUTH_BASE_URL=http://localhost:4010\n"+
			"TOTALLY_UNKNOWN=nope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envPushFrom, envPushCreateItem = src, true
	if err := runEnvPush(nil, []string{"backend"}); err != nil {
		t.Fatalf("push: %v", err)
	}

	got := readFileString(t, state)
	if !strings.Contains(got, "real-key") {
		t.Errorf("the selected secret was not written:\n%s", got)
	}
	for _, forbidden := range []string{
		"apple-should-not-move", // defined, but not selected by this output
		"apple_key_id",
		"http://localhost:4010", // ordinary configuration
		"nope",                  // undeclared entirely
		"TOTALLY_UNKNOWN",
	} {
		if strings.Contains(got, forbidden) {
			t.Errorf("push wrote %q, which the backend output does not select:\n%s", forbidden, got)
		}
	}
}

// A store read is now on the first-run path of every fresh clone, so an `op`
// that never answers must not turn `wet setup` into a hang. This is the case
// `policy: fallback` promises to survive ("authentication is unavailable"),
// and it could not survive it while the read was unbounded — there was no
// failure yet for the policy to act on.
//
// THREE secrets, and the assertion is on TOTAL wall clock, because a per-call
// bound is not a bound on the run. The first version of this test declared one
// secret: it proved a single read was bounded and was blind to a pull paying
// the timeout once per secret, which is how "no longer hangs" came back as
// twenty-one minutes for the design doc's own ten-secret example.
//
// The subtlety worth keeping: killing `op` is not enough. Its children inherit
// the stdout pipe, so `Wait` blocks as long as the child lives regardless of
// the context. Without cmd.WaitDelay this test hangs rather than fails.
func TestStoreReadIsBoundedSoFallbackCanApply(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	stub := t.TempDir()
	if err := os.WriteFile(filepath.Join(stub, "op"), []byte("#!/bin/sh\nsleep 600\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stub+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(opTimeoutEnv, "1")

	cfg := loadTestConfig(t, `project_name: app
port_stride: 10
ports:
  - { name: APP_PORT, base: 4000 }
env:
  DEV_HOST: localhost
secrets:
  store: op
  policy: fallback
  vault: V
  item: I
  definitions:
    ONE:
      field: one
      default: 'd1'
    TWO:
      field: two
      default: 'd2'
    THREE:
      field: three
      default: 'd3'
outputs:
  - name: backend
    out: backend/.env.dev
    format: dotenv
    include: [DEV_HOST]
    secrets: [ONE, TWO, THREE]
`)

	type result struct {
		tree string
		err  error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		tree, _, err := pullInto(t, PullRequest{Cfg: cfg, Data: worktreeData(t, cfg, 1, nil)})
		done <- result{tree, err}
	}()

	var got result
	select {
	case got = <-done:
	case <-time.After(90 * time.Second):
		t.Fatal("the pull never returned — an unresponsive store must not hang setup")
	}
	elapsed := time.Since(start)
	if got.err != nil {
		t.Fatalf("fallback should have carried the pull: %v", got.err)
	}

	// One timeout plus one WaitDelay, not three of each. The bound is generous
	// enough not to flake on a loaded machine and still far below the 3x cost
	// of the defect (~18s here).
	if elapsed > 12*time.Second {
		t.Errorf("the pull paid the timeout more than once: %s for 3 secrets — "+
			"an unreachable store must be discovered once, not per secret", elapsed)
	}
	body := readOutput(t, got.tree, "backend/.env.dev")
	for _, want := range []string{"ONE=d1", "TWO=d2", "THREE=d3"} {
		if !strings.Contains(body, want) {
			t.Errorf("declared default missing after the store timed out (%s):\n%s", want, body)
		}
	}
}

// Without the guard `resolveOPTarget` dereferences a nil cfg.Secrets. A repo
// with no secrets: block is a normal repo, not a crash.
func TestOPVaultWithoutASecretsBlock(t *testing.T) {
	requireJQ(t)
	stubOP(t, `{}`)
	repo := t.TempDir()
	if out, err := execCommand("git", "-C", repo, "init", "-q"); err != nil {
		t.Skipf("git unavailable: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(repo, ".wet.yaml"),
		[]byte("project_name: app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	resetEnvFlags(t)
	err := runOPVault(nil, nil)
	if err == nil {
		t.Fatal("expected an error for a config with no secrets: block")
	}
	if !strings.Contains(err.Error(), "secrets:") {
		t.Errorf("err should say what is missing, got: %v", err)
	}
}
