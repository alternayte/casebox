package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// newDocsCommand writes the CLI reference page of the docs site from the command tree, so the
// reference cannot go stale (checks/docs-generated.sh runs it and compares).
func newDocsCommand() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:    "docs --out <file>",
		Short:  "Write the CLI reference page of the docs site",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f, err := os.Create(out)
			if err != nil {
				return err
			}
			defer f.Close()
			return writeReference(f, cmd.Root())
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "the Markdown file to write")
	_ = cmd.MarkFlagRequired("out")
	return cmd
}

func writeReference(w io.Writer, root *cobra.Command) error {
	fmt.Fprint(w, `---
title: CLI
description: Every casebox command, its flags and what it does, generated from the CLI itself.
---

This page lists every `+"`casebox`"+` command with its flags. It is generated from the CLI's own help, so it matches the version you run: `+"`casebox <command> --help`"+` shows the same text.

`)
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		children := c.Commands()
		sort.Slice(children, func(i, j int) bool { return children[i].Name() < children[j].Name() })
		for _, sub := range children {
			if sub.Hidden || !sub.IsAvailableCommand() || sub.Name() == "help" || sub.Name() == "completion" {
				continue
			}
			fmt.Fprintf(w, "## %s\n\n", sub.CommandPath())
			long := strings.TrimSpace(sub.Long)
			if long == "" {
				long = sub.Short
			}
			fmt.Fprintf(w, "%s\n\n", long)
			fmt.Fprintf(w, "```text\n%s\n```\n\n", sub.UseLine())
			if flags := strings.TrimRight(sub.NonInheritedFlags().FlagUsages(), "\n"); flags != "" {
				fmt.Fprintf(w, "```text\n%s\n```\n\n", flags)
			}
			walk(sub)
		}
	}
	walk(root)
	return nil
}
