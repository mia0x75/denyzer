package e2e

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

	. "github.com/mia0x75/denyzer/helpertest"
	"github.com/mia0x75/denyzer/util"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/testcontainers/testcontainers-go"
)

var _ = Describe("Per-client rate limiting", func() {
	var (
		e2eNet  *testcontainers.DockerNetwork
		denyzer testcontainers.Container
		err     error
	)

	BeforeEach(func(ctx context.Context) {
		e2eNet = getRandomNetwork(ctx)
		_, err = createDNSMokkaContainer(ctx, "moka", e2eNet,
			`A example.com/NOERROR("A 1.2.3.4 123")`,
		)
		Expect(err).Should(Succeed())
	})

	When("rate limiting is disabled", func() {
		BeforeEach(func(ctx context.Context) {
			denyzer, err = createDenyzerContainerFromString(ctx, e2eNet, dedent(`
				upstreams:
				  groups:
				    default:
				      - moka
			`))
			Expect(err).Should(Succeed())
		})

		It("passes traffic through unhindered", func(ctx context.Context) {
			for range 100 {
				msg := util.NewMsgWithQuestion("example.com.", A)
				Expect(doDNSRequest(ctx, denyzer, msg)).
					Should(BeDNSRecord("example.com.", A, "1.2.3.4"))
			}
		})
	})

	When("rate=1 burst=1 (no allowlist)", func() {
		BeforeEach(func(ctx context.Context) {
			denyzer, err = createDenyzerContainerFromString(ctx, e2eNet, dedent(`
				upstreams:
				  groups:
				    default:
				      - moka
				rateLimit:
				  enable: true
				  rate: 1
				  burst: 1
			`))
			Expect(err).Should(Succeed())
		})

		It("drops the second back-to-back query", func(ctx context.Context) {
			msg := util.NewMsgWithQuestion("example.com.", A)

			_, err := doDNSRequest(ctx, denyzer, msg)
			Expect(err).Should(Succeed())

			_, err = doDNSRequest(ctx, denyzer, msg)
			Expect(err).Should(HaveOccurred())
			var netErr net.Error
			Expect(errors.As(err, &netErr)).Should(BeTrue())
			Expect(netErr.Timeout()).Should(BeTrue())
		})

		It("allows again after a refill window", func(ctx context.Context) {
			msg := util.NewMsgWithQuestion("example.com.", A)
			_, err := doDNSRequest(ctx, denyzer, msg)
			Expect(err).Should(Succeed())

			Eventually(func() error {
				_, err := doDNSRequest(ctx, denyzer, msg)

				return err
			}).WithTimeout(15 * time.Second).WithPolling(time.Second).Should(Succeed())
		})

		It("emits a fail2ban-matchable WARN line", func(ctx context.Context) {
			msg := util.NewMsgWithQuestion("example.com.", A)
			_, _ = doDNSRequest(ctx, denyzer, msg)
			_, _ = doDNSRequest(ctx, denyzer, msg)

			Eventually(func() bool {
				lines, err := getContainerLogs(ctx, denyzer)
				if err != nil {
					return false
				}
				for _, line := range lines {
					if strings.Contains(line, "rate-limiting:") &&
						strings.Contains(line, "dropped query") &&
						strings.Contains(line, "client_ip=") {
						return true
					}
				}

				return false
			}, 5*time.Second, 200*time.Millisecond).Should(BeTrue())
		})
	})

	When("rate=1 burst=1 with allowlist covering the test gateway", func() {
		BeforeEach(func(ctx context.Context) {
			denyzer, err = createDenyzerContainerFromString(ctx, e2eNet, dedent(`
				upstreams:
				  groups:
				    default:
				      - moka
				rateLimit:
				  enable: true
				  rate: 1
				  burst: 1
				  allowlist:
				    - 172.16.0.0/12
				    - 10.0.0.0/8
				    - 192.168.0.0/16
			`))
			Expect(err).Should(Succeed())
		})

		It("never drops allowlisted clients", func(ctx context.Context) {
			for range 5 {
				msg := util.NewMsgWithQuestion("example.com.", A)
				Expect(doDNSRequest(ctx, denyzer, msg)).
					Should(BeDNSRecord("example.com.", A, "1.2.3.4"))
			}
		})
	})
})
