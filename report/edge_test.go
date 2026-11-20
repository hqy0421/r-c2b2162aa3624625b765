package report

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManifestRejectsTrailingData(t *testing.T) {
	_, manifest := buildCanonical(t)
	data := mustMarshal(t, manifest)
	data = append(data, []byte("{\"extra\":true}")...)
	if _, err := ParseManifest(data); err == nil {
		t.Fatal("expected trailing data to be rejected")
	}
}

func TestSchemaRoundTrip(t *testing.T) {
	schema := canonicalSchema()
	data, err := MarshalSchema(schema)
	if err != nil {
		t.Fatalf("marshal: %s", err)
	}
	parsed, err := ParseSchema(data)
	if err != nil {
		t.Fatalf("parse: %s", err)
	}
	if err := parsed.Validate(); err != nil {
		t.Fatalf("validate: %s", err)
	}
	if len(parsed.Files) != 1 || len(parsed.Files[0].Columns) != 4 {
		t.Fatalf("round trip lost data: %+v", parsed)
	}
}

func TestSchemaValidation(t *testing.T) {
	cases := map[string]*Schema{
		"nil files":        {Files: nil},
		"empty columns":    {Files: []FileSchema{{Name: "a.csv"}}},
		"duplicate file":   {Files: []FileSchema{{Name: "a.csv", Columns: []Column{{Name: "x"}}}, {Name: "a.csv", Columns: []Column{{Name: "x"}}}}},
		"unsafe name":      {Files: []FileSchema{{Name: "../a.csv", Columns: []Column{{Name: "x"}}}}},
		"manifest collide": {Files: []FileSchema{{Name: DefaultManifestName, Columns: []Column{{Name: "x"}}}}},
		"bad delimiter":    {Files: []FileSchema{{Name: "a.csv", Delimiter: "ab", Columns: []Column{{Name: "x"}}}}},
		"quote delimiter":  {Files: []FileSchema{{Name: "a.csv", Delimiter: "\"", Columns: []Column{{Name: "x"}}}}},
		"empty column":     {Files: []FileSchema{{Name: "a.csv", Columns: []Column{{Name: ""}}}}},
		"duplicate column": {Files: []FileSchema{{Name: "a.csv", Columns: []Column{{Name: "x"}, {Name: "x"}}}}},
		"bad type":         {Files: []FileSchema{{Name: "a.csv", Columns: []Column{{Name: "x", Type: ColumnType("float")}}}}},
		"empty_is_text on int": {Files: []FileSchema{{Name: "a.csv", Columns: []Column{{
			Name: "x", Type: IntegerType, EmptyIsText: true,
		}}}}},
		"half boolean tokens": {Files: []FileSchema{{Name: "a.csv", Columns: []Column{{
			Name: "x", Type: BooleanType, TrueTokens: []string{"Y"},
		}}}}},
	}
	for name, schema := range cases {
		if err := schema.Validate(); err == nil {
			t.Errorf("%s: expected validation failure", name)
		}
	}
}

func TestVerifyRejectsDuplicateMemberNamesCaseInsensitively(t *testing.T) {
	// Two members differing only by case are ambiguous on case-insensitive
	// file systems and must be refused.
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, name := range []string{"Orders.csv", "orders.csv", DefaultManifestName} {
		w, err := writer.Create(name)
		if err != nil {
			t.Fatalf("create: %s", err)
		}
		w.Write([]byte(canonicalCSV))
	}
	writer.Close()
	data := buffer.Bytes()
	manifest := canonicalSpec().manifest(t)
	manifest.Size = int64(len(data))
	manifest.SHA256 = digestBytes(data)
	if _, err := VerifyArchive(data, manifest, canonicalSchema()); err == nil {
		t.Fatal("expected case-insensitive duplicate members to be rejected")
	}
}

func TestVerifyRejectsEncryptedOrUnsupportedMembers(t *testing.T) {
	// ZipCrypto is flagged by the encrypted bit; use the stored flag directly.
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	header := &zip.FileHeader{Name: "orders.csv", Method: zip.Store, Flags: 0x1}
	w, err := writer.CreateHeader(header)
	if err != nil {
		t.Fatalf("create: %s", err)
	}
	w.Write([]byte(canonicalCSV))
	writer.Close()
	data := buffer.Bytes()
	manifest := canonicalSpec().manifest(t)
	manifest.Size = int64(len(data))
	manifest.SHA256 = digestBytes(data)
	if _, err := VerifyArchive(data, manifest, canonicalSchema()); err == nil {
		t.Fatal("expected an encrypted member to be rejected")
	}
}

func TestVerifyRejectsDirectoryEntry(t *testing.T) {
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	writer.Create("orders.csv")
	writer.Create("dir/")
	writer.Create(DefaultManifestName)
	writer.Close()
	data := buffer.Bytes()
	manifest := canonicalSpec().manifest(t)
	manifest.Size = int64(len(data))
	manifest.SHA256 = digestBytes(data)
	if _, err := VerifyArchive(data, manifest, canonicalSchema()); err == nil {
		t.Fatal("expected a directory entry to be rejected")
	}
}

func TestStoreVersionsAreContentAddressed(t *testing.T) {
	store := newTestStore(t)
	spec := canonicalSpec()
	data, manifest := buildPackage(t, spec)
	result, err := store.Deliver("job-A", "", data, manifest, canonicalSchema())
	if err != nil {
		t.Fatalf("deliver: %s", err)
	}
	_ = result
	versions, err := store.Versions("job-A")
	if err != nil || len(versions) != 1 {
		t.Fatalf("versions: %v %v", versions, err)
	}
	if len(versions[0]) != 32 {
		t.Fatalf("version should be a content-addressed token, got %q", versions[0])
	}
	if strings.Contains(versions[0], "report") {
		t.Fatalf("version must not be derived from the file name: %q", versions[0])
	}
}

func TestStoreRejectsSameFileNameDifferentJobsButAllowsSameJob(t *testing.T) {
	store := newTestStore(t)
	// Same file bytes, same file name, two different jobs: independent.
	data, manifest := buildCanonical(t)
	if manifest.JobID != "job-A" {
		t.Fatalf("fixture job changed: %q", manifest.JobID)
	}
	if _, err := store.Deliver("job-A", "", data, manifest, canonicalSchema()); err != nil {
		t.Fatalf("job-A: %s", err)
	}
	// A completion manifest for job-A must not be filed under another job,
	// even when the archive and file name are identical.
	if _, err := store.Deliver("job-B", "", data, manifest, canonicalSchema()); err == nil {
		t.Fatal("a manifest bound to job-A must not be delivered as job-B")
	}
	if _, err := store.Open("job-B"); err == nil {
		t.Fatal("job-B must have no package")
	}
	// job-A remains intact.
	if _, err := store.Open("job-A"); err != nil {
		t.Fatalf("job-A must stay openable: %s", err)
	}
}

func TestOpenVersionRejectsUnknownOrTraversalVersion(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.OpenVersion("job-A", "../escape"); err == nil {
		t.Fatal("expected traversal version to be rejected")
	}
	if _, err := store.OpenVersion("job-A", "deadbeef"); err == nil {
		t.Fatal("expected unknown version to fail")
	}
}

func TestStoreDeliverRequiresValidSchema(t *testing.T) {
	store := newTestStore(t)
	data, manifest := buildCanonical(t)
	if _, err := store.Deliver("job-A", "", data, manifest, nil); err == nil {
		t.Fatal("expected nil schema to be rejected")
	}
}

func TestOpenReportReturnsTypedView(t *testing.T) {
	store := newTestStore(t)
	data, manifest := buildCanonical(t)
	if _, err := store.Deliver("job-A", "", data, manifest, canonicalSchema()); err != nil {
		t.Fatalf("deliver: %s", err)
	}
	report, err := store.OpenReport("job-A")
	if err != nil {
		t.Fatalf("open report: %s", err)
	}
	if report.RecordCount != 4 {
		t.Fatalf("unexpected record count: %d", report.RecordCount)
	}
}

// An embedded manifest that disagrees with the completion manifest about the
// declared file list must be refused even when the archive bytes otherwise
// verify.
func TestVerifyRejectsEmbeddedManifestFileListMismatch(t *testing.T) {
	spec := canonicalSpec()
	embedded := spec.manifest(t)
	embedded.Files = append(embedded.Files, ManifestFile{Name: "phantom.csv", Size: 1, SHA256: strings.Repeat("a", 64)})
	archive := &archiveBuilder{}
	for _, file := range spec.files {
		archive.add(file.name, file.data)
	}
	archive.add(DefaultManifestName, mustMarshal(t, embedded))
	data := archive.bytes(t)
	manifest := spec.manifest(t)
	manifest.Size = int64(len(data))
	manifest.SHA256 = digestBytes(data)
	if _, err := VerifyArchive(data, manifest, canonicalSchema()); err == nil {
		t.Fatal("expected embedded manifest mismatch to be rejected")
	}
}

func TestStoreDirectoryLayout(t *testing.T) {
	store := newTestStore(t)
	data, manifest := buildCanonical(t)
	if _, err := store.Deliver("job-A", "", data, manifest, canonicalSchema()); err != nil {
		t.Fatalf("deliver: %s", err)
	}
	version, _ := store.Current("job-A")
	dir, _ := store.versionDir("job-A", version)
	for _, name := range []string{packageFileName, manifestFileName, schemaFileName, metaFileName} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("missing %s: %s", name, err)
		}
	}
	// No staging directories may be left behind.
	entries, _ := ioutil.ReadDir(filepath.Join(store.Root, "job-A", "versions"))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".staging") {
			t.Errorf("staging directory left behind: %s", entry.Name())
		}
	}
	metaData, err := ioutil.ReadFile(filepath.Join(dir, metaFileName))
	if err != nil {
		t.Fatalf("meta: %s", err)
	}
	var meta StoreMeta
	if err := json.Unmarshal(metaData, &meta); err != nil {
		t.Fatalf("meta json: %s", err)
	}
	if meta.JobID != "job-A" || meta.SHA256 != manifest.SHA256 {
		t.Fatalf("unexpected meta: %+v", meta)
	}
}
