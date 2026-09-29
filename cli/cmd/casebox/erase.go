package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/spf13/cobra"

	"github.com/alternayte/casebox/cli/internal/api"
	"github.com/alternayte/casebox/cli/internal/config"
)

func newEraseCommand() *cobra.Command {
	var identity string
	cmd := &cobra.Command{
		Use:   "erase --identity <kind:value>",
		Short: "Erase a person everywhere (Admin)",
		Long: "Erase every token a person had in every period: their sessions, trace events and telemetry are deleted,\n" +
			"and their correction text becomes unreadable. Name the identity with its kind, such as email:alice@example.com,\n" +
			"github:alice or jira:alice; the server also erases the identities its roster knows for the same person.\n" +
			"Backups taken before the erasure keep the old keys until they age out.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !strings.Contains(identity, ":") {
				return errors.New("name the identity with its kind, such as --identity email:alice@example.com")
			}
			creds, err := config.LoadCredentials()
			if err != nil {
				return err
			}
			if creds.CLIToken == "" {
				return errors.New("this machine has no CLI login; run casebox init or casebox join")
			}
			var result struct {
				Subjects int `json:"subjects"`
				Sessions int `json:"sessions"`
			}
			client := api.New(creds.Server, creds.CLIToken)
			if err := client.Do(cmd.Context(), http.MethodPost, "/api/v1/privacy/erasures", map[string]string{"identity": identity}, &result); err != nil {
				if api.StatusOf(err) == http.StatusForbidden {
					return errors.New("only an Admin can erase a person")
				}
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Erased %d tokens across all periods and deleted %d sessions.\n", result.Subjects, result.Sessions)
			return nil
		},
	}
	cmd.Flags().StringVar(&identity, "identity", "", "the identity to erase, such as email:alice@example.com")
	_ = cmd.MarkFlagRequired("identity")
	return cmd
}
