package e2e

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/testcontainers/testcontainers-go"

	"github.com/mia0x75/denyzer/util"
)

var _ = Describe("PROXY protocol behind an nginx stream proxy", func() {
	When("denyzer and the nginx stream proxy both use the PROXY protocol", Ordered, func() {
		var (
			e2eNet  *testcontainers.DockerNetwork
			denyzer testcontainers.Container
			nginx   testcontainers.Container
		)

		BeforeAll(func(ctx context.Context) {
			e2eNet = getRandomNetwork(ctx)

			_, err := createDNSMokkaContainer(ctx, "moka1", e2eNet,
				`A dotquery/NOERROR("A 1.2.3.4 123")`,
				`A dohquery/NOERROR("A 1.2.3.4 123")`)
			Expect(err).Should(Succeed())

			denyzer, err = createDenyzerContainerFromString(ctx, e2eNet, proxyProtocolDenyzerConfig("tls, https"))
			Expect(err).Should(Succeed())

			// nginx must start after denyzer so it can resolve the 'denyzer' alias.
			nginx, err = createNginxStreamProxyContainer(ctx, e2eNet, true)
			Expect(err).Should(Succeed())
		})

		It("uses the PROXY header source as the client IP for DoT", func(ctx context.Context) {
			Eventually(func() error {
				return queryDoTViaProxy(ctx, nginx, "dotquery.")
			}, "30s", "1s").Should(Succeed())

			clientIP := proxyProtocolClientIP(ctx, denyzer, "dotquery")
			Expect(net.ParseIP(clientIP)).ShouldNot(BeNil())

			// The PROXY protocol header contains the address nginx saw for the client
			// connection. Depending on the container runtime, this is either the address of
			// the real client or the address of the runtime's port forwarder. Asserting that
			// denyzer used the address from the header is therefore independent of the
			// runtime, while still verifying that the header was honored.
			proxyAddr := nginxRemoteAddr(ctx, nginx, e2eDoTPort)
			Expect(clientIP).Should(Equal(proxyAddr),
				"denyzer should log the client IP from the PROXY header sent by nginx")
		})

		It("uses the PROXY header source as the client IP for DoH", func(ctx context.Context) {
			Eventually(func() error {
				return queryDoHViaProxy(ctx, nginx, "dohquery.")
			}, "30s", "1s").Should(Succeed())

			clientIP := proxyProtocolClientIP(ctx, denyzer, "dohquery")
			Expect(net.ParseIP(clientIP)).ShouldNot(BeNil())

			proxyAddr := nginxRemoteAddr(ctx, nginx, e2eDoHPort)
			Expect(clientIP).Should(Equal(proxyAddr),
				"denyzer should log the client IP from the PROXY header sent by nginx")
		})
	})

	When("the PROXY protocol is not enabled", Ordered, func() {
		var (
			e2eNet  *testcontainers.DockerNetwork
			denyzer testcontainers.Container
			nginx   testcontainers.Container
		)

		BeforeAll(func(ctx context.Context) {
			e2eNet = getRandomNetwork(ctx)

			_, err := createDNSMokkaContainer(ctx, "moka1", e2eNet,
				`A dotquery/NOERROR("A 1.2.3.4 123")`,
				`A dohquery/NOERROR("A 1.2.3.4 123")`)
			Expect(err).Should(Succeed())

			denyzer, err = createDenyzerContainerFromString(ctx, e2eNet, proxyProtocolDenyzerConfig(""))
			Expect(err).Should(Succeed())

			nginx, err = createNginxStreamProxyContainer(ctx, e2eNet, false)
			Expect(err).Should(Succeed())
		})

		It("records the proxy address as the client IP for DoT", func(ctx context.Context) {
			Eventually(func() error {
				return queryDoTViaProxy(ctx, nginx, "dotquery.")
			}, "30s", "1s").Should(Succeed())

			nginxIP, err := getContainerNetworkIP(ctx, nginx, e2eNet.Name)
			Expect(err).Should(Succeed())

			clientIP := proxyProtocolClientIP(ctx, denyzer, "dotquery")
			Expect(clientIP).Should(Equal(nginxIP),
				"without PROXY protocol denyzer sees the connection coming from the proxy")
		})

		It("records the proxy address as the client IP for DoH", func(ctx context.Context) {
			Eventually(func() error {
				return queryDoHViaProxy(ctx, nginx, "dohquery.")
			}, "30s", "1s").Should(Succeed())

			nginxIP, err := getContainerNetworkIP(ctx, nginx, e2eNet.Name)
			Expect(err).Should(Succeed())

			clientIP := proxyProtocolClientIP(ctx, denyzer, "dohquery")
			Expect(clientIP).Should(Equal(nginxIP),
				"without PROXY protocol denyzer sees the connection coming from the proxy")
		})
	})
})

// proxyProtocolDenyzerConfig returns a denyzer config listening for DNS (e2eDNSPort), DoT
// (e2eDoTPort) and HTTPS/DoH (e2eDoHPort). It requires the PROXY protocol on the given
// comma-separated listener families (empty disables it) and logs resolved queries to the console,
// so the recorded client IP can be asserted from the container logs.
func proxyProtocolDenyzerConfig(families string) string {
	return fmt.Sprintf(`log:
  level: info
ports:
  dns: %d
  tls: %d
  https: %d
  proxyProtocol: [%s]
queryLog:
  type: console
  flushInterval: 1s
upstreams:
  groups:
    default:
      - moka1
`, e2eDNSPort, e2eDoTPort, e2eDoHPort, families)
}

// queryDoTViaProxy sends a DNS-over-TLS query for the given question through the nginx proxy's
// mapped e2eDoTPort port. The TLS handshake terminates at denyzer (nginx is a plain TCP passthrough).
func queryDoTViaProxy(ctx context.Context, nginx testcontainers.Container, question string) error {
	host, port, err := getContainerHostPort(ctx, nginx, strconv.Itoa(e2eDoTPort)+"/tcp")
	if err != nil {
		return err
	}

	c := &dns.Client{
		Net:       "tcp-tls",
		Timeout:   5 * time.Second,
		TLSConfig: &tls.Config{InsecureSkipVerify: true},
	}

	_, _, err = c.Exchange(util.NewMsgWithQuestion(question, dns.Type(dns.TypeA)), net.JoinHostPort(host, port))

	return err
}

// queryDoHViaProxy sends a DNS-over-HTTPS query for the given question through the nginx proxy's
// mapped e2eDoHPort port.
func queryDoHViaProxy(ctx context.Context, nginx testcontainers.Container, question string) error {
	host, port, err := getContainerHostPort(ctx, nginx, strconv.Itoa(e2eDoHPort)+"/tcp")
	if err != nil {
		return err
	}

	packed, err := util.NewMsgWithQuestion(question, dns.Type(dns.TypeA)).Pack()
	if err != nil {
		return err
	}

	url := "https://" + net.JoinHostPort(host, port) + "/dns-query?dns=" + base64.RawURLEncoding.EncodeToString(packed)

	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected DoH status %d", resp.StatusCode)
	}

	// Close the keep-alive connection. The nginx stream proxy writes its access log entry
	// when the proxied session ends, so the connection has to be closed to make the entry
	// available to the test.
	client.CloseIdleConnections()

	return nil
}

var clientIPLogRe = regexp.MustCompile(`client_ip="?([0-9a-fA-F.:]+)`)

var nginxRemoteAddrLogRe = regexp.MustCompile(`proxy_protocol_conn remote_addr=([0-9a-fA-F.:]+) server_port=(\d+)`)

// proxyProtocolClientIP polls the denyzer container logs for the resolved-query log entry of the
// given question name and returns the client_ip it recorded.
func proxyProtocolClientIP(ctx context.Context, denyzer testcontainers.Container, questionName string) string {
	var clientIP string

	Eventually(func() string {
		lines, err := getContainerLogs(ctx, denyzer)
		if err != nil {
			return ""
		}

		clientIP = ""

		for _, line := range lines {
			if !strings.Contains(line, "query resolved") || !strings.Contains(line, questionName) {
				continue
			}

			if m := clientIPLogRe.FindStringSubmatch(line); m != nil {
				clientIP = m[1]
			}
		}

		return clientIP
	}, "15s", "500ms").ShouldNot(BeEmpty(),
		"expected a resolved-query log entry for %q with a client_ip field", questionName)

	return clientIP
}

// nginxRemoteAddr polls the nginx container logs for the access log entry of the session
// accepted on the given server port and returns the remote address nginx saw for it.
// This is the address nginx sends in the PROXY protocol header to the upstream.
func nginxRemoteAddr(ctx context.Context, nginx testcontainers.Container, serverPort int) string {
	var remoteAddr string

	Eventually(func() string {
		lines, err := getContainerLogs(ctx, nginx)
		if err != nil {
			return ""
		}

		remoteAddr = ""

		for _, line := range lines {
			m := nginxRemoteAddrLogRe.FindStringSubmatch(line)
			if m == nil || m[2] != strconv.Itoa(serverPort) {
				continue
			}

			remoteAddr = m[1]
		}

		return remoteAddr
	}, "15s", "500ms").ShouldNot(BeEmpty(),
		"expected an nginx access log entry for server port %d", serverPort)

	return remoteAddr
}
