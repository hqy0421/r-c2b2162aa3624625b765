package report

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"sort"
	"strings"
)

// maxTotalUncompressed bounds the total uncompressed size of a package so that
// a malicious archive cannot exhaust memory. It is a defence in depth: the
// byte counts in the completion manifest are the primary check, and a manifest
// that declares an oversized file is refused before anything is read.
const maxTotalUncompressed = 1 << 30 // 1 GiB

// maxManifestBytes bounds the completion-manifest member, which must be tiny.
const maxManifestBytes = 8 << 20 // 8 MiB

// Package is a fully verified and decoded report package. Its fields are only
// populated after every file in the archive has matched the completion
// manifest and decoded against the declared schema.
type Package struct {
	JobID      string
	SnapshotID string
	PackageID  string
	FileName   string
	Size       int64
	SHA256     string
	Manifest   *Manifest
	Schema     *Schema
	Files      []PackageFile
}

// PackageFile is one decoded CSV member.
type PackageFile struct {
	Name    string
	Size    int64
	SHA256  string
	Schema  *FileSchema
	Records []Record
}

// VerifyArchive checks downloaded bytes against the completion manifest and
// the declared schema, and decodes every declared file.
//
// The sequence is deliberately fail-closed: identity, then package bytes, then
// member set, then each member's bytes, then decoding. Nothing is returned
// unless every step succeeds.
func VerifyArchive(data []byte, manifest *Manifest, schema *Schema) (*Package, error) {
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	if err := schema.Validate(); err != nil {
		return nil, err
	}

	// 1. The downloaded package itself must be exactly what was declared.
	if int64(len(data)) != manifest.Size {
		return nil, fmt.Errorf("report: package is %d bytes, completion manifest declares %d", len(data), manifest.Size)
	}
	if digest := digestBytes(data); digest != manifest.SHA256 {
		return nil, fmt.Errorf("report: package sha256 is %s, completion manifest declares %s", digest, manifest.SHA256)
	}
	if !bytes.HasPrefix(data, []byte("PK\x03\x04")) {
		return nil, errors.New("report: downloaded bytes are not a ZIP archive")
	}

	// 2. The member set must match the completion manifest exactly.
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("report: open package archive: %s", err)
	}
	members, err := indexMembers(reader)
	if err != nil {
		return nil, err
	}
	if err := checkMemberSet(members, manifest); err != nil {
		return nil, err
	}

	// 3. Every declared file must be present in the declared schema, and
	//    every schema file must be declared by the manifest.
	if err := checkSchemaSet(manifest, schema); err != nil {
		return nil, err
	}

	// 4. Each member's bytes and digest must match the manifest.
	contents := make(map[string][]byte, len(manifest.Files))
	var total int64
	for _, file := range manifest.Files {
		member := members[file.Name]
		uncompressed := int64(member.UncompressedSize64)
		if uncompressed != file.Size {
			return nil, fmt.Errorf("report: member %q is %d bytes, completion manifest declares %d", file.Name, uncompressed, file.Size)
		}
		if total += uncompressed; total > maxTotalUncompressed {
			return nil, fmt.Errorf("report: package declares more than %d uncompressed bytes", int64(maxTotalUncompressed))
		}
		buf, err := readMember(member, file.Size)
		if err != nil {
			return nil, err
		}
		if digest := digestBytes(buf); digest != file.SHA256 {
			return nil, fmt.Errorf("report: member %q sha256 is %s, completion manifest declares %s", file.Name, digest, file.SHA256)
		}
		contents[file.Name] = buf
	}

	// 5. Decode every declared CSV file. Any decoding failure fails delivery.
	pkg := &Package{
		JobID:      manifest.JobID,
		SnapshotID: manifest.SnapshotID,
		PackageID:  manifest.PackageID,
		FileName:   manifest.FileName,
		Size:       manifest.Size,
		SHA256:     manifest.SHA256,
		Manifest:   manifest,
		Schema:     schema,
		Files:      make([]PackageFile, 0, len(schema.Files)),
	}
	for i := range schema.Files {
		fileSchema := &schema.Files[i]
		var declared *ManifestFile
		for j := range manifest.Files {
			if manifest.Files[j].Name == fileSchema.Name {
				declared = &manifest.Files[j]
				break
			}
		}
		records, err := decodeCSV(contents[fileSchema.Name], fileSchema)
		if err != nil {
			return nil, err
		}
		pkg.Files = append(pkg.Files, PackageFile{
			Name:    fileSchema.Name,
			Size:    declared.Size,
			SHA256:  declared.SHA256,
			Schema:  fileSchema,
			Records: records,
		})
	}
	return pkg, nil
}

func indexMembers(reader *zip.Reader) (map[string]*zip.File, error) {
	members := make(map[string]*zip.File, len(reader.File))
	seen := make(map[string]string, len(reader.File))
	for _, member := range reader.File {
		name := member.Name
		if strings.HasSuffix(name, "/") {
			return nil, fmt.Errorf("report: package contains the directory entry %q", name)
		}
		if err := validateMemberName(name); err != nil {
			return nil, fmt.Errorf("report: package contains an unsafe member: %s", err)
		}
		key := strings.ToLower(name)
		if existing, ok := seen[key]; ok {
			return nil, fmt.Errorf("report: package contains duplicate members %q and %q", existing, name)
		}
		seen[key] = name
		if member.Flags&0x1 != 0 {
			return nil, fmt.Errorf("report: member %q is encrypted", name)
		}
		if member.Method != zip.Store && member.Method != zip.Deflate {
			return nil, fmt.Errorf("report: member %q uses unsupported compression method %d", name, member.Method)
		}
		if member.UncompressedSize64 > maxTotalUncompressed {
			return nil, fmt.Errorf("report: member %q is unreasonably large", name)
		}
		members[name] = member
	}
	return members, nil
}

func checkMemberSet(members map[string]*zip.File, manifest *Manifest) error {
	expected := make(map[string]bool, len(manifest.Files)+1)
	expected[DefaultManifestName] = true
	for _, file := range manifest.Files {
		expected[file.Name] = true
	}
	var missing, unexpected []string
	for name := range expected {
		if members[name] == nil {
			missing = append(missing, name)
		}
	}
	for name := range members {
		if !expected[name] {
			unexpected = append(unexpected, name)
		}
	}
	if len(missing) != 0 {
		sort.Strings(missing)
		return fmt.Errorf("report: package is missing %s", quotedList(missing))
	}
	if len(unexpected) != 0 {
		sort.Strings(unexpected)
		return fmt.Errorf("report: package contains undeclared %s", quotedList(unexpected))
	}
	member := members[DefaultManifestName]
	if member.UncompressedSize64 > maxManifestBytes {
		return fmt.Errorf("report: package manifest member is unreasonably large (%d bytes)", member.UncompressedSize64)
	}
	manifestData, err := readMember(member, int64(member.UncompressedSize64))
	if err != nil {
		return err
	}
	embedded, err := parseEmbeddedManifest(manifestData)
	if err != nil {
		return fmt.Errorf("report: package manifest member: %s", err)
	}
	if !sameIdentity(embedded, manifest) {
		return errors.New("report: package manifest does not match the completion manifest that was bound to this job")
	}
	return nil
}

func checkSchemaSet(manifest *Manifest, schema *Schema) error {
	declared := make(map[string]bool, len(manifest.Files))
	for _, file := range manifest.Files {
		declared[file.Name] = true
	}
	schemaFiles := make(map[string]bool, len(schema.Files))
	var missing, unexpected []string
	for i := range schema.Files {
		schemaFiles[schema.Files[i].Name] = true
		if !declared[schema.Files[i].Name] {
			missing = append(missing, schema.Files[i].Name)
		}
	}
	for _, file := range manifest.Files {
		if !schemaFiles[file.Name] {
			unexpected = append(unexpected, file.Name)
		}
	}
	if len(missing) != 0 {
		sort.Strings(missing)
		return fmt.Errorf("report: package is missing schema file %s", quotedList(missing))
	}
	if len(unexpected) != 0 {
		sort.Strings(unexpected)
		return fmt.Errorf("report: package contains file %s that the declared schema does not describe", quotedList(unexpected))
	}
	return nil
}

func readMember(member *zip.File, declared int64) ([]byte, error) {
	reader, err := member.Open()
	if err != nil {
		return nil, fmt.Errorf("report: open member %q: %s", member.Name, err)
	}
	defer reader.Close()
	// Read one byte beyond the declared size so that an archive whose header
	// lies about its uncompressed size is caught rather than truncated.
	data, err := ioutil.ReadAll(io.LimitReader(reader, declared+1))
	if err != nil {
		return nil, fmt.Errorf("report: read member %q: %s", member.Name, err)
	}
	if int64(len(data)) != declared {
		return nil, fmt.Errorf("report: member %q decompressed to %d bytes, completion manifest declares %d", member.Name, len(data), declared)
	}
	return data, nil
}

// sameIdentity compares the identity and declared file list of two manifests.
// It deliberately ignores Size and SHA256, which describe the archive that
// contains the embedded copy and therefore cannot appear in that copy.
func sameIdentity(a, b *Manifest) bool {
	if a.Version != b.Version || a.JobID != b.JobID || a.SnapshotID != b.SnapshotID ||
		a.PackageID != b.PackageID || a.FileName != b.FileName {
		return false
	}
	if len(a.Files) != len(b.Files) {
		return false
	}
	for i := range a.Files {
		if a.Files[i] != b.Files[i] {
			return false
		}
	}
	return true
}

func quotedList(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = fmt.Sprintf("%q", name)
	}
	return strings.Join(quoted, ", ")
}
