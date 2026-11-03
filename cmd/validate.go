package cmd

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/mia0x75/denyzer/config"
	"github.com/mia0x75/denyzer/log"

	"github.com/spf13/cobra"
)

const validateCmdName = "validate"

// NewValidateCommand creates new command instance
func NewValidateCommand() *cobra.Command {
	return &cobra.Command{
		Use:   validateCmdName,
		Args:  cobra.NoArgs,
		Short: "Validates the configuration",
		RunE:  validateConfiguration,
	}
}

func validateConfiguration(_ *cobra.Command, _ []string) error {
	log.Log().Info("Validating configuration file: " + configPath)

	_, err := os.Stat(configPath)
	if err != nil && errors.Is(err, os.ErrNotExist) {
		return errors.New("configuration path does not exist")
	}

	cfg, err := loadConfig(false, func(cfg *config.Config) {
		logCfg := commandLogConfig(cfg)

		// validate reports what loading the config logs, so log.level must not hide it
		logCfg.Level = min(logCfg.Level, log.Level(slog.LevelInfo))

		log.Configure(&logCfg)
	})
	if err != nil {
		return fmt.Errorf("unable to load configuration file '%s': %w", configPath, err)
	}
	if err := cfg.ValidateAdminListeners(log.Log()); err != nil {
		return fmt.Errorf("invalid configuration file '%s': %w", configPath, err)
	}

	log.Log().Info("Configuration is valid")

	return nil
}
