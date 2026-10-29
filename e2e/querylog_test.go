package e2e

import (
	"context"
	"os"

	"github.com/mia0x75/denyzer/util"
	"github.com/miekg/dns"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/testcontainers/testcontainers-go"
)

var _ = Describe("Query logs functional tests", func() {
	var (
		e2eNet  *testcontainers.DockerNetwork
		denyzer testcontainers.Container
		err     error
	)

	BeforeEach(func(ctx context.Context) {
		e2eNet = getRandomNetwork(ctx)

		_, err = createDNSMokkaContainer(ctx, "moka1", e2eNet, `A google/NOERROR("A 1.2.3.4 123")`,
			`A unknown/NXDOMAIN()`)
		Expect(err).Should(Succeed())
	})

	Describe("Query logging into the SQLite database", func() {
		var hostDir string

		BeforeEach(func(ctx context.Context) {
			// Create a host dir, world-writable so the container's non-root user can write the DB.
			hostDir, err = os.MkdirTemp("", "denyzer_e2e_sqlite-")
			Expect(err).Should(Succeed())
			Expect(os.Chmod(hostDir, 0o777)).Should(Succeed())
			DeferCleanup(func() error { return os.RemoveAll(hostDir) })

			denyzer, err = createDenyzerContainerWithBinds(ctx, e2eNet,
				[]string{hostDir + ":/data"},
				"log:",
				"  level: warn",
				"upstreams:",
				"  groups:",
				"    default:",
				"      - moka1",
				"queryLog:",
				"  type: sqlite",
				"  target: /data/querylog.db",
				"  flushInterval: 1s",
			)
			Expect(err).Should(Succeed())
		})

		When("Some queries were performed", func() {
			msg := util.NewMsgWithQuestion("google.de.", dns.Type(dns.TypeA))
			It("Should store query log in the SQLite database", func(ctx context.Context) {
				By("Performing 2 queries", func() {
					Expect(doDNSRequest(ctx, denyzer, msg)).ShouldNot(BeNil())
					Expect(doDNSRequest(ctx, denyzer, msg)).ShouldNot(BeNil())
				})

				By("checking the database directory was created by denyzer", func() {
					Eventually(func() bool {
						_, statErr := os.Stat(hostDir + "/querylog.db")

						return statErr == nil
					}, "10s", "1s").Should(BeTrue())
				})
			})
		})
	})
})
