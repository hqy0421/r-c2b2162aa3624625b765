package report

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

func buildCanonical(t *testing.T) ([]byte, *Manifest) {
	t.Helper()
	return buildPackage(t, canonicalSpec())
}

func TestVerifyArchiveAcceptsCanonicalPackage(t *testing.T) {
	data, manifest := buildCanonical(t)
	pkg, err := VerifyArchive(data, manifest, canonicalSchema())
	if err != nil {
		t.Fatalf("verify: %s", err)
	}
	if pkg.JobID != "job-A" || pkg.SnapshotID != "snap-1" || pkg.PackageID != "pkg-1" {
		t.Fatalf("identity not bound: %+v", pkg)
	}
	verifyCanonical(t, pkg)
}

func TestVerifyArchiveRejectsTruncatedPackage(t *testing.T) {
	data, manifest := buildCanonical(t)
	if _, err := VerifyArchive(data[:len(data)-10], manifest, canonicalSchema()); err == nil {
		t.Fatal("expected truncated package to be rejected")
	}
}

func TestVerifyArchiveRejectsWrongDigest(t *testing.T) {
	data, manifest := buildCanonical(t)
	manifest.SHA256 = strings.Repeat("0", 64)
	if _, err := VerifyArchive(data, manifest, canonicalSchema()); err == nil {
		t.Fatal("expected wrong package digest to be rejected")
	}
}

func TestVerifyArchiveRejectsWrongSize(t *testing.T) {
	data, manifest := buildCanonical(t)
	manifest.Size += 1
	if _, err := VerifyArchive(data, manifest, canonicalSchema()); err == nil {
		t.Fatal("expected wrong package size to be rejected")
	}
}

func TestVerifyArchiveRejectsNonZIPBytes(t *testing.T) {
	manifest := canonicalSpec().manifest(t)
	payload := []byte("<html>login</html>")
	manifest.Size = int64(len(payload))
	manifest.SHA256 = digestBytes(payload)
	if _, err := VerifyArchive(payload, manifest, canonicalSchema()); err == nil {
		t.Fatal("expected non-ZIP bytes to be rejected")
	}
}

func TestVerifyArchiveRejectsMissingMember(t *testing.T) {
	spec := canonicalSpec()
	data, _ := buildPackage(t, spec)
	// Remove the CSV member but keep manifest.json and re-declare both files
	// as if they were present.
	rebuilt := &archiveBuilder{}
	rebuilt.add(DefaultManifestName, mustMarshal(t, spec.manifest(t)))
	short := rebuilt.bytes(t)
	manifest := spec.manifest(t)
	manifest.Size = int64(len(short))
	manifest.SHA256 = digestBytes(short)
	// The manifest still declares orders.csv, which the archive lacks.
	if _, err := VerifyArchive(short, manifest, canonicalSchema()); err == nil {
		t.Fatal("expected missing member to be rejected")
	}
	_ = data
}

func TestVerifyArchiveRejectsExtraMember(t *testing.T) {
	spec := canonicalSpec()
	spec.files = append(spec.files, packageFileSpec{name: "extra.csv", data: []byte("a\n1\n")})
	data, _ := buildPackage(t, spec)
	// Verify against a manifest that does not declare extra.csv.
	declared := canonicalSpec().manifest(t)
	declared.Size = int64(len(data))
	declared.SHA256 = digestBytes(data)
	if _, err := VerifyArchive(data, declared, canonicalSchema()); err == nil {
		t.Fatal("expected undeclared member to be rejected")
	}
}

func TestVerifyArchiveRejectsWrongMemberDigest(t *testing.T) {
	spec := canonicalSpec()
	data, manifest := buildPackage(t, spec)
	manifest.Files[0].SHA256 = strings.Repeat("1", 64)
	if _, err := VerifyArchive(data, manifest, canonicalSchema()); err == nil {
		t.Fatal("expected wrong member digest to be rejected")
	}
}

func TestVerifyArchiveRejectsWrongMemberSize(t *testing.T) {
	data, manifest := buildCanonical(t)
	manifest.Files[0].Size += 5
	if _, err := VerifyArchive(data, manifest, canonicalSchema()); err == nil {
		t.Fatal("expected wrong member size to be rejected")
	}
}

func TestVerifyArchiveRejectsCrossJobPackage(t *testing.T) {
	data, manifest := buildCanonical(t)
	// A different job's completion manifest that happens to describe the same
	// bytes (same file name, same content) must still be refused because the
	// embedded manifest names the real job.
	wrong := *manifest
	wrong.JobID = "job-B"
	if _, err := VerifyArchive(data, &wrong, canonicalSchema()); err == nil {
		t.Fatal("expected cross-job package to be rejected")
	}
}

func TestVerifyArchiveRejectsSchemaMemberNotDeclared(t *testing.T) {
	spec := canonicalSpec()
	spec.files = append(spec.files, packageFileSpec{name: "notes.csv", data: []byte("a\n1\n")})
	data, manifest := buildPackage(t, spec)
	// Schema only declares orders.csv.
	if _, err := VerifyArchive(data, manifest, canonicalSchema()); err == nil {
		t.Fatal("expected undeclared-schema member to be rejected")
	}
}

func TestVerifyArchiveRejectsSchemaFileMissing(t *testing.T) {
	schema := canonicalSchema()
	schema.Files = append(schema.Files, FileSchema{
		Name:    "other.csv",
		Columns: []Column{{Name: "a"}},
	})
	data, manifest := buildCanonical(t)
	if _, err := VerifyArchive(data, manifest, schema); err == nil {
		t.Fatal("expected missing schema file to be rejected")
	}
}

func TestVerifyArchiveRejectsUnsafeMemberName(t *testing.T) {
	spec := canonicalSpec()
	spec.files = []packageFileSpec{{name: "../escape.csv", data: []byte("a\n1\n")}}
	identity := spec.manifest(t)
	// The fixture builder refuses to produce an invalid manifest, so build the
	// archive directly and construct the completion manifest by hand.
	archive := &archiveBuilder{}
	archive.add("../escape.csv", []byte("a\n1\n"))
	archive.add(DefaultManifestName, mustMarshal(t, identity))
	data := archive.bytes(t)
	manifest := canonicalSpec().manifest(t)
	manifest.Files[0].Name = "../escape.csv"
	manifest.Files[0].Size = int64(len("a\n1\n"))
	manifest.Files[0].SHA256 = digestBytes([]byte("a\n1\n"))
	manifest.Size = int64(len(data))
	manifest.SHA256 = digestBytes(data)
	if err := manifest.Validate(); err == nil {
		t.Fatal("expected the fixture's own unsafe name to be invalid")
	}
	if _, err := VerifyArchive(data, manifest, canonicalSchema()); err == nil {
		t.Fatal("expected unsafe member name to be rejected")
	}
}

func TestVerifyArchiveRejectsDuplicateMember(t *testing.T) {
	// Build an archive manually with two identical names.
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	manifestData := mustMarshal(t, canonicalSpec().manifest(t))
	for _, name := range []string{"orders.csv", "orders.csv", DefaultManifestName} {
		w, err := writer.Create(name)
		if err != nil {
			t.Fatalf("create: %s", err)
		}
		w.Write([]byte(canonicalCSV))
		_ = manifestData
	}
	writer.Close()
	data := buffer.Bytes()
	manifest := canonicalSpec().manifest(t)
	manifest.Size = int64(len(data))
	manifest.SHA256 = digestBytes(data)
	if _, err := VerifyArchive(data, manifest, canonicalSchema()); err == nil {
		t.Fatal("expected duplicate member to be rejected")
	}
}

func TestVerifyArchiveRejectsManifestOnlyInArchiveMismatch(t *testing.T) {
	spec := canonicalSpec()
	spec.packageID = "pkg-1"
	data, manifest := buildPackage(t, spec)
	// Completion manifest claims a different snapshot than the embedded copy.
	manifest.SnapshotID = "snap-other"
	if _, err := VerifyArchive(data, manifest, canonicalSchema()); err == nil {
		t.Fatal("expected embedded/completion mismatch to be rejected")
	}
}

func TestVerifyArchiveRequiresSchemaDecodingBeforeSuccess(t *testing.T) {
	spec := canonicalSpec()
	spec.files = []packageFileSpec{{name: "orders.csv", data: []byte("order_id,note,paid,discount\nnot-an-int,x,true,1\n")}}
	data, manifest := buildPackage(t, spec)
	if _, err := VerifyArchive(data, manifest, canonicalSchema()); err == nil {
		t.Fatal("expected a CSV type error to fail verification")
	}
}

func TestManifestValidateRejectsBadDigests(t *testing.T) {
	cases := []func(*Manifest){
		func(m *Manifest) { m.Version = 2 },
		func(m *Manifest) { m.JobID = "../job" },
		func(m *Manifest) { m.SHA256 = "xyz" },
		func(m *Manifest) { m.Files = nil },
		func(m *Manifest) { m.Files[0].SHA256 = "not-hex" },
		func(m *Manifest) { m.Files[0].Name = DefaultManifestName },
	}
	for i, mutate := range cases {
		spec := canonicalSpec()
		manifest := spec.manifest(t)
		manifest.Size = 100
		manifest.SHA256 = strings.Repeat("a", 64)
		mutate(manifest)
		if err := manifest.Validate(); err == nil {
			t.Errorf("case %d: expected invalid manifest", i)
		}
	}
}
