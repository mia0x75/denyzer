package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadConfigRejectsInvalidBlocking(t *testing.T) {
	for _, tc := range []struct {
		name, blocking, want string
	}{
		{"unknown schedule", "  denylists: {ads: [/unused/list]}\n  clientGroupsBlock: {default: [ads]}\n  listSchedules: {ads: [missing]}\n", "undefined schedule"},
		{"unknown group", "  denylists: {ads: [/unused/list]}\n  clientGroupsBlock: {default: [missing]}\n", "undefined allowlist or denylist"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yml")
			require.NoError(t, os.WriteFile(path, []byte("upstreams:\n  groups: {default: [192.0.2.53]}\nblocking:\n"+tc.blocking), 0o600))
			_, err := LoadConfigWithLogger(slog.New(slog.DiscardHandler), path, true)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestLoadConfigYAMLDocuments(t *testing.T) {
	for _, tc := range []struct {
		name, suffix string
		valid        bool
	}{
		{"single document", "\n# trailing comment\n", true},
		{"second document", "\n---\nports: {dns: 5354}\n", false},
		{"empty second document", "\n---\n", false},
		{"malformed second document", "\n---\n[\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yml")
			require.NoError(t, os.WriteFile(path, []byte("upstreams:\n  groups: {default: [192.0.2.53]}\n"+tc.suffix), 0o600))
			cfg, err := LoadConfigWithLogger(slog.New(slog.DiscardHandler), path, true)
			if tc.valid {
				require.NoError(t, err)
				_, err = cfg.ReadAuth()
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "single YAML document")
			}
		})
	}
	t.Run("directory still merges multiple documents", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yml"), []byte("upstreams:\n  groups: {default: [192.0.2.53]}\n---\nports: {dns: 5354}\n"), 0o600))
		cfg, err := LoadConfigWithLogger(slog.New(slog.DiscardHandler), dir, true)
		require.NoError(t, err)
		_, err = cfg.ReadAuth()
		require.NoError(t, err)
	})
}
