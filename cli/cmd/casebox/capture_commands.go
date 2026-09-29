package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/alternayte/casebox/cli/internal/config"
	"github.com/alternayte/casebox/cli/internal/repo"
)

func newPauseCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "pause",
		Short: "Stop capture on this machine until casebox resume",
		Long:  "Stop capture on this machine at once, for every repository. No reason is needed, and nobody is told.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := config.SetPaused(true); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Capture is paused on this machine. Run casebox resume to start it again.")
			return nil
		},
	}
}

func newResumeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "resume",
		Short: "Start capture again on this machine",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := config.SetPaused(false); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Capture is on again for the enrolled repositories.")
			return nil
		},
	}
}

var workItemKey = regexp.MustCompile(`^([A-Z][A-Z0-9]+-[0-9]+|#?[0-9]+)$`)

func newLinkCommand() *cobra.Command {
	var clear bool
	cmd := &cobra.Command{
		Use:   "link <work item>",
		Short: "Link the current and next sessions in this repository to a work item",
		Long: "Link the sessions in this repository to a work item, such as PAY-123 or #42, until you link another one or run casebox link --clear.\n" +
			"An explicit link is the strongest signal: sessions linked this way can become cases.",
		Args: func(cmd *cobra.Command, args []string) error {
			if clear {
				return cobra.NoArgs(cmd, args)
			}
			return cobra.ExactArgs(1)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			root, _, err := repo.LoadConfig(cmd.Context(), cwd)
			if err != nil {
				return err
			}
			if clear {
				if err := config.SetLink(root, ""); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "Sessions in this repository are no longer linked to a work item.")
				return nil
			}
			item := strings.TrimSpace(args[0])
			if !workItemKey.MatchString(item) {
				return fmt.Errorf("%q is not a work item key; use a Jira key such as PAY-123 or an issue number such as #42", item)
			}
			if err := config.SetLink(root, item); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Sessions in this repository now belong to %s.\n", item)
			return nil
		},
	}
	cmd.Flags().BoolVar(&clear, "clear", false, "remove the link")
	return cmd
}
