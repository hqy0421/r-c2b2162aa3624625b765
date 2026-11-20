package report

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Store keeps delivered report packages on disk. Packages are immutable and
// versioned per job; a separate CURRENT pointer decides which version Open
// returns. Nothing about a package is trusted just because it exists: Open
// re-verifies the bytes it finds.
type Store struct {
	Root string
}

const (
	packageFileName  = "package.zip"
	manifestFileName = "manifest.json"
	schemaFileName   = "schema.json"
	metaFileName     = "meta.json"
	currentFileName  = "CURRENT"
)

// StoreMeta records where a delivered version came from. It is written last,
// inside the version directory, so its presence marks a completed delivery.
type StoreMeta struct {
	JobID       string `json:"job_id"`
	SnapshotID  string `json:"snapshot_id"`
	PackageID   string `json:"package_id"`
	FileName    string `json:"file_name"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	DownloadURL string `json:"download_url"`
	DeliveredAt string `json:"delivered_at"`
}

// versionID derives the on-disk version name from the job and package
// identity. Two different jobs can never share a version directory even when
// their archives have the same file name.
func versionID(jobID, packageID, sha string) string {
	digest := digestBytes([]byte(jobID + "\x00" + packageID + "\x00" + sha))
	return digest[:32]
}

// jobDir returns the directory holding every version of one job.
func (s *Store) jobDir(jobID string) (string, error) {
	if err := validateJobID(jobID); err != nil {
		return "", err
	}
	return filepath.Join(s.Root, jobID), nil
}

func (s *Store) versionDir(jobID, version string) (string, error) {
	dir, err := s.jobDir(jobID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "versions", version), nil
}

// Deliver verifies a downloaded package and, only if every check succeeds,
// installs it as the job's current version.
//
// The new version is assembled in a private staging directory, re-verified
// there from the bytes that were actually written, and then renamed into
// place. The CURRENT pointer is replaced last. Any failure before that final
// rename leaves the previous version untouched and current.
//
// A version directory is named after the job identity and the package digest,
// never after the archive's file name, so two jobs that both export
// "report.zip" cannot overwrite each other.
func (s *Store) Deliver(jobID, downloadURL string, data []byte, manifest *Manifest, schema *Schema) (*Package, error) {
	if s == nil || s.Root == "" {
		return nil, errors.New("report: store has no root")
	}
	if err := validateJobID(jobID); err != nil {
		return nil, err
	}
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	// The directory a package is filed under must be the job the completion
	// manifest names; otherwise one job's verified bytes would be readable as
	// another job's report.
	if manifest.JobID != jobID {
		return nil, fmt.Errorf("report: completion manifest names job %s, not %s", manifest.JobID, jobID)
	}
	// Verify the caller's bytes before touching the disk.
	pkg, err := VerifyArchive(data, manifest, schema)
	if err != nil {
		return nil, err
	}
	schemaData, err := MarshalSchema(schema)
	if err != nil {
		return nil, err
	}
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("report: marshal completion manifest: %s", err)
	}
	manifestData = append(manifestData, '\n')
	meta := StoreMeta{
		JobID:       jobID,
		SnapshotID:  manifest.SnapshotID,
		PackageID:   manifest.PackageID,
		FileName:    manifest.FileName,
		Size:        manifest.Size,
		SHA256:      manifest.SHA256,
		DownloadURL: downloadURL,
		DeliveredAt: nowRFC3339(),
	}
	metaData, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("report: marshal delivery metadata: %s", err)
	}
	metaData = append(metaData, '\n')

	version := versionID(jobID, manifest.PackageID, manifest.SHA256)
	dir, err := s.jobDir(jobID)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("report: create job directory: %s", err)
	}
	versionsDir := filepath.Join(dir, "versions")
	if err := os.MkdirAll(versionsDir, 0755); err != nil {
		return nil, fmt.Errorf("report: create versions directory: %s", err)
	}

	// Named staging directories avoid CreateTemp, which is not available on
	// every platform this package supports.
	staging := filepath.Join(versionsDir, ".staging-"+version+"-"+uniqueSuffix())
	if err := writeStaging(staging, data, manifestData, schemaData, metaData); err != nil {
		os.RemoveAll(staging)
		return nil, err
	}
	defer os.RemoveAll(staging)

	// Re-verify the bytes as written to disk, not the bytes in memory. This
	// catches a short write or a truncated file system before publication.
	written, err := ioutil.ReadFile(filepath.Join(staging, packageFileName))
	if err != nil {
		return nil, fmt.Errorf("report: read staged package: %s", err)
	}
	if _, err := VerifyArchive(written, manifest, schema); err != nil {
		return nil, fmt.Errorf("report: staged package failed re-verification: %s", err)
	}

	finalDir := filepath.Join(versionsDir, version)
	if _, statErr := os.Stat(finalDir); statErr == nil {
		// Re-delivering the same job/package/digest is idempotent. Only reuse
		// the existing version if it still verifies; otherwise replace it so
		// that CURRENT never points at a damaged directory.
		if _, verifyErr := s.openVersion(jobID, version); verifyErr == nil {
			os.RemoveAll(staging)
		} else {
			if err := os.RemoveAll(finalDir); err != nil {
				os.RemoveAll(staging)
				return nil, fmt.Errorf("report: replace damaged version: %s", err)
			}
			if err := os.Rename(staging, finalDir); err != nil {
				return nil, fmt.Errorf("report: publish version: %s", err)
			}
		}
	} else if err := os.Rename(staging, finalDir); err != nil {
		return nil, fmt.Errorf("report: publish version: %s", err)
	}

	if err := s.setCurrent(jobID, version); err != nil {
		return nil, err
	}
	return pkg, nil
}

func writeStaging(dir string, packageData, manifestData, schemaData, metaData []byte) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("report: create staging directory: %s", err)
	}
	for _, file := range []struct {
		name string
		data []byte
	}{
		{packageFileName, packageData},
		{manifestFileName, manifestData},
		{schemaFileName, schemaData},
		{metaFileName, metaData},
	} {
		if err := writeFileSync(filepath.Join(dir, file.name), file.data); err != nil {
			return err
		}
	}
	return nil
}

// writeFileSync writes and fsyncs a file so that a crash cannot leave a
// published file with stale or incomplete contents.
func writeFileSync(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return fmt.Errorf("report: create %s: %s", filepath.Base(path), err)
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return fmt.Errorf("report: write %s: %s", filepath.Base(path), err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("report: sync %s: %s", filepath.Base(path), err)
	}
	return file.Close()
}

func (s *Store) setCurrent(jobID, version string) error {
	dir, err := s.jobDir(jobID)
	if err != nil {
		return err
	}
	target := filepath.Join(dir, currentFileName)
	temp := target + ".tmp-" + uniqueSuffix()
	if err := writeFileSync(temp, []byte(version+"\n")); err != nil {
		os.Remove(temp)
		return err
	}
	if err := os.Rename(temp, target); err != nil {
		os.Remove(temp)
		return fmt.Errorf("report: switch current version: %s", err)
	}
	return nil
}

// Open returns the current verified package for a job. If the current version
// no longer verifies, Open falls back to the newest older version that does,
// repairs CURRENT, and leaves the damaged version in place.
func (s *Store) Open(jobID string) (*Package, error) {
	if s == nil || s.Root == "" {
		return nil, errors.New("report: store has no root")
	}
	dir, err := s.jobDir(jobID)
	if err != nil {
		return nil, err
	}
	versionsDir := filepath.Join(dir, "versions")
	versions, err := listVersions(versionsDir)
	if err != nil {
		return nil, err
	}
	if len(versions) == 0 {
		return nil, fmt.Errorf("report: no delivered package for job %s", jobID)
	}
	current, currentErr := readCurrent(dir)
	order := versions
	if currentErr == nil {
		order = append([]string{current}, remove(versions, current)...)
	}
	var firstErr error
	for _, version := range order {
		pkg, err := s.openVersion(jobID, version)
		if err == nil {
			if currentErr != nil || version != current {
				// Best effort repair; failure here does not invalidate the
				// package that just verified.
				_ = s.setCurrent(jobID, version)
			}
			return pkg, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return nil, fmt.Errorf("report: no usable version for job %s: %s", jobID, firstErr)
}

// OpenVersion opens one specific version, re-verifying it from disk.
func (s *Store) OpenVersion(jobID, version string) (*Package, error) {
	if err := validateVersion(version); err != nil {
		return nil, err
	}
	return s.openVersion(jobID, version)
}

func (s *Store) openVersion(jobID, version string) (*Package, error) {
	dir, err := s.versionDir(jobID, version)
	if err != nil {
		return nil, err
	}
	manifestData, err := ioutil.ReadFile(filepath.Join(dir, manifestFileName))
	if err != nil {
		return nil, fmt.Errorf("report: read stored manifest: %s", err)
	}
	manifest, err := ParseManifest(manifestData)
	if err != nil {
		return nil, err
	}
	if manifest.JobID != jobID {
		return nil, fmt.Errorf("report: stored version belongs to job %s, not %s", manifest.JobID, jobID)
	}
	schemaData, err := ioutil.ReadFile(filepath.Join(dir, schemaFileName))
	if err != nil {
		return nil, fmt.Errorf("report: read stored schema: %s", err)
	}
	schema, err := ParseSchema(schemaData)
	if err != nil {
		return nil, err
	}
	data, err := ioutil.ReadFile(filepath.Join(dir, packageFileName))
	if err != nil {
		return nil, fmt.Errorf("report: read stored package: %s", err)
	}
	// Presence on disk is not enough: re-run the full verification.
	return VerifyArchive(data, manifest, schema)
}

// Versions lists a job's stored versions, newest first by modification time.
func (s *Store) Versions(jobID string) ([]string, error) {
	dir, err := s.jobDir(jobID)
	if err != nil {
		return nil, err
	}
	return listVersions(filepath.Join(dir, "versions"))
}

// Current returns the CURRENT pointer for a job, if one is recorded.
func (s *Store) Current(jobID string) (string, error) {
	dir, err := s.jobDir(jobID)
	if err != nil {
		return "", err
	}
	return readCurrent(dir)
}

// OpenReport opens the current version for a job and returns its independent,
// typed read view. It is the recommended way to consume a delivered package:
// the same full re-verification that Open performs is applied first.
func (s *Store) OpenReport(jobID string) (*Report, error) {
	pkg, err := s.Open(jobID)
	if err != nil {
		return nil, err
	}
	return ReadPackage(pkg)
}

func listVersions(dir string) ([]string, error) {
	entries, err := ioutil.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("report: list versions: %s", err)
	}
	type versionInfo struct {
		name string
		time int64
	}
	var found []versionInfo
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if validateVersion(entry.Name()) != nil {
			continue
		}
		found = append(found, versionInfo{entry.Name(), entry.ModTime().UnixNano()})
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].time == found[j].time {
			return found[i].name > found[j].name
		}
		return found[i].time > found[j].time
	})
	versions := make([]string, len(found))
	for i := range found {
		versions[i] = found[i].name
	}
	return versions, nil
}

func readCurrent(dir string) (string, error) {
	data, err := ioutil.ReadFile(filepath.Join(dir, currentFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return "", errors.New("no current version recorded")
		}
		return "", fmt.Errorf("report: read current version: %s", err)
	}
	version := strings.TrimSpace(string(data))
	if err := validateVersion(version); err != nil {
		return "", fmt.Errorf("report: current version: %s", err)
	}
	return version, nil
}

func validateVersion(version string) error {
	if len(version) != 32 {
		return fmt.Errorf("%q is not a version identifier", version)
	}
	for _, r := range version {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return fmt.Errorf("%q is not a version identifier", version)
		}
	}
	return nil
}

func remove(values []string, value string) []string {
	out := make([]string, 0, len(values))
	for _, candidate := range values {
		if candidate != value {
			out = append(out, candidate)
		}
	}
	return out
}
