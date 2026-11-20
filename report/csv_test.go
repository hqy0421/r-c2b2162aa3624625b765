package report

import (
	"strings"
	"testing"
)

func TestDecodeCSVPreservesLogicalRecords(t *testing.T) {
	schema := canonicalSchema().Files[0]
	records, err := decodeCSV([]byte(canonicalCSV), &schema)
	if err != nil {
		t.Fatalf("decode: %s", err)
	}
	if len(records) != 4 {
		t.Fatalf("expected 4 logical records, got %d", len(records))
	}
	// The embedded newline must not split a record, and quoting must be gone.
	if records[0].Fields[1].Value != "first\nline" || records[0].Fields[1].Quoted != true {
		t.Fatalf("quoted newline record: %+v", records[0].Fields[1])
	}
	// Duplicate rows survive.
	if records[0].Fields[0].Value != records[2].Fields[0].Value ||
		records[0].Fields[1].Value != records[2].Fields[1].Value {
		t.Fatalf("duplicate records not preserved")
	}
	// Typed values.
	if v := records[0].Values[3]; v.Null || v.Int != 0 {
		t.Errorf("int zero: %+v", v)
	}
	if v := records[1].Values[3]; !v.Null {
		t.Errorf("missing int should be NULL: %+v", v)
	}
	if v := records[1].Values[2]; v.Null || v.Bool {
		t.Errorf("false boolean: %+v", v)
	}
	if v := records[3].Values[1]; v.Null || v.Text != "" {
		t.Errorf("empty text (EmptyIsText): %+v", v)
	}
}

func TestDecodeCSVNullVersusEmptyText(t *testing.T) {
	schema := &FileSchema{
		Name: "values.csv",
		Columns: []Column{
			{Name: "maybe", Type: TextType, NullToken: "\\N"},
			{Name: "empty_ok", Type: TextType, EmptyIsText: true},
			{Name: "plain", Type: TextType},
		},
	}
	data := "\\N,,\n" + // NULL token; empty text; NULL (no EmptyIsText)
		"\\N,\"\",\"\"\n" + // NULL token; empty text; quoted empty is empty text
		`"\N",,` + "\n" + // quoted literal backslash-N becomes text, not NULL
		"\\N,x,\"y\"\n"
	records, err := decodeCSV([]byte(data), schema)
	if err != nil {
		t.Fatalf("decode: %s", err)
	}
	cases := []struct {
		null [3]bool
		text [3]string
	}{
		{[3]bool{true, false, true}, [3]string{"", "", ""}},
		{[3]bool{true, false, false}, [3]string{"", "", ""}},
		{[3]bool{false, false, true}, [3]string{"\\N", "", ""}},
		{[3]bool{true, false, false}, [3]string{"", "x", "y"}},
	}
	for i, c := range cases {
		for j := range c.null {
			value := records[i].Values[j]
			if value.Null != c.null[j] {
				t.Errorf("record %d column %d Null = %v, want %v", i, j, value.Null, c.null[j])
			}
			if !value.Null && value.Text != c.text[j] {
				t.Errorf("record %d column %d Text = %q, want %q", i, j, value.Text, c.text[j])
			}
		}
	}
}

func TestDecodeCSVQuotedMultilineAndDoubledQuotes(t *testing.T) {
	schema := &FileSchema{
		Name:    "text.csv",
		Columns: []Column{{Name: "body", Type: TextType}},
	}
	data := "\"a\"\"b\"\r\n\"line1\r\nline2\"\r\n"
	records, err := decodeCSV([]byte(data), schema)
	if err != nil {
		t.Fatalf("decode: %s", err)
	}
	if len(records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(records))
	}
	if got := records[0].Fields[0].Value; got != `a"b` {
		t.Errorf("doubled quote: got %q", got)
	}
	if got := records[1].Fields[0].Value; got != "line1\r\nline2" {
		t.Errorf("CRLF inside quotes: got %q", got)
	}
}

func TestDecodeCSVRejectsMalformedInput(t *testing.T) {
	schema := &FileSchema{
		Name:    "bad.csv",
		Columns: []Column{{Name: "a"}, {Name: "b"}},
	}
	cases := map[string]string{
		"unterminated quote":   `"a,b` + "\n",
		"bare quote in field":  `a"b,c` + "\n",
		"characters after end": `"a"x,b` + "\n",
		"lone carriage return": "a,b\rc\n",
		"too few fields":       "a\n",
		"too many fields":      "a,b,c\n",
	}
	for name, data := range cases {
		if _, err := decodeCSV([]byte(data), schema); err == nil {
			t.Errorf("%s: expected an error", name)
		} else if !strings.Contains(err.Error(), "bad.csv") {
			t.Errorf("%s: error should name the file: %s", name, err)
		}
	}
}

func TestDecodeCSVHeaderMismatch(t *testing.T) {
	schema := &FileSchema{
		Name:    "h.csv",
		Header:  true,
		Columns: []Column{{Name: "a"}, {Name: "b"}},
	}
	if _, err := decodeCSV([]byte("a,c\n1,2\n"), schema); err == nil {
		t.Fatal("expected header mismatch error")
	}
	if _, err := decodeCSV(nil, schema); err == nil {
		t.Fatal("expected missing header error")
	}
}

func TestDecodeCSVTypeErrors(t *testing.T) {
	schema := &FileSchema{
		Name: "typed.csv",
		Columns: []Column{
			{Name: "n", Type: IntegerType},
			{Name: "b", Type: BooleanType},
		},
	}
	for _, data := range []string{"1.5,true\n", "01a,true\n", "1,1\n", "1,yes\n", "1,x\n"} {
		if _, err := decodeCSV([]byte(data), schema); err == nil {
			t.Errorf("expected type error for %q", data)
		}
	}
	// An unquoted empty typed value is NULL, not a type error and not zero.
	records, err := decodeCSV([]byte(",true\n1,\n"), schema)
	if err != nil {
		t.Fatalf("empty typed values should decode as NULL: %s", err)
	}
	if !records[0].Values[0].Null || !records[1].Values[1].Null {
		t.Fatalf("empty typed values must be NULL: %s", recordStrings(records))
	}
}

func TestDecodeCSVDeclaredBooleanTokens(t *testing.T) {
	schema := &FileSchema{
		Name: "tokens.csv",
		Columns: []Column{{
			Name:        "flag",
			Type:        BooleanType,
			TrueTokens:  []string{"Y"},
			FalseTokens: []string{"N"},
		}},
	}
	records, err := decodeCSV([]byte("Y\nN\n"), schema)
	if err != nil {
		t.Fatalf("decode: %s", err)
	}
	if !records[0].Values[0].Bool || records[1].Values[0].Bool {
		t.Fatalf("declared tokens decoded incorrectly: %+v", recordStrings(records))
	}
	if _, err := decodeCSV([]byte("true\n"), schema); err == nil {
		t.Fatal("declared token list must reject other spellings")
	}
}

func TestDecodeCSVTrailingNewlineAndNoTrailingNewline(t *testing.T) {
	schema := &FileSchema{Name: "n.csv", Columns: []Column{{Name: "a"}}}
	for _, data := range []string{"x", "x\n", "x\r\n"} {
		records, err := decodeCSV([]byte(data), schema)
		if err != nil {
			t.Fatalf("%q: %s", data, err)
		}
		if len(records) != 1 {
			t.Fatalf("%q: expected 1 record, got %d", data, len(records))
		}
	}
	// A file that is only a trailing newline has one empty record.
	records, err := decodeCSV([]byte("\n"), schema)
	if err != nil {
		t.Fatalf("blank line: %s", err)
	}
	if len(records) != 1 || !records[0].Values[0].Null {
		t.Fatalf("blank line should be one NULL record: %+v", records)
	}
}

func TestDecodeCSVBOM(t *testing.T) {
	schema := &FileSchema{
		Name:    "bom.csv",
		Header:  true,
		Columns: []Column{{Name: "a"}},
	}
	data := append([]byte{0xef, 0xbb, 0xbf}, []byte("a\n1\n")...)
	records, err := decodeCSV(data, schema)
	if err != nil {
		t.Fatalf("BOM should be stripped: %s", err)
	}
	if len(records) != 1 || records[0].Values[0].Text != "1" {
		t.Fatalf("unexpected records: %+v", recordStrings(records))
	}
}

func TestDecodeCSVCustomDelimiterAndSemicolonInText(t *testing.T) {
	schema := &FileSchema{
		Name:      "semi.csv",
		Delimiter: ";",
		Columns:   []Column{{Name: "a"}, {Name: "b"}},
	}
	records, err := decodeCSV([]byte("x;y\n"), schema)
	if err != nil {
		t.Fatalf("decode: %s", err)
	}
	if records[0].Values[0].Text != "x" || records[0].Values[1].Text != "y" {
		t.Fatalf("delimiter not respected: %+v", recordStrings(records))
	}
}

func TestDecodeCSVRejectsInvalidUTF8(t *testing.T) {
	schema := &FileSchema{Name: "u.csv", Columns: []Column{{Name: "a"}}}
	if _, err := decodeCSV([]byte{0xff, 0xfe, '\n'}, schema); err == nil {
		t.Fatal("expected invalid UTF-8 to be rejected")
	}
}
