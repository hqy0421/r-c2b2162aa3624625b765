package report

import (
	"testing"
	"time"
)

func TestReadPackageReturnsTypedRecordsSourceAndSummary(t *testing.T) {
	data, manifest := buildCanonical(t)
	pkg, err := VerifyArchive(data, manifest, canonicalSchema())
	if err != nil {
		t.Fatalf("verify: %s", err)
	}
	report, err := ReadPackage(pkg)
	if err != nil {
		t.Fatalf("read: %s", err)
	}
	if report.JobID != "job-A" || report.SnapshotID != "snap-1" || report.PackageID != "pkg-1" {
		t.Fatalf("source not preserved: %+v", report)
	}
	if report.RecordCount != 4 || len(report.Files) != 1 {
		t.Fatalf("counts: %+v", report)
	}
	file := report.Files[0]
	if file.Name != "orders.csv" || !file.HasHeader || len(file.Records) != 4 {
		t.Fatalf("file report: %+v", file)
	}
	// Typed access to an actual record.
	value, ok := file.Records[0].Value("order_id")
	if !ok || value.Null || value.Int != 1 {
		t.Fatalf("typed lookup: %+v", value)
	}
	// NULL and zero are distinguishable through the summary.
	if file.Summary.Records != 4 {
		t.Fatalf("summary records: %d", file.Summary.Records)
	}
	var orderSummary, discountSummary ColumnSummary
	for _, column := range file.Summary.Columns {
		switch column.Name {
		case "order_id":
			orderSummary = column
		case "discount":
			discountSummary = column
		}
	}
	if orderSummary.Nulls != 0 || orderSummary.NonNuls != 4 {
		t.Errorf("order_id summary: %+v", orderSummary)
	}
	// discount: two zeros (the duplicated row), one NULL, three non-null.
	if discountSummary.Zeroes != 2 || discountSummary.Nulls != 1 || discountSummary.NonNuls != 3 {
		t.Errorf("discount summary: %+v", discountSummary)
	}
	if file.Summary.ZeroCount != 2 || file.Summary.NullCount != 1 || file.Summary.FalseCount != 1 {
		t.Errorf("file totals: %+v", file.Summary)
	}
}

func TestReadPackageRejectsNil(t *testing.T) {
	if _, err := ReadPackage(nil); err == nil {
		t.Fatal("expected nil package to be rejected")
	}
}

func TestValueStringDistinguishesNullFromEmpty(t *testing.T) {
	if got := (Value{Type: TextType, Null: true}).String(); got != "NULL" {
		t.Errorf("NULL rendered as %q", got)
	}
	if got := (Value{Type: TextType, Text: ""}).String(); got != "" {
		t.Errorf("empty text rendered as %q", got)
	}
	if got := (Value{Type: IntegerType, Int: 0}).String(); got != "0" {
		t.Errorf("integer zero rendered as %q", got)
	}
	if got := (Value{Type: BooleanType, Bool: false}).String(); got != "false" {
		t.Errorf("false rendered as %q", got)
	}
	stamp := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	if got := (Value{Type: TimestampType, Time: stamp}).String(); got != "2024-05-06T07:08:09Z" {
		t.Errorf("timestamp rendered as %q", got)
	}
	if got := (Value{Type: TimestampType, Null: true}).TimeString(); got != "" {
		t.Errorf("NULL timestamp rendered as %q", got)
	}
}

func TestReadPackageMultipleFiles(t *testing.T) {
	schema := &Schema{Files: []FileSchema{
		{Name: "a.csv", Columns: []Column{{Name: "x", Type: IntegerType}}},
		{Name: "b.csv", Columns: []Column{{Name: "y", Type: TextType}}},
	}}
	spec := packageSpec{
		jobID: "job-M", snapshotID: "snap-M", packageID: "pkg-M", fileName: "multi.zip",
		files: []packageFileSpec{
			{name: "a.csv", data: []byte("1\n2\n")},
			{name: "b.csv", data: []byte("hello\nworld\n")},
		},
	}
	data, manifest := buildPackage(t, spec)
	pkg, err := VerifyArchive(data, manifest, schema)
	if err != nil {
		t.Fatalf("verify: %s", err)
	}
	report, err := ReadPackage(pkg)
	if err != nil {
		t.Fatalf("read: %s", err)
	}
	if len(report.Files) != 2 || report.RecordCount != 4 {
		t.Fatalf("multi-file report: %+v", report)
	}
	if report.Files[0].Name != "a.csv" || report.Files[1].Name != "b.csv" {
		t.Fatalf("file order should follow the schema: %+v", report.Files)
	}
}

func TestHttpSessionCloseIsSafe(t *testing.T) {
	var nilSession *HttpSession
	nilSession.Close() // must not panic
	session, err := NewHttpSession(nil)
	if err != nil {
		t.Fatalf("session: %s", err)
	}
	session.Close()
}
