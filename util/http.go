package util

import (
	"fmt"
	"net"
	"net/http"
)

//nolint:gochecknoglobals
var baseTransport *http.Transport

//nolint:gochecknoinits
func init() {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		panic(fmt.Errorf(
			"unsupported Go version: http.DefaultTransport is not of type *http.Transport: it is a %T",
			http.DefaultTransport,
		))
	}

	baseTransport = base
}

// DefaultHTTPTransport returns a new Transport with the same defaults as net/http.
func DefaultHTTPTransport() *http.Transport {
	return baseTransport.Clone()
}

// HTTPClientIP uses the connection address (including configured PROXY protocol), never spoofable HTTP headers.
func HTTPClientIP(r *http.Request) net.IP {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return net.ParseIP(r.RemoteAddr)
	}

	return net.ParseIP(ip)
}
