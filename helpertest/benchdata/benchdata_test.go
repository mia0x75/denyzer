package benchdata

import (
	"regexp"
	"testing"
)

func TestOISDRegexRules(t *testing.T) {
	t.Run("sampled rules match their domains", func(t *testing.T) {
		domains, err := oisdWildcardDomains(OISDWildcardListPath())
		if err != nil {
			t.Fatal(err)
		}
		rules, err := OISDRegexRules(3)
		if err != nil {
			t.Fatal(err)
		}
		if len(rules) != 3 {
			t.Fatalf("got %d rules, want 3", len(rules))
		}
		for i, rule := range rules {
			re := regexp.MustCompile(rule[1 : len(rule)-1])
			domain := domains[i*(len(domains)/3)]
			if !re.MatchString(domain) || !re.MatchString("sub."+domain) || re.MatchString(domain+".invalid") {
				t.Fatalf("rule %q does not match the sampled domain correctly", rule)
			}
		}
	})
	t.Run("nonpositive count", func(t *testing.T) {
		for _, count := range []int{0, -1} {
			if _, err := OISDRegexRules(count); err == nil {
				t.Fatalf("count %d: expected an error", count)
			}
		}
	})
}
