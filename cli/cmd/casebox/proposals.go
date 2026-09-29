package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/alternayte/casebox/cli/internal/api"
	"github.com/alternayte/casebox/cli/internal/propose"
)

// proposalSummary and proposalDetail are the answers of /api/v1/proposals.
type proposalSummary struct {
	ID           string  `json:"id"`
	Workspace    string  `json:"workspace"`
	PatternTitle string  `json:"patternTitle"`
	Kind         string  `json:"kind"`
	Title        string  `json:"title"`
	Status       string  `json:"status"`
	AppliedMode  *string `json:"appliedMode"`
	Outcome      *struct {
		Before float64 `json:"before"`
		After  float64 `json:"after"`
		Fell   bool    `json:"fell"`
	} `json:"outcome"`
}

type proposalDetail struct {
	Proposal  proposalSummary   `json:"proposal"`
	Rationale string            `json:"rationale"`
	Reason    *string           `json:"reason"`
	Edits     []propose.Edit    `json:"edits"`
	Preview   []propose.Preview `json:"preview"`
	Note      *propose.Note     `json:"note"`
	Evidence  struct {
		Corrections int `json:"corrections"`
		People      int `json:"people"`
		Quotes      []struct {
			Text string `json:"text"`
			Day  string `json:"day"`
		} `json:"quotes"`
	} `json:"evidence"`
}

func newProposalsCommand() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "proposals",
		Short: "List the proposals of this repository's workspace",
		Long: "List the changes Casebox proposes for this workspace: open ones wait for you to approve or reject them,\n" +
			"approved ones wait for casebox apply. --all also lists rejected and applied ones.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			_, cfg, err := repoConfig(ctx)
			if err != nil {
				return err
			}
			client, err := cliClient()
			if err != nil {
				return err
			}
			var list struct {
				Proposals []proposalSummary `json:"proposals"`
				Hidden    int               `json:"hidden"`
			}
			if err := client.Do(ctx, http.MethodGet, "/api/v1/proposals/?workspace="+url.QueryEscape(cfg.Workspace), nil, &list); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			shown := 0
			for _, p := range list.Proposals {
				if !all && p.Status != "open" && p.Status != "approved" {
					continue
				}
				shown++
				fmt.Fprintf(out, "%s  %-8s  %-12s  %s\n", p.ID, p.Status, p.Kind, p.Title)
				fmt.Fprintf(out, "%s  pattern: %s%s\n", strings.Repeat(" ", len(p.ID)), p.PatternTitle, outcomeText(p))
			}
			if shown == 0 {
				fmt.Fprintln(out, "No proposals wait for you. Casebox drafts one when a pattern of corrections appears.")
				return nil
			}
			fmt.Fprintln(out, "\ncasebox proposals show <id> shows one; approve or reject it, then casebox apply <id>.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "also list rejected and applied proposals")

	show := &cobra.Command{
		Use:   "show <id>",
		Short: "Show a proposal: why, the corrections behind it, and the change",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := cliClient()
			if err != nil {
				return err
			}
			d, err := getProposal(cmd.Context(), client, args[0])
			if err != nil {
				return err
			}
			printProposal(cmd.OutOrStdout(), d)
			return nil
		},
	}

	approve := &cobra.Command{
		Use:   "approve <id>",
		Short: "Approve a proposal; casebox apply <id> then writes it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := cliClient()
			if err != nil {
				return err
			}
			if err := client.Do(cmd.Context(), http.MethodPost, "/api/v1/proposals/"+url.PathEscape(args[0])+"/approval", nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Approved. Run casebox apply %s in the repository to write it privately, or with --commit for your team.\n", args[0])
			return nil
		},
	}

	var reason string
	reject := &cobra.Command{
		Use:   "reject <id> --reason <why>",
		Short: "Reject a proposal; the reason keeps the change from coming back for 90 days",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(reason) == "" {
				return errors.New("give a reason with --reason; the next draft reads it")
			}
			client, err := cliClient()
			if err != nil {
				return err
			}
			if err := client.Do(cmd.Context(), http.MethodPost, "/api/v1/proposals/"+url.PathEscape(args[0])+"/rejection", map[string]string{"reason": reason}, nil); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Rejected. Casebox does not propose this change again for 90 days.")
			return nil
		},
	}
	reject.Flags().StringVar(&reason, "reason", "", "why, in one line")

	cmd.AddCommand(show, approve, reject)
	return cmd
}

func getProposal(ctx context.Context, client *api.Client, id string) (proposalDetail, error) {
	var d proposalDetail
	err := client.Do(ctx, http.MethodGet, "/api/v1/proposals/"+url.PathEscape(id), nil, &d)
	return d, err
}

func outcomeText(p proposalSummary) string {
	switch {
	case p.Outcome != nil && p.Outcome.Fell:
		return fmt.Sprintf("; corrections fell from %.1f to %.1f per 100 sessions", p.Outcome.Before, p.Outcome.After)
	case p.Outcome != nil:
		return fmt.Sprintf("; corrections did not fall (%.1f, then %.1f per 100 sessions)", p.Outcome.Before, p.Outcome.After)
	case p.Status == "applied" && p.AppliedMode != nil:
		return "; applied " + *p.AppliedMode + ", checked 30 days after"
	}
	return ""
}

func printProposal(out io.Writer, d proposalDetail) {
	p := d.Proposal
	fmt.Fprintf(out, "%s (%s, %s)\n", p.Title, p.Kind, p.Status)
	fmt.Fprintf(out, "Pattern: %s: %d corrections from %d people.\n", p.PatternTitle, d.Evidence.Corrections, d.Evidence.People)
	for _, q := range d.Evidence.Quotes {
		fmt.Fprintf(out, "  %s  %q\n", q.Day, strings.Join(strings.Fields(q.Text), " "))
	}
	fmt.Fprintf(out, "\nWhy: %s\n", d.Rationale)
	if d.Reason != nil {
		fmt.Fprintf(out, "Rejected: %s\n", *d.Reason)
	}
	if d.Note != nil {
		fmt.Fprintf(out, "\nWhat to change: %s\nWhy: %s\n\nThe prompt casebox apply gives your agent:\n%s\n", d.Note.What, d.Note.Why, d.Note.Prompt)
	}
	for _, pv := range d.Preview {
		fmt.Fprintf(out, "\n--- %s\n", pv.Path)
		fmt.Fprint(out, lineDiff(pv.Before, pv.After))
	}
	switch p.Status {
	case "open":
		fmt.Fprintf(out, "\ncasebox proposals approve %s, or reject %s --reason \"…\"\n", p.ID, p.ID)
	case "approved":
		fmt.Fprintf(out, "\ncasebox apply %s writes it privately; --commit writes it for your team.\n", p.ID)
	}
}

// lineDiff marks the lines after has and before lacks with "+", and the removed ones with "-".
func lineDiff(before *string, after string) string {
	var b strings.Builder
	if before == nil {
		for _, l := range strings.Split(strings.TrimRight(after, "\n"), "\n") {
			b.WriteString("+ " + l + "\n")
		}
		return b.String()
	}
	had := map[string]int{}
	for _, l := range strings.Split(*before, "\n") {
		had[l]++
	}
	has := map[string]int{}
	for _, l := range strings.Split(after, "\n") {
		has[l]++
	}
	for _, l := range strings.Split(*before, "\n") {
		if has[l] == 0 {
			b.WriteString("- " + l + "\n")
		}
	}
	for _, l := range strings.Split(after, "\n") {
		if had[l] == 0 {
			b.WriteString("+ " + l + "\n")
		} else {
			had[l]--
		}
	}
	return b.String()
}
