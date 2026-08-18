package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
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
	if c.account != "" {
		args = append([]string{"--account", c.account}, args...)
	}
	cmd := exec.Command("op", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Stdin = os.Stdin // `op` may need to prompt for biometric unlock.
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("op %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.Bytes(), nil
}

// requireOP fails with the fix rather than "executable file not found".
func requireOP() error {
	if _, err := exec.LookPath("op"); err != nil {
		return errors.New("the 1Password CLI is not installed: brew install 1password-cli " +
			"(then `op signin`)")
	}
	return nil
}

type opAccount struct {
	URL         string `json:"url"`
	Email       string `json:"email"`
	AccountUUID string `json:"account_uuid"`
}

func (a opAccount) label() string {
	if a.Email != "" {
		return fmt.Sprintf("%s (%s)", a.URL, a.Email)
	}
	return a.URL
}

func (c opClient) accounts() ([]opAccount, error) {
	out, err := c.run("account", "list", "--format", "json")
	if err != nil {
		return nil, err
	}
	var accounts []opAccount
	if err := json.Unmarshal(out, &accounts); err != nil {
		return nil, fmt.Errorf("parse op account list: %w", err)
	}
	return accounts, nil
}

type opVault struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (c opClient) vaults() ([]opVault, error) {
	out, err := c.run("vault", "list", "--format", "json")
	if err != nil {
		return nil, err
	}
	var vaults []opVault
	if err := json.Unmarshal(out, &vaults); err != nil {
		return nil, fmt.Errorf("parse op vault list: %w", err)
	}
	return vaults, nil
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
func (c opClient) setFields(vault, title string, fields map[string]string) error {
	if len(fields) == 0 {
		return nil
	}
	args := []string{"item", "edit", title, "--vault", vault, "--format", "json"}
	for _, label := range sortedKeys(fields) {
		args = append(args, fmt.Sprintf("%s[password]=%s", label, fields[label]))
	}
	_, err := c.run(args...)
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
