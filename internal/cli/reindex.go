package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/curruwilla/vaultd/internal/app"
)

func newReindexCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "reindex <target>",
		Short: "Rebuild the listing index from the manifests in the bucket",
		Long: "reindex reads every manifest under the target's prefix and writes the index\n" +
			"from what it finds. The bucket is the source of truth, so this repairs an\n" +
			"index that was lost, truncated or left behind by an interrupted run.\n\n" +
			"Records of failed runs live only in the index, so a rebuild does not bring\n" +
			"them back.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, target, err := g.targetFromConfig(args[0])
			if err != nil {
				return err
			}

			idx, err := app.New(cfg, g.logger).Index(cmd.Context(), target)
			if err != nil {
				return err
			}

			entries, err := idx.Rebuild(cmd.Context())
			if err != nil {
				return err
			}
			if err := idx.Replace(cmd.Context(), entries); err != nil {
				return err
			}

			fmt.Fprintf(g.out, "ok: rebuilt %s from %s\n", idx.Key(), plural(len(entries), "manifest", "manifests"))
			return nil
		},
	}
}
