package e2e

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/avast/retry-go/v4"
	"github.com/mia0x75/denyzer/config"
	"github.com/mia0x75/denyzer/util"
	"github.com/miekg/dns"
	"github.com/moby/moby/api/types/container"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// container image names
const (
	mokaImage         = "ghcr.io/0xerr0r/dns-mokka:0.4.0"
	staticServerImage = "halverneus/static-file-server:latest"
	nginxImage        = "nginx:1.27-alpine"
	denyzerImage      = "denyzer-e2e"
)

// helper constants
const (
	// modeWorldReadable is used for files mounted into the denyzer container: denyzer
	// runs as a non-root user (see Dockerfile `USER 100`) that doesn't own the
	// copied files, so it can only read them via the world-readable bit.
	modeWorldReadable = 0o444
	// modeWorldReadableDir is used for host directories bind-mounted into containers,
	// so a container user with a different UID can traverse and read them.
	modeWorldReadableDir = 0o755
	startupTimeout       = 30 * time.Second

	// healthcheckStartInterval overrides Docker's 5s default start-interval so
	// denyzer (ready in <1s) is probed and marked healthy almost immediately.
	healthcheckStartInterval = 250 * time.Millisecond
)

// createDNSMokkaContainer creates a DNS mokka container with the given rules attached to the test network
// under the given alias.
// It is automatically terminated when the test is finished.
func createDNSMokkaContainer(ctx context.Context, alias string, e2eNet *testcontainers.DockerNetwork,
	rules ...string,
) (testcontainers.Container, error) {
	mokaRules := make(map[string]string)

	for i, rule := range rules {
		mokaRules[fmt.Sprintf("MOKKA_RULE_%d", i)] = rule
	}

	req := testcontainers.ContainerRequest{
		Image:        mokaImage,
		ExposedPorts: []string{"53/tcp", "53/udp"},
		WaitingFor:   wait.ForExposedPort(),
		Env:          mokaRules,
	}

	return startContainerWithNetwork(ctx, req, alias, e2eNet)
}

// createHTTPServerContainer creates a static HTTP server container that serves one file with the given lines
// and is attached to the test network under the given alias.
// It is automatically terminated when the test is finished.
//
// The file is served via a bind-mounted host directory so that the host file's
// modification time is visible inside the container. This allows Go's
// http.FileServer (used by halverneus/static-file-server) to emit a valid
// Last-Modified header and return 304 for conditional requests.
func createHTTPServerContainer(ctx context.Context, alias string, e2eNet *testcontainers.DockerNetwork,
	filename string, lines ...string,
) (testcontainers.Container, error) {
	dir, err := os.MkdirTemp("", "denyzer_e2e_httpdir-")
	Expect(err).Should(Succeed())
	Expect(os.Chmod(dir, modeWorldReadableDir)).Should(Succeed()) // container user (different UID) must traverse/read the bind mount
	DeferCleanup(func() error {
		return os.RemoveAll(dir)
	})

	f, err := os.OpenFile(filepath.Join(dir, filename), os.O_CREATE|os.O_WRONLY, modeWorldReadable)
	Expect(err).Should(Succeed())
	for i, l := range lines {
		if i != 0 {
			_, err = f.WriteString("\n")
			Expect(err).Should(Succeed())
		}
		_, err = f.WriteString(l)
		Expect(err).Should(Succeed())
	}
	Expect(f.Close()).Should(Succeed())

	req := testcontainers.ContainerRequest{
		Image:        staticServerImage,
		ExposedPorts: []string{"8080/tcp"},
		Env:          map[string]string{"FOLDER": "/data"},
		HostConfigModifier: func(hc *container.HostConfig) {
			hc.Binds = append(hc.Binds, dir+":/data:ro")
		},
	}

	return startContainerWithNetwork(ctx, req, alias, e2eNet)
}

// createNginxStreamProxyContainer starts an nginx stream (L4) proxy in front of denyzer on the test
// network under the alias 'nginx'. It forwards TCP 853 -> denyzer:853 (DoT) and 443 -> denyzer:443
// (DoH/HTTPS). When proxyProtocol is true, nginx prepends a HAProxy PROXY protocol header to each
// upstream connection. denyzer must already be running so nginx can resolve the 'denyzer' alias at
// startup. It is automatically terminated when the test is finished.
func createNginxStreamProxyContainer(ctx context.Context, e2eNet *testcontainers.DockerNetwork,
	proxyProtocol bool,
) (testcontainers.Container, error) {
	proxyProtocolDirective := ""
	if proxyProtocol {
		proxyProtocolDirective = "proxy_protocol on;"
	}

	// Minimal nginx.conf with only the stream module: TCP passthrough to denyzer so the TLS
	// handshake (DoT/DoH) terminates at denyzer, optionally prefixed with a PROXY protocol header.
	conf := fmt.Sprintf(`worker_processes 1;
events {}
stream {
    server {
        listen 853;
        proxy_pass denyzer:853;
        %[1]s
    }
    server {
        listen 443;
        proxy_pass denyzer:443;
        %[1]s
    }
}
`, proxyProtocolDirective)

	req := testcontainers.ContainerRequest{
		Image:        nginxImage,
		ExposedPorts: []string{"853/tcp", "443/tcp"},
		// The nginx image EXPOSEs port 80, which it does not listen on with our stream-only
		// config, so wait for the DoT port we actually bind instead of the first exposed port.
		WaitingFor: wait.ForListeningPort("853/tcp"),
		Files: []testcontainers.ContainerFile{
			{
				HostFilePath:      createTempFile(conf),
				ContainerFilePath: "/etc/nginx/nginx.conf",
				FileMode:          modeWorldReadable,
			},
		},
	}

	return startContainerWithNetwork(ctx, req, "nginx", e2eNet)
}

// buildDenyzerContainerRequest builds a container request for denyzer with the given config file.
func buildDenyzerContainerRequest(confFile string) testcontainers.ContainerRequest {
	coverDir := os.Getenv("GOCOVERDIR")
	image := denyzerImage
	if coverImageOverride := os.Getenv("DENYZER_IMAGE"); coverImageOverride != "" {
		image = coverImageOverride
	}

	req := testcontainers.ContainerRequest{
		Image:        image,
		ExposedPorts: []string{"53/tcp", "53/udp", "4000/tcp", "4000/udp"},
		Files: []testcontainers.ContainerFile{
			{
				HostFilePath:      confFile,
				ContainerFilePath: "/app/config.yml",
				FileMode:          modeWorldReadable,
			},
		},
		ConfigModifier: func(c *container.Config) {
			c.Healthcheck = &container.HealthConfig{
				Interval: time.Second,
				// During the image's start period Docker probes at the
				// start-interval, which defaults to 5s. denyzer is ready in
				// <1s, so without this every container wastes ~5s waiting to
				// be marked healthy.
				StartInterval: healthcheckStartInterval,
			}
			// Enable coverage collection if GOCOVERDIR is set
			if coverDir != "" {
				c.Env = append(c.Env, "GOCOVERDIR=/tmp/coverage")
			}
		},
		HostConfigModifier: func(hc *container.HostConfig) {
			// Mount coverage directory if enabled
			if coverDir != "" {
				hc.Binds = append(hc.Binds, coverDir+":/tmp/coverage")
			}
		},
		WaitingFor: wait.ForHealthCheck().WithStartupTimeout(startupTimeout),
	}

	return req
}

// createDenyzerContainerInternal builds and starts a denyzer container from the given config
// lines, mounting any extraFiles in addition to the generated config.yml and adding any
// Docker bind mounts (each in "hostPath:containerPath" form).
func createDenyzerContainerInternal(ctx context.Context, e2eNet *testcontainers.DockerNetwork,
	extraFiles []testcontainers.ContainerFile, binds []string, lines ...string,
) (testcontainers.Container, error) {
	// Add timeout to context
	ctx, cancel := context.WithTimeout(ctx, 2*startupTimeout)
	defer cancel()

	confFile := createTempFile(lines...)

	cfg, err := config.LoadConfig(confFile, true)
	if err != nil {
		return nil, fmt.Errorf("can't create config struct %w", err)
	}

	req := buildDenyzerContainerRequest(confFile)
	req.Files = append(req.Files, extraFiles...)

	if len(binds) > 0 {
		baseHostConfigModifier := req.HostConfigModifier
		req.HostConfigModifier = func(hc *container.HostConfig) {
			baseHostConfigModifier(hc)
			hc.Binds = append(hc.Binds, binds...)
		}
	}

	container, err := startContainerWithNetwork(ctx, req, "denyzer", e2eNet)
	if err != nil {
		// attach container log if error occurs
		if r, err := container.Logs(ctx); err == nil {
			if b, err := io.ReadAll(r); err == nil {
				AddReportEntry("denyzer container log", string(b))
			}
		}

		return container, err
	}

	// check if DNS/HTTP interface is working.
	// Sometimes the internal health check returns OK, but the container port is not mapped yet
	err = checkDenyzerReadiness(ctx, cfg, container)
	if err != nil {
		return container, fmt.Errorf("container not ready: %w", err)
	}

	return container, nil
}

// createDenyzerContainerWithBinds creates a denyzer container like createDenyzerContainer, but additionally
// mounts the given Docker bind mounts (each in "hostPath:containerPath" form) into the container.
// It is automatically terminated when the test is finished.
func createDenyzerContainerWithBinds(ctx context.Context, e2eNet *testcontainers.DockerNetwork,
	binds []string, lines ...string,
) (testcontainers.Container, error) {
	return createDenyzerContainerInternal(ctx, e2eNet, nil, binds, lines...)
}

// createDenyzerContainer creates a denyzer container with a config provided by the given lines.
// It is attached to the test network under the alias 'denyzer'.
// It is automatically terminated when the test is finished.
func createDenyzerContainer(ctx context.Context, e2eNet *testcontainers.DockerNetwork,
	lines ...string,
) (testcontainers.Container, error) {
	return createDenyzerContainerInternal(ctx, e2eNet, nil, nil, lines...)
}

// createDenyzerContainerFromString creates a denyzer container with a config provided as a single YAML string.
// It is attached to the test network under the alias 'denyzer'.
// It is automatically terminated when the test is finished.
func createDenyzerContainerFromString(ctx context.Context, e2eNet *testcontainers.DockerNetwork,
	configYAML string,
) (testcontainers.Container, error) {
	return createDenyzerContainer(ctx, e2eNet, strings.Split(configYAML, "\n")...)
}

func checkDenyzerReadiness(ctx context.Context, cfg *config.Config, container testcontainers.Container) error {
	var err error

	const retryAttempts = 3

	err = retry.Do(
		func() error {
			_, err = doDNSRequest(ctx, container, util.NewMsgWithQuestion("healthcheck.denyzer.", dns.Type(dns.TypeA)))
			if err != nil {
				return fmt.Errorf("DNS request failed: %w", err)
			}

			return nil
		},
		retry.OnRetry(func(n uint, err error) {
			log.Infof("Performing retry DNS request #%d: %s\n", n, err)
		}),
		retry.Attempts(retryAttempts),
		retry.DelayType(retry.BackOffDelay),
		retry.Delay(time.Second))
	if err != nil {
		return fmt.Errorf("can't perform the DNS healthcheck request: %w", err)
	}

	for _, httpPort := range cfg.Ports.HTTP {
		parts := strings.Split(httpPort, ":")
		port := parts[len(parts)-1]

		err = retry.Do(
			func() error {
				return doHTTPRequest(ctx, container, port)
			},
			retry.OnRetry(func(n uint, err error) {
				log.Infof("Performing retry HTTP request #%d: %s\n", n, err)
			}),
			retry.Attempts(retryAttempts),
			retry.DelayType(retry.BackOffDelay),
			retry.Delay(time.Second))
		if err != nil {
			return fmt.Errorf("can't perform the HTTP request: %w", err)
		}
	}

	return nil
}

func doHTTPRequest(ctx context.Context, container testcontainers.Container, containerPort string) error {
	host, port, err := getContainerHostPort(ctx, container, containerPort+"/tcp")
	if err != nil {
		return err
	}

	client := &http.Client{
		Timeout: 5 * time.Second,
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://"+net.JoinHostPort(host, port), nil)
	if err != nil {
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("received not OK status: %d", resp.StatusCode)
	}

	return nil
}

// createDenyzerContainerWithCapDrop creates a denyzer container that listens on
// the given DNS port with ALL Linux capabilities dropped. It verifies the image
// execs and serves DNS under a PSS-Restricted-style runtime (Kubernetes
// Restricted profile / docker --cap-drop ALL). It builds on the standard denyzer
// container request, overriding only the exposed ports, the healthcheck target
// port, and the dropped capabilities — so image resolution and coverage wiring
// are inherited.
func createDenyzerContainerWithCapDrop(ctx context.Context, e2eNet *testcontainers.DockerNetwork,
	dnsPort int, lines ...string,
) (testcontainers.Container, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*startupTimeout)
	defer cancel()

	confFile := createTempFile(lines...)
	portStr := strconv.Itoa(dnsPort)

	req := buildDenyzerContainerRequest(confFile)
	req.ExposedPorts = []string{portStr + "/tcp", portStr + "/udp"}

	baseConfigModifier := req.ConfigModifier
	req.ConfigModifier = func(c *container.Config) {
		baseConfigModifier(c)
		// Point the healthcheck at the configured DNS port (the image default is 53).
		c.Healthcheck.Test = []string{"CMD", "/app/denyzer", "healthcheck", "-p", portStr}
	}

	baseHostConfigModifier := req.HostConfigModifier
	req.HostConfigModifier = func(hc *container.HostConfig) {
		baseHostConfigModifier(hc)
		hc.CapDrop = []string{"ALL"}
	}

	return startContainerWithNetwork(ctx, req, "denyzer", e2eNet)
}

// createTempFile creates a temporary file with the given lines which is deleted after the test
// Each created file is prefixed with 'denyzer_e2e_file-'
func createTempFile(lines ...string) string {
	file, err := os.CreateTemp("", "denyzer_e2e_file-")
	Expect(err).Should(Succeed())

	defer file.Close()

	DeferCleanup(func() error {
		return os.Remove(file.Name())
	})

	for i, l := range lines {
		if i != 0 {
			_, err := file.WriteString("\n")
			Expect(err).Should(Succeed())
		}

		_, err := file.WriteString(l)
		Expect(err).Should(Succeed())
	}

	return file.Name()
}
