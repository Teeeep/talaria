// Package config holds the two things that come from outside the spec: the
// profile file and the environment. Between them they answer "which base URL,
// which extra headers, and which credential names does this call use" — and
// they answer it in *names*, never values, so the credential firewall in
// internal/secret extends all the way out to the user's configuration.
package config

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/Teeeep/talaria/internal/clierr"
)

// FileMode is the mode a profile file must not exceed. The file names
// credentials and describes internal hosts, and DESIGN.md §5 requires 0600, so
// anything group- or world-readable is refused rather than warned about.
const FileMode os.FileMode = 0o600

// Config is the parsed profile file. The zero value is a usable empty config,
// which is what a missing file loads as.
type Config struct {
	Profiles map[string]*Profile `yaml:"profiles"`

	// path is where this config was read from, so an error about a profile can
	// name the file to edit.
	path string
}

// Profile is one named environment: where to send requests, what to send with
// them, and which credentials to use.
type Profile struct {
	// Name is the key this profile appeared under. It is filled in on lookup
	// rather than read from the file.
	Name string `yaml:"-"`
	// BaseURL overrides the spec's servers[0].url. It loses to --base-url.
	BaseURL string `yaml:"base-url"`
	// Headers are sent with every request made under this profile.
	Headers map[string]string `yaml:"headers"`
	// Auth maps a security scheme name from the spec to an environment-variable
	// reference such as ${STAGING_TOKEN}. It holds references, never values:
	// see Resolve.
	Auth map[string]string `yaml:"auth"`
}

// DefaultPath is where the profile file lives: $XDG_CONFIG_HOME/talaria/config.yaml,
// falling back to the platform user config dir (~/.config on Linux).
func DefaultPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		var err error
		if dir, err = os.UserConfigDir(); err != nil {
			return "", clierr.Usage("cannot locate the talaria config directory: %w", err)
		}
	}

	return filepath.Join(dir, "talaria", "config.yaml"), nil
}

// Load reads the profile file at path, or the default location when path is
// empty. A file that is not there is not an error — most invocations have no
// profile file at all — but one that exists and is unreadable, unparseable or
// too permissive is.
func Load(path string) (*Config, error) {
	if path == "" {
		var err error
		if path, err = DefaultPath(); err != nil {
			return nil, err
		}
	}

	data, err := readPrivate(path)
	if err != nil {
		return nil, err
	}
	if data == nil {
		return &Config{path: path}, nil
	}

	// KnownFields makes a mistyped key an error instead of silence. A profile
	// whose `baseurl:` was quietly ignored sends the request somewhere else,
	// which is exactly the class of surprise this tool exists to remove.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	cfg := &Config{path: path}
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, clierr.Usage("parsing %s: %w", path, err)
	}

	return cfg, nil
}

// readPrivate reads path after checking that nobody but its owner can read it.
// It returns nil data and no error when the file does not exist.
func readPrivate(path string) ([]byte, error) {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, clierr.Usage("reading %s: %w", path, err)
	}

	// Only the extra bits matter: 0400 is stricter than required and fine,
	// while any group or other bit exposes the credential names to the machine.
	if extra := info.Mode().Perm() &^ FileMode; extra != 0 {
		return nil, clierr.Usage(
			"%s has mode %#o; it names credentials, so it must be %#o or stricter: chmod 0600 %s",
			path, info.Mode().Perm(), FileMode, path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, clierr.Usage("reading %s: %w", path, err)
	}

	return data, nil
}

// Profile looks up a named profile. An empty name selects none and returns
// nil, which is the common case: no --profile was given.
//
// An unknown name is a bad invocation (exit 2) carrying the names that do
// exist, so an agent that guessed can correct itself in one step.
func (c *Config) Profile(name string) (*Profile, error) {
	if name == "" {
		return nil, nil
	}

	if prof, ok := c.Profiles[name]; ok && prof != nil {
		prof.Name = name
		return prof, nil
	}

	if len(c.Profiles) == 0 {
		return nil, clierr.Usage("no profile %q: %s defines none", name, c.path)
	}

	return nil, clierr.Usage("no profile %q in %s", name, c.path).
		WithAlternatives(c.ProfileNames()...)
}

// ProfileNames lists the defined profiles in sorted order, for error messages
// and for `talaria` surfaces that offer the user a choice.
func (c *Config) ProfileNames() []string {
	names := make([]string, 0, len(c.Profiles))
	for name := range c.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)

	return names
}
