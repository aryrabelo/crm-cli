package bench

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeTask(t *testing.T, body string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "t.json")
	require.NoError(t, os.WriteFile(f, []byte(body), 0o600))
	return f
}

func TestLoadTask_Validation(t *testing.T) {
	const ok = `{"name":"x","batch":{"steps":[],"checks":[{"sql":"select 1","equals":1}]},`
	cases := map[string]struct{ body, errPart string }{
		"valid":                   {ok + `"per_entity_na":"no"}`, ""},
		"both per_entity and na":  {ok + `"per_entity":{"steps":[],"checks":[{"sql":"select 1","equals":1}]},"per_entity_na":"no"}`, "not both"},
		"check without source":    {`{"name":"x","batch":{"steps":[],"checks":[{"equals":1}]},"per_entity_na":"no"}`, "exactly one of sql or result"},
		"check with both sources": {`{"name":"x","batch":{"steps":[],"checks":[{"sql":"select 1","result":0,"equals":1}]},"per_entity_na":"no"}`, "exactly one of sql or result"},
		"check without assertion": {`{"name":"x","batch":{"steps":[],"checks":[{"sql":"select 1"}]},"per_entity_na":"no"}`, "asserts nothing"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := loadTask(writeTask(t, tc.body))
			if tc.errPart == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errPart)
		})
	}
}

func TestExpand_PlaceholderWhitespace(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a b.txt"), []byte("hello"), 0o600))
	e := &executor{task: &Task{dir: dir}}
	row := map[string]string{"email": "a@b.io"}

	got, err := e.expand("{{ file:a b.txt }}", row, nil)
	require.NoError(t, err)
	assert.Equal(t, "hello", got)
	got, err = e.expand("{{ row.email }}", row, nil)
	require.NoError(t, err)
	assert.Equal(t, "a@b.io", got)
	got, err = e.expand("to: {{ row.email  }}!", row, nil)
	require.NoError(t, err)
	assert.Equal(t, "to: a@b.io!", got)
}
