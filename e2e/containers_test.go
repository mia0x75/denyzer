package e2e

import (
	"github.com/mia0x75/denyzer/config"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Denyzer container configuration", func() {
	Describe("withDNSPort", func() {
		It("adds the e2e DNS port to a config without a ports section", func() {
			Expect(withDNSPort([]string{
				"upstreams:",
				"  groups:",
				"    default:",
				"      - moka1",
			})).Should(Equal([]string{
				"upstreams:",
				"  groups:",
				"    default:",
				"      - moka1",
				"ports:",
				"  dns: 1053",
			}))
		})

		It("adds the e2e DNS port to an existing ports section", func() {
			Expect(withDNSPort([]string{
				"ports:",
				"  http: 4000",
			})).Should(Equal([]string{
				"ports:",
				"  dns: 1053",
				"  http: 4000",
			}))
		})

		It("keeps a configured DNS port", func() {
			lines := []string{
				"ports:",
				"  dns: 4000",
				"  http: 4000",
			}

			Expect(withDNSPort(lines)).Should(Equal(lines))
		})

		It("keeps a configured DNS port in a ports section with other listeners", func() {
			lines := []string{
				"ports:",
				"  dns: 1053",
				"  tls: 8853",
				"  https: 8443",
				"  proxyProtocol: [tls, https]",
			}

			Expect(withDNSPort(lines)).Should(Equal(lines))
		})

		It("produces a config that loads with an unprivileged DNS port", func() {
			lines := withDNSPort([]string{
				"upstreams:",
				"  groups:",
				"    default:",
				"      - moka1",
			})

			cfg, err := config.LoadConfig(createTempFile(lines...), true)
			Expect(err).Should(Succeed())
			Expect(cfg.Ports.DNS).Should(Equal(config.ListenConfig{":1053"}))
			Expect(cfg.Ports.PrivilegedPorts()).Should(BeEmpty())
		})
	})
})
