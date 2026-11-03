package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/mia0x75/denyzer/auth"
	"github.com/mia0x75/denyzer/config"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func newAuthCommand() *cobra.Command {
	c := &cobra.Command{
		Use:          "auth --username admin",
		Short:        "set or reset the single administrator's password",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			username, err := cmd.Flags().GetString("username")
			if err != nil {
				return err
			}
			if err := config.ValidateUsername(username); err != nil {
				return err
			}

			fromStdin, err := cmd.Flags().GetBool("password-stdin")
			if err != nil {
				return err
			}
			password, err := readAuthPassword(cmd, fromStdin)
			defer clear(password)
			if err != nil {
				return err
			}

			hash, err := auth.HashPassword(password)
			if err != nil {
				return err
			}
			resolveConfigPath()
			if err := config.SetAuth(configPath, config.Auth{Username: username, PasswordHash: hash}); err != nil {
				return fmt.Errorf("can't update administrator credentials: %w", err)
			}

			_, err = fmt.Fprintln(
				cmd.OutOrStdout(),
				"Administrator credentials updated; existing sessions are revoked on their next request.",
			)

			return err
		},
	}
	c.Flags().String("username", "", "administrator username")
	c.Flags().Bool("password-stdin", false, "read one password line from stdin instead of prompting twice")
	_ = c.MarkFlagRequired("username")

	return c
}

func readAuthPassword(cmd *cobra.Command, fromStdin bool) ([]byte, error) {
	if fromStdin {
		const passwordReadLimit = auth.MaxPasswordBytes + 3 // CRLF plus one byte to detect overflow.
		password, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), passwordReadLimit))
		if err != nil {
			clear(password)

			return nil, err
		}
		password = bytes.TrimSuffix(password, []byte("\n"))
		password = bytes.TrimSuffix(password, []byte("\r"))
		if bytes.ContainsAny(password, "\r\n") {
			clear(password)

			return nil, errors.New("password-stdin requires one password line")
		}

		return password, nil
	}

	input, ok := cmd.InOrStdin().(*os.File)
	if !ok || !term.IsTerminal(int(input.Fd())) {
		return nil, errors.New("interactive password input requires a terminal; use --password-stdin for automation")
	}

	return promptAuthPassword(cmd.ErrOrStderr(), func() ([]byte, error) {
		return term.ReadPassword(int(input.Fd()))
	})
}

func promptAuthPassword(output io.Writer, read func() ([]byte, error)) ([]byte, error) {
	if _, err := fmt.Fprint(output, "Password: "); err != nil {
		return nil, err
	}
	password, err := read()
	if err != nil {
		clear(password)

		return nil, err
	}
	if _, err := fmt.Fprint(output, "\nConfirm password: "); err != nil {
		clear(password)

		return nil, err
	}
	confirmation, err := read()
	defer clear(confirmation)
	_, _ = fmt.Fprintln(output)
	if err != nil {
		clear(password)

		return nil, err
	}
	if !bytes.Equal(password, confirmation) {
		clear(password)

		return nil, errors.New("passwords do not match")
	}

	return password, nil
}
