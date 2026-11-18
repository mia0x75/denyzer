package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mia0x75/denyzer/config"
	"github.com/mia0x75/denyzer/evt"
	"github.com/mia0x75/denyzer/log"
	"github.com/mia0x75/denyzer/server"
	"github.com/mia0x75/denyzer/util"

	"github.com/spf13/cobra"
)

//nolint:gochecknoglobals
var (
	isConfigMandatory = true
	signals           = make(chan os.Signal, 1)

	// raiseNetBindService is a seam so tests can stub the capability raise.
	raiseNetBindService = util.RaiseNetBindService

	// unprivilegedPortStartFile is a seam so tests can point at a fake sysctl.
	unprivilegedPortStartFile = "/proc/sys/net/ipv4/ip_unprivileged_port_start"
)

const shutdownTimeout = 10 * time.Second

func newServeCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "serve",
		Args:         cobra.NoArgs,
		Short:        "start denyzer DNS server (default command)",
		RunE:         startServer,
		SilenceUsage: true,
	}
}

// warnMissingPrivilegedPortCapability raises CAP_NET_BIND_SERVICE if it is
// available, and warns when a privileged port is configured but the
// capability could not be obtained. It never fails: any real bind error
// surfaces later from server.NewServer.
//
// The capability is only required for privileged ports, so configurations
// without them neither raise it nor warn about a failed raise: raising it can
// fail on its own (e.g. in cgo builds, where promoting a capability across all
// threads is unsupported) without affecting an unprivileged listener.
func warnMissingPrivilegedPortCapability(ports config.Ports) {
	privileged, start := privilegedPorts(ports)
	if len(privileged) == 0 {
		return
	}

	effective, err := raiseNetBindService()
	if err != nil {
		log.Log().Warn(fmt.Sprintf("could not adjust process capabilities (%v); binding "+
			"privileged port(s) %s may fail — %s", err, strings.Join(privileged, ", "), privilegedPortCapHint(start)))

		return
	}

	if effective {
		return
	}

	log.Log().Warn(fmt.Sprintf("configured to listen on privileged port(s) %s without "+
		"CAP_NET_BIND_SERVICE; %s", strings.Join(privileged, ", "), privilegedPortCapHint(start)))
}

// privilegedPortCapHint describes how to satisfy the CAP_NET_BIND_SERVICE
// requirement. start is the first port that doesn't need it.
func privilegedPortCapHint(start uint16) string {
	return fmt.Sprintf("grant CAP_NET_BIND_SERVICE (Kubernetes securityContext "+
		"capabilities.add, or docker run --cap-add NET_BIND_SERVICE) or use a port >= %d", start)
}

// privilegedPorts returns the configured ports that need CAP_NET_BIND_SERVICE,
// and the first port that doesn't.
func privilegedPorts(ports config.Ports) ([]string, uint16) {
	start := unprivilegedPortStart()

	return ports.PortsBelow(start), start
}

// unprivilegedPortStart returns net.ipv4.ip_unprivileged_port_start, the first
// port that can be bound without CAP_NET_BIND_SERVICE. Docker sets it to 0 in
// a container's network namespace. When it can't be read, it falls back to
// config.PrivilegedPortCeiling.
func unprivilegedPortStart() uint16 {
	b, err := os.ReadFile(unprivilegedPortStartFile)
	if err != nil {
		return config.PrivilegedPortCeiling
	}

	start, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 16)
	if err != nil {
		return config.PrivilegedPortCeiling
	}

	return uint16(start)
}

// loadServerConfig loads the config, sets up logging from it and logs the banner
func loadServerConfig() (*config.Config, error) {
	cfg, err := loadConfig(isConfigMandatory, func(cfg *config.Config) {
		log.Configure(&cfg.Log)
		printBanner()
	})
	if err != nil {
		return nil, fmt.Errorf("unable to load configuration: %w", err)
	}

	return cfg, nil
}

func startServer(_ *cobra.Command, _ []string) error {
	cfg, err := loadServerConfig()
	if err != nil {
		return err
	}

	warnMissingPrivilegedPortCapability(cfg.Ports)

	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	ctx, cancelFn := context.WithCancel(context.Background())
	defer cancelFn()

	srv, err := server.NewServer(ctx, cfg)
	if err != nil {
		return fmt.Errorf("can't start server: %w", err)
	}

	const errChanSize = 10
	errChan := make(chan error, errChanSize)

	srv.Start(ctx, errChan)

	var terminationErr error

	evt.Bus().Publish(evt.ApplicationStarted, util.Version, util.BuildTime)
	select {
	case <-signals:
		log.Log().InfoContext(ctx, "Terminating...")
	case err := <-errChan:
		terminationErr = err
	}

	// Drain requests and flush writers before cancelling their background context.
	stopCtx, stopCancel := context.WithTimeout(ctx, shutdownTimeout)
	defer stopCancel()

	return errors.Join(terminationErr, srv.Stop(stopCtx))
}

func printBanner() {
	log.Log().Info("+-------------------------------------------------------------+")
	log.Log().Info("|                                                             |")
	log.Log().Info("|   ▀▀▀▀██▄                                                   |")
	log.Log().Info("|   ░░░ ███                                                   |")
	log.Log().Info("|   ▒▒▒ ███ ▄██▀██▄ ███▀██▄ ███ ███ ███████ ▄██▀██▄ ▄██▀██▄   |")
	log.Log().Info("|   ▓▓▓ ███ ███ ███ ███ ███ ███ ███    ▄██▀ ███ ███ ███ ███   |")
	log.Log().Info("|   ███ ███ ███▀▀▀▀ ███ ███  ▀▀▀███   ██▀   ███▀▀▀▀ ███       |")
	log.Log().Info("|   ███ ███ ███ ███ ███ ███ ███ ███ ▄██▀    ███ ███ ███       |")
	log.Log().Info("|   ███▄██▀ ▀██▄██▀ ███ ███ ▀██▄██▀ ███████ ▀██▄██▀ ███       |")
	log.Log().Info(fmt.Sprintf("|   Version: %-14s Build time: %-18s    |", util.Version, util.BuildTime))
	log.Log().Info("|                                                             |")
	log.Log().Info("+-------------------------------------------------------------+")
}
