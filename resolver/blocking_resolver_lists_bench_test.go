package resolver

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/mia0x75/denyzer/config"
	"github.com/mia0x75/denyzer/lists"
)

const benchListEntries = 100_000

// benchWriteListFile writes a list file with one synthetic domain per line.
func benchWriteListFile(tb testing.TB, path string, lines int) {
	tb.Helper()

	f, err := os.Create(path)
	if err != nil {
		tb.Fatal(err)
	}

	w := bufio.NewWriter(f)
	for i := range lines {
		_, _ = fmt.Fprintf(w, "domain-%d.example.com\n", i)
	}

	if err := w.Flush(); err != nil {
		_ = f.Close()
		tb.Fatal(err)
	}

	if err := f.Close(); err != nil {
		tb.Fatal(err)
	}
}

// BenchmarkNewListCaches measures the startup cost of the blocking resolver's two
// caches. "sequential" loads the denylist and then the allowlist, "concurrent" is
// the production path, which loads both at the same time.
func BenchmarkNewListCaches(b *testing.B) {
	dir := b.TempDir()
	denyFile := filepath.Join(dir, "denylist.txt")
	allowFile := filepath.Join(dir, "allowlist.txt")

	benchWriteListFile(b, denyFile, benchListEntries)
	benchWriteListFile(b, allowFile, benchListEntries)

	loading, err := config.WithDefaults[config.SourceLoading]()
	if err != nil {
		b.Fatal(err)
	}

	loading.RefreshPeriod = -1

	cfg := config.Blocking{
		Loading: loading,
		Denylists: map[string][]config.BytesSource{
			"gr": config.NewBytesSources(denyFile),
		},
		Allowlists: map[string][]config.BytesSource{
			"gr": config.NewBytesSources(allowFile),
		},
	}

	downloader := lists.NewDownloader(loading.Downloads, nil)
	ctx := context.Background()

	b.Run("sequential", func(b *testing.B) {
		b.ReportAllocs()

		for b.Loop() {
			_, err := lists.NewListCache(ctx, lists.ListCacheTypeDenylist, cfg.Loading, cfg.Denylists, downloader)
			if err != nil {
				b.Fatal(err)
			}

			_, err = lists.NewListCache(ctx, lists.ListCacheTypeAllowlist, cfg.Loading, cfg.Allowlists, downloader)
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("concurrent", func(b *testing.B) {
		b.ReportAllocs()

		for b.Loop() {
			if _, _, err := newListCaches(ctx, cfg, downloader); err != nil {
				b.Fatal(err)
			}
		}
	})
}
