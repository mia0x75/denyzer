package cmd

import (
	"log/slog"

	"github.com/mia0x75/denyzer/auth"
	"github.com/mia0x75/denyzer/config"
	"github.com/mia0x75/denyzer/helpertest"
	"github.com/mia0x75/denyzer/log"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Validate command", func() {
	var tmpDir *helpertest.TmpFolder
	BeforeEach(func() {
		tmpDir = helpertest.NewTmpFolder("config")
	})
	When("Validate is called with not existing configuration file", func() {
		It("should terminate with error", func() {
			c := NewRootCommand()
			c.SetArgs([]string{"validate", "--config", "/notexisting/path.yaml"})

			Expect(c.Execute()).Should(HaveOccurred())
		})
	})

	When("Validate is called with existing valid configuration file", func() {
		It("should terminate without error", func() {
			cfgFile := tmpDir.CreateStringFile("config.yaml",
				"upstreams:",
				"  groups:",
				"    default:",
				"      - 1.1.1.1")

			c := NewRootCommand()
			c.SetArgs([]string{"validate", "--config", cfgFile.Path})

			Expect(c.Execute()).Should(Succeed())
		})
	})

	DescribeTable("management authentication rules", func(admin string, authenticated, valid bool) {
		hook, cleanup := log.Capture(log.Log())
		DeferCleanup(cleanup)

		cfgFile := tmpDir.CreateStringFile("config.yaml",
			"upstreams:",
			"  groups:",
			"    default:",
			"      - 1.1.1.1",
			"ports:",
			"  admin: "+admin,
		)
		if authenticated {
			hash, err := auth.HashPassword([]byte("test-only-validate-secret"))
			Expect(err).Should(Succeed())
			Expect(config.SetAuth(cfgFile.Path, config.Auth{Username: "admin", PasswordHash: hash})).Should(Succeed())
		}

		c := NewRootCommand()
		c.SetArgs([]string{"validate", "--config", cfgFile.Path})
		err := c.Execute()
		if valid {
			Expect(err).Should(Succeed())
			Expect(hook.LastRecord().Message).Should(Equal("Configuration is valid"))
		} else {
			Expect(err).Should(MatchError(
				ContainSubstring("ports.admin without authentication requires an explicit loopback IP")))
			Expect(hook.Messages()).ShouldNot(ContainElement("Configuration is valid"))
		}
	},
		Entry("unauthenticated wildcard", "4000", false, false),
		Entry("unauthenticated IPv4 wildcard", "0.0.0.0:4000", false, false),
		Entry("unauthenticated IPv6 wildcard", "'[::]:4000'", false, false),
		Entry("unauthenticated hostname", "localhost:4000", false, false),
		Entry("unauthenticated non-loopback", "192.0.2.1:4000", false, false),
		Entry("later unauthenticated non-loopback", "['127.0.0.1:4000', '192.0.2.1:4001']", false, false),
		Entry("unauthenticated IPv4 loopback", "127.0.0.1:4000", false, true),
		Entry("unauthenticated IPv6 loopback", "'[::1]:4000'", false, true),
		Entry("authenticated wildcard", "4000", true, true),
	)

	When("Validate is called with a configuration file that only logs errors", func() {
		It("should still report the configuration's warnings", func() {
			hook, cleanup := log.Capture(log.Log())
			DeferCleanup(cleanup)

			cfgFile := tmpDir.CreateStringFile("config.yaml",
				"upstreams:",
				"  groups:",
				"    default:",
				"      - 1.1.1.1",
				"caching:",
				"  minTime: 5",
				"log:",
				"  level: error")

			c := NewRootCommand()
			c.SetArgs([]string{"validate", "--config", cfgFile.Path})

			Expect(c.Execute()).Should(Succeed())

			Expect(hook.Records()).Should(ContainElement(SatisfyAll(
				HaveField("Level", slog.LevelWarn),
				HaveField("Message", ContainSubstring("duration without a unit")),
			)))
			Expect(hook.LastRecord().Message).Should(Equal("Configuration is valid"))
		})
	})

	When("Validate is called with existing invalid configuration file", func() {
		It("should terminate with error", func() {
			cfgFile := tmpDir.CreateStringFile("config.yaml",
				"upstreams:",
				"  groups:",
				"    default:",
				"      - 1.broken file")

			c := NewRootCommand()
			c.SetArgs([]string{"validate", "--config", cfgFile.Path})

			Expect(c.Execute()).Should(HaveOccurred())
		})
	})
})
