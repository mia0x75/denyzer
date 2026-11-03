package e2e

import (
	"context"
	"net"
	"net/http"

	. "github.com/mia0x75/denyzer/helpertest"
	"github.com/mia0x75/denyzer/util"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/testcontainers/testcontainers-go"
)

var _ = Describe("Basic functionality", func() {
	var (
		e2eNet  *testcontainers.DockerNetwork
		denyzer testcontainers.Container
		err     error
	)

	BeforeEach(func(ctx context.Context) {
		// Create a fresh network for each test
		e2eNet = getRandomNetwork(ctx)
	})

	Context("with upstream DNS server", func() {
		BeforeEach(func(ctx context.Context) {
			// Setup mock DNS server that will respond to queries
			_, err = createDNSMokkaContainer(ctx, "moka1", e2eNet, `A google/NOERROR("A 1.2.3.4 123")`)
			Expect(err).Should(Succeed())
		})

		Describe("Container startup", func() {
			Context("with conflicting port configuration", func() {
				BeforeEach(func(ctx context.Context) {
					// Create denyzer with the same port for HTTP and DNS
					denyzer, err = createDenyzerContainerFromString(ctx, e2eNet, dedent(`
						upstreams:
						  groups:
						    default:
						      - moka1
						ports:
						  http: 4000
						  dns: 4000
						`))
					Expect(err).Should(HaveOccurred())

					// Verify container exit status
					state, err := denyzer.State(ctx)
					Expect(err).Should(Succeed())
					Expect(state.ExitCode).Should(Equal(1))
				})

				It("fails to start with appropriate error message", func(ctx context.Context) {
					Eventually(denyzer.IsRunning, "5s", "2ms").Should(BeFalse())
					Expect(getContainerLogs(ctx, denyzer)).
						Should(ContainElement(ContainSubstring("address already in use")))
				})
			})

			Context("with minimal configuration", func() {
				BeforeEach(func(ctx context.Context) {
					// Create denyzer with minimal config
					denyzer, err = createDenyzerContainerFromString(ctx, e2eNet, dedent(`
						upstreams:
						  groups:
						    default:
						      - moka1
						`))
					Expect(err).Should(Succeed())
				})

				It("starts successfully and resolves DNS queries", func(ctx context.Context) {
					msg := util.NewMsgWithQuestion("google.de.", A)

					Expect(doDNSRequest(ctx, denyzer, msg)).
						Should(
							SatisfyAll(
								BeDNSRecord("google.de.", A, "1.2.3.4"),
								HaveTTL(BeNumerically("==", 123)),
							))
				})

				It("reports 'healthy' status via container healthcheck", func(ctx context.Context) {
					Eventually(func(g Gomega) string {
						state, err := denyzer.State(ctx)
						g.Expect(err).NotTo(HaveOccurred())

						return string(state.Health.Status)
					}, "2m", "1s").Should(Equal("healthy"))
				})
			})
		})

		Describe("Management port configuration", func() {
			Context("when management port is not defined", func() {
				BeforeEach(func(ctx context.Context) {
					denyzer, err = createDenyzerContainerFromString(ctx, e2eNet, dedent(`
						upstreams:
						  groups:
						    default:
						      - moka1
						`))
					Expect(err).Should(Succeed())
				})

				It("does not expose management service", func(ctx context.Context) {
					host, port, err := getContainerHostPort(ctx, denyzer, "4000/tcp")
					Expect(err).Should(Succeed())

					_, err = http.Get("https://" + net.JoinHostPort(host, port))
					Expect(err).Should(HaveOccurred())
				})
			})

			Context("when HTTPS management and public HTTP DoH ports are defined", func() {
				BeforeEach(func(ctx context.Context) {
					denyzer, err = createDenyzerContainerFromString(ctx, e2eNet, dedent(`
						upstreams:
						  groups:
						    default:
						      - moka1
						ports:
						  admin: 4000
						  http: 4001
						debug:
						  pprof: true
						`))
					Expect(err).Should(Succeed())
				})

				It("serves authenticated HTTPS management independently of public DoH", func(ctx context.Context) {
					host, port, err := getContainerHostPort(ctx, denyzer, "4000/tcp")
					Expect(err).Should(Succeed())
					url := "https://" + net.JoinHostPort(host, port)
					client, err := newManagementClient(ctx, url)
					Expect(err).Should(Succeed())

					By("serving static HTML content", func() {
						Eventually(client.Get).WithArguments(url).Should(HaveHTTPStatus(http.StatusOK))
					})

					By("serving robots.txt", func() {
						Eventually(client.Get).WithArguments(url + "/robots.txt").Should(And(
							HaveHTTPStatus(http.StatusOK),
							HaveHTTPBody(ContainSubstring("User-agent: *\nDisallow: /")),
						))
					})

					By("serving pprof debugging endpoint", func() {
						Eventually(client.Get).WithArguments(url + "/debug/pprof/").Should(HaveHTTPStatus(http.StatusOK))
					})

					By("serving the login page and removing the old dashboard prefix", func() {
						Eventually(client.Get).WithArguments(url + "/login/").Should(HaveHTTPStatus(http.StatusOK))
						Eventually(client.Get).WithArguments(url + "/dashboard/").Should(HaveHTTPStatus(http.StatusNotFound))
					})

					By("keeping DNS queries off the management listener", func() {
						Eventually(client.Get).WithArguments(url + "/dns-query").Should(HaveHTTPStatus(http.StatusNotFound))
					})

					By("serving public DoH without exposing management", func() {
						host, port, err := getContainerHostPort(ctx, denyzer, "4001/tcp")
						Expect(err).Should(Succeed())
						publicURL := "http://" + net.JoinHostPort(host, port)
						for _, path := range []string{"/", "/login/", "/api/login", "/api/blocking/status", "/debug/pprof/"} {
							Eventually(http.Get).WithArguments(publicURL + path).Should(HaveHTTPStatus(http.StatusNotFound))
						}
						Eventually(http.Get).WithArguments(publicURL +
							"/dns-query?dns=q80BAAABAAAAAAAAA3d3dwdleGFtcGxlA2NvbQAAAQAB").Should(HaveHTTPStatus(http.StatusOK))
					})
				})
			})
		})
	})

	Describe("Logging privacy", func() {
		BeforeEach(func(ctx context.Context) {
			_, err = createDNSMokkaContainer(ctx, "moka1", e2eNet, `A google/NOERROR("A 1.2.3.4 123")`)
			Expect(err).Should(Succeed())
		})

		Context("when privacy mode is enabled", func() {
			BeforeEach(func(ctx context.Context) {
				denyzer, err = createDenyzerContainerFromString(ctx, e2eNet, dedent(`
					upstreams:
					  groups:
					    default:
					      - moka1
					log:
					  level: trace
					  privacy: true
					`))
				Expect(err).Should(Succeed())
			})

			It("redacts sensitive information from logs", func(ctx context.Context) {
				msg := util.NewMsgWithQuestion("google.com.", A)

				// Make two requests to ensure consistent behavior
				Expect(doDNSRequest(ctx, denyzer, msg)).
					Should(
						SatisfyAll(
							BeDNSRecord("google.com.", A, "1.2.3.4"),
							HaveTTL(BeNumerically("==", 123)),
						))

				Expect(doDNSRequest(ctx, denyzer, msg)).
					Should(
						SatisfyAll(
							BeDNSRecord("google.com.", A, "1.2.3.4"),
							HaveTTL(BeNumerically("<=", 123)),
						))

				// Verify logs don't contain sensitive information
				Expect(getContainerLogs(ctx, denyzer)).ShouldNot(ContainElement(ContainSubstring("google.com")))
				Expect(getContainerLogs(ctx, denyzer)).ShouldNot(ContainElement(ContainSubstring("1.2.3.4")))
			})
		})
	})
})
