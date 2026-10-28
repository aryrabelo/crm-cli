package cli

import (
	"fmt"
	"os"

	"github.com/jdanielnd/crm-cli/internal/format"
	"github.com/jdanielnd/crm-cli/internal/service"
	"github.com/spf13/cobra"
)

var pushColumns = []format.ColumnDef{
	{Header: "Row", Field: "row"},
	{Header: "Action", Field: "action"},
	{Header: "ID", Field: "id"},
	{Header: "Key", Field: "key"},
	{Header: "Matched", Field: "matched"},
	{Header: "Score", Field: "score"},
	{Header: "Error", Field: "error"},
}

func registerPushCommand(rootCmd *cobra.Command) {
	rootCmd.AddCommand(pushCmd())
}

func pushCmd() *cobra.Command {
	var to, dedupe string
	var only, maps []string
	var threshold float64
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "push <table>",
		Short: "Turn table rows into people or organizations, deduplicated",
		Long: `Turn the rows of a table into people (--to person) or organizations
(--to org). Columns map to fields as in crm import (name, email, company,
domain... aliases; other columns become custom fields; --map overrides).

Each row is matched against existing entities and the rows pushed before it:
  email   same normalized email (people; the default for --to person)
  domain  same domain, from a domain, URL or email (orgs; the default for --to org)
  fuzz    most similar normalized name at or above --threshold
          (accents, punctuation and Inc/LLC/Ltda... ignored)
A match keeps its values: the row only fills empty fields and missing custom
fields. Rows without a name or dedupe key are reported and skipped.
--dry-run prints the plan and writes nothing.`,
		Example: `  crm push leads --to person --dry-run
  crm push leads --to org --dedupe fuzz --only "qualified=yes"
  crm push leads --to person --map handle=email`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := openDB()
			if err != nil {
				return err
			}
			defer db.Close()
			res, err := newService(db).Push(cmd.Context(), service.PushOptions{Table: args[0], To: to, Dedupe: dedupe,
				Only: only, Maps: maps, Threshold: threshold, DryRun: dryRun})
			if err != nil {
				return err
			}
			verb := "pushed"
			if res.DryRun {
				verb = "dry run:"
			}
			fmt.Fprintf(os.Stderr, "%s %d rows of %s to %s (dedupe %s): %d created, %d updated, %d merged, %d failed\n",
				verb, res.Rows, res.Table, res.To, res.Dedupe, res.Created, res.Updated, res.Merged, res.Failed)
			data := make([]map[string]any, len(res.Results))
			for i, r := range res.Results {
				data[i] = map[string]any{"row": r.Row, "action": r.Action, "key": r.Key,
					"matched": r.Matched, "score": r.Score, "error": r.Error}
				if r.ID != 0 { // failed rows and dry-run creates have no entity yet; --quiet prints real ids only
					data[i]["id"] = r.ID
				}
			}
			return format.Output(os.Stdout, resolveFormat(), data, pushColumns, flagQuiet)
		},
	}
	f := cmd.Flags()
	f.StringVar(&to, "to", "", "person or org (required)")
	f.StringVar(&dedupe, "dedupe", "", "email, domain or fuzz (default: email for people, domain for orgs)")
	f.StringArrayVar(&only, "only", nil, onlyHelp)
	f.StringArrayVar(&maps, "map", nil, "column=field mapping override (repeatable)")
	f.Float64Var(&threshold, "threshold", service.DefaultFuzzThreshold, "fuzz: name similarity cut-off between 0 and 1")
	f.BoolVar(&dryRun, "dry-run", false, "report creates, updates and merges; write nothing")
	_ = cmd.MarkFlagRequired("to")
	return cmd
}
