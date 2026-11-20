package report

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestStore returns a store in a fresh temporary directory. The caller is
// responsible for nothing: the directory is removed when the test binary
// exits, which keeps the Go 1.10 test API usable.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	root, err := ioutil.TempDir("", "report-store-")
	if err != nil {
		t.Fatalf("temp dir: %s", err)
	}
	tempDirs = append(tempDirs, root)
	return &Store{Root: root}
}

var tempDirs []string

func TestMain(m *testing.M) {
	code := m.Run()
	for _, dir := range tempDirs {
		os.RemoveAll(dir)
	}
	os.Exit(code)
}

func TestStoreDeliverAndOpen(t *testing.T) {
	store := newTestStore(t)
	data, manifest := buildCanonical(t)
	pkg, err := store.Deliver("job-A", "https://app.example.com/report.zip", data, manifest, canonicalSchema())
	if err != nil {
		t.Fatalf("deliver: %s", err)
	}
	verifyCanonical(t, pkg)

	opened, err := store.Open("job-A")
	if err != nil {
		t.Fatalf("open: %s", err)
	}
	verifyCanonical(t, opened)
	version, err := store.Current("job-A")
	if err != nil || version == "" {
		t.Fatalf("current version: %q %v", version, err)
	}
}

func TestStoreDeliverRejectsInvalidPackageWithoutTouchingCurrent(t *testing.T) {
	store := newTestStore(t)
	data, manifest := buildCanonical(t)
	if _, err := store.Deliver("job-A", "", data, manifest, canonicalSchema()); err != nil {
		t.Fatalf("first deliver: %s", err)
	}
	before, _ := store.Current("job-A")

	// A second, invalid package must not replace the first.
	bad := append([]byte{}, data...)
	bad[len(bad)-1] ^= 0xff
	if _, err := store.Deliver("job-A", "", bad, manifest, canonicalSchema()); err == nil {
		t.Fatal("expected invalid package to be rejected")
	}
	after, _ := store.Current("job-A")
	if before != after {
		t.Fatalf("current version changed after a failed delivery: %q -> %q", before, after)
	}
	if _, err := store.Open("job-A"); err != nil {
		t.Fatalf("previous version must stay usable: %s", err)
	}
}

func TestStoreKeepsVersionsAndFallsBack(t *testing.T) {
	store := newTestStore(t)

	// Version 1: two records.
	spec1 := canonicalSpec()
	spec1.files[0].data = []byte("order_id,note,paid,discount\n1,a,true,0\n")
	data1, manifest1 := buildPackage(t, spec1)
	if _, err := store.Deliver("job-A", "", data1, manifest1, canonicalSchema()); err != nil {
		t.Fatalf("deliver v1: %s", err)
	}
	v1, _ := store.Current("job-A")

	// Version 2: another package for the same job.
	spec2 := canonicalSpec()
	spec2.packageID = "pkg-2"
	spec2.files[0].data = []byte("order_id,note,paid,discount\n2,b,false,1\n3,c,true,2\n")
	data2, manifest2 := buildPackage(t, spec2)
	if _, err := store.Deliver("job-A", "", data2, manifest2, canonicalSchema()); err != nil {
		t.Fatalf("deliver v2: %s", err)
	}
	v2, _ := store.Current("job-A")
	if v1 == v2 {
		t.Fatal("expected a new version identifier")
	}

	// Corrupt the current version's archive on disk; Open must fall back to v1
	// and repair the pointer.
	dir, _ := store.versionDir("job-A", v2)
	if err := ioutil.WriteFile(filepath.Join(dir, packageFileName), []byte("garbage"), 0644); err != nil {
		t.Fatalf("corrupt: %s", err)
	}
	opened, err := store.Open("job-A")
	if err != nil {
		t.Fatalf("open should fall back: %s", err)
	}
	if len(opened.Files[0].Records) != 1 {
		t.Fatalf("fallback opened the wrong version: %d records", len(opened.Files[0].Records))
	}
	repaired, _ := store.Current("job-A")
	if repaired != v1 {
		t.Fatalf("current pointer was not repaired: %q", repaired)
	}
	// The damaged version must remain for inspection.
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("damaged version should be kept: %s", err)
	}
}

func TestStoreOpenRejectsMissingFileNotJustDirectory(t *testing.T) {
	store := newTestStore(t)
	data, manifest := buildCanonical(t)
	if _, err := store.Deliver("job-A", "", data, manifest, canonicalSchema()); err != nil {
		t.Fatalf("deliver: %s", err)
	}
	version, _ := store.Current("job-A")
	dir, _ := store.versionDir("job-A", version)
	if err := os.Remove(filepath.Join(dir, packageFileName)); err != nil {
		t.Fatalf("remove: %s", err)
	}
	if _, err := store.Open("job-A"); err == nil {
		t.Fatal("open must verify existing files, not just their presence")
	}
}

func TestStoreOpenRejectsTamperedFileWithPreservedSize(t *testing.T) {
	store := newTestStore(t)
	data, manifest := buildCanonical(t)
	if _, err := store.Deliver("job-A", "", data, manifest, canonicalSchema()); err != nil {
		t.Fatalf("deliver: %s", err)
	}
	version, _ := store.Current("job-A")
	dir, _ := store.versionDir("job-A", version)
	tampered := append([]byte{}, data...)
	tampered[len(tampered)/2] ^= 0xff
	if err := ioutil.WriteFile(filepath.Join(dir, packageFileName), tampered, 0644); err != nil {
		t.Fatalf("tamper: %s", err)
	}
	if _, err := store.Open("job-A"); err == nil {
		t.Fatal("open must re-verify digests")
	}
}

func TestStoreSeparatesDifferentJobsWithSameFileName(t *testing.T) {
	store := newTestStore(t)
	// Two jobs produce byte-identical file names and content; they must not
	// share a version directory or a current pointer.
	specA := canonicalSpec()
	dataA, manifestA := buildPackage(t, specA)
	specB := canonicalSpec()
	specB.jobID = "job-B"
	dataB, manifestB := buildPackage(t, specB)

	if _, err := store.Deliver("job-A", "", dataA, manifestA, canonicalSchema()); err != nil {
		t.Fatalf("deliver A: %s", err)
	}
	if _, err := store.Deliver("job-B", "", dataB, manifestB, canonicalSchema()); err != nil {
		t.Fatalf("deliver B: %s", err)
	}
	vA, _ := store.Current("job-A")
	vB, _ := store.Current("job-B")
	if vA == "" || vB == "" {
		t.Fatal("both jobs must have versions")
	}
	dirA, _ := store.versionDir("job-A", vA)
	dirB, _ := store.versionDir("job-B", vB)
	if dirA == dirB {
		t.Fatal("different jobs must not share a version directory")
	}
	openA, err := store.Open("job-A")
	if err != nil {
		t.Fatalf("open A: %s", err)
	}
	if openA.JobID != "job-A" {
		t.Fatalf("job A opened %q", openA.JobID)
	}
	openB, err := store.Open("job-B")
	if err != nil {
		t.Fatalf("open B: %s", err)
	}
	if openB.JobID != "job-B" {
		t.Fatalf("job B opened %q", openB.JobID)
	}
}

func TestStoreRejectsTraversalJobID(t *testing.T) {
	store := newTestStore(t)
	data, manifest := buildCanonical(t)
	for _, jobID := range []string{"../escape", "a/b", "", ".", ".."} {
		if _, err := store.Deliver(jobID, "", data, manifest, canonicalSchema()); err == nil {
			t.Errorf("job ID %q should be rejected", jobID)
		}
	}
}

func TestStoreDeliverIsIdempotent(t *testing.T) {
	store := newTestStore(t)
	data, manifest := buildCanonical(t)
	if _, err := store.Deliver("job-A", "", data, manifest, canonicalSchema()); err != nil {
		t.Fatalf("deliver: %s", err)
	}
	first, _ := store.Versions("job-A")
	if _, err := store.Deliver("job-A", "", data, manifest, canonicalSchema()); err != nil {
		t.Fatalf("re-deliver: %s", err)
	}
	second, _ := store.Versions("job-A")
	if len(second) != len(first) {
		t.Fatalf("identical re-delivery should not add a version: %v -> %v", first, second)
	}
}

func TestStoreNoPackageOpenFails(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Open("job-none"); err == nil {
		t.Fatal("expected open of an unknown job to fail")
	}
}

func TestStoreRejectsCorruptStoredSchema(t *testing.T) {
	store := newTestStore(t)
	data, manifest := buildCanonical(t)
	if _, err := store.Deliver("job-A", "", data, manifest, canonicalSchema()); err != nil {
		t.Fatalf("deliver: %s", err)
	}
	version, _ := store.Current("job-A")
	dir, _ := store.versionDir("job-A", version)
	if err := ioutil.WriteFile(filepath.Join(dir, schemaFileName), []byte("{not json"), 0644); err != nil {
		t.Fatalf("corrupt: %s", err)
	}
	_, err := store.Open("job-A")
	if err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("expected a schema error, got %v", err)
	}
}
