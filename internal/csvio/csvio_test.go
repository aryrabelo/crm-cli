package csvio_test

import (
	"bytes"
	"testing"

	"github.com/jdanielnd/crm-cli/internal/csvio"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse_QuotedFields(t *testing.T) {
	in := "name,notes\n\"Smith, Jane\",\"said \"\"hi\"\"\nthen left\"\nJosé,\n"
	tbl, err := csvio.Parse([]byte(in))
	require.NoError(t, err)
	assert.Equal(t, []string{"name", "notes"}, tbl.Header)
	require.Len(t, tbl.Rows, 2)
	assert.Equal(t, []string{"Smith, Jane", "said \"hi\"\nthen left"}, tbl.Rows[0])
	assert.Equal(t, []string{"José", ""}, tbl.Rows[1])
	assert.Equal(t, csvio.Dialect{TrailingNewline: true}, tbl.Dialect)
}

func TestParse_BOMAndCRLF(t *testing.T) {
	in := "\xEF\xBB\xBFa,b\r\n1,\"x\r\ny\"\r\n2,z"
	tbl, err := csvio.Parse([]byte(in))
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, tbl.Header)
	assert.Equal(t, [][]string{{"1", "x\r\ny"}, {"2", "z"}}, tbl.Rows)
	assert.Equal(t, csvio.Dialect{BOM: true, CRLF: true, TrailingNewline: false}, tbl.Dialect)
}

func TestParse_BlankLinesSkippedForMultiColumn(t *testing.T) {
	tbl, err := csvio.Parse([]byte("a,b\n\n1,2\n\n"))
	require.NoError(t, err)
	assert.Equal(t, [][]string{{"1", "2"}}, tbl.Rows)
}

func TestParse_BlankLineIsEmptyCellForSingleColumn(t *testing.T) {
	tbl, err := csvio.Parse([]byte("a\n1\n\n2\n"))
	require.NoError(t, err)
	assert.Equal(t, [][]string{{"1"}, {""}, {"2"}}, tbl.Rows)
}

func TestParse_Errors(t *testing.T) {
	cases := map[string]string{
		"":                  "empty file",
		"a,b\n1\n":          "line 2: row 1 has 1 fields, header has 2",
		"a,b\n1,\"open\n":   "line 2: unterminated quoted field",
		"a,b\n\"x\"y,2\n":   "line 2: unexpected character after closing quote",
		"a,b\n\xff\xfe,2\n": "not valid UTF-8",
	}
	for in, want := range cases {
		_, err := csvio.Parse([]byte(in))
		require.Error(t, err, "input %q", in)
		assert.Contains(t, err.Error(), want, "input %q", in)
	}
}

func TestWrite_RoundTrip(t *testing.T) {
	inputs := []string{
		"name,notes\n\"Smith, Jane\",\"said \"\"hi\"\"\nthen left\"\nJosé,\n",
		"\xEF\xBB\xBFa,b\r\n1,\"x\r\ny\"\r\n2,z",
		"a,b,c\n,,\n\" lead,trail \",x, y \n",
		"h\nA\n\n",
		"only,header",
	}
	for _, in := range inputs {
		tbl, err := csvio.Parse([]byte(in))
		require.NoError(t, err, "input %q", in)
		var buf bytes.Buffer
		require.NoError(t, csvio.Write(&buf, ',', tbl.Header, tbl.Rows, tbl.Dialect))
		assert.Equal(t, in, buf.String())
	}
}

func TestWrite_TSVQuotesTabs(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, csvio.Write(&buf, '\t', []string{"a", "b"}, [][]string{{"x\ty", "1,2"}}, csvio.DefaultDialect))
	assert.Equal(t, "a\tb\n\"x\ty\"\t1,2\n", buf.String())
}
