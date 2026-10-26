package repo_test

import (
	"context"
	"testing"

	"github.com/jdanielnd/crm-cli/internal/db/repo"
	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImportBatchCreateFind(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	r := repo.NewImportRepo(d)

	key := "email"
	b := &model.ImportBatch{
		EntityType: "person",
		Source:     "/data/leads.csv",
		Columns: []model.ImportColumn{
			{Header: "Nome", Field: "name"},
			{Header: "email", Field: "email"},
			{Header: "Cidade", Field: "Cidade", Custom: true},
		},
		KeyColumn:       &key,
		HasBOM:          true,
		CRLF:            true,
		TrailingNewline: false,
		RowCount:        2,
	}
	require.NoError(t, r.CreateBatch(ctx, b))
	assert.NotZero(t, b.ID)

	got, err := r.FindBatch(ctx, b.ID)
	require.NoError(t, err)
	assert.Equal(t, b.Columns, got.Columns)
	assert.Equal(t, "email", *got.KeyColumn)
	assert.True(t, got.HasBOM)
	assert.True(t, got.CRLF)
	assert.False(t, got.TrailingNewline)
	assert.Equal(t, 2, got.RowCount)

	_, err = r.FindBatch(ctx, 999)
	assert.ErrorIs(t, err, model.ErrNotFound)
}

func TestImportCreateUpdateFindEntity(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	r := repo.NewImportRepo(d)

	last := ""
	email := "bia@x.com"
	id, err := r.CreateEntity(ctx, "person", map[string]*string{"first_name": ptr("Bia"), "last_name": &last, "email": &email})
	require.NoError(t, err)

	ids, err := r.FindEntityIDsByCore(ctx, "person", "name", "Bia ")
	require.NoError(t, err)
	assert.Equal(t, []int64{id}, ids)
	ids, err = r.FindEntityIDsByCore(ctx, "person", "email", "bia@x.com")
	require.NoError(t, err)
	assert.Equal(t, []int64{id}, ids)

	require.NoError(t, r.UpdateEntity(ctx, "person", id, map[string]*string{"email": nil, "phone": ptr("123")}))
	p, err := repo.NewPersonRepo(d).FindByID(ctx, id)
	require.NoError(t, err)
	assert.Nil(t, p.Email)
	assert.Equal(t, "123", *p.Phone)
	assert.Equal(t, "", *p.LastName)

	orgID, err := r.CreateEntity(ctx, "organization", map[string]*string{"name": nil, "domain": ptr("acme.com")})
	require.NoError(t, err)
	o, err := repo.NewOrgRepo(d).FindByID(ctx, orgID)
	require.NoError(t, err)
	assert.Equal(t, "", o.Name)
}

func TestImportRepoRejectsUnknownFields(t *testing.T) {
	r := repo.NewImportRepo(openTestDB(t))
	ctx := context.Background()
	_, err := r.CreateEntity(ctx, "person", map[string]*string{"password": ptr("x")})
	assert.ErrorIs(t, err, model.ErrValidation)
	_, err = r.CreateEntity(ctx, "deal", map[string]*string{})
	assert.ErrorIs(t, err, model.ErrValidation)
	_, err = r.FindEntityIDsByCore(ctx, "person", "id; DROP TABLE people", "x")
	assert.ErrorIs(t, err, model.ErrValidation)
	assert.ErrorIs(t, r.UpdateEntity(ctx, "person", 42, map[string]*string{"email": ptr("a")}), model.ErrNotFound)
}

func TestImportBatchRowsOrderAndArchived(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	r := repo.NewImportRepo(d)
	cf := repo.NewCustomFieldRepo(d)

	b := &model.ImportBatch{EntityType: "person", Source: "-", Columns: []model.ImportColumn{{Header: "n", Field: "first_name"}}, TrailingNewline: true}
	require.NoError(t, r.CreateBatch(ctx, b))
	var ids []int64
	for i, name := range []string{"C", "A", "B"} {
		id, err := r.CreateEntity(ctx, "person", map[string]*string{"first_name": ptr(name)})
		require.NoError(t, err)
		ids = append(ids, id)
		require.NoError(t, cf.Set(ctx, "person", id, "pos", name))
		// store rows in reverse position order to prove ORDER BY position
		require.NoError(t, r.AddRow(ctx, b.ID, 3-i, id))
	}
	require.NoError(t, repo.NewPersonRepo(d).Archive(ctx, ids[1]))

	rows, err := r.BatchRows(ctx, b)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "B", *rows[0].Core["first_name"])
	assert.Equal(t, "C", rows[1].Custom["pos"])

	all, keys, err := r.ListEntityRows(ctx, "person")
	require.NoError(t, err)
	assert.Len(t, all, 2)
	assert.Equal(t, []string{"pos"}, keys)
}
