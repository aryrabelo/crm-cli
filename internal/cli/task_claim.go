package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/jdanielnd/crm-cli/internal/db/repo"
	"github.com/jdanielnd/crm-cli/internal/format"
	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/spf13/cobra"
)

// Task claims (fork): next, claim, note, wait and release let parallel
// agents split the task list without working the same task twice.

// taskClaimColumns are the list columns plus the claim of each task.
var taskClaimColumns = append(append([]format.ColumnDef{}, taskColumns...),
	format.ColumnDef{Header: "State", Field: "claim_state"},
	format.ColumnDef{Header: "Holder", Field: "claim_holder"},
	format.ColumnDef{Header: "Idle", Field: "claim_idle"},
)

// taskWaitingColumns add the artifact link (list --waiting, wait).
var taskWaitingColumns = append(append([]format.ColumnDef{}, taskClaimColumns...),
	format.ColumnDef{Header: "Link", Field: "claim_link"},
)

// taskWithClaimToMap is taskToMap plus the claim_* keys of c (none when c
// is nil).
func taskWithClaimToMap(t *model.Task, c *model.Claim) map[string]any {
	m := taskToMap(t)
	if c == nil {
		return m
	}
	m["claim_state"] = c.State
	m["claim_updated_at"] = c.UpdatedAt
	m["claim_idle"] = c.Idle
	for key, v := range map[string]*string{
		"claim_holder":          c.Holder,
		"claim_previous_holder": c.PreviousHolder,
		"claim_note":            c.Note,
		"claim_link":            c.Link,
	} {
		if v != nil {
			m[key] = *v
		}
	}
	return m
}

// printStale writes one line per stale hold: the only formatter of the
// stale report (next and list). The note is quoted, so a note with a
// newline stays on its hold's line and cannot forge another one.
func printStale(w io.Writer, holds []model.StaleHold) {
	for _, h := range holds {
		line := fmt.Sprintf("stale: #%d held by %s, idle %s", h.TaskID, h.Holder, h.Idle)
		if h.Note != nil {
			line += ", note: " + strconv.Quote(*h.Note)
		}
		fmt.Fprintln(w, line)
	}
}

// localToday is the local calendar date that bounds next.
func localToday(now time.Time) string { return now.Format("2006-01-02") }

func parseTaskID(arg string) (int64, error) {
	id, err := strconv.ParseInt(arg, 10, 64)
	if err != nil {
		return 0, model.NewExitError(model.ErrValidation, "invalid task ID: %s", arg)
	}
	return id, nil
}

func outputTaskClaim(t *model.Task, c *model.Claim, cols []format.ColumnDef) error {
	return format.Output(os.Stdout, resolveFormat(), []map[string]any{taskWithClaimToMap(t, c)}, cols, flagQuiet)
}

// withClaims runs fn on a TaskClaimRepo of a freshly opened database.
func withClaims(fn func(claims *repo.TaskClaimRepo) error) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	return fn(repo.NewTaskClaimRepo(db))
}

func registerTaskClaimCommands(taskCmd *cobra.Command) {
	taskCmd.AddCommand(taskNextCmd(), taskClaimCmd(), taskNoteCmd(), taskWaitCmd(), taskReleaseCmd())
}

func taskNextCmd() *cobra.Command {
	var as string
	cmd := &cobra.Command{
		Use:   "next",
		Short: "Hold the most urgent free task due today or earlier (or undated)",
		Long: "Picks and holds, in one statement, the first free task of the list order that is due today or\n" +
			"earlier, or undated. Running it again with the same --as returns the task you already hold.\n" +
			"Exits 3 when there is nothing to pick. Stale holds (idle 2h or more) are reported on stderr.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClaims(func(claims *repo.TaskClaimRepo) error {
				now := time.Now()
				task, c, err := claims.Next(cmd.Context(), as, now, localToday(now))
				if errors.Is(err, model.ErrValidation) {
					return err
				}
				// The pick is already committed: a failed stale read is
				// reported, and the pick decides the outcome.
				stale, serr := claims.Stale(cmd.Context(), now)
				if serr != nil {
					fmt.Fprintf(os.Stderr, "stale: report unavailable: %v\n", serr)
				}
				printStale(os.Stderr, stale)
				if errors.Is(err, model.ErrNotFound) {
					if resolveFormat() == format.FormatJSON && !flagQuiet {
						fmt.Fprintln(os.Stdout, "[]")
					}
					return err
				}
				if err != nil {
					return err
				}
				return outputTaskClaim(task, c, taskClaimColumns)
			})
		},
	}
	cmd.Flags().StringVar(&as, "as", "", "your locator: <runtime>:<where>:<session> (required)")
	return cmd
}

func taskClaimCmd() *cobra.Command {
	var as string
	var takeOver bool
	cmd := &cobra.Command{
		Use:   "claim <id>",
		Short: "Hold a chosen task (also future-dated ones); --take-over replaces a stale holder",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseTaskID(args[0])
			if err != nil {
				return err
			}
			return withClaims(func(claims *repo.TaskClaimRepo) error {
				var task *model.Task
				var c *model.Claim
				if takeOver {
					task, c, err = claims.TakeOver(cmd.Context(), id, as, time.Now())
				} else {
					task, c, err = claims.Claim(cmd.Context(), id, as, time.Now())
				}
				if err != nil {
					return err
				}
				return outputTaskClaim(task, c, taskClaimColumns)
			})
		},
	}
	cmd.Flags().StringVar(&as, "as", "", "your locator: <runtime>:<where>:<session> (required)")
	cmd.Flags().BoolVar(&takeOver, "take-over", false, "replace the holder of a hold idle 2h or more (check the holder first)")
	return cmd
}

func taskNoteCmd() *cobra.Command {
	var as string
	cmd := &cobra.Command{
		Use:   "note <id> <text>",
		Short: "Record progress on a task you hold (write one before each external side effect)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseTaskID(args[0])
			if err != nil {
				return err
			}
			return withClaims(func(claims *repo.TaskClaimRepo) error {
				task, c, err := claims.Note(cmd.Context(), id, as, args[1], time.Now())
				if err != nil {
					return err
				}
				return outputTaskClaim(task, c, taskClaimColumns)
			})
		},
	}
	cmd.Flags().StringVar(&as, "as", "", "your locator (required; you must hold the task)")
	return cmd
}

func taskWaitCmd() *cobra.Command {
	var as, link, note string
	cmd := &cobra.Command{
		Use:   "wait <id>",
		Short: "Hand a task you hold to the owner: waiting, with the artifact link",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseTaskID(args[0])
			if err != nil {
				return err
			}
			var notep *string
			if cmd.Flags().Changed("note") {
				notep = &note
			}
			return withClaims(func(claims *repo.TaskClaimRepo) error {
				task, c, err := claims.Wait(cmd.Context(), id, as, link, notep, time.Now())
				if err != nil {
					return err
				}
				return outputTaskClaim(task, c, taskWaitingColumns)
			})
		},
	}
	cmd.Flags().StringVar(&as, "as", "", "your locator (required; you must hold the task)")
	cmd.Flags().StringVar(&link, "link", "", "artifact the owner must act on, e.g. a Gmail draft URL (required)")
	cmd.Flags().StringVar(&note, "note", "", "progress note")
	return cmd
}

func taskReleaseCmd() *cobra.Command {
	var as, note string
	cmd := &cobra.Command{
		Use:   "release <id>",
		Short: "Free a task: a held one by its holder (--as), a waiting one by anyone",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseTaskID(args[0])
			if err != nil {
				return err
			}
			var asp, notep *string
			if cmd.Flags().Changed("as") {
				asp = &as
			}
			if cmd.Flags().Changed("note") {
				notep = &note
			}
			return withClaims(func(claims *repo.TaskClaimRepo) error {
				task, c, err := claims.Release(cmd.Context(), id, asp, notep, time.Now())
				if err != nil {
					return err
				}
				return outputTaskClaim(task, c, taskClaimColumns)
			})
		},
	}
	cmd.Flags().StringVar(&as, "as", "", "your locator (required to release a held task)")
	cmd.Flags().StringVar(&note, "note", "", "note for the next agent (e.g. why a draft was rejected)")
	return cmd
}
