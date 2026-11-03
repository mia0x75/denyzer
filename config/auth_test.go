package config

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v2"
)

func testAuth() Auth {
	return Auth{
		Username: "admin",
		PasswordHash: "$argon2id$v=19$m=65536,t=3,p=2$" +
			base64.RawStdEncoding.EncodeToString(make([]byte, 16)) + "$" +
			base64.RawStdEncoding.EncodeToString(make([]byte, 32)),
	}
}

func TestAuthValidation(t *testing.T) {
	t.Parallel()
	valid := testAuth()
	for _, tc := range []struct {
		name string
		auth Auth
		ok   bool
	}{
		{name: "disabled", ok: true},
		{name: "valid", auth: valid, ok: true},
		{name: "username only", auth: Auth{Username: "admin"}},
		{name: "hash only", auth: Auth{PasswordHash: valid.PasswordHash}},
		{name: "invalid hash", auth: Auth{Username: "admin", PasswordHash: "plaintext"}},
		{name: "surrounding whitespace", auth: Auth{Username: " admin", PasswordHash: valid.PasswordHash}},
		{name: "control character", auth: Auth{Username: "ad\tmin", PasswordHash: valid.PasswordHash}},
		{name: "oversized name", auth: Auth{Username: strings.Repeat("a", 65), PasswordHash: valid.PasswordHash}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.ok {
				assert.NoError(t, tc.auth.validate())
			} else {
				assert.Error(t, tc.auth.validate())
			}
		})
	}
}

func TestValidateAdminListeners(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		address string
		auth    Auth
		ok      bool
		warning string
	}{
		{name: "IPv4 loopback", address: "127.0.0.1:4000", ok: true, warning: "authentication is disabled"},
		{name: "IPv6 loopback", address: "[::1]:4000", ok: true, warning: "authentication is disabled"},
		{name: "wildcard", address: ":4000"},
		{name: "IPv4 wildcard", address: "0.0.0.0:4000"},
		{name: "IPv6 wildcard", address: "[::]:4000"},
		{name: "hostname", address: "localhost:4000"},
		{name: "LAN address", address: "192.0.2.1:4000"},
		{name: "invalid address", address: "invalid"},
		{name: "authenticated loopback", address: "127.0.0.1:4000", auth: testAuth(), ok: true},
		{name: "authenticated wildcard", address: ":4000", auth: testAuth(), ok: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			cfg := Config{Ports: Ports{Admin: ListenConfig{tc.address}}, Auth: tc.auth}
			err := cfg.ValidateAdminListeners(slog.New(slog.NewTextHandler(&output, nil)))
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			if tc.warning != "" {
				assert.Contains(t, output.String(), tc.warning)
			} else {
				assert.Empty(t, output.String())
			}
		})
	}
}

func TestReadAuth(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		yaml string
		ok   bool
	}{
		{name: "disabled", yaml: "ports: {admin: '127.0.0.1:4000'}\n", ok: true},
		{name: "empty file", ok: true},
		{name: "unknown auth field", yaml: "auth: {password: secret}\n"},
		{name: "duplicate auth", yaml: "auth: {}\nauth: {}\n"},
		{name: "duplicate username", yaml: "auth: {username: admin, username: other}\n"},
		{name: "partial credentials", yaml: "auth: {username: admin}\n"},
		{name: "invalid hash", yaml: "auth: {username: admin, passwordHash: bad}\n"},
		{name: "broken YAML", yaml: "auth: [\n"},
		{name: "non mapping", yaml: "- auth\n"},
		{name: "multiple documents", yaml: "auth: {}\n---\nauth: {}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yml")
			require.NoError(t, os.WriteFile(path, []byte(tc.yaml), 0o600))
			cfg := Config{sourcePath: path}
			_, err := cfg.ReadAuth()
			if tc.ok {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		cfg := Config{sourcePath: filepath.Join(t.TempDir(), "missing.yml")}
		_, err := cfg.ReadAuth()
		assert.Error(t, err)
	})
	t.Run("YAML merge and scalar spelling", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yml")
		data := fmt.Sprintf("defaults: &defaults\n  auth: {username: yes, passwordHash: '%s'}\n<<: *defaults\n", testAuth().PasswordHash)
		require.NoError(t, os.WriteFile(path, []byte(data), 0o600))
		cfg := Config{sourcePath: path}
		credentials, err := cfg.ReadAuth()
		require.NoError(t, err)
		assert.Equal(t, "yes", credentials.Username)
	})
}

func TestSetAuth(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yml")
	original := []byte("# keep this comment\nports:\n  admin: 127.0.0.1:4000\nvalues:\n  boolean: yes\n  decimal: 1.0\n  octal: 0700\n  quoted: 'yes' # keep this too\n  list: [one, two]\n")
	require.NoError(t, os.WriteFile(path, original, 0o644))
	require.NoError(t, SetAuth(path, testAuth()))
	updated, err := os.ReadFile(path)
	require.NoError(t, err)

	var before, after map[string]any
	require.NoError(t, yaml.UnmarshalStrict(original, &before))
	require.NoError(t, yaml.UnmarshalStrict(updated, &after))
	delete(after, "auth")
	assert.Equal(t, before, after)
	assert.Contains(t, string(updated), "# keep this comment")
	assert.Contains(t, string(updated), "'yes' # keep this too")
	info, err := os.Stat(path)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
	cfg := Config{sourcePath: path}
	credentials, err := cfg.ReadAuth()
	require.NoError(t, err)
	assert.Equal(t, testAuth(), credentials)

	credentials.Username = "updated"
	require.NoError(t, SetAuth(path, credentials))
	reread, err := cfg.ReadAuth()
	require.NoError(t, err)
	assert.Equal(t, credentials, reread)
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	assert.Len(t, entries, 1, "successful writes must not leave locks or temporary files")
}

func TestSetAuthTargets(t *testing.T) {
	t.Parallel()
	t.Run("new file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yml")
		require.NoError(t, SetAuth(path, testAuth()))
		cfg := Config{sourcePath: path}
		credentials, err := cfg.ReadAuth()
		require.NoError(t, err)
		assert.Equal(t, testAuth(), credentials)
	})
	t.Run("empty directory", func(t *testing.T) {
		path := t.TempDir()
		require.NoError(t, SetAuth(path, testAuth()))
		_, err := os.Stat(filepath.Join(path, "auth.yml"))
		assert.NoError(t, err)
	})
	t.Run("last directory file wins", func(t *testing.T) {
		path := t.TempDir()
		first := []byte("# untouched\nports: {admin: '127.0.0.1:4000'}\nauth: {username: old}\n")
		require.NoError(t, os.WriteFile(filepath.Join(path, "a.yml"), first, 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(path, "z.yaml"), []byte("# last file\nlog: {level: info}\n"), 0o644))
		require.NoError(t, SetAuth(path, testAuth()))
		cfg := Config{sourcePath: path}
		credentials, err := cfg.ReadAuth()
		require.NoError(t, err)
		assert.Equal(t, testAuth(), credentials)
		untouched, err := os.ReadFile(filepath.Join(path, "a.yml"))
		require.NoError(t, err)
		assert.Equal(t, first, untouched)
	})
	t.Run("symlink", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlink creation requires privileges on Windows")
		}
		path := t.TempDir()
		target := filepath.Join(path, "target.yml")
		link := filepath.Join(path, "config.yml")
		require.NoError(t, os.WriteFile(target, []byte("log: {level: info}\n"), 0o600))
		require.NoError(t, os.Symlink(target, link))
		require.NoError(t, SetAuth(link, testAuth()))
		info, err := os.Lstat(link)
		require.NoError(t, err)
		assert.NotZero(t, info.Mode()&os.ModeSymlink)
		cfg := Config{sourcePath: link}
		credentials, err := cfg.ReadAuth()
		require.NoError(t, err)
		assert.Equal(t, testAuth(), credentials)
	})
}

func TestSetAuthRefusesUnsafeWrites(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		data string
		lock bool
	}{
		{name: "existing lock", data: "log: {level: info}\n", lock: true},
		{name: "broken YAML", data: "auth: [\n"},
		{name: "non mapping", data: "- item\n"},
		{name: "duplicate keys", data: "log: {}\nlog: {}\n"},
		{name: "multiple documents", data: "log: {}\n---\nports: {}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yml")
			require.NoError(t, os.WriteFile(path, []byte(tc.data), 0o600))
			if tc.lock {
				require.NoError(t, os.WriteFile(path+".auth.lock", []byte("existing"), 0o600))
			}
			require.Error(t, SetAuth(path, testAuth()))
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, tc.data, string(data))
			if tc.lock {
				lock, err := os.ReadFile(path + ".auth.lock")
				require.NoError(t, err)
				assert.Equal(t, "existing", string(lock))
			}
		})
	}
}

func TestSetAuthAtomicReaders(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yml")
	first, second := testAuth(), testAuth()
	second.Username = "second"
	require.NoError(t, SetAuth(path, first))
	cfg := Config{sourcePath: path}
	errors := make(chan error, 1)
	var writers sync.WaitGroup
	writers.Go(func() {
		for range 20 {
			for _, credentials := range []Auth{second, first} {
				if err := SetAuth(path, credentials); err != nil {
					errors <- err

					return
				}
			}
		}
	})
	for range 100 {
		credentials, err := cfg.ReadAuth()
		require.NoError(t, err)
		assert.True(t, credentials == first || credentials == second, "reader observed incomplete credentials")
	}
	writers.Wait()
	close(errors)
	for err := range errors {
		assert.NoError(t, err)
	}
}
