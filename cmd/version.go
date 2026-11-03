package cmd

import (
	"fmt"

	"github.com/mia0x75/denyzer/util"
	"github.com/spf13/cobra"
)

// NewVersionCommand creates new command instance
func NewVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Args:  cobra.NoArgs,
		Short: "Print the version number of denyzer",
		RunE:  printVersion,
	}
}

func printVersion(cmd *cobra.Command, _ []string) error {
	_, err := fmt.Fprintf(
		cmd.OutOrStdout(),
		"Version: %s\nBuild time: %s\nArchitecture: %s\n",
		util.Version,
		util.BuildTime,
		util.Architecture,
	)

	return err
}
