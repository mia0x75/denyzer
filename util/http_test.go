package util

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("HTTP Util", func() {
	Describe("DefaultHTTPTransport", func() {
		It("returns a new transport", func() {
			a := DefaultHTTPTransport()
			Expect(a).ShouldNot(BeIdenticalTo(baseTransport))

			b := DefaultHTTPTransport()
			Expect(a).ShouldNot(BeIdenticalTo(b))
		})

		It("preserves the default connection settings", func() {
			transport := DefaultHTTPTransport()
			Expect(transport.DialContext).ShouldNot(BeNil())
			Expect(transport.Proxy).ShouldNot(BeNil())
			Expect(transport.ForceAttemptHTTP2).Should(Equal(baseTransport.ForceAttemptHTTP2))
			Expect(transport.MaxIdleConns).Should(Equal(baseTransport.MaxIdleConns))
			Expect(transport.IdleConnTimeout).Should(Equal(baseTransport.IdleConnTimeout))
			Expect(transport.TLSHandshakeTimeout).Should(Equal(baseTransport.TLSHandshakeTimeout))
			Expect(transport.ExpectContinueTimeout).Should(Equal(baseTransport.ExpectContinueTimeout))
		})

		It("preserves configuration without sharing mutable headers or TLS settings", func() {
			DeferCleanup(func(orig *http.Transport) { baseTransport = orig }, baseTransport)

			baseTransport = &http.Transport{
				ResponseHeaderTimeout: 3 * time.Second,
				ProxyConnectHeader:    http.Header{"X-Test": []string{"1"}},
				TLSClientConfig:       &tls.Config{ServerName: "example.com"},
			}

			transport := DefaultHTTPTransport()
			Expect(transport.ResponseHeaderTimeout).Should(Equal(3 * time.Second))
			Expect(transport.ProxyConnectHeader).Should(Equal(baseTransport.ProxyConnectHeader))
			Expect(transport.TLSClientConfig.ServerName).Should(Equal("example.com"))

			transport.ProxyConnectHeader["X-Test"][0] = "changed"
			transport.TLSClientConfig.ServerName = "changed.example.com"
			Expect(baseTransport.ProxyConnectHeader.Get("X-Test")).Should(Equal("1"))
			Expect(baseTransport.TLSClientConfig.ServerName).Should(Equal("example.com"))
		})
	})

	DescribeTable("HTTPClientIP ignores spoofed forwarding headers",
		func(remoteAddr, expected string) {
			r, err := http.NewRequest(http.MethodGet, "http://example.com", nil)
			Expect(err).Should(Succeed())
			r.RemoteAddr = remoteAddr
			r.Header.Set("Forwarded", `for="[2001:db8::99]:8080", for=203.0.113.99`)
			r.Header.Set("X-Forwarded-For", "203.0.113.99, 192.0.2.99")

			Expect(HTTPClientIP(r)).Should(Equal(net.ParseIP(expected)))
		},
		Entry("IPv4 with port", "192.0.2.1:12345", "192.0.2.1"),
		Entry("IPv4 without port", "192.0.2.1", "192.0.2.1"),
		Entry("IPv6 with port", "[2001:db8::1]:12345", "2001:db8::1"),
		Entry("IPv6 without port", "2001:db8::1", "2001:db8::1"),
		Entry("scoped IPv6 with port", "[fe80::1%eth0]:12345", "fe80::1"),
		Entry("scoped IPv6 without port", "fe80::1%eth0", "fe80::1"),
		Entry("numeric IPv6 zone", "[fe80::1%2]:12345", "fe80::1"),
		Entry("IPv4-mapped IPv6", "[::ffff:192.0.2.1]:12345", "192.0.2.1"),
		Entry("invalid peer", "not-an-ip:12345", ""),
		Entry("missing peer", "", ""),
	)
})
