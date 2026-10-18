package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// DefaultFileName is the config file name inside the config directory.
const DefaultFileName = "config.json"

// ErrNotFound is returned when the config file does not exist.
var ErrNotFound = errors.New("config file not found")

// writeMu serialises writes inside this process. The daemon owns the file
// during normal operation, so an in-process lock plus an atomic rename is
// enough to keep readers from ever seeing a half-written file.
var writeMu sync.Mutex

// DefaultDir returns the platform-appropriate configuration directory.
func DefaultDir() (string, error) {
	if dir := os.Getenv("WAKIL_CONFIG_DIR"); dir != "" {
		return dir, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locating user config dir: %w", err)
	}
	return filepath.Join(base, "wakil-mcp"), nil
}

// DefaultPath returns the full path to the default config file.
func DefaultPath() (string, error) {
	dir, err := DefaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, DefaultFileName), nil
}

// Default returns a config with sensible starting values.
//
// Auth is on by default. A config built by hand must therefore supply an admin
// token and at least one client, or set disableAuth; LoadOrCreate does this
// automatically by minting both.
func Default() *Config {
	return &Config{
		Version: CurrentVersion,
		Server: ServerSettings{
			Bind:     "127.0.0.1:8765",
			LogLevel: "info",
		},
		Defaults: ServerDefaults{
			StartPolicy:    StartLazy,
			IdleTimeout:    Duration(15 * minute),
			CallTimeout:    Duration(2 * minute),
			ConnectTimeout: Duration(30 * second),
			Separator:      ".",
		},
		MCPServers: map[string]*Upstream{},
	}
}

// Path returns where this config was loaded from, or "" if it was built in
// memory.
func (c *Config) Path() string { return c.path }

// SetPath records the source path without touching the filesystem.
func (c *Config) SetPath(p string) { c.path = p }

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}
	c, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}
	c.path = path
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return c, nil
}

// Bootstrap carries the credentials minted when a config file is first
// created. The plaintext values are shown to the operator once and never
// stored.
type Bootstrap struct {
	// ClientName is the profile the client token belongs to.
	ClientName string
	// ClientToken authenticates a harness to the MCP endpoint.
	ClientToken string
	// AdminToken authenticates an operator to the REST API and web UI.
	AdminToken string
}

// LoadOrCreate loads the config, bootstrapping a fresh one when none exists.
//
// On first creation the config gets one client profile for harnesses plus a
// separate admin token for the UI. Plaintext values are returned once so the
// caller can display them; only hashes are written. On later calls the
// Bootstrap is nil.
func LoadOrCreate(path string) (*Config, *Bootstrap, error) {
	c, err := Load(path)
	if err == nil {
		return c, nil, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, nil, err
	}

	c = Default()
	c.path = path

	clientToken, clientHash, err := GenerateToken()
	if err != nil {
		return nil, nil, err
	}
	adminToken, adminHash, err := GenerateToken()
	if err != nil {
		return nil, nil, err
	}

	const bootstrapName = "default"
	c.Clients = []ClientProfile{{
		Name:      bootstrapName,
		TokenHash: clientHash,
	}}
	c.Server.AdminTokenHash = adminHash

	if err := c.Save(); err != nil {
		return nil, nil, err
	}
	return c, &Bootstrap{
		ClientName:  bootstrapName,
		ClientToken: clientToken,
		AdminToken:  adminToken,
	}, nil
}

// Parse decodes config bytes without applying defaults or validating.
func Parse(b []byte) (*Config, error) {
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	// Refuse trailing content, which usually means a stray brace.
	if dec.More() {
		return nil, errors.New("unexpected content after the top-level JSON object")
	}
	c.Normalize()
	return &c, nil
}

// Marshal renders the config as indented JSON.
func (c *Config) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Normalize fills in zero-valued defaults. It never fails.
func (c *Config) Normalize() {
	if c.Version == 0 {
		c.Version = CurrentVersion
	}
	if c.Server.Bind == "" {
		c.Server.Bind = "127.0.0.1:8765"
	}
	if c.Server.LogLevel == "" {
		c.Server.LogLevel = "info"
	}
	if c.Defaults.StartPolicy == "" {
		c.Defaults.StartPolicy = StartLazy
	}
	if c.Defaults.Separator == "" {
		c.Defaults.Separator = "."
	}
	if c.Defaults.IdleTimeout == 0 {
		c.Defaults.IdleTimeout = Duration(15 * minute)
	}
	if c.Defaults.CallTimeout == 0 {
		c.Defaults.CallTimeout = Duration(2 * minute)
	}
	if c.Defaults.ConnectTimeout == 0 {
		c.Defaults.ConnectTimeout = Duration(30 * second)
	}
	if c.MCPServers == nil {
		c.MCPServers = map[string]*Upstream{}
	}
	for _, u := range c.MCPServers {
		if u == nil {
			continue
		}
		if u.Type == "" {
			u.Type = u.EffectiveType()
		}
	}
}

// Save writes the config back to the path it was loaded from.
func (c *Config) Save() error {
	if c.path == "" {
		return errors.New("config has no path; use SaveTo")
	}
	return c.SaveTo(c.path)
}

// SaveTo writes the config atomically to path. The existing file is kept as a
// .bak copy so a bad edit can always be recovered by hand.
func (c *Config) SaveTo(path string) error {
	c.Normalize()
	if err := c.Validate(); err != nil {
		return fmt.Errorf("refusing to write invalid config: %w", err)
	}
	b, err := c.Marshal()
	if err != nil {
		return err
	}

	writeMu.Lock()
	defer writeMu.Unlock()

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating config dir %s: %w", dir, err)
	}

	// Keep a backup of the previous contents.
	if prev, err := os.ReadFile(path); err == nil && len(prev) > 0 {
		_ = os.WriteFile(path+".bak", prev, 0o600)
	}

	tmp, err := os.CreateTemp(dir, ".config-*.json.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		// Best effort cleanup if anything below fails.
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}

	// os.Rename is atomic on the same volume; on Windows it replaces the
	// destination, which is what we want.
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replacing config file: %w", err)
	}
	c.path = path
	return nil
}

// Clone returns a deep copy via a JSON round trip. Configs are small, so this
// is cheap enough and avoids a hand-written copy that can drift.
func (c *Config) Clone() *Config {
	b, err := json.Marshal(c)
	if err != nil {
		return nil
	}
	out, err := Parse(b)
	if err != nil {
		return nil
	}
	out.path = c.path
	return out
}
