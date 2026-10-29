package config

import (
	"os"
	"path/filepath"
	"time"

	"github.com/creasty/defaults"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v2"
)

var _ = Describe("QueryLogConfig", func() {
	var cfg QueryLog

	suiteBeforeEach()

	BeforeEach(func() {
		cfg = QueryLog{
			Target:           "/dev/null",
			Type:             QueryLogTypeSqlite,
			CreationAttempts: 1,
			CreationCooldown: Duration(time.Millisecond),
		}
	})

	Describe("IsEnabled", func() {
		It("should be true by default", func() {
			cfg := QueryLog{}
			Expect(defaults.Set(&cfg)).Should(Succeed())

			Expect(cfg.IsEnabled()).Should(BeTrue())
		})

		When("enabled", func() {
			It("should be true", func() {
				Expect(cfg.IsEnabled()).Should(BeTrue())
			})
		})

		When("disabled", func() {
			It("should be false", func() {
				cfg := QueryLog{
					Type: QueryLogTypeNone,
				}

				Expect(cfg.IsEnabled()).Should(BeFalse())
			})
		})
	})

	Describe("LogConfig", func() {
		It("should log configuration", func() {
			cfg.LogConfig(logger)

			Expect(hook.Calls).ShouldNot(BeEmpty())
			Expect(hook.Messages).Should(ContainElement(ContainSubstring("retention:")))
			Expect(hook.Messages).Should(ContainElement(ContainSubstring("sudn:")))
		})

		It("should log the ignored domains", func() {
			cfg.Ignore.Domains = []string{"example.com", "*.lan", "/\\.arpa$/"}

			cfg.LogConfig(logger)

			Expect(hook.Messages).Should(ContainElement(ContainSubstring("domains (3):")))
		})
	})

	Describe("ignore.domains parsing", func() {
		It("parses the domains list", func() {
			yamlStr := "type: console\nignore:\n  domains:\n    - example.com\n    - \"*.lan\"\n"

			var qlCfg QueryLog
			Expect(yaml.UnmarshalStrict([]byte(yamlStr), &qlCfg)).Should(Succeed())
			Expect(qlCfg.Ignore.Domains).Should(Equal([]string{"example.com", "*.lan"}))
		})
	})

	Describe("secret target handling", func() {
		It("loads the target from a file", func() {
			dir := GinkgoT().TempDir()
			path := filepath.Join(dir, "dbpath")
			Expect(os.WriteFile(path, []byte("/var/lib/denyzer/querylog.db\n"), 0o600)).Should(Succeed())

			yamlStr := "type: sqlite\ntarget: file:" + path + "\n"

			var qlCfg QueryLog
			Expect(yaml.UnmarshalStrict([]byte(yamlStr), &qlCfg)).Should(Succeed())
			Expect(qlCfg.Target.Reveal()).Should(Equal("/var/lib/denyzer/querylog.db"))
		})
	})

	Describe("QueryLogType enum", func() {
		It("parses the sqlite type", func() {
			t, err := ParseQueryLogType("sqlite")
			Expect(err).Should(Succeed())
			Expect(t).Should(Equal(QueryLogTypeSqlite))
		})

		It("defaults to sqlite when the type is omitted", func() {
			var cfg QueryLog
			Expect(cfg.Type).Should(Equal(QueryLogTypeSqlite))

			Expect(defaults.Set(&cfg)).Should(Succeed())
			Expect(cfg.Type).Should(Equal(QueryLogTypeSqlite))
		})
	})

	Describe("SetDefaults", func() {
		It("should log configuration", func() {
			cfg := QueryLog{}
			Expect(cfg.Fields).Should(BeEmpty())

			Expect(defaults.Set(&cfg)).Should(Succeed())

			Expect(cfg.Fields).ShouldNot(BeEmpty())
		})
	})
})
