package report

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// DefaultManifestName is the member of the package that carries the completion
// manifest. It is not listed among its own file entries.
const DefaultManifestName = "manifest.json"

// ManifestFile is one declared member of a report package.
type ManifestFile struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest is the completion manifest for a finished export job. It binds the
// job identity to the exact bytes that must be delivered.
type Manifest struct {
	Version    int            `json:"version"`
	JobID      string         `json:"job_id"`
	SnapshotID string         `json:"snapshot_id"`
	PackageID  string         `json:"package_id"`
	FileName   string         `json:"file_name"`
	Size       int64          `json:"size"`
	SHA256     string         `json:"sha256"`
	Files      []ManifestFile `json:"files"`
}

// ParseManifest decodes and validates a completion manifest. The completion
// manifest is the one the exporting site reports once a job finishes; it
// includes the package's own size and digest, so it cannot be the copy stored
// inside the archive (that would be self-referential).
func ParseManifest(data []byte) (*Manifest, error) {
	manifest, err := decodeManifest(data)
	if err != nil {
		return nil, err
	}
	if manifest.Size < 0 {
		return nil, fmt.Errorf("report: completion manifest size %d is negative", manifest.Size)
	}
	if !validSHA256(manifest.SHA256) {
		return nil, fmt.Errorf("report: completion manifest package sha256 %q is not 64 lowercase hex characters", manifest.SHA256)
	}
	return manifest, nil
}

// parseEmbeddedManifest decodes the manifest member inside the archive. That
// copy states the job identity and the declared files but deliberately does
// not state the archive's own size and digest.
func parseEmbeddedManifest(data []byte) (*Manifest, error) {
	return decodeManifest(data)
}

func decodeManifest(data []byte) (*Manifest, error) {
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("report: parse completion manifest: %s", err)
	}
	// Reject trailing data after the manifest object: a truncation or a
	// concatenation of two documents must not be accepted as a whole manifest.
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("report: completion manifest has trailing data")
	}
	if err := manifest.validateIdentity(); err != nil {
		return nil, err
	}
	return &manifest, nil
}

// Validate checks the completion manifest's full internal consistency,
// including the package size and digest.
func (m *Manifest) Validate() error {
	if m == nil {
		return errors.New("report: completion manifest is nil")
	}
	if err := m.validateIdentity(); err != nil {
		return err
	}
	if m.Size < 0 {
		return fmt.Errorf("report: completion manifest size %d is negative", m.Size)
	}
	if !validSHA256(m.SHA256) {
		return fmt.Errorf("report: completion manifest package sha256 %q is not 64 lowercase hex characters", m.SHA256)
	}
	return nil
}

// validateIdentity checks everything about a manifest except the archive's own
// size and digest, which are the only fields a manifest member inside the
// archive cannot truthfully carry.
func (m *Manifest) validateIdentity() error {
	if m.Version != 1 {
		return fmt.Errorf("report: unsupported completion manifest version %d", m.Version)
	}
	if err := validateJobID(m.JobID); err != nil {
		return err
	}
	if err := validateIdentifier("snapshot_id", m.SnapshotID); err != nil {
		return err
	}
	if err := validateIdentifier("package_id", m.PackageID); err != nil {
		return err
	}
	if m.FileName == "" {
		return errors.New("report: completion manifest has no file_name")
	}
	if err := validateBaseName(m.FileName); err != nil {
		return fmt.Errorf("report: completion manifest file_name: %s", err)
	}
	if len(m.Files) == 0 {
		return errors.New("report: completion manifest declares no files")
	}
	seen := map[string]bool{}
	for i := range m.Files {
		file := &m.Files[i]
		if err := validateMemberName(file.Name); err != nil {
			return fmt.Errorf("report: completion manifest file %d: %s", i, err)
		}
		if file.Name == DefaultManifestName {
			return fmt.Errorf("report: completion manifest must not list %q among its files", DefaultManifestName)
		}
		key := strings.ToLower(file.Name)
		if seen[key] {
			return fmt.Errorf("report: completion manifest lists %q more than once", file.Name)
		}
		seen[key] = true
		if file.Size < 0 {
			return fmt.Errorf("report: completion manifest file %q has negative size", file.Name)
		}
		if !validSHA256(file.SHA256) {
			return fmt.Errorf("report: completion manifest file %q has invalid sha256", file.Name)
		}
	}
	return nil
}

func validateJobID(id string) error {
	if id == "" {
		return errors.New("report: job ID is empty")
	}
	if len(id) > 256 {
		return errors.New("report: job ID is too long")
	}
	for _, r := range id {
		allowed := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '-' || r == '_' || r == '.' || r == ':'
		if !allowed {
			return fmt.Errorf("report: job ID %q contains the disallowed character %q", id, r)
		}
	}
	if id == "." || id == ".." {
		return fmt.Errorf("report: job ID %q is not usable as a directory name", id)
	}
	return nil
}

func validateIdentifier(field, value string) error {
	if value == "" {
		return fmt.Errorf("report: %s is empty", field)
	}
	if len(value) > 256 {
		return fmt.Errorf("report: %s is too long", field)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("report: %s contains a control character", field)
		}
	}
	return nil
}

func validSHA256(digest string) bool {
	if len(digest) != 64 {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil && strings.ToLower(digest) == digest
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
