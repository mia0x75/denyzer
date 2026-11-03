package cmd

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mia0x75/denyzer/auth"
	"github.com/mia0x75/denyzer/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuthCommand(t *testing.T) {
	saveCommandGlobals(t)
	path := filepath.Join(t.TempDir(), "config.yml")
	require.NoError(t, os.WriteFile(path, []byte("# keep\nports: {admin: '127.0.0.1:4000'}\n"), 0o600))
	var firstHash string
	for _, password := range []string{" first password ", "second password"} {
		cmd := NewRootCommand()
		var output bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetErr(io.Discard)
		cmd.SetIn(strings.NewReader(password + "\n"))
		cmd.SetArgs([]string{"auth", "--username", "admin", "--password-stdin", "--config", path})
		require.NoError(t, cmd.Execute())
		assert.NotContains(t, output.String(), password)
		cfg, err := config.LoadConfig(path, true)
		require.NoError(t, err)
		assert.Equal(t, "admin", cfg.Auth.Username)
		assert.True(t, auth.VerifyPassword(cfg.Auth.PasswordHash, []byte(password)))
		assert.NotContains(t, output.String(), cfg.Auth.PasswordHash)
		assert.NotEqual(t, firstHash, cfg.Auth.PasswordHash)
		firstHash = cfg.Auth.PasswordHash
	}
}

func TestAuthCommandInputErrors(t *testing.T) {
	saveCommandGlobals(t)
	for _, tc := range []struct {
		name  string
		args  []string
		input string
	}{
		{name: "missing username", args: []string{"--password-stdin"}, input: "password"},
		{name: "empty username", args: []string{"--username", "", "--password-stdin"}, input: "password"},
		{name: "empty password", args: []string{"--username", "admin", "--password-stdin"}},
		{name: "empty line", args: []string{"--username", "admin", "--password-stdin"}, input: "\n"},
		{name: "multiple lines", args: []string{"--username", "admin", "--password-stdin"}, input: "first\nsecond\n"},
		{name: "oversized password", args: []string{"--username", "admin", "--password-stdin"}, input: strings.Repeat("x", auth.MaxPasswordBytes+1)},
		{name: "noninteractive prompt", args: []string{"--username", "admin"}, input: "password"},
		{name: "no plaintext flag", args: []string{"--username", "admin", "--password", "password"}},
		{name: "no reset subcommand", args: []string{"reset", "--username", "admin", "--password-stdin"}, input: "password"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yml")
			cmd := NewRootCommand()
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetIn(strings.NewReader(tc.input))
			cmd.SetArgs(append([]string{"auth", "--config", path}, tc.args...))
			require.Error(t, cmd.Execute())
			_, err := os.Stat(path)
			assert.ErrorIs(t, err, os.ErrNotExist, "input errors must not create or modify credentials")
		})
	}
}

func TestAuthCommandEnvironmentPath(t *testing.T) {
	saveCommandGlobals(t)
	path := filepath.Join(t.TempDir(), "config.yml")
	t.Setenv(configFileEnvVar, path)
	cmd := NewRootCommand()
	cmd.SetOut(io.Discard)
	cmd.SetIn(strings.NewReader("password\r\n"))
	cmd.SetArgs([]string{"auth", "--username", "admin", "--password-stdin"})
	require.NoError(t, cmd.Execute())
	cfg, err := config.LoadConfig(path, true)
	require.NoError(t, err)
	assert.True(t, auth.VerifyPassword(cfg.Auth.PasswordHash, []byte("password")))
}

func TestPromptAuthPassword(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		confirmation string
		readError    error
		ok           bool
	}{
		{name: "matching", confirmation: "test-only-secret", ok: true},
		{name: "mismatching", confirmation: "different"},
		{name: "interrupted", readError: errors.New("interrupted")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			reads := 0
			password, err := promptAuthPassword(&output, func() ([]byte, error) {
				reads++
				if reads == 1 {
					return []byte("test-only-secret"), nil
				}

				return []byte(tc.confirmation), tc.readError
			})
			defer clear(password)
			assert.Equal(t, 2, reads)
			assert.NotContains(t, output.String(), "test-only-secret")
			if tc.ok {
				require.NoError(t, err)
				assert.Equal(t, "test-only-secret", string(password))
			} else {
				require.Error(t, err)
				assert.Empty(t, password)
			}
		})
	}
}

func saveCommandGlobals(t *testing.T) {
	t.Helper()
	savedConfig, savedHost, savedPort := configPath, apiHost, apiPort
	savedDNSHost, savedDNSPort := dnsHost, dnsPort
	t.Cleanup(func() {
		configPath, apiHost, apiPort = savedConfig, savedHost, savedPort
		dnsHost, dnsPort = savedDNSHost, savedDNSPort
	})
}
