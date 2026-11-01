package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
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
	done              = make(chan bool, 1)
	isConfigMandatory = true
	signals           = make(chan os.Signal, 1)

	// raiseNetBindService is a seam so tests can stub the capability raise.
	raiseNetBindService = util.RaiseNetBindService
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

// privilegedPortCapHint describes how to satisfy the CAP_NET_BIND_SERVICE
// requirement for binding ports below 1024.
const privilegedPortCapHint = "grant CAP_NET_BIND_SERVICE (Kubernetes " +
	"securityContext capabilities.add, or docker run --cap-add NET_BIND_SERVICE) " +
	"or use a port >= 1024"

// warnMissingPrivilegedPortCapability raises CAP_NET_BIND_SERVICE if it is
// available, and warns when a privileged port (< 1024) is configured but the
// capability could not be obtained. It never fails: any real bind error
// surfaces later from server.NewServer.
//
// The capability is only required for privileged ports, so configurations
// without them neither raise it nor warn about a failed raise: raising it can
// fail on its own (e.g. in cgo builds, where promoting a capability across all
// threads is unsupported) without affecting an unprivileged listener.
func warnMissingPrivilegedPortCapability(ports config.Ports) {
	privileged := ports.PrivilegedPorts()
	if len(privileged) == 0 {
		return
	}

	effective, err := raiseNetBindService()
	if err != nil {
		log.Log().Warnf("could not adjust process capabilities (%v); binding "+
			"privileged port(s) %s may fail — %s",
			err, strings.Join(privileged, ", "), privilegedPortCapHint)

		return
	}

	if effective {
		return
	}

	log.Log().Warnf("configured to listen on privileged port(s) %s without "+
		"CAP_NET_BIND_SERVICE; %s", strings.Join(privileged, ", "), privilegedPortCapHint)
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

	go func() {
		select {
		case <-signals:
			log.Log().Infof("Terminating...")

			// Cancel background operations (periodic refresh, etc.)
			cancelFn()

			// Create timeout context for graceful shutdown
			stopCtx, stopCancel := context.WithTimeout(context.Background(), shutdownTimeout)
			defer stopCancel()

			util.LogOnError(stopCtx, "can't stop server: ", srv.Stop(stopCtx))
			done <- true

		case err := <-errChan:
			log.Log().Error("server start failed: ", err)
			terminationErr = err
			done <- true
		}
	}()

	evt.Bus().Publish(evt.ApplicationStarted, util.Version, util.BuildTime)
	<-done

	return terminationErr
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
	log.Log().Infof("|   Version: %-14s Build time: %-18s    |", util.Version, util.BuildTime)
	log.Log().Info("|                                                             |")
	log.Log().Info("+-------------------------------------------------------------+")
}
