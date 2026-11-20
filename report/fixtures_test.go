package report

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"testing"
)

// archiveBuilder builds a deterministic ZIP in memory. Determinism matters:
// tests compare digests and want to rebuild a package byte-for-byte.
type archiveBuilder struct {
	entries []archiveEntry
}

type archiveEntry struct {
	name string
	data []byte
}

func (b *archiveBuilder) add(name string, data []byte) *archiveBuilder {
	b.entries = append(b.entries, archiveEntry{name, data})
	return b
}

func (b *archiveBuilder) bytes(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, entry := range b.entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		header.Modified = header.Modified.UTC()
		w, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatalf("create zip member %q: %s", entry.name, err)
		}
		if _, err := w.Write(entry.data); err != nil {
			t.Fatalf("write zip member %q: %s", entry.name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close zip: %s", err)
	}
	return buffer.Bytes()
}

// buildPackage assembles a package from CSV contents plus a completion
// manifest, with all sizes and digests computed consistently.
type packageSpec struct {
	jobID      string
	snapshotID string
	packageID  string
	fileName   string
	files      []packageFileSpec
}

type packageFileSpec struct {
	name string
	data []byte
}

func (s packageSpec) manifestBytes(t *testing.T) []byte {
	t.Helper()
	manifest := s.manifest(t)
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %s", err)
	}
	return append(data, '\n')
}

func (s packageSpec) manifest(t *testing.T) *Manifest {
	t.Helper()
	manifest := &Manifest{
		Version:    1,
		JobID:      s.jobID,
		SnapshotID: s.snapshotID,
		PackageID:  s.packageID,
		FileName:   s.fileName,
	}
	for _, file := range s.files {
		manifest.Files = append(manifest.Files, ManifestFile{
			Name:   file.name,
			Size:   int64(len(file.data)),
			SHA256: digestBytes(file.data),
		})
	}
	return manifest
}

// buildPackage builds a real package: the embedded manifest carries identity
// and the declared files, and the returned completion manifest additionally
// carries the settled archive size and digest.
func buildPackage(t *testing.T, spec packageSpec) ([]byte, *Manifest) {
	t.Helper()
	identity := spec.manifest(t) // Size and SHA256 deliberately left zero
	archive := &archiveBuilder{}
	for _, file := range spec.files {
		archive.add(file.name, file.data)
	}
	archive.add(DefaultManifestName, mustMarshal(t, identity))
	data := archive.bytes(t)

	completion := spec.manifest(t)
	completion.Size = int64(len(data))
	completion.SHA256 = digestBytes(data)
	if err := completion.Validate(); err != nil {
		t.Fatalf("built completion manifest is invalid: %s", err)
	}
	return data, completion
}

func mustMarshal(t *testing.T, value interface{}) []byte {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %s", err)
	}
	return append(data, '\n')
}

// canonicalPackage is the standard fixture used by most tests: one CSV file
// with a header, quoted newline, duplicate row, zero, FALSE, NULL and empty
// text all represented.
func canonicalSchema() *Schema {
	return &Schema{
		Version: 1,
		Files: []FileSchema{
			{
				Name:   "orders.csv",
				Header: true,
				Columns: []Column{
					{Name: "order_id", Type: IntegerType},
					{Name: "note", Type: TextType, EmptyIsText: true},
					{Name: "paid", Type: BooleanType},
					{Name: "discount", Type: IntegerType},
				},
			},
		},
	}
}

const canonicalCSV = "order_id,note,paid,discount\n" +
	"1,\"first\nline\",true,0\n" +
	"2,plain,false,\n" +
	"1,\"first\nline\",true,0\n" +
	"3,,TRUE,7\n"

func canonicalSpec() packageSpec {
	return packageSpec{
		jobID:      "job-A",
		snapshotID: "snap-1",
		packageID:  "pkg-1",
		fileName:   "report.zip",
		files:      []packageFileSpec{{name: "orders.csv", data: []byte(canonicalCSV)}},
	}
}

// verifyCanonical asserts the semantic content that must survive decoding.
func verifyCanonical(t *testing.T, pkg *Package) {
	t.Helper()
	if len(pkg.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(pkg.Files))
	}
	records := pkg.Files[0].Records
	if len(records) != 4 {
		t.Fatalf("expected 4 records (duplicates preserved), got %d: %+v", len(records), records)
	}
	// Quoted newline preserved inside a single logical record.
	if got := records[0].Values[1].Text; got != "first\nline" {
		t.Errorf("quoted newline: got %q", got)
	}
	// Duplicate row preserved verbatim.
	if records[0].Values[0].Int != records[2].Values[0].Int || records[0].Values[1].Text != records[2].Values[1].Text {
		t.Errorf("duplicate row was not preserved")
	}
	// Integer zero is not NULL.
	zero := records[0].Values[3]
	if zero.Null || zero.Int != 0 {
		t.Errorf("integer zero: got %+v", zero)
	}
	// Unquoted empty integer is NULL, not zero.
	nullInt := records[1].Values[3]
	if !nullInt.Null {
		t.Errorf("empty integer should be NULL, got %+v", nullInt)
	}
	// Boolean FALSE is not NULL.
	falseValue := records[1].Values[2]
	if falseValue.Null || falseValue.Bool != false {
		t.Errorf("boolean false: got %+v", falseValue)
	}
	// Unquoted empty text with EmptyIsText is empty text, not NULL.
	emptyText := records[3].Values[1]
	if emptyText.Null || emptyText.Text != "" {
		t.Errorf("empty text with EmptyIsText: got %+v", emptyText)
	}
}

func readRecords(pkg *Package) []Record {
	if len(pkg.Files) == 0 {
		return nil
	}
	return pkg.Files[0].Records
}

func recordStrings(records []Record) []string {
	out := make([]string, len(records))
	for i, record := range records {
		var values []string
		for _, v := range record.Values {
			values = append(values, v.String())
		}
		out[i] = fmt.Sprint(values)
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
