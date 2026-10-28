// Package csvio reads and writes RFC 4180 CSV while keeping the details needed
// for a byte-identical round trip: UTF-8 BOM, record terminator (LF or CRLF),
// whether the file ends with a terminator, and the exact bytes of every field
// (including CR/LF inside quoted fields).
//
// Writing uses minimal quoting: a field is quoted only when it contains the
// delimiter, a double quote, CR or LF. Input that quotes fields unnecessarily
// parses to the same values but is written back without the extra quotes.
package csvio

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// Dialect describes the byte-level layout of a CSV file.
type Dialect struct {
	BOM             bool
	CRLF            bool
	TrailingNewline bool
}

// DefaultDialect is used when no original file layout is known.
var DefaultDialect = Dialect{TrailingNewline: true}

// Table is a parsed CSV file: a header row plus data rows of equal width.
type Table struct {
	Header  []string
	Rows    [][]string
	Dialect Dialect
}

// ParseError reports malformed CSV input with its 1-based line number.
type ParseError struct {
	Line int
	Msg  string
}

func (e *ParseError) Error() string { return fmt.Sprintf("line %d: %s", e.Line, e.Msg) }

// Parse reads a comma-separated file. The first record is the header.
// Blank lines are skipped when the header has more than one column (in a
// single-column file a blank line is a row with an empty cell).
func Parse(data []byte) (*Table, error) {
	var t Table
	if bytes.HasPrefix(data, utf8BOM) {
		t.Dialect.BOM = true
		data = data[len(utf8BOM):]
	}
	if !utf8.Valid(data) {
		return nil, &ParseError{Line: 1, Msg: "file is not valid UTF-8 (convert it first, e.g. iconv -f latin1 -t utf-8)"}
	}
	if len(data) == 0 {
		return nil, &ParseError{Line: 1, Msg: "empty file: no header row"}
	}

	p := parser{data: data, line: 1}
	first := true
	seenTerm := ""
	for !p.eof() {
		startLine := p.line
		rec, term, blank, err := p.record()
		if err != nil {
			return nil, err
		}
		if term != "" {
			if seenTerm == "" {
				seenTerm = term
			} else if term != seenTerm {
				return nil, &ParseError{Line: startLine, Msg: "mixed line endings (LF and CRLF): normalize the file first"}
			}
		}
		if first {
			t.Dialect.CRLF = term == "\r\n"
			t.Header = rec
			first = false
		} else {
			if blank && len(t.Header) > 1 {
				t.Dialect.TrailingNewline = term != ""
				continue
			}
			if len(rec) != len(t.Header) {
				return nil, &ParseError{Line: startLine, Msg: fmt.Sprintf("row %d has %d fields, header has %d", len(t.Rows)+1, len(rec), len(t.Header))}
			}
			t.Rows = append(t.Rows, rec)
		}
		t.Dialect.TrailingNewline = term != ""
	}
	return &t, nil
}

type parser struct {
	data []byte
	pos  int
	line int
}

func (p *parser) eof() bool { return p.pos >= len(p.data) }

// record parses one record. term is the terminator consumed ("", "\n" or
// "\r\n"); blank reports an empty unquoted line.
func (p *parser) record() (rec []string, term string, blank bool, err error) {
	startPos := p.pos
	for {
		field, quoted, ferr := p.field()
		if ferr != nil {
			return nil, "", false, ferr
		}
		rec = append(rec, field)
		if p.eof() {
			return rec, "", false, nil
		}
		switch p.data[p.pos] {
		case ',':
			p.pos++
		case '\n':
			p.pos++
			p.line++
			return rec, "\n", len(rec) == 1 && !quoted && p.pos-1 == startPos, nil
		case '\r':
			if p.pos+1 >= len(p.data) || p.data[p.pos+1] != '\n' {
				return nil, "", false, &ParseError{Line: p.line, Msg: "unexpected character after closing quote"}
			}
			p.pos += 2
			p.line++
			return rec, "\r\n", len(rec) == 1 && !quoted && p.pos-2 == startPos, nil
		default:
			return nil, "", false, &ParseError{Line: p.line, Msg: "unexpected character after closing quote"}
		}
	}
}

func (p *parser) field() (string, bool, error) {
	if !p.eof() && p.data[p.pos] == '"' {
		return p.quotedField()
	}
	start := p.pos
	for !p.eof() {
		c := p.data[p.pos]
		if c == ',' || c == '\n' || (c == '\r' && p.pos+1 < len(p.data) && p.data[p.pos+1] == '\n') {
			break
		}
		if c == '"' {
			return "", false, &ParseError{Line: p.line, Msg: "bare quote in unquoted field (quote the whole field and double inner quotes)"}
		}
		if c == '\r' {
			return "", false, &ParseError{Line: p.line, Msg: "bare carriage return in unquoted field"}
		}
		p.pos++
	}
	return string(p.data[start:p.pos]), false, nil
}

func (p *parser) quotedField() (string, bool, error) {
	startLine := p.line
	p.pos++ // opening quote
	var sb strings.Builder
	for {
		if p.eof() {
			return "", true, &ParseError{Line: startLine, Msg: "unterminated quoted field"}
		}
		c := p.data[p.pos]
		if c == '"' {
			if p.pos+1 < len(p.data) && p.data[p.pos+1] == '"' {
				sb.WriteByte('"')
				p.pos += 2
				continue
			}
			p.pos++ // closing quote
			return sb.String(), true, nil
		}
		if c == '\n' {
			p.line++
		}
		sb.WriteByte(c)
		p.pos++
	}
}

// Write emits header and rows using comma (',' or '\t') as delimiter and the
// layout described by d.
func Write(w io.Writer, comma rune, header []string, rows [][]string, d Dialect) error {
	var buf bytes.Buffer
	if d.BOM {
		buf.Write(utf8BOM)
	}
	term := "\n"
	if d.CRLF {
		term = "\r\n"
	}
	writeRecord(&buf, comma, header)
	for _, rec := range rows {
		buf.WriteString(term)
		writeRecord(&buf, comma, rec)
	}
	if d.TrailingNewline {
		buf.WriteString(term)
	}
	_, err := w.Write(buf.Bytes())
	return err
}

func writeRecord(buf *bytes.Buffer, comma rune, rec []string) {
	for i, f := range rec {
		if i > 0 {
			buf.WriteRune(comma)
		}
		writeField(buf, comma, f)
	}
}

func writeField(buf *bytes.Buffer, comma rune, f string) {
	if !strings.ContainsRune(f, comma) && !strings.ContainsAny(f, "\"\r\n") {
		buf.WriteString(f)
		return
	}
	buf.WriteByte('"')
	buf.WriteString(strings.ReplaceAll(f, `"`, `""`))
	buf.WriteByte('"')
}
