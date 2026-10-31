package lists

import (
	"context"
	"fmt"
	"testing"

	"github.com/mia0x75/denyzer/config"
)

const (
	benchmarkEntriesPerGroup   = 100_000
	benchmarkSourceConcurrency = 4
)

func BenchmarkRefresh(b *testing.B) {
	file1, _ := createTestListFile(b, b.TempDir(), 100000)
	file2, _ := createTestListFile(b, b.TempDir(), 150000)
	file3, _ := createTestListFile(b, b.TempDir(), 130000)
	lists := map[string][]config.BytesSource{
		"gr1": config.NewBytesSources(file1, file2, file3),
	}

	cfg := config.SourceLoading{
		Concurrency:   5,
		RefreshPeriod: config.Duration(-1),
	}
	downloader := NewDownloader(config.Downloader{}, nil)
	cache, err := NewListCache(context.Background(), ListCacheTypeDenylist, cfg, lists, downloader)
	if err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()

	for b.Loop() {
		if err := cache.Refresh(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkNewListCacheMultiGroup measures a cold startup load: every iteration
// builds a new cache and parses each group's sources from disk. It compares the
// former sequential group loading (groupConcurrency=1) with the automatic value
// (groupConcurrency=0), which is min(number of groups, source concurrency).
func BenchmarkNewListCacheMultiGroup(b *testing.B) {
	for _, groupCount := range []int{1, 2, 4} {
		for _, groupConcurrency := range []uint{1, 0} {
			name := fmt.Sprintf("groups=%d/groupConcurrency=auto", groupCount)
			if groupConcurrency != 0 {
				name = fmt.Sprintf("groups=%d/groupConcurrency=%d", groupCount, groupConcurrency)
			}

			b.Run(name, func(b *testing.B) {
				dir := b.TempDir()

				groupSources := make(map[string][]config.BytesSource, groupCount)
				for i := range groupCount {
					file, _ := createTestListFile(b, dir, benchmarkEntriesPerGroup)
					groupSources[fmt.Sprintf("gr%d", i)] = config.NewBytesSources(file)
				}

				cfg := config.SourceLoading{
					Concurrency:      benchmarkSourceConcurrency,
					GroupConcurrency: groupConcurrency,
					RefreshPeriod:    config.Duration(-1),
				}
				downloader := NewDownloader(config.Downloader{}, nil)

				b.ReportAllocs()

				for b.Loop() {
					if _, err := NewListCache(context.Background(), ListCacheTypeDenylist,
						cfg, groupSources, downloader); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
