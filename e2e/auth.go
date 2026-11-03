package e2e

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mia0x75/denyzer/auth"
	"github.com/mia0x75/denyzer/config"
	. "github.com/onsi/ginkgo/v2"
)

const e2ePassword = "test-only-management-password"

//nolint:gochecknoglobals // Share the expensive, test-only password hash across container fixtures.
var e2ePasswordHash = sync.OnceValues(func() (string, error) {
	return auth.HashPassword([]byte(e2ePassword))
})

func setManagementAuth(path string) error {
	hash, err := e2ePasswordHash()
	if err != nil {
		return err
	}

	return config.SetAuth(path, config.Auth{Username: "admin", PasswordHash: hash})
}

type bearerTransport struct {
	http.RoundTripper

	token string
}

func (t bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)

	return t.RoundTripper.RoundTrip(r)
}

func newManagementClient(ctx context.Context, baseURL string) (*http.Client, error) {
	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // Disposable e2e fixtures use generated self-signed certificates.
	DeferCleanup(transport.CloseIdleConnections)
	client := &http.Client{
		Transport: transport, Timeout: 5 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/login",
		strings.NewReader(`{"username":"admin","password":"`+e2ePassword+`"}`))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("management login failed: %s", response.Status)
	}
	var login struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&login); err != nil {
		return nil, err
	}
	if login.Token == "" {
		return nil, errors.New("management login returned an empty token")
	}
	client.Transport = bearerTransport{RoundTripper: transport, token: login.Token}

	return client, nil
}
