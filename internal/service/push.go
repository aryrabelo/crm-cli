package service

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jdanielnd/crm-cli/internal/db/repo"
	"github.com/jdanielnd/crm-cli/internal/importer"
	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/jdanielnd/crm-cli/internal/value"
)

// Push dedupe modes.
const (
	DedupeEmail  = "email"
	DedupeDomain = "domain"
	DedupeFuzz   = "fuzz"
)

// DefaultFuzzThreshold is the name similarity (0–1) above which fuzz dedupe
// treats two names as the same entity.
const DefaultFuzzThreshold = 0.88

// Push row actions.
const (
	PushCreate = "create" // a new entity
	PushUpdate = "update" // matched an entity that existed before the push
	PushMerge  = "merge"  // matched an entity created earlier in the same push
	PushError  = "error"  // not written
)

// PushOptions configures Push.
type PushOptions struct {
	Table string
	// To is person or org.
	To string
	// Dedupe is email, domain or fuzz (default: email for people, domain
	// for orgs).
	Dedupe string
	// Only restricts the push to rows matching every filter.
	Only []string
	// Maps holds column=field overrides, as in crm import --map.
	Maps []string
	// Threshold is the fuzz similarity cut-off, 0 included (nil: DefaultFuzzThreshold).
	Threshold *float64
	// DryRun reports what would happen and writes nothing.
	DryRun bool
}

// PushRow is what Push did (or would do) with one table row.
type PushRow struct {
	// Row is the table row position.
	Row    int    `json:"row"`
	Action string `json:"action"`
	// ID is the person/org written; 0 for an entity a dry run would create.
	ID int64 `json:"id,omitempty"`
	// Key is the normalized dedupe key of the row.
	Key string `json:"key,omitempty"`
	// Matched is the key of the entity a fuzz match chose, Score its similarity.
	Matched string  `json:"matched,omitempty"`
	Score   float64 `json:"score,omitempty"`
	Error   string  `json:"error,omitempty"`
}

// PushResult summarizes a push.
type PushResult struct {
	Table   string    `json:"table"`
	To      string    `json:"to"`
	Dedupe  string    `json:"dedupe"`
	DryRun  bool      `json:"dry_run"`
	Rows    int       `json:"rows"`
	Created int       `json:"created"`
	Updated int       `json:"updated"`
	Merged  int       `json:"merged"`
	Failed  int       `json:"failed"`
	Results []PushRow `json:"results"`
}

// pushTarget resolves the entity type and default dedupe mode of --to.
func pushTarget(to string) (entity, dedupe string, err error) {
	switch to {
	case "person":
		return "person", DedupeEmail, nil
	case "org", "organization":
		return "organization", DedupeDomain, nil
	default:
		return "", "", model.NewExitError(model.ErrValidation, "--to must be person or org, not %q", to)
	}
}

// pushed is an entity a push row can merge into.
type pushed struct {
	id      int64
	core    map[string]*string
	custom  map[string]string
	key     string
	created bool
}

// Push turns table rows into people or organizations, deduplicating by email,
// domain or fuzzy name against existing entities and earlier rows of the
// same push. Columns map to entity fields as in crm import (aliases, then
// custom fields; --map overrides). A matched entity keeps its values: the
// row only fills empty core fields and custom fields it does not have yet.
// Rows without a name or dedupe key are reported as errors and skipped.
func (s *Service) Push(ctx context.Context, opts PushOptions) (*PushResult, error) {
	entity, defDedupe, err := pushTarget(opts.To)
	if err != nil {
		return nil, err
	}
	if opts.Dedupe == "" {
		opts.Dedupe = defDedupe
	}
	switch {
	case opts.Dedupe == DedupeEmail && entity != "person":
		return nil, model.NewExitError(model.ErrValidation, "--dedupe email needs --to person (organizations have no email)")
	case opts.Dedupe == DedupeDomain && entity != "organization":
		return nil, model.NewExitError(model.ErrValidation, "--dedupe domain needs --to org (people have no domain)")
	case opts.Dedupe != DedupeEmail && opts.Dedupe != DedupeDomain && opts.Dedupe != DedupeFuzz:
		return nil, model.NewExitError(model.ErrValidation, "--dedupe must be email, domain or fuzz, not %q", opts.Dedupe)
	}
	threshold := DefaultFuzzThreshold
	if opts.Threshold != nil {
		threshold = *opts.Threshold
	}
	if threshold < 0 || threshold > 1 {
		return nil, model.NewExitError(model.ErrValidation, "--threshold must be between 0 and 1")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	ws, view, err := loadView(ctx, tx, opts.Table, opts.Only)
	if err != nil {
		return nil, err
	}
	header := view.Header()
	cols, err := importer.ResolveColumns(entity, header, opts.Maps)
	if err != nil {
		return nil, err
	}
	keyField := map[string]string{DedupeEmail: "email", DedupeDomain: "domain", DedupeFuzz: "name"}[opts.Dedupe]
	if opts.Dedupe != DedupeFuzz && !slices.ContainsFunc(cols, func(c model.ImportColumn) bool { return !c.Custom && c.Field == keyField }) {
		return nil, model.NewExitError(model.ErrValidation, "no column of table %s maps to %s; use --map <column>=%s", ws.Name, keyField, keyField)
	}

	imports := repo.NewImportRepo(tx)
	customs := repo.NewCustomFieldRepo(tx)
	existing, _, err := imports.ListEntityRows(ctx, entity)
	if err != nil {
		return nil, err
	}
	idx := &dedupeIndex{mode: opts.Dedupe, entity: entity, threshold: threshold, byKey: map[string]*pushed{}}
	for _, e := range existing {
		custom := e.Custom
		if custom == nil {
			custom = map[string]string{}
		}
		idx.add(&pushed{id: e.ID, core: e.Core, custom: custom, key: idx.key(e.Core)})
	}

	res := &PushResult{Table: ws.Name, To: opts.To, Dedupe: opts.Dedupe, DryRun: opts.DryRun, Rows: len(view.Rows), Results: []PushRow{}}
	for i, cells := range view.Cells {
		pr := PushRow{Row: view.Rows[i].Position}
		core, custom := pushFields(entity, cols, header, cells)
		pr.Key = idx.key(core)
		switch {
		case !hasName(entity, core):
			pr.Action, pr.Error = PushError, "row has no name"
		case pr.Key == "":
			pr.Action, pr.Error = PushError, "row has no "+keyField+" to dedupe on"
		}
		if pr.Action == PushError {
			res.Failed++
			res.Results = append(res.Results, pr)
			continue
		}
		match, score := idx.find(pr.Key)
		if match == nil {
			id, err := imports.CreateEntity(ctx, entity, core)
			if err != nil {
				return nil, fmt.Errorf("row %d: %w", pr.Row, err)
			}
			if err := setCustom(ctx, customs, entity, id, custom, nil); err != nil {
				return nil, fmt.Errorf("row %d: %w", pr.Row, err)
			}
			idx.add(&pushed{id: id, core: maps.Clone(core), custom: custom, key: pr.Key, created: true})
			pr.Action, pr.ID = PushCreate, id
			res.Created++
		} else {
			fill := map[string]*string{}
			for f, v := range core {
				if cur := match.core[f]; cur == nil || *cur == "" {
					fill[f] = v
					match.core[f] = v
				}
			}
			if len(fill) > 0 {
				if err := imports.UpdateEntity(ctx, entity, match.id, fill); err != nil {
					return nil, fmt.Errorf("row %d: %w", pr.Row, err)
				}
			}
			if err := setCustom(ctx, customs, entity, match.id, custom, match.custom); err != nil {
				return nil, fmt.Errorf("row %d: %w", pr.Row, err)
			}
			pr.ID = match.id
			if opts.Dedupe == DedupeFuzz && match.key != pr.Key {
				pr.Matched, pr.Score = match.key, score
			}
			if match.created {
				pr.Action = PushMerge
				res.Merged++
			} else {
				pr.Action = PushUpdate
				res.Updated++
			}
		}
		if opts.DryRun && (pr.Action == PushCreate || (pr.Action == PushMerge && match.created)) {
			pr.ID = 0
		}
		res.Results = append(res.Results, pr)
	}
	if !opts.DryRun {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit: %w", err)
		}
	}
	return res, nil
}

// pushFields maps one row's cells to core and custom fields; empty cells
// are left out.
func pushFields(entity string, cols []model.ImportColumn, header []string, cells map[string]any) (map[string]*string, map[string]string) {
	core := map[string]*string{}
	custom := map[string]string{}
	for j, c := range cols {
		v := strings.TrimSpace(value.Text(cells[header[j]]))
		if v == "" {
			continue
		}
		switch {
		case c.Custom:
			custom[c.Field] = v
		case c.Field == "name" && entity == "person":
			first, last := importer.SplitName(v)
			core["first_name"] = &first
			if last != nil {
				core["last_name"] = last
			}
		default:
			core[c.Field] = &v
		}
	}
	return core, custom
}

func hasName(entity string, core map[string]*string) bool {
	f := "first_name"
	if entity == "organization" {
		f = "name"
	}
	return core[f] != nil && *core[f] != ""
}

// setCustom sets the fields of entity id that are not in have (nil: none).
func setCustom(ctx context.Context, customs *repo.CustomFieldRepo, entity string, id int64, fields, have map[string]string) error {
	for _, k := range slices.Sorted(maps.Keys(fields)) {
		if _, ok := have[k]; ok {
			continue
		}
		if err := customs.Set(ctx, entity, id, k, fields[k]); err != nil {
			return err
		}
		if have != nil {
			have[k] = fields[k]
		}
	}
	return nil
}
