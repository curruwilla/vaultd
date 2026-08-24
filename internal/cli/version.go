package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/curruwilla/vaultd/internal/buildinfo"
)

func newVersionCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the vaultd version",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			fmt.Fprintln(g.out, buildinfo.String())
			return nil
		},
	}
}
