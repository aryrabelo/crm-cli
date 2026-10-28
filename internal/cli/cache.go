package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/jdanielnd/crm-cli/internal/format"
	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/jdanielnd/crm-cli/internal/waterfall"
	"github.com/spf13/cobra"
)

func registerCacheCommands(rootCmd *cobra.Command) {
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "Manage the enrichment cache (provider answers)",
	}
	cmd.AddCommand(cacheClearCmd())
	rootCmd.AddCommand(cmd)
}

var cacheClearColumns = []format.ColumnDef{
	{Header: "Provider", Field: "provider"},
	{Header: "Older Than", Field: "older_than"},
	{Header: "Deleted", Field: "deleted"},
}

func cacheClearCmd() *cobra.Command {
	var providerName, olderThan string
	cmd := &cobra.Command{
		Use:   "clear",
		Short: "Delete cached provider answers",
		Long: `Delete cached provider answers, so the next run calls (and pays) the
provider again. Without flags every entry is deleted; --provider keeps the
other providers' entries, --older-than keeps the entries fetched recently.
Entries older than a provider's --cache-ttl (default 30d) are already ignored
by runs; clearing them only reclaims space.`,
		Example: `  crm cache clear                       # everything
  crm cache clear --provider clearbit   # one provider
  crm cache clear --older-than 7d       # entries fetched more than 7 days ago`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var age time.Duration
			if olderThan != "" {
				var err error
				if age, err = provider.ParseAge(olderThan); err != nil {
					return model.NewExitError(model.ErrValidation, "--older-than: %s", err)
				}
			}
			db, err := openDB()
			if err != nil {
				return err
			}
			defer db.Close()
			if providerName != "" {
				if _, err := provider.NewStore(db).Get(cmd.Context(), providerName); err != nil {
					return err
				}
			}
			n, err := waterfall.NewCache(db).Clear(cmd.Context(), providerName, age)
			if err != nil {
				return err
			}
			if !flagQuiet {
				fmt.Fprintf(os.Stderr, "Deleted %d cached answer(s)\n", n)
			}
			data := map[string]any{"provider": providerName, "older_than": olderThan, "deleted": n}
			return format.Output(os.Stdout, resolveFormat(), []map[string]any{data}, cacheClearColumns, flagQuiet)
		},
	}
	cmd.Flags().StringVar(&providerName, "provider", "", "only this provider's entries")
	cmd.Flags().StringVar(&olderThan, "older-than", "", "only entries fetched more than this long ago (e.g. 7d, 12h)")
	return cmd
}
