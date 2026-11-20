package report

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// Downloader is the part of *HttpSession that delivery needs, so that a caller
// can supply its own authenticated transport in tests or for a site that uses
// a non-standard mechanism.
type Downloader interface {
	Download(downloadURL string, manifest *Manifest, attempts int) ([]byte, error)
}

// Deliverer ties the browser, the authenticated download client and the store
// together.
type Deliverer struct {
	Page     Page
	Site     Site
	Client   Downloader
	Store    *Store
	Attempts int // whole-package download attempts; defaults to 3
}

// Result is the outcome of a successful delivery.
type Result struct {
	Package     *Package
	Job         *Job
	Completion  *Completion
	DownloadURL string
	Version     string
}

// Deliver performs one complete, recoverable report delivery:
//
//	start export -> bind real job identity -> wait for completion manifest ->
//	download whole ZIP -> verify identity, members, bytes and digests ->
//	decode every CSV file -> switch the local current version
//
// It returns only after every step has succeeded. On any failure the store's
// previously current version is unchanged and remains openable.
func (d *Deliverer) Deliver(filters map[string]string, schema *Schema) (*Result, error) {
	if d == nil {
		return nil, errors.New("report: deliverer is nil")
	}
	if d.Page == nil {
		return nil, errors.New("report: deliverer has no page")
	}
	if d.Client == nil {
		return nil, errors.New("report: deliverer has no authenticated download client")
	}
	if d.Store == nil {
		return nil, errors.New("report: deliverer has no store")
	}
	if schema == nil {
		return nil, errors.New("report: schema is nil")
	}
	if err := schema.Validate(); err != nil {
		return nil, err
	}
	attempts := d.Attempts
	if attempts <= 0 {
		attempts = 3
	}

	job, err := StartExport(d.Page, d.Site, filters)
	if err != nil {
		return nil, err
	}
	completion, err := WaitForCompletion(d.Page, d.Site, job)
	if err != nil {
		return nil, err
	}
	downloadURL, err := ResolveDownloadURL(d.Page, completion)
	if err != nil {
		return nil, err
	}
	data, err := d.Client.Download(downloadURL, completion.Manifest, attempts)
	if err != nil {
		return nil, err
	}
	// The store re-verifies independently before publishing; it is the single
	// publication path.
	pkg, err := d.Store.Deliver(job.JobID, downloadURL, data, completion.Manifest, schema)
	if err != nil {
		return nil, err
	}
	version, err := d.Store.Current(job.JobID)
	if err != nil {
		version = ""
	}
	return &Result{
		Package:     pkg,
		Job:         job,
		Completion:  completion,
		DownloadURL: downloadURL,
		Version:     version,
	}, nil
}

// NewSessionForPage seeds a shared HTTP session from a logged-in page's
// cookies. It is a convenience for the common case where delivery starts from
// a browser page.
func NewSessionForPage(page Page) (*HttpSession, error) {
	if page == nil {
		return nil, errors.New("report: page is nil")
	}
	cookiePage, ok := page.(interface {
		GetCookies() ([]*http.Cookie, error)
	})
	if !ok {
		return nil, errors.New("report: page cannot provide cookies")
	}
	cookies, err := cookiePage.GetCookies()
	if err != nil {
		return nil, fmt.Errorf("report: read page cookies: %s", err)
	}
	host := ""
	if pageURL, err := page.URL(); err == nil {
		if parsed, err := url.Parse(pageURL); err == nil {
			host = parsed.Hostname()
		}
	}
	return newHttpSession(cookies, host)
}

// ReadPackage returns typed records, their source and per-file summaries from
// a verified package. It is independent of delivery: it works on any package
// that was verified, whether just downloaded or opened from the store.
func ReadPackage(pkg *Package) (*Report, error) {
	if pkg == nil {
		return nil, errors.New("report: package is nil")
	}
	report := &Report{
		JobID:      pkg.JobID,
		SnapshotID: pkg.SnapshotID,
		PackageID:  pkg.PackageID,
		FileName:   pkg.FileName,
		SHA256:     pkg.SHA256,
		Size:       pkg.Size,
		Files:      make([]FileReport, 0, len(pkg.Files)),
	}
	for i := range pkg.Files {
		fileReport, err := summarizeFile(&pkg.Files[i])
		if err != nil {
			return nil, err
		}
		report.Files = append(report.Files, *fileReport)
		report.RecordCount += len(pkg.Files[i].Records)
	}
	return report, nil
}
