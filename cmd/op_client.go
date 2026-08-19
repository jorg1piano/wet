package cmd

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// opClient is the thin 1Password CLI wrapper. Every call shells out to `op`:
// wet never holds a credential of its own, and the user's existing session is
// the authentication story.
type opClient struct {
	account string
}

// opRun executes `op` with the account flag when one is set. Stderr is kept
// separate so a failure reports what op said, not an empty exit code.
func (c opClient) run(args ...string) ([]byte, error) {
	out, _, err := c.runReporting(strings.Join(args, " "), args...)
	return out, err
}

// runReporting is run with the command description decoupled from the argv.
//
// They are the same thing for every call but `item edit`, whose argv carries
// plaintext secret values (see setFields). Passing the description separately
// is what lets that one caller report a useful failure — op's own stderr,
// which never contains a value — without the argv going along with it. The
// raw stderr is returned too so a caller can wrap it rather than restate it.
func (c opClient) runReporting(what string, args ...string) ([]byte, string, error) {
	return c.runInput(what, nil, args...)
}

// runInput is runReporting with a stdin to feed the command.
//
// A non-nil stdin displaces the terminal op would otherwise prompt on, so it
// is only for calls whose caller can survive an unavailable session — inject
// falls back to per-item reads, which do get a terminal.
func (c opClient) runInput(what string, stdin []byte, args ...string) ([]byte, string, error) {
	if c.account != "" {
		args = append([]string{"--account", c.account}, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout())
	defer cancel()
	cmd := exec.CommandContext(ctx, "op", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Only hand `op` a stdin it can actually prompt on. Inheriting a
	// non-terminal stdin is how `wet setup` came to block forever on a fresh
	// clone: op waited for an unlock nobody could give it, and the `fallback`
	// policy — whose whole promise is to survive unavailable authentication —
	// never got to see a failure, because there wasn't one yet.
	switch {
	case stdin != nil:
		cmd.Stdin = bytes.NewReader(stdin)
	case stdinIsTerminal():
		cmd.Stdin = os.Stdin
	}
	// Killing `op` is not enough on its own. Stdout here is a buffer, so exec
	// wires a pipe and Wait blocks until every writer closes it — and `op`'s
	// own children inherit that write end, so a killed parent leaves Wait
	// hanging exactly as long as the child lives. WaitDelay forces the pipes
	// shut shortly after the context fires, which is what actually makes the
	// timeout a timeout.
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		if ctx.Err() == context.DeadlineExceeded {
			msg = fmt.Sprintf("timed out after %s waiting for the 1Password CLI "+
				"(run `op signin`, or use --secrets=disabled)", opTimeout())
			// Marked so callers can stop re-paying this cost. A deadline says
			// the STORE is unreachable, not that one field is missing, and a
			// pull that asks for ten secrets must not wait ten times.
			return nil, msg, fmt.Errorf("op %s: %s: %w", what, msg, errStoreUnavailable)
		}
		return nil, msg, fmt.Errorf("op %s: %s", what, msg)
	}
	return stdout.Bytes(), "", nil
}

// opTimeoutEnv lets a slow machine or a CI runner raise the bound without a
// rebuild. Seconds; anything unparseable falls back to the default.
const opTimeoutEnv = "WET_OP_TIMEOUT_SECONDS"

// opTimeout bounds every `op` call.
//
// Unbounded was not a neutral default: a store read is now on the first-run
// path of every fresh clone, so an `op` that never answers turned `wet setup`
// from "boots on declared defaults" into "hangs". The two bounds differ
// because the thing being waited for differs — a human touching a fingerprint
// reader deserves minutes, while a non-interactive run has nobody to wait for
// and should fail fast so the policy can apply its fallback.
func opTimeout() time.Duration {
	if raw := os.Getenv(opTimeoutEnv); raw != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	if stdinIsTerminal() {
		return 2 * time.Minute
	}
	return 15 * time.Second
}

// errStoreUnavailable marks a failure that means the store itself cannot be
// reached, as opposed to one secret being absent from it. The difference
// matters for cost: an unreachable store is a fact about the whole run, so it
// is answered once, while a missing field says nothing about the next secret.
var errStoreUnavailable = errors.New("secret store unavailable")

// requireOP fails with the fix rather than "executable file not found".
func requireOP() error {
	if _, err := exec.LookPath("op"); err != nil {
		return errors.New("the 1Password CLI is not installed: brew install 1password-cli " +
			"(then `op signin`)")
	}
	return nil
}

type opField struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Value string `json:"value"`
}

type opItem struct {
	ID     string    `json:"id"`
	Title  string    `json:"title"`
	Fields []opField `json:"fields"`
}

// value reports a field's value by label, and whether a field with that label
// exists at all. The two are different: an existing but empty field is a
// placeholder waiting to be seeded, where a missing one has to be created.
func (i opItem) value(label string) (string, bool) {
	for _, f := range i.Fields {
		if f.Label == label {
			return f.Value, true
		}
	}
	return "", false
}

// item fetches one item, reporting whether it exists rather than treating
// absence as an error — `seed` creates it, `read` explains it.
func (c opClient) item(vault, title string) (opItem, bool, error) {
	out, err := c.run("item", "get", title, "--vault", vault, "--format", "json")
	if err != nil {
		if strings.Contains(err.Error(), "isn't an item") ||
			strings.Contains(err.Error(), "not found") {
			return opItem{}, false, nil
		}
		return opItem{}, false, err
	}
	var item opItem
	if err := json.Unmarshal(out, &item); err != nil {
		return opItem{}, false, fmt.Errorf("parse op item get %s: %w", title, err)
	}
	return item, true, nil
}

func (c opClient) createItem(vault, title string) (opItem, error) {
	out, err := c.run("item", "create", "--category", "API Credential",
		"--title", title, "--vault", vault, "--format", "json")
	if err != nil {
		return opItem{}, err
	}
	var item opItem
	if err := json.Unmarshal(out, &item); err != nil {
		return opItem{}, fmt.Errorf("parse op item create %s: %w", title, err)
	}
	return item, nil
}

// setFields writes label/value pairs onto an item in one call.
//
// The values are passed as arguments, which puts them in this process's argv.
// That is what `op item edit` accepts, and argv is readable by the same user's
// other processes on Linux — acceptable because the alternative (a temp file)
// puts them on disk instead, and the user already has read access to these
// secrets by definition.
//
// What is NOT acceptable is wet echoing that argv back. run() reports the
// command it ran, so an expired session or a revoked vault would print the
// real credential to the terminal and into whatever CI log captured it —
// from the one command whose contract is that it never prints a value. So the
// error is rewritten here to name the destination and the field labels only.
func (c opClient) setFields(vault, title string, fields map[string]string) error {
	if len(fields) == 0 {
		return nil
	}
	labels := sortedKeys(fields)
	args := []string{"item", "edit", title, "--vault", vault, "--format", "json"}
	for _, label := range labels {
		args = append(args, fmt.Sprintf("%s[password]=%s", label, fields[label]))
	}
	what := fmt.Sprintf("item edit %s --vault %s (fields: %s)", title, vault, strings.Join(labels, ", "))
	_, _, err := c.runReporting(what, args...)
	return err
}

// read resolves one op:// reference to its value.
func (c opClient) read(uri string) (string, error) {
	out, err := c.run("read", "--no-newline", uri)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// injectMarker brackets each reference in an inject template so the values can
// be cut back out of one blob of output.
//
// The markers are not a parsing nicety, they are the reason multi-line values
// survive: a line-oriented "KEY=ref" template cannot tell where a PEM ends and
// the next key begins, and inject emits the value verbatim.
// The nonce is what makes the marker safe rather than merely unlikely: a
// secret whose value happened to contain the sentinel would otherwise truncate
// the one after it, and vault contents are not wet's to predict.
func injectMarker(nonce string, i int, closing bool) string {
	if closing {
		return fmt.Sprintf("[/wet-%s-%d]", nonce, i)
	}
	return fmt.Sprintf("[wet-%s-%d]", nonce, i)
}

// injectNonce returns the per-call marker salt.
func injectNonce() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate inject marker: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// inject resolves many references in ONE `op` process.
//
// This is the difference between one authorization prompt and one per item.
// `op read` takes a single field and `op item get` a single item, so a repo
// whose secrets are spread over eleven items paid eleven prompts even after
// fetching per item rather than per field. inject is the only op subcommand
// that resolves an arbitrary set of references in a single invocation.
//
// It is all-or-nothing by design: op fails the whole template if any one
// reference is unresolvable, so a caller must treat an error as "fall back to
// per-item reads", never as "these secrets are missing". Per-secret absence is
// exactly what the policy needs to see, and only the per-item path can report
// it.
func (c opClient) inject(refs []string) (map[string]string, error) {
	if len(refs) == 0 {
		return map[string]string{}, nil
	}
	nonce, err := injectNonce()
	if err != nil {
		return nil, err
	}
	var tpl bytes.Buffer
	for i, ref := range refs {
		fmt.Fprintf(&tpl, "%s{{ %s }}%s\n",
			injectMarker(nonce, i, false), ref, injectMarker(nonce, i, true))
	}
	// The description deliberately omits the template: refs are not secret,
	// but keeping the argv-shaped description free of them keeps every error
	// path in this file uniformly value-free.
	out, _, err := c.runInput(fmt.Sprintf("inject (%d references)", len(refs)), tpl.Bytes(), "inject")
	if err != nil {
		return nil, err
	}
	values := make(map[string]string, len(refs))
	text := string(out)
	for i, ref := range refs {
		open, close := injectMarker(nonce, i, false), injectMarker(nonce, i, true)
		start := strings.Index(text, open)
		if start < 0 {
			return nil, fmt.Errorf("op inject did not return a value for reference %d", i)
		}
		rest := text[start+len(open):]
		end := strings.Index(rest, close)
		if end < 0 {
			return nil, fmt.Errorf("op inject returned an unterminated value for reference %d", i)
		}
		// Last write wins, matching the per-item path: two definitions may
		// legitimately name one field.
		values[ref] = rest[:end]
	}
	return values, nil
}
