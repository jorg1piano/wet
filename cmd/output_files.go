package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// FileReader is the optional binary read capability of a secret provider.
// Files bypass text batching and encoding, so every byte survives the pull.
type FileReader interface {
	ReadFile(uri string) ([]byte, error)
}

func (r *opSecretReader) ReadFile(uri string) ([]byte, error) {
	if _, err := parseOPRef(uri); err != nil {
		return nil, err
	}
	return r.client.run("read", "--no-newline", uri)
}

func (r failingReader) ReadFile(string) ([]byte, error) { return nil, r.err }

func (r *latchingReader) ReadFile(uri string) ([]byte, error) {
	if r.fatal != nil {
		return nil, r.fatal
	}
	f, ok := r.inner.(FileReader)
	if !ok {
		return nil, errors.New("secret provider does not support file reads")
	}
	value, err := f.ReadFile(uri)
	if errors.Is(err, errStoreUnavailable) {
		r.fatal = err
	}
	return value, err
}

// fileReaderAdapter reuses secret policies and per-reference deduplication.
// Go strings preserve arbitrary bytes. This adapter never offers Prefetch,
// because text injection cannot safely carry binary data.
type fileReaderAdapter struct{ reader SecretReader }

func (r fileReaderAdapter) Read(uri string) (string, error) {
	f, ok := r.reader.(FileReader)
	if !ok {
		return "", errors.New("secret provider does not support file reads")
	}
	data, err := f.ReadFile(uri)
	return string(data), err
}

type secretBinding struct {
	key  string
	name string
}

func (o OutputConfig) secretBindings() []secretBinding {
	var bindings []secretBinding
	for _, name := range o.Secrets {
		bindings = append(bindings, secretBinding{key: name, name: name})
	}
	for _, key := range o.Values.Keys {
		if name := o.Values.By[key].Secret; name != "" {
			bindings = append(bindings, secretBinding{key: key, name: name})
		}
	}
	return bindings
}

func (o OutputConfig) selectedSecretNames() []string {
	var names []string
	seen := map[string]bool{}
	if o.Secret != "" {
		names = append(names, o.Secret)
		seen[o.Secret] = true
	}
	for _, binding := range o.secretBindings() {
		if !seen[binding.name] {
			names = append(names, binding.name)
			seen[binding.name] = true
		}
	}
	return names
}

func (c *WetConfig) outputByName(name string) (OutputConfig, bool) {
	for _, o := range c.Outputs {
		if o.Name == name {
			return o, true
		}
	}
	return OutputConfig{}, false
}

// File outputs cannot depend on other outputs. Put them before their consumers
// and include on-demand files only when explicitly selected or referenced.
func (c *WetConfig) withFileDependencies(selected []OutputConfig) ([]OutputConfig, error) {
	var files, values []OutputConfig
	seen := map[string]bool{}
	addFile := func(o OutputConfig) {
		if !seen[o.Name] {
			files = append(files, o)
			seen[o.Name] = true
		}
	}
	for _, o := range selected {
		if o.Format == OutputFormatFile {
			addFile(o)
			continue
		}
		values = append(values, o)
		for _, key := range o.Values.Keys {
			name := o.Values.By[key].File
			if name == "" {
				continue
			}
			file, ok := c.outputByName(name)
			if !ok || file.Format != OutputFormatFile {
				return nil, fmt.Errorf("outputs %s: file %q must name a format: file output", o.Name, name)
			}
			addFile(file)
		}
	}
	return append(files, values...), nil
}

func resolveFileOutput(o OutputConfig, files map[string]resolvedSecret) (*ResolvedOutput, error) {
	f, ok := files[o.Secret]
	if !ok {
		return nil, fmt.Errorf("outputs %s: file secret %s was not resolved", o.Name, o.Secret)
	}
	mode, err := o.FileMode()
	if err != nil {
		return nil, fmt.Errorf("outputs %s: %w", o.Name, err)
	}
	r := &ResolvedOutput{Config: o, Content: f.value, Mode: mode}
	if f.origin == SecretFromDefault {
		r.FromDefault = []string{o.Secret}
	} else {
		r.FromStore = []string{o.Secret}
	}
	return r, nil
}

func (r *ResolvedOutput) sizeLabel() string {
	if r.Config.Format == OutputFormatFile {
		return fmt.Sprintf("%d bytes", len(r.Content))
	}
	return fmt.Sprintf("%d keys", len(r.Pairs))
}

func (r *envFileSecretReader) ReadFile(uri string) ([]byte, error) {
	ref, err := parseEnvFileRef(uri)
	if err != nil {
		return nil, err
	}
	paths := r.files[ref.Field]
	for _, path := range paths {
		data, err := os.ReadFile(filepath.Join(r.root, path))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read main-branch file %s: %w", path, err)
		}
		return data, nil
	}
	return nil, fmt.Errorf("file secret %s not found in main-branch file outputs", ref.Field)
}
