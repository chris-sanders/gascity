package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/runtime/k8s"
)

func newStartupNudgeCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "startup-nudge",
		Short: "inspect and resolve durable startup prompt deliveries",
	}
	var cityPath, intentID, incarnation, decision string
	resolve := &cobra.Command{
		Use:   "resolve",
		Short: "confirm delivery or explicitly authorize one retry for an immutable intent",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			status, err := k8s.ResolveStartupNudge(cityPath, intentID, incarnation, decision)
			if err != nil {
				fmt.Fprintf(stderr, "gc startup-nudge resolve: %v\n", err) //nolint:errcheck
				return errExit
			}
			fmt.Fprintf(stdout, "intent\t%s\nstatus\t%s\n", intentID, status) //nolint:errcheck
			return nil
		},
	}
	resolve.Flags().StringVar(&cityPath, "city-path", "", "owning city directory")
	resolve.Flags().StringVar(&intentID, "intent", "", "immutable 128-bit startup intent ID")
	resolve.Flags().StringVar(&incarnation, "incarnation", "", "exact session incarnation reported by gc doctor")
	resolve.Flags().StringVar(&decision, "decision", "", "delivered or retry (retry asserts the prior prompt was not consumed)")
	for _, flag := range []string{"city-path", "intent", "incarnation", "decision"} {
		_ = resolve.MarkFlagRequired(flag)
	}
	cmd.AddCommand(resolve)
	return cmd
}
