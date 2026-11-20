package report

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Field is one raw CSV field. Quoted records whether the field was written
// with double quotes; that distinction is what makes NULL and empty text
// separable, so it is preserved rather than discarded.
type Field struct {
	// Value is the decoded field content: outer quotes removed and doubled
	// quotes collapsed to one.
	Value string
	// Quoted is true when the source field was quoted.
	Quoted bool
	// Line is the 1-based source line on which the field started.
	Line int
}

// Value is a single decoded, typed cell.
//
// Null is the single source of truth for missing data. The other fields are
// only meaningful when Null is false and match Type:
//
//	TextType      -> Text
//	IntegerType   -> Int
//	BooleanType   -> Bool
//	TimestampType -> Time
//
// In particular a NULL integer is not 0 and a NULL boolean is not false.
type Value struct {
	Column string
	Type   ColumnType
	Null   bool

	Text string
	Int  int64
	Bool bool
	Time time.Time
}

// Record is one logical CSV record (a row), after CSV decoding but before or
// after type decoding depending on the call site. A record with a quoted
// newline is still one record, and duplicate records are never collapsed.
type Record struct {
	// Number is the 1-based data record number within its file. The header,
	// when declared, is not a data record.
	Number int
	// Line is the 1-based source line where the record starts.
	Line int
	// Fields holds the raw decoded fields in column order.
	Fields []Field
	// Values holds the typed values in column order. It is populated by the
	// typed decode path.
	Values []Value
}

// Value returns the typed value for the named column.
func (r *Record) Value(column string) (Value, bool) {
	for _, value := range r.Values {
		if value.Column == column {
			return value, true
		}
	}
	return Value{}, false
}

// CSVError describes a malformed CSV document or a value that does not match
// its declared type. It names the file, record, line and column so that a
// failed delivery can be diagnosed without opening the archive.
type CSVError struct {
	File    string
	Record  int
	Line    int
	Column  string
	Message string
}

func (e *CSVError) Error() string {
	var b strings.Builder
	b.WriteString("report: ")
	if e.File != "" {
		b.WriteString(e.File)
		b.WriteString(": ")
	}
	if e.Record > 0 {
		fmt.Fprintf(&b, "record %d: ", e.Record)
	}
	if e.Line > 0 {
		fmt.Fprintf(&b, "line %d: ", e.Line)
	}
	if e.Column != "" {
		fmt.Fprintf(&b, "column %q: ", e.Column)
	}
	b.WriteString(e.Message)
	return b.String()
}

// parseCSV splits data into logical records. It is strict: bare quotes,
// characters after a closing quote, unterminated quotes, and stray carriage
// returns are errors rather than silent reinterpretation of the data.
func parseCSV(data []byte, delimiter byte, file string) ([]Record, error) {
	if len(data) >= 3 && data[0] == 0xef && data[1] == 0xbb && data[2] == 0xbf {
		data = data[3:]
	}
	var (
		records []Record
		fields  []Field
		field   strings.Builder
		quoted  bool
		inQuote bool
		line    = 1
		start   = 1
		pos     = 0
	)
	fail := func(message string) error {
		return &CSVError{File: file, Record: len(records) + 1, Line: line, Message: message}
	}
	finishField := func() {
		fields = append(fields, Field{Value: field.String(), Quoted: quoted, Line: start})
		field.Reset()
		quoted = false
	}
	finishRecord := func() {
		finishField()
		records = append(records, Record{Number: len(records) + 1, Line: start, Fields: fields})
		fields = nil
	}

	for pos < len(data) {
		c := data[pos]
		if inQuote {
			switch c {
			case '"':
				if pos+1 < len(data) && data[pos+1] == '"' {
					field.WriteByte('"')
					pos += 2
					continue
				}
				inQuote = false
				pos++
				if pos < len(data) {
					switch data[pos] {
					case delimiter, '\r', '\n':
					default:
						return nil, fail("characters after a closing quote")
					}
				}
			case '\r':
				// Only CRLF may appear inside a quoted field; a lone CR is
				// almost always a mangled line ending.
				if pos+1 < len(data) && data[pos+1] == '\n' {
					field.WriteString("\r\n")
					line++
					pos += 2
					continue
				}
				return nil, fail("carriage return without line feed inside a quoted field")
			case '\n':
				field.WriteByte('\n')
				line++
				pos++
			default:
				field.WriteByte(c)
				pos++
			}
			continue
		}

		switch c {
		case '"':
			if field.Len() != 0 || quoted {
				return nil, fail("quote character inside an unquoted field")
			}
			quoted = true
			inQuote = true
			start = line
			pos++
		case delimiter:
			finishField()
			pos++
		case '\r':
			if pos+1 < len(data) && data[pos+1] == '\n' {
				finishRecord()
				pos += 2
				line++
				start = line
				continue
			}
			return nil, fail("carriage return without line feed")
		case '\n':
			finishRecord()
			pos++
			line++
			start = line
		default:
			if field.Len() == 0 {
				start = line
			}
			field.WriteByte(c)
			pos++
		}
	}

	if inQuote {
		return nil, &CSVError{File: file, Record: len(records) + 1, Line: start, Message: "unterminated quoted field"}
	}
	if quoted || field.Len() != 0 || len(fields) != 0 {
		finishRecord()
	}
	return records, nil
}

// decodeCSV parses data, validates the header and column count, and decodes
// every value against the declared schema. The returned records preserve
// order and duplicates.
func decodeCSV(data []byte, schema *FileSchema) ([]Record, error) {
	delimiter, err := schema.delimiter()
	if err != nil {
		return nil, err
	}
	records, err := parseCSV(data, delimiter, schema.Name)
	if err != nil {
		return nil, err
	}

	index := 0
	if schema.Header {
		if len(records) == 0 {
			return nil, &CSVError{File: schema.Name, Message: "missing declared header row"}
		}
		header := records[0]
		if len(header.Fields) != len(schema.Columns) {
			return nil, &CSVError{
				File:    schema.Name,
				Record:  1,
				Line:    header.Line,
				Message: fmt.Sprintf("header has %d columns, schema declares %d", len(header.Fields), len(schema.Columns)),
			}
		}
		for i, column := range schema.Columns {
			if header.Fields[i].Value != column.Name {
				return nil, &CSVError{
					File:    schema.Name,
					Record:  1,
					Line:    header.Fields[i].Line,
					Column:  column.Name,
					Message: fmt.Sprintf("header column %d is %q, schema declares %q", i+1, header.Fields[i].Value, column.Name),
				}
			}
		}
		index = 1
	}

	result := make([]Record, 0, len(records)-index)
	for _, record := range records[index:] {
		if len(record.Fields) != len(schema.Columns) {
			return nil, &CSVError{
				File:    schema.Name,
				Record:  record.Number - index,
				Line:    record.Line,
				Message: fmt.Sprintf("record has %d fields, schema declares %d columns", len(record.Fields), len(schema.Columns)),
			}
		}
		decoded, err := decodeRecord(record, schema, record.Number-index)
		if err != nil {
			return nil, err
		}
		result = append(result, decoded)
	}
	return result, nil
}

func decodeRecord(record Record, schema *FileSchema, number int) (Record, error) {
	out := Record{Number: number, Line: record.Line, Fields: record.Fields, Values: make([]Value, len(schema.Columns))}
	for i, column := range schema.Columns {
		field := record.Fields[i]
		if !utf8.ValidString(field.Value) {
			return Record{}, &CSVError{File: schema.Name, Record: number, Line: field.Line, Column: column.Name, Message: "value is not valid UTF-8"}
		}
		value, err := decodeValue(field, &column)
		if err != nil {
			return Record{}, &CSVError{File: schema.Name, Record: number, Line: field.Line, Column: column.Name, Message: err.Error()}
		}
		out.Values[i] = value
	}
	return out, nil
}

func decodeValue(field Field, column *Column) (Value, error) {
	value := Value{Column: column.Name, Type: column.Type}
	if value.Type == "" {
		value.Type = TextType
	}

	// Quoted fields are always literal. Unquoted fields may be NULL.
	if !field.Quoted {
		if column.NullToken != "" && field.Value == column.NullToken {
			value.Null = true
			return value, nil
		}
		if field.Value == "" {
			if !(value.Type == TextType && column.EmptyIsText) {
				value.Null = true
				return value, nil
			}
		}
	}

	switch value.Type {
	case TextType:
		value.Text = field.Value
	case IntegerType:
		if field.Value == "" {
			return Value{}, fmt.Errorf("empty value is not an integer")
		}
		parsed, err := strconv.ParseInt(field.Value, 10, 64)
		if err != nil {
			return Value{}, fmt.Errorf("value %q is not a base-10 integer", field.Value)
		}
		value.Int = parsed
	case BooleanType:
		parsed, err := decodeBool(field.Value, column)
		if err != nil {
			return Value{}, err
		}
		value.Bool = parsed
	case TimestampType:
		layouts := column.Layouts
		if len(layouts) == 0 {
			layouts = defaultTimestampLayouts
		}
		var lastErr error
		for _, layout := range layouts {
			parsed, err := time.Parse(layout, field.Value)
			if err == nil {
				value.Time = parsed
				return value, nil
			}
			lastErr = err
		}
		return Value{}, fmt.Errorf("value %q matches none of the declared timestamp layouts: %s", field.Value, lastErr)
	default:
		return Value{}, fmt.Errorf("unknown column type %q", value.Type)
	}
	return value, nil
}

func decodeBool(text string, column *Column) (bool, error) {
	if text == "" {
		return false, fmt.Errorf("empty value is not a boolean")
	}
	if len(column.TrueTokens) != 0 || len(column.FalseTokens) != 0 {
		for _, token := range column.TrueTokens {
			if text == token {
				return true, nil
			}
		}
		for _, token := range column.FalseTokens {
			if text == token {
				return false, nil
			}
		}
		return false, fmt.Errorf("value %q is not one of the declared boolean tokens", text)
	}
	// With no declared tokens only the literal words are accepted; "0" and
	// "1" are integers, not booleans, and are rejected rather than guessed.
	if strings.EqualFold(text, "true") {
		return true, nil
	}
	if strings.EqualFold(text, "false") {
		return false, nil
	}
	return false, fmt.Errorf("value %q is not a boolean (expected true or false)", text)
}
