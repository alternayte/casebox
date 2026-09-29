package main

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/spf13/cobra"
)

func newTokenCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "token", Short: "Issue tokens for workers, ingest and harness CI (admin)"}
	var kind, name string
	create := &cobra.Command{
		Use:   "create --kind worker|ingest|ci --name <name>",
		Short: "Issue a token; the secret is shown once",
		Long: "Issue a token for this organisation. A worker token runs casebox worker; an ingest token sends OTel data;\n" +
			"a ci token runs casebox ci in a GitHub Action (CASEBOX_TOKEN) and its inline worker. The server keeps only its\n" +
			"hash, so the secret is shown once: put it in a secret store now.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if kind != "worker" && kind != "ingest" && kind != "ci" {
				return fmt.Errorf("--kind %s: use worker, ingest or ci", kind)
			}
			if strings.TrimSpace(name) == "" {
				return fmt.Errorf("name the token with --name, such as \"harness CI for acme/payments\"")
			}
			client, err := cliClient()
			if err != nil {
				return err
			}
			var issued struct {
				Secret string `json:"secret"`
			}
			if err := client.Do(cmd.Context(), http.MethodPost, "/api/v1/tokens", map[string]string{"kind": kind, "name": name}, &issued); err != nil {
				return fmt.Errorf("issue the token: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), issued.Secret)
			return nil
		},
	}
	create.Flags().StringVar(&kind, "kind", "", "worker, ingest or ci")
	create.Flags().StringVar(&name, "name", "", "what the token is for")
	cmd.AddCommand(create)
	return cmd
}
