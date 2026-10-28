package repo_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/jdanielnd/crm-cli/internal/db"
	"github.com/jdanielnd/crm-cli/internal/db/repo"
	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })
	return d
}

func TestCustomFieldSetGetDelete(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p, err := repo.NewPersonRepo(d).Create(ctx, model.CreatePersonInput{FirstName: "Jane"})
	require.NoError(t, err)

	cf := repo.NewCustomFieldRepo(d)
	require.NoError(t, cf.Set(ctx, "person", p.ID, "Cidade", "Recife"))
	require.NoError(t, cf.Set(ctx, "person", p.ID, "Origem", "evento"))
	require.NoError(t, cf.Set(ctx, "person", p.ID, "Cidade", "Natal")) // upsert keeps position

	fields, err := cf.GetForEntity(ctx, "person", p.ID)
	require.NoError(t, err)
	require.Len(t, fields, 2)
	assert.Equal(t, "Cidade", fields[0].Key)
	assert.Equal(t, "Natal", fields[0].Value)
	assert.Equal(t, "Origem", fields[1].Key)

	require.NoError(t, cf.Delete(ctx, "person", p.ID, "Cidade"))
	fields, err = cf.GetForEntity(ctx, "person", p.ID)
	require.NoError(t, err)
	require.Len(t, fields, 1)
	assert.Equal(t, "Origem", fields[0].Key)
}

func TestCustomFieldValidation(t *testing.T) {
	cf := repo.NewCustomFieldRepo(openTestDB(t))
	ctx := context.Background()
	assert.ErrorIs(t, cf.Set(ctx, "spaceship", 1, "k", "v"), model.ErrValidation)
	assert.ErrorIs(t, cf.Set(ctx, "person", 1, "", "v"), model.ErrValidation)
}

func TestCustomFieldFindEntityIDs_SkipsArchived(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	people := repo.NewPersonRepo(d)
	a, err := people.Create(ctx, model.CreatePersonInput{FirstName: "A"})
	require.NoError(t, err)
	b, err := people.Create(ctx, model.CreatePersonInput{FirstName: "B"})
	require.NoError(t, err)

	cf := repo.NewCustomFieldRepo(d)
	require.NoError(t, cf.Set(ctx, "person", a.ID, "cpf", "123"))
	require.NoError(t, cf.Set(ctx, "person", b.ID, "cpf", "123"))
	require.NoError(t, people.Archive(ctx, b.ID))

	ids, err := cf.FindEntityIDs(ctx, "person", "cpf", "123")
	require.NoError(t, err)
	assert.Equal(t, []int64{a.ID}, ids)

	all, err := cf.ListByEntityType(ctx, "person")
	require.NoError(t, err)
	assert.Len(t, all, 2)
}
