// Package benchdata provides the list data the repository's benchmarks share.
//
// Several packages benchmark the same code paths with the same list data, so
// loading it lives here instead of being copied into every benchmark file.
package benchdata

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// repoRoot returns the root directory of the repository. The lookup walks up
// from the working directory (the package directory `go test` runs the test
// binary in) until it finds the module's go.mod. It deliberately avoids the
// compile-time source path, because `-trimpath` replaces it with the import
// path and the data files could no longer be found.
func repoRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}

		dir = parent
	}
}

// OISDWildcardListPath returns the path of the OISD wildcard list shipped with
// the repository.
func OISDWildcardListPath() string {
	return filepath.Join(repoRoot(), "helpertest", "data", "oisd-big-wildcard.txt")
}

// OISDRegexRules loads n regex rules from the OISD wildcard list, in the shape
// the real OISD-derived lists use: "/^(.*\.)?<domain>$/".
//
// The rules are taken with an even stride over the list, so the sample keeps the
// diversity of the original list instead of its first n entries. A rule of this
// shape has no literal prefix, so the regexp engine cannot reject a candidate
// domain without scanning the rule — which is exactly the cost the benchmarks
// have to expose.
func OISDRegexRules(n int) ([]string, error) {
	path := OISDWildcardListPath()

	domains, err := oisdWildcardDomains(path)
	if err != nil {
		return nil, err
	}

	if len(domains) == 0 {
		return nil, fmt.Errorf("no wildcard entries in %s", path)
	}

	if n > len(domains) {
		n = len(domains)
	}

	stride := max(len(domains)/n, 1)

	rules := make([]string, 0, n)

	for i := 0; len(rules) < n; i++ {
		domain := domains[(i*stride)%len(domains)]
		rules = append(rules, fmt.Sprintf("/^(.*\\.)?%s$/", regexp.QuoteMeta(domain)))
	}

	return rules, nil
}

// oisdWildcardDomains returns the domains of the wildcard entries of the list
// at path, in file order.
func oisdWildcardDomains(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("can't open %s: %w", path, err)
	}
	defer f.Close()

	var domains []string

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "*.") {
			continue
		}

		domains = append(domains, strings.TrimPrefix(line, "*."))
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("can't read %s: %w", path, err)
	}

	return domains, nil
}
