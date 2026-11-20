package report

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

// ColumnType is the declared logical type of a CSV column. Every value in the
// package is decoded into that type; a value that cannot be decoded is a
// delivery failure, not a zero value.
type ColumnType string

const (
	// TextType stores the field verbatim as a string.
	TextType ColumnType = "text"
	// IntegerType stores a base-10 signed 64-bit integer.
	IntegerType ColumnType = "integer"
	// BooleanType stores a boolean using the declared token lists.
	BooleanType ColumnType = "boolean"
	// TimestampType stores a time.Time parsed with the declared layouts.
	TimestampType ColumnType = "timestamp"
)

// Column declares one CSV column. Name must match the header exactly when the
// file has a header, and otherwise names the column positionally.
//
// NULL handling is deliberately explicit:
//
//   - A quoted field is always literal text. A quoted empty field is empty
//     text, never NULL.
//   - An unquoted empty field is NULL unless EmptyIsText is true.
//   - An unquoted field equal to NullToken (when NullToken is not empty) is
//     also NULL.
//
// This makes NULL and empty text distinguishable without guessing, and keeps
// integer 0 and boolean false distinct from missing values.
type Column struct {
	Name string     `json:"name"`
	Type ColumnType `json:"type,omitempty"`

	// NullToken is an additional unquoted spelling of NULL, for example "\\N".
	NullToken string `json:"null_token,omitempty"`

	// EmptyIsText makes an unquoted empty field empty text instead of NULL.
	// It is only meaningful for text columns; for typed columns an empty
	// value cannot be decoded and is a type error.
	EmptyIsText bool `json:"empty_is_text,omitempty"`

	// TrueTokens and FalseTokens declare the exact boolean spellings accepted
	// by a boolean column. When empty, "true"/"false" are matched without
	// regard to case.
	TrueTokens  []string `json:"true_tokens,omitempty"`
	FalseTokens []string `json:"false_tokens,omitempty"`

	// Layouts declares the accepted timestamp layouts, tried in order. When
	// empty, RFC3339 and a few common date forms are accepted.
	Layouts []string `json:"layouts,omitempty"`
}

// FileSchema declares how one CSV member of the package is decoded.
type FileSchema struct {
	// Name is the member name inside the ZIP, using forward slashes.
	Name string `json:"name"`

	// Delimiter is a single ASCII character. It defaults to ",".
	Delimiter string `json:"delimiter,omitempty"`

	// Header declares whether the first logical record is a header. When true
	// the header must match Columns exactly, in order.
	Header bool `json:"header,omitempty"`

	Columns []Column `json:"columns"`
}

// Schema is the complete declared decoding contract for a report package.
// Every data member of the package (every member except the completion
// manifest) must be declared here, and every declared file must be present.
type Schema struct {
	// Version is an optional caller-defined schema version, recorded with the
	// delivered package.
	Version int `json:"version,omitempty"`

	Files []FileSchema `json:"files"`
}

// ParseSchema decodes and validates a schema document.
func ParseSchema(data []byte) (*Schema, error) {
	var schema Schema
	if err := json.Unmarshal(data, &schema); err != nil {
		return nil, fmt.Errorf("report: parse schema: %s", err)
	}
	if err := schema.Validate(); err != nil {
		return nil, err
	}
	return &schema, nil
}

// MarshalSchema serialises a schema deterministically for storage.
func MarshalSchema(schema *Schema) ([]byte, error) {
	if err := schema.Validate(); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("report: marshal schema: %s", err)
	}
	return append(data, '\n'), nil
}

// Validate checks that the schema can safely and unambiguously decode a
// package. It does not look at any package contents.
func (s *Schema) Validate() error {
	if s == nil {
		return errors.New("report: schema is nil")
	}
	if len(s.Files) == 0 {
		return errors.New("report: schema declares no files")
	}
	seen := map[string]bool{}
	for i := range s.Files {
		file := &s.Files[i]
		if err := validateMemberName(file.Name); err != nil {
			return fmt.Errorf("report: schema file %d: %s", i, err)
		}
		key := strings.ToLower(file.Name)
		if seen[key] {
			return fmt.Errorf("report: schema declares file %q more than once", file.Name)
		}
		seen[key] = true
		if file.Name == DefaultManifestName {
			return fmt.Errorf("report: schema file %q collides with the completion manifest", file.Name)
		}
		if _, err := file.delimiter(); err != nil {
			return fmt.Errorf("report: schema file %q: %s", file.Name, err)
		}
		if len(file.Columns) == 0 {
			return fmt.Errorf("report: schema file %q declares no columns", file.Name)
		}
		columnNames := map[string]bool{}
		for j := range file.Columns {
			column := &file.Columns[j]
			if column.Name == "" {
				return fmt.Errorf("report: schema file %q column %d has no name", file.Name, j)
			}
			if !utf8.ValidString(column.Name) {
				return fmt.Errorf("report: schema file %q column %d name is not valid UTF-8", file.Name, j)
			}
			if strings.ContainsAny(column.Name, "\r\n") {
				return fmt.Errorf("report: schema file %q column %q contains a newline", file.Name, column.Name)
			}
			if columnNames[column.Name] {
				return fmt.Errorf("report: schema file %q declares column %q more than once", file.Name, column.Name)
			}
			columnNames[column.Name] = true
			if err := column.validate(); err != nil {
				return fmt.Errorf("report: schema file %q column %q: %s", file.Name, column.Name, err)
			}
		}
	}
	return nil
}

func (c *Column) validate() error {
	switch c.Type {
	case "", TextType:
		if c.TrueTokens != nil || c.FalseTokens != nil {
			return errors.New("boolean tokens declared on a text column")
		}
	case IntegerType:
		if c.TrueTokens != nil || c.FalseTokens != nil || len(c.Layouts) != 0 {
			return errors.New("boolean or timestamp options declared on an integer column")
		}
	case BooleanType:
		if len(c.Layouts) != 0 {
			return errors.New("timestamp layouts declared on a boolean column")
		}
		if len(c.TrueTokens) == 0 && len(c.FalseTokens) != 0 ||
			len(c.TrueTokens) != 0 && len(c.FalseTokens) == 0 {
			return errors.New("both true_tokens and false_tokens must be declared together")
		}
		for _, token := range append(append([]string{}, c.TrueTokens...), c.FalseTokens...) {
			if token == "" {
				return errors.New("boolean token must not be empty")
			}
		}
	case TimestampType:
		if c.TrueTokens != nil || c.FalseTokens != nil {
			return errors.New("boolean tokens declared on a timestamp column")
		}
		for _, layout := range c.Layouts {
			if layout == "" {
				return errors.New("empty timestamp layout")
			}
		}
	default:
		return fmt.Errorf("unknown column type %q", c.Type)
	}
	if c.EmptyIsText && c.Type != "" && c.Type != TextType {
		return errors.New("empty_is_text is only supported for text columns")
	}
	return nil
}

func (f *FileSchema) delimiter() (byte, error) {
	if f.Delimiter == "" {
		return ',', nil
	}
	if len(f.Delimiter) != 1 {
		return 0, fmt.Errorf("delimiter %q must be a single ASCII character", f.Delimiter)
	}
	c := f.Delimiter[0]
	if c >= utf8.RuneSelf || c == '"' || c == '\r' || c == '\n' || c == 0 {
		return 0, fmt.Errorf("delimiter %q is not allowed", f.Delimiter)
	}
	return c, nil
}

var defaultTimestampLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05Z07:00",
	"2006-01-02 15:04:05Z07:00",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

// validateMemberName rejects absolute paths, traversal, backslashes, control
// characters and other names that could escape the delivery directory or be
// ambiguous across platforms.
func validateMemberName(name string) error {
	if name == "" {
		return errors.New("member name is empty")
	}
	if len(name) > 1024 {
		return fmt.Errorf("member name is too long (%d bytes)", len(name))
	}
	if !utf8.ValidString(name) {
		return fmt.Errorf("member name %q is not valid UTF-8", name)
	}
	if strings.Contains(name, "\\") {
		return fmt.Errorf("member name %q contains a backslash", name)
	}
	if strings.Contains(name, ":") {
		return fmt.Errorf("member name %q contains a colon", name)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("member name %q contains a control character", name)
		}
	}
	if strings.HasPrefix(name, "/") {
		return fmt.Errorf("member name %q is absolute", name)
	}
	if strings.HasSuffix(name, "/") {
		return fmt.Errorf("member name %q ends with a slash", name)
	}
	if path.Clean(name) != name {
		return fmt.Errorf("member name %q is not a clean relative path", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("member name %q contains an unsafe path component", name)
		}
	}
	return nil
}

func validateBaseName(name string) error {
	if err := validateMemberName(name); err != nil {
		return err
	}
	if strings.Contains(name, "/") {
		return fmt.Errorf("%q is not a base file name", name)
	}
	return nil
}
