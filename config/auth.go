package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"

	"github.com/mia0x75/denyzer/auth"
	yaml "gopkg.in/yaml.v2"
	yaml3 "gopkg.in/yaml.v3"
)

const authFilePermissions = 0o600

// Auth configures the single administrator; both credential fields empty disables authentication.
type Auth struct {
	// Administrator username, set with denyzer auth --username.
	Username string `yaml:"username"`
	// Argon2id PHC password hash. Never store a plaintext password here.
	PasswordHash string `yaml:"passwordHash"`
	// Trusted management client IPs or CIDRs exempt from the global login-attempt limit, not password verification.
	TrustedIPs []string `yaml:"trustedIPs,omitempty"`
}

// Debug controls optional management debugging endpoints.
type Debug struct {
	// Enable /debug/pprof on the management listener (disabled by default).
	Pprof bool `yaml:"pprof"`
}

// IsEnabled reports whether both administrator credentials are present.
func (a Auth) IsEnabled() bool {
	return a.Username != "" && a.PasswordHash != ""
}

func (a Auth) validate() error {
	for _, entry := range a.TrustedIPs {
		if _, err := parseCIDRorIP(entry); err != nil {
			return fmt.Errorf("auth: trustedIPs entry %q: %w", entry, err)
		}
	}
	if a.Username == "" && a.PasswordHash == "" {
		return nil
	}
	if err := ValidateUsername(a.Username); err != nil {
		return err
	}

	return auth.ValidateHash(a.PasswordHash)
}

// IsTrustedIP reports whether ip is in the configured management client allowlist.
func (a Auth) IsTrustedIP(ip net.IP) bool {
	for _, entry := range a.TrustedIPs {
		trusted, err := parseCIDRorIP(entry)
		if err == nil && trusted.Contains(ip) {
			return true
		}
	}

	return false
}

// ValidateAdminListeners refuses unauthenticated management access outside explicit loopback IPs.
func (cfg *Config) ValidateAdminListeners(logger *slog.Logger) error {
	if err := cfg.Auth.validate(); err != nil {
		return fmt.Errorf("invalid auth configuration: %w", err)
	}
	for _, address := range cfg.Ports.Admin {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("invalid ports.admin listen address %q: %w", address, err)
		}
		ip := net.ParseIP(host)
		isLoopback := ip != nil && ip.IsLoopback()
		if !cfg.Auth.IsEnabled() && !isLoopback {
			return errors.New("ports.admin without authentication requires an explicit loopback IP; run denyzer auth first")
		}
		if !cfg.Auth.IsEnabled() {
			logger.Warn("management authentication is disabled on loopback; run denyzer auth to protect local access")
		}
	}

	return nil
}

// ValidateUsername rejects empty, oversized, or ambiguous administrator names.
func ValidateUsername(username string) error {
	const maxUsernameBytes = 64
	if username == "" || len(username) > maxUsernameBytes || strings.TrimSpace(username) != username {
		return errors.New("username must contain between 1 and 64 bytes without surrounding whitespace")
	}
	if strings.ContainsFunc(username, unicode.IsControl) {
		return errors.New("username must not contain control characters")
	}

	return nil
}

// ReadAuth reloads only credentials, without changing the running DNS configuration.
// Reading at the authentication boundary makes an atomic CLI password reset take effect immediately.
func (cfg *Config) ReadAuth() (Auth, error) {
	if cfg.sourcePath == "" {
		return cfg.Auth, cfg.Auth.validate()
	}

	info, err := os.Stat(cfg.sourcePath)
	if err != nil {
		return Auth{}, fmt.Errorf("can't read authentication configuration: %w", err)
	}

	data, _, _, err := readConfigSource(slog.New(slog.DiscardHandler), cfg.sourcePath, info)
	if err != nil {
		return Auth{}, err
	}

	var document struct {
		Auth  Auth           `yaml:"auth"`
		Other map[string]any `yaml:",inline"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.SetStrict(true)
	if err := decoder.Decode(&document); err != nil && !errors.Is(err, io.EOF) {
		return Auth{}, fmt.Errorf("can't decode authentication configuration: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return Auth{}, errors.New("authentication configuration must contain a single YAML document")
	}

	return document.Auth, document.Auth.validate()
}

// SetAuth atomically updates credentials, preserving the other YAML values, comments and quoting.
// In directory mode the last YAML file wins, so it receives the complete auth block.
func SetAuth(path string, credentials Auth) error {
	if !credentials.IsEnabled() {
		return errors.New("both username and password hash are required")
	}
	if err := credentials.validate(); err != nil {
		return err
	}

	target, err := authWriteTarget(path)
	if err != nil {
		return err
	}

	// shortcut: a crashed CLI can leave this lock; remove it only after confirming no auth command runs.
	lock, err := os.OpenFile(target+".auth.lock", os.O_WRONLY|os.O_CREATE|os.O_EXCL, authFilePermissions)
	if err != nil {
		return fmt.Errorf("can't lock authentication configuration (another auth command may be running): %w", err)
	}
	defer func() {
		_ = lock.Close()
		_ = os.Remove(lock.Name())
	}()

	data, err := os.ReadFile(target)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	documents, err := decodeYAMLDocuments(data)
	if err != nil {
		return fmt.Errorf("can't parse configuration: %w", err)
	}
	if len(documents) > 1 {
		return errors.New("auth cannot rewrite a multi-document file; move credentials to a single YAML document")
	}

	root := &yaml3.Node{Kind: yaml3.MappingNode, Tag: "!!map"}
	if len(documents) == 1 {
		root = documents[0]
	}

	var newAuth yaml3.Node
	if err := newAuth.Encode(struct {
		Auth Auth `yaml:"auth"`
	}{Auth: credentials}); err != nil {
		return err
	}
	mergeMappingNodes(root, &newAuth)

	var output bytes.Buffer
	encoder := yaml3.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := encoder.Encode(root); err != nil {
		return err
	}
	if err := encoder.Close(); err != nil {
		return err
	}

	return writeAuthFile(target, output.Bytes())
}

func authWriteTarget(path string) (string, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return filepath.Abs(path)
	}
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		files, err := readFromDir(path)
		if err != nil {
			return "", err
		}
		if len(files) == 0 {
			return filepath.Abs(filepath.Join(path, "auth.yml"))
		}

		path = files[len(files)-1].path
	} else if !info.Mode().IsRegular() {
		return "", errors.New("authentication configuration must be a regular YAML file or directory")
	}

	// Keep a config-file symlink intact; replace its target instead.
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}

	return filepath.Abs(resolved)
}

func writeAuthFile(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".denyzer-auth-*")
	if err != nil {
		return err
	}
	defer func() {
		_ = file.Close()
		_ = os.Remove(file.Name())
	}()

	if err := file.Chmod(authFilePermissions); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("can't replace authentication configuration: %w", err)
	}
	if runtime.GOOS == "windows" {
		return nil // Windows does not support fsync on directories.
	}

	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()

	return dir.Sync()
}
