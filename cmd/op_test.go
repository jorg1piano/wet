package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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

// opRepo builds a repo whose .wet.yaml has two sections sharing one key, which
// is the arrangement the whole design turns on.
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
render:
  - template: backend/.env.tpl
    out: backend/.env
    secrets: backend
  - template: mobile/.env.tpl
    out: mobile/.env
    secrets: mobile
secrets:
  store: op
  vault: fbd
  item: fantasy
  account: my.1password.com
  sections:
    backend:
      SECRET_KEY_BASE: secret_key_base
      GOOGLE_CLIENT_ID: google_client_id
    mobile:
      GOOGLE_CLIENT_ID: google_client_id
      APPLE_KEY_ID: apple_key_id
`
	if err := os.WriteFile(filepath.Join(repo, ".wet.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	return repo
}

// resetOPFlags clears the package-level flag state cobra would otherwise carry
// between tests in one process.
func resetOPFlags(t *testing.T) {
	t.Helper()
	opFlags = opTargetFlags{}
	opSections, opSeedInput, opReadOutput = nil, "", ""
	opSeedForce, opSeedDryRun, opReadDryRun, opReadStdout, opSeedUnknown = false, false, false, false, false
	opNoRemember = true
	t.Cleanup(func() { resetOPFlagsValues() })
}

func resetOPFlagsValues() {
	opFlags = opTargetFlags{}
	opSections, opSeedInput, opReadOutput = nil, "", ""
	opSeedForce, opSeedDryRun, opReadDryRun, opReadStdout, opSeedUnknown = false, false, false, false, false
	opNoRemember = false
}

func requireJQ(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
}

// TestOPVaultSeedThenRead is the round trip the commands exist for: an env
// file goes into the vault, and comes back out as one file per section, each
// in the directory its render target writes to.
func TestOPVaultSeedThenRead(t *testing.T) {
	requireJQ(t)
	repo := opRepo(t)
	stubOP(t, `{}`)
	resetOPFlags(t)

	input := filepath.Join(repo, ".env.development")
	body := "SECRET_KEY_BASE=base123\nGOOGLE_CLIENT_ID=gid\nAPPLE_KEY_ID='a key'\n"
	if err := os.WriteFile(input, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	opSeedInput = input
	if err := runOPVaultSeed(nil, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	resetOPFlags(t)
	opReadOutput = ".env.development"
	if err := runOPVaultRead(nil, nil); err != nil {
		t.Fatalf("read: %v", err)
	}

	backend, _, err := ParseEnvFile(filepath.Join(repo, "backend", ".env.development"))
	if err != nil {
		t.Fatalf("backend file: %v", err)
	}
	if backend["SECRET_KEY_BASE"] != "base123" || backend["GOOGLE_CLIENT_ID"] != "gid" {
		t.Errorf("backend = %v, want the seeded values", backend)
	}
	// The section boundary is the point: mobile's file must not carry the
	// backend's secret even though both live in one vault item.
	if _, leaked := backend["APPLE_KEY_ID"]; leaked {
		t.Error("backend file carries mobile's APPLE_KEY_ID")
	}

	mobile, _, err := ParseEnvFile(filepath.Join(repo, "mobile", ".env.development"))
	if err != nil {
		t.Fatalf("mobile file: %v", err)
	}
	if mobile["APPLE_KEY_ID"] != "a key" {
		t.Errorf("APPLE_KEY_ID = %q, want %q", mobile["APPLE_KEY_ID"], "a key")
	}
	if mobile["GOOGLE_CLIENT_ID"] != "gid" {
		t.Errorf("shared key GOOGLE_CLIENT_ID = %q, want it in both sections", mobile["GOOGLE_CLIENT_ID"])
	}
	if _, leaked := mobile["SECRET_KEY_BASE"]; leaked {
		t.Error("mobile file carries the backend's SECRET_KEY_BASE")
	}

	for _, p := range []string{"backend/.env.development", "mobile/.env.development"} {
		info, err := os.Stat(filepath.Join(repo, p))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %o, want 600", p, info.Mode().Perm())
		}
	}
}

// TestOPVaultSeedNeverClobbers pins the promise that re-running a seed cannot
// undo a rotation done in the vault.
func TestOPVaultSeedNeverClobbers(t *testing.T) {
	requireJQ(t)
	repo := opRepo(t)
	_, state := stubOP(t, `{"fantasy":{"secret_key_base":"rotated","google_client_id":"gid"}}`)
	resetOPFlags(t)

	input := filepath.Join(repo, ".env.development")
	body := "SECRET_KEY_BASE=stale\nGOOGLE_CLIENT_ID=gid\nAPPLE_KEY_ID=new\n"
	if err := os.WriteFile(input, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	opSeedInput = input

	err := runOPVaultSeed(nil, nil)
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("seed err = %v, want a conflict that names --force", err)
	}
	after, readErr := os.ReadFile(state)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(after), "rotated") {
		t.Errorf("the vault's rotated value was overwritten: %s", after)
	}
	// The non-conflicting key still lands — a conflict elsewhere must not
	// block unrelated work.
	if !strings.Contains(string(after), "new") {
		t.Errorf("APPLE_KEY_ID was not written despite no conflict: %s", after)
	}

	// --force is the documented way through.
	resetOPFlags(t)
	opSeedInput, opSeedForce = input, true
	if err := runOPVaultSeed(nil, nil); err != nil {
		t.Fatalf("seed --force: %v", err)
	}
	after, readErr = os.ReadFile(state)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(after), "stale") {
		t.Errorf("--force did not overwrite: %s", after)
	}
}

// TestOPVaultSeedRejectsUnmappedKeys covers the quiet failure: a key in the
// env file that no section maps would otherwise never reach the vault, and
// the run would still print success.
func TestOPVaultSeedRejectsUnmappedKeys(t *testing.T) {
	requireJQ(t)
	repo := opRepo(t)
	stubOP(t, `{}`)
	resetOPFlags(t)

	input := filepath.Join(repo, ".env.development")
	if err := os.WriteFile(input, []byte("SECRET_KEY_BASE=b\nSTRIPE_KEY=sk_live\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opSeedInput = input
	err := runOPVaultSeed(nil, nil)
	if err == nil || !strings.Contains(err.Error(), "STRIPE_KEY") {
		t.Fatalf("err = %v, want it to name the unmapped key", err)
	}

	resetOPFlags(t)
	opSeedInput, opSeedUnknown = input, true
	if err := runOPVaultSeed(nil, nil); err != nil {
		t.Fatalf("seed --allow-unmapped: %v", err)
	}
}

// TestOPVaultReadRequiresADirectory covers a section no render target
// consumes: there is no directory to write into, and guessing the repo root
// would scatter env files.
func TestOPVaultReadRequiresADirectory(t *testing.T) {
	requireJQ(t)
	repo := opRepo(t)
	stubOP(t, `{"fantasy":{"secret_key_base":"b","google_client_id":"g","apple_key_id":"k"}}`)
	resetOPFlags(t)

	cfg, err := os.ReadFile(filepath.Join(repo, ".wet.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// Drop the render targets, keeping the sections.
	stripped := strings.Replace(string(cfg), `render:
  - template: backend/.env.tpl
    out: backend/.env
    secrets: backend
  - template: mobile/.env.tpl
    out: mobile/.env
    secrets: mobile
`, "", 1)
	if err := os.WriteFile(filepath.Join(repo, ".wet.yaml"), []byte(stripped), 0o644); err != nil {
		t.Fatal(err)
	}

	opReadOutput = ".env.development"
	err = runOPVaultRead(nil, nil)
	if err == nil || !strings.Contains(err.Error(), "render") {
		t.Fatalf("err = %v, want it to explain that no render target claims the section", err)
	}
}

// TestEnvPopulateUsesSectionSecrets closes the loop the whole feature exists
// for: a render target naming a section gets those keys as template fields,
// and a target naming a different section cannot see them.
func TestEnvPopulateUsesSectionSecrets(t *testing.T) {
	requireJQ(t)
	repo := opRepo(t)
	stubOP(t, `{"fantasy":{"secret_key_base":"base123","google_client_id":"gid","apple_key_id":"akid"}}`)
	resetOPFlags(t)

	for dir, body := range map[string]string{
		"backend": "SECRET_KEY_BASE={{.SECRET_KEY_BASE}}\nGOOGLE_CLIENT_ID={{.GOOGLE_CLIENT_ID}}\n",
		"mobile":  "APPLE_KEY_ID={{.APPLE_KEY_ID}}\nGOOGLE_CLIENT_ID={{.GOOGLE_CLIENT_ID}}\n",
	} {
		if err := os.MkdirAll(filepath.Join(repo, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, dir, ".env.tpl"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cfg, err := LoadWetConfig(repo)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	for _, target := range cfg.Render {
		values, err := readSectionSecrets(cfg, target.Secrets)
		if err != nil {
			t.Fatalf("readSectionSecrets(%s): %v", target.Secrets, err)
		}
		if target.Secrets == "backend" {
			if values["SECRET_KEY_BASE"] != "base123" {
				t.Errorf("backend SECRET_KEY_BASE = %q, want base123", values["SECRET_KEY_BASE"])
			}
			if _, leaked := values["APPLE_KEY_ID"]; leaked {
				t.Error("the backend section resolved a mobile-only key")
			}
		}
		if target.Secrets == "mobile" {
			if values["APPLE_KEY_ID"] != "akid" {
				t.Errorf("mobile APPLE_KEY_ID = %q, want akid", values["APPLE_KEY_ID"])
			}
			if _, leaked := values["SECRET_KEY_BASE"]; leaked {
				t.Error("the mobile section resolved a backend-only key")
			}
		}
		if values["GOOGLE_CLIENT_ID"] != "gid" {
			t.Errorf("%s GOOGLE_CLIENT_ID = %q, want the shared value", target.Secrets, values["GOOGLE_CLIENT_ID"])
		}
	}

	// A target with no section fetches nothing at all — no vault round trip
	// for a repo that does not use secrets.
	if values, err := readSectionSecrets(cfg, ""); err != nil || values != nil {
		t.Errorf("readSectionSecrets(\"\") = %v, %v; want no values and no error", values, err)
	}
}
