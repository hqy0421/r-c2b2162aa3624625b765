package report

import (
	"fmt"
	"time"
)

// Report is the independent, read-only view of a verified package: real typed
// records, where each file came from, and per-file summaries.
type Report struct {
	JobID       string
	SnapshotID  string
	PackageID   string
	FileName    string
	SHA256      string
	Size        int64
	Files       []FileReport
	RecordCount int
}

// FileReport is the summary of one decoded CSV file. Records are the actual
// typed records in source order; duplicates are kept.
type FileReport struct {
	Name      string
	Size      int64
	SHA256    string
	Delimiter string
	HasHeader bool
	Columns   []Column
	Records   []Record
	Summary   Summary
}

// Summary counts records and per-column value kinds. NullCount, ZeroCount and
// FalseCount are counted separately so that a report with zero-valued integers
// or false booleans cannot be mistaken for one with missing values.
type Summary struct {
	Records    int
	Columns    []ColumnSummary
	NullCount  int
	ZeroCount  int
	FalseCount int
}

// ColumnSummary is the summary for one column.
type ColumnSummary struct {
	Name    string
	Type    ColumnType
	Nulls   int
	Zeroes  int
	Falses  int
	NonNuls int
}

func summarizeFile(file *PackageFile) (*FileReport, error) {
	if file.Schema == nil {
		return nil, fmt.Errorf("report: file %q has no schema", file.Name)
	}
	delimiter, err := file.Schema.delimiter()
	if err != nil {
		return nil, err
	}
	report := &FileReport{
		Name:      file.Name,
		Size:      file.Size,
		SHA256:    file.SHA256,
		Delimiter: string(delimiter),
		HasHeader: file.Schema.Header,
		Columns:   append([]Column{}, file.Schema.Columns...),
		Records:   file.Records,
	}
	report.Summary.Records = len(file.Records)
	report.Summary.Columns = make([]ColumnSummary, len(file.Schema.Columns))
	for i, column := range file.Schema.Columns {
		columnType := column.Type
		if columnType == "" {
			columnType = TextType
		}
		report.Summary.Columns[i] = ColumnSummary{Name: column.Name, Type: columnType}
	}
	for _, record := range file.Records {
		for i, value := range record.Values {
			summary := &report.Summary.Columns[i]
			if value.Null {
				summary.Nulls++
				report.Summary.NullCount++
				continue
			}
			summary.NonNuls++
			switch value.Type {
			case IntegerType:
				if value.Int == 0 {
					summary.Zeroes++
					report.Summary.ZeroCount++
				}
			case BooleanType:
				if !value.Bool {
					summary.Falses++
					report.Summary.FalseCount++
				}
			}
		}
	}
	return report, nil
}

// TimeString formats a timestamp value in UTC for callers that want a stable
// text form without depending on the declared layout.
func (v Value) TimeString() string {
	if v.Null || v.Type != TimestampType {
		return ""
	}
	return v.Time.UTC().Format(time.RFC3339Nano)
}

// String renders a typed value for display, preserving the NULL/empty-text
// distinction: NULL renders as the word NULL, empty text renders as nothing.
func (v Value) String() string {
	if v.Null {
		return "NULL"
	}
	switch v.Type {
	case IntegerType:
		return fmt.Sprintf("%d", v.Int)
	case BooleanType:
		if v.Bool {
			return "true"
		}
		return "false"
	case TimestampType:
		return v.TimeString()
	default:
		return v.Text
	}
}
