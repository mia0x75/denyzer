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

	// healthcheckStartPeriod and healthcheckTimeout mirror the timing of the
	// healthcheck in the Dockerfile, so that overriding the healthcheck doesn't
	// change how a container is judged healthy.
	healthcheckStartPeriod = time.Minute
	healthcheckTimeout     = 3 * time.Second

	// e2eDNSPort is the unprivileged DNS port the denyzer containers listen on
	// in the e2e suite. The default port 53 is privileged: binding it requires
	// CAP_NET_BIND_SERVICE, which the container image only carries in its
	// permitted set (via a file capability) and can only promote at runtime in
	// non-cgo builds. Listening on an unprivileged port keeps the suite
	// independent of the container's capabilities.
	e2eDNSPort = 1053

	// e2eDoTPort and e2eDoHPort are the unprivileged ports used by the PROXY
	// protocol tests for the DoT and DoH/HTTPS listeners. They replace the
	// privileged ports 853 and 443 for the same reason as e2eDNSPort.
	e2eDoTPort = 8853
	e2eDoHPort = 8443
)

// withRelabelOption adds the "z" mount option to a bind mount. It lets the
// container runtime relabel the host path for the container. Without it the
// container can't read or write the mounted files on hosts with SELinux
// enabled. On hosts without SELinux the option has no effect.
func withRelabelOption(bind string) string {
	// The mount options are the third colon separated field of a bind mount.
	if strings.Count(bind, ":") >= 2 {
		return bind + ",z"
	}

	return bind + ":z"
}

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
			hc.Binds = append(hc.Binds, withRelabelOption(dir+":/data:ro"))
		},
	}

	return startContainerWithNetwork(ctx, req, alias, e2eNet)
}

// createNginxStreamProxyContainer starts an nginx stream (L4) proxy in front of denyzer on the test
// network under the alias 'nginx'. It forwards TCP e2eDoTPort -> denyzer:e2eDoTPort (DoT) and
// e2eDoHPort -> denyzer:e2eDoHPort (DoH/HTTPS). When proxyProtocol is true, nginx prepends a HAProxy
// PROXY protocol header to each upstream connection. denyzer must already be running so nginx can
// resolve the 'denyzer' alias at startup. It is automatically terminated when the test is finished.
func createNginxStreamProxyContainer(ctx context.Context, e2eNet *testcontainers.DockerNetwork,
	proxyProtocol bool,
) (testcontainers.Container, error) {
	proxyProtocolDirective := ""
	if proxyProtocol {
		proxyProtocolDirective = "proxy_protocol on;"
	}

	// Minimal nginx.conf with only the stream module: TCP passthrough to denyzer so the TLS
	// handshake (DoT/DoH) terminates at denyzer, optionally prefixed with a PROXY protocol header.
	//
	// The stream access log records the address nginx saw for the client connection. This is the
	// address nginx puts into the PROXY protocol header, so the tests can verify that denyzer
	// used it as the client IP, independently of the container runtime.
	conf := fmt.Sprintf(`worker_processes 1;
events {}
stream {
    log_format proxy_protocol 'proxy_protocol_conn remote_addr=$remote_addr server_port=$server_port';
    access_log /dev/stdout proxy_protocol;
    server {
        listen %[1]d;
        proxy_pass denyzer:%[1]d;
        %[2]s
    }
    server {
        listen %[3]d;
        proxy_pass denyzer:%[3]d;
        %[2]s
    }
}
`, e2eDoTPort, proxyProtocolDirective, e2eDoHPort)

	dotPort := strconv.Itoa(e2eDoTPort)

	req := testcontainers.ContainerRequest{
		Image:        nginxImage,
		ExposedPorts: []string{dotPort + "/tcp", strconv.Itoa(e2eDoHPort) + "/tcp"},
		// The nginx image EXPOSEs port 80, which it does not listen on with our stream-only
		// config, so wait for the DoT port we actually bind instead of the first exposed port.
		WaitingFor: wait.ForListeningPort(dotPort + "/tcp"),
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

	dnsPort := strconv.Itoa(e2eDNSPort)

	req := testcontainers.ContainerRequest{
		Image:        image,
		ExposedPorts: []string{dnsPort + "/tcp", dnsPort + "/udp", "4000/tcp", "4000/udp"},
		Files: []testcontainers.ContainerFile{
			{
				HostFilePath:      confFile,
				ContainerFilePath: "/app/config.yml",
				FileMode:          modeWorldReadable,
			},
		},
		ConfigModifier: func(c *container.Config) {
			c.Healthcheck = &container.HealthConfig{
				// The test command has to be set explicitly. Some container
				// runtimes (e.g. podman) reject a healthcheck without a command
				// instead of falling back to the one from the image.
				Test:     []string{"CMD", "/app/denyzer", "healthcheck"},
				Interval: time.Second,
				// During the image's start period Docker probes at the
				// start-interval, which defaults to 5s. denyzer is ready in
				// <1s, so without this every container wastes ~5s waiting to
				// be marked healthy.
				StartInterval: healthcheckStartInterval,
				// Keep the timing of the image's healthcheck, so the override
				// only changes how fast the container is probed.
				StartPeriod: healthcheckStartPeriod,
				Timeout:     healthcheckTimeout,
			}
			// Enable coverage collection if GOCOVERDIR is set
			if coverDir != "" {
				c.Env = append(c.Env, "GOCOVERDIR=/tmp/coverage")
			}
		},
		HostConfigModifier: func(hc *container.HostConfig) {
			// Mount coverage directory if enabled
			if coverDir != "" {
				hc.Binds = append(hc.Binds, withRelabelOption(coverDir+":/tmp/coverage"))
			}
		},
		WaitingFor: wait.ForHealthCheck().WithStartupTimeout(startupTimeout),
	}

	return req
}

// createDenyzerContainerInternal builds and starts a denyzer container from the given config
// lines, mounting any extraFiles in addition to the generated config.yml and adding any
// Docker bind mounts (each in "hostPath:containerPath" form). The bind mounts are relabeled
// for the container, see withRelabelOption.
func createDenyzerContainerInternal(ctx context.Context, e2eNet *testcontainers.DockerNetwork,
	extraFiles []testcontainers.ContainerFile, binds []string, lines ...string,
) (testcontainers.Container, error) {
	// Add timeout to context
	ctx, cancel := context.WithTimeout(ctx, 2*startupTimeout)
	defer cancel()

	confFile := createTempFile(withDNSPort(lines)...)

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
			for _, bind := range binds {
				hc.Binds = append(hc.Binds, withRelabelOption(bind))
			}
		}
	}

	container, err := startContainerWithNetwork(ctx, req, "denyzer", e2eNet)
	if err != nil {
		// attach container log if error occurs
		attachContainerLog(ctx, container)

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

// attachContainerLog adds the log output of the given container to the test report.
// It is used to capture the reason why a container didn't start.
func attachContainerLog(ctx context.Context, container testcontainers.Container) {
	// A container that could not be started is handed out as a typed nil pointer.
	if isNilContainer(container) {
		return
	}

	r, err := container.Logs(ctx)
	if err != nil {
		return
	}

	b, err := io.ReadAll(r)
	if err != nil {
		return
	}

	AddReportEntry("denyzer container log", string(b))
}

// createDenyzerContainerWithBinds creates a denyzer container like createDenyzerContainer, but additionally
// mounts the given Docker bind mounts (each in "hostPath:containerPath" form) into the container.
// The bind mounts are relabeled for the container, see withRelabelOption.
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

// withDNSPort returns the given config lines with an explicit DNS listen port.
// The e2e suite talks to denyzer on the unprivileged e2eDNSPort, so configs
// that do not configure a DNS port get one; configs with their own DNS port
// are left untouched.
func withDNSPort(lines []string) []string {
	for i, line := range lines {
		// Only a top-level "ports:" mapping is considered.
		if strings.TrimRight(line, " \t") != "ports:" {
			continue
		}

		// A DNS port configured in the ports block wins over the e2e default.
		for _, child := range lines[i+1:] {
			trimmed := strings.TrimSpace(child)
			if trimmed == "" {
				continue
			}

			if len(child)-len(strings.TrimLeft(child, " \t")) == 0 {
				// The next top-level key ends the ports block.
				break
			}

			if strings.HasPrefix(trimmed, "dns:") {
				return lines
			}
		}

		withPort := make([]string, 0, len(lines)+1)
		withPort = append(withPort, lines[:i+1]...)
		withPort = append(withPort, "  dns: "+strconv.Itoa(e2eDNSPort))
		withPort = append(withPort, lines[i+1:]...)

		return withPort
	}

	withPort := make([]string, 0, len(lines)+2)
	withPort = append(withPort, lines...)
	withPort = append(withPort, "ports:", "  dns: "+strconv.Itoa(e2eDNSPort))

	return withPort
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
