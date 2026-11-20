package report

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Page is the subset of *agouti.Page that the export workflow needs. It is
// satisfied by *agouti.Page, so a live logged-in page can be passed directly.
// It is an interface so that the workflow can be tested without a browser.
type Page interface {
	RunScript(body string, arguments map[string]interface{}, result interface{}) error
	URL() (string, error)
	Title() (string, error)
	HTML() (string, error)
}

// Site is the site-specific contract for starting and polling an export. It
// is data, not code, so that a different application can be described without
// modifying this package.
type Site struct {
	// FilterArgument is the JavaScript argument name that receives the
	// declared filters. It defaults to "filters".
	FilterArgument string

	// StartScript is evaluated with the filter argument in scope and must
	// return a JSON *string* describing {job_id, snapshot_id, package_id,
	// download_url}. It defaults to StartBridgeScript.
	StartScript string

	// StatusScript is evaluated with "jobID" in scope and must return a JSON
	// string describing {state, snapshot_id, package_id, download_url,
	// manifest}. It defaults to StatusBridgeScript.
	StatusScript string

	// PollInterval and PollTimeout bound how long a job may stay
	// non-terminal. They default to 500ms and 10 minutes.
	PollInterval time.Duration
	PollTimeout  time.Duration
}

// StartBridgeScript is the default JavaScript used to start an export. The
// bridge receives the declared filters verbatim and must return the job ID and
// snapshot that the server actually assigned. The result is stringified because
// the Agouti page script result is decoded as a JSON string.
const StartBridgeScript = `return JSON.stringify(window.agoutiReport.start(filters));`

// StatusBridgeScript is the default JavaScript used to read a job's state. It
// must return {state, snapshot_id, package_id, download_url, manifest}.
const StatusBridgeScript = `return JSON.stringify(window.agoutiReport.status(jobID));`

// Job is the outcome of starting an export, as reported by the page bridge.
type Job struct {
	JobID       string `json:"job_id"`
	SnapshotID  string `json:"snapshot_id"`
	PackageID   string `json:"package_id"`
	DownloadURL string `json:"download_url"`

	// Filters is preserved on the job so that the exact request is recorded.
	Filters map[string]string `json:"-"`
}

// Completion is the terminal state of an export job.
type Completion struct {
	SnapshotID  string    `json:"snapshot_id"`
	PackageID   string    `json:"package_id"`
	DownloadURL string    `json:"download_url"`
	Manifest    *Manifest `json:"manifest"`
}

type startResponse struct {
	JobID       string `json:"job_id"`
	SnapshotID  string `json:"snapshot_id"`
	PackageID   string `json:"package_id"`
	DownloadURL string `json:"download_url"`
}

type statusResponse struct {
	State       string    `json:"state"`
	JobID       string    `json:"job_id"`
	SnapshotID  string    `json:"snapshot_id"`
	PackageID   string    `json:"package_id"`
	DownloadURL string    `json:"download_url"`
	Manifest    *Manifest `json:"manifest"`
	Error       string    `json:"error"`
}

// Normalize fills in defaults.
func (s Site) Normalize() Site {
	if s.FilterArgument == "" {
		s.FilterArgument = "filters"
	}
	if s.StartScript == "" {
		s.StartScript = StartBridgeScript
	}
	if s.StatusScript == "" {
		s.StatusScript = StatusBridgeScript
	}
	if s.PollInterval <= 0 {
		s.PollInterval = 500 * time.Millisecond
	}
	if s.PollTimeout <= 0 {
		s.PollTimeout = 10 * time.Minute
	}
	return s
}

func (s Site) startScript(filters map[string]string) (string, error) {
	if !validIdentifier(s.FilterArgument) {
		return "", fmt.Errorf("report: filter argument %q is not a JavaScript identifier", s.FilterArgument)
	}
	return fmt.Sprintf("return (function(%s) { %s }).call(this, JSON.parse(%s));",
		s.FilterArgument, s.StartScript, jsString(mustJSON(filters))), nil
}

func (s Site) statusScript(jobID string) (string, error) {
	return fmt.Sprintf("return (function(jobID) { %s }).call(this, %s);", s.StatusScript, jsString(jobID)), nil
}

// StartExport asks the already-logged-in page to start an export with exactly
// the declared filters, and returns the real job identity that the page
// reports. A page that silently substitutes different filters, or a bridge
// that fabricates an identity, is the site's problem; the identity is still
// bound to the completion manifest later, so a mismatched manifest cannot be
// delivered.
func StartExport(page Page, site Site, filters map[string]string) (*Job, error) {
	if page == nil {
		return nil, errors.New("report: page is nil")
	}
	site = site.Normalize()
	if filters == nil {
		filters = map[string]string{}
	}
	// Refuse to drive an obviously logged-out page before touching it.
	if blockedByLoginURL(page) {
		return nil, errors.New("report: page is on a login or error URL")
	}
	script, err := site.startScript(filters)
	if err != nil {
		return nil, err
	}
	var raw string
	if err := page.RunScript(script, nil, &raw); err != nil {
		return nil, fmt.Errorf("report: start export: %s", err)
	}
	if blockedByLogin(page) {
		return nil, errors.New("report: start export was redirected to a login page")
	}
	var response startResponse
	if err := decodeBridgeJSON(raw, &response); err != nil {
		return nil, fmt.Errorf("report: start export: %s", err)
	}
	if err := validateJobID(response.JobID); err != nil {
		return nil, fmt.Errorf("report: start export: %s", err)
	}
	if response.SnapshotID == "" {
		return nil, errors.New("report: start export: page did not report a snapshot ID")
	}
	job := &Job{
		JobID:       response.JobID,
		SnapshotID:  response.SnapshotID,
		PackageID:   response.PackageID,
		DownloadURL: response.DownloadURL,
		Filters:     copyFilters(filters),
	}
	return job, nil
}

// WaitForCompletion polls the page until the job reports completion and
// returns the bound completion manifest. It fails on failure states, on a
// snapshot/package identity that changed under the job, and on timeout.
func WaitForCompletion(page Page, site Site, job *Job) (*Completion, error) {
	if page == nil {
		return nil, errors.New("report: page is nil")
	}
	if job == nil || job.JobID == "" {
		return nil, errors.New("report: job is nil or has no ID")
	}
	site = site.Normalize()
	script, err := site.statusScript(job.JobID)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(site.PollTimeout)
	var lastState string
	for {
		var raw string
		if err := page.RunScript(script, nil, &raw); err != nil {
			return nil, fmt.Errorf("report: poll job %s: %s", job.JobID, err)
		}
		if blockedByLoginURL(page) {
			return nil, fmt.Errorf("report: polling job %s was redirected to a login page", job.JobID)
		}
		var response statusResponse
		if err := decodeBridgeJSON(raw, &response); err != nil {
			return nil, fmt.Errorf("report: poll job %s: %s", job.JobID, err)
		}
		if response.JobID != "" && response.JobID != job.JobID {
			return nil, fmt.Errorf("report: poll job %s: page reported job %q", job.JobID, response.JobID)
		}
		lastState = response.State
		switch response.State {
		case "complete":
			// Only pay for a full page-source check when a job claims to be
			// done, which is the point at which a silently expired session
			// would otherwise be mistaken for a finished export.
			if blockedByLogin(page) {
				return nil, fmt.Errorf("report: job %s reported complete on a login page", job.JobID)
			}
			return bindCompletion(job, &response)
		case "failed", "error", "cancelled", "canceled":
			message := response.Error
			if message == "" {
				message = "no error message reported"
			}
			return nil, fmt.Errorf("report: job %s ended in state %q: %s", job.JobID, response.State, message)
		case "pending", "queued", "running", "processing", "":
			// continue polling
		default:
			return nil, fmt.Errorf("report: job %s reported unknown state %q", job.JobID, response.State)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("report: job %s did not complete within %s (last state %q)", job.JobID, site.PollTimeout, lastState)
		}
		time.Sleep(site.PollInterval)
	}
}

func bindCompletion(job *Job, response *statusResponse) (*Completion, error) {
	if response.Manifest == nil {
		return nil, fmt.Errorf("report: job %s reported complete without a completion manifest", job.JobID)
	}
	manifest := response.Manifest
	if err := manifest.Validate(); err != nil {
		return nil, fmt.Errorf("report: job %s: %s", job.JobID, err)
	}
	if manifest.JobID != job.JobID {
		return nil, fmt.Errorf("report: job %s received completion manifest for job %s", job.JobID, manifest.JobID)
	}
	if manifest.SnapshotID != job.SnapshotID {
		return nil, fmt.Errorf("report: job %s snapshot changed from %s to %s", job.JobID, job.SnapshotID, manifest.SnapshotID)
	}
	if response.SnapshotID != "" && response.SnapshotID != manifest.SnapshotID {
		return nil, fmt.Errorf("report: job %s reported snapshot %s but its completion manifest names %s", job.JobID, response.SnapshotID, manifest.SnapshotID)
	}
	if job.PackageID != "" && manifest.PackageID != job.PackageID {
		return nil, fmt.Errorf("report: job %s package changed from %s to %s", job.JobID, job.PackageID, manifest.PackageID)
	}
	if response.PackageID != "" && response.PackageID != manifest.PackageID {
		return nil, fmt.Errorf("report: job %s reported package %s but its completion manifest names %s", job.JobID, response.PackageID, manifest.PackageID)
	}
	downloadURL := response.DownloadURL
	if downloadURL == "" {
		downloadURL = job.DownloadURL
	}
	if downloadURL == "" {
		return nil, fmt.Errorf("report: job %s has no download URL", job.JobID)
	}
	return &Completion{
		SnapshotID:  manifest.SnapshotID,
		PackageID:   manifest.PackageID,
		DownloadURL: downloadURL,
		Manifest:    manifest,
	}, nil
}

// ResolveDownloadURL resolves the completion's download URL against the page's
// current URL so that relative links work, and refuses non-HTTP schemes.
func ResolveDownloadURL(page Page, completion *Completion) (string, error) {
	if completion == nil {
		return "", errors.New("report: completion is nil")
	}
	pageURL, err := page.URL()
	if err != nil {
		return "", fmt.Errorf("report: read page URL: %s", err)
	}
	base, err := url.Parse(pageURL)
	if err != nil {
		return "", fmt.Errorf("report: parse page URL %q: %s", pageURL, err)
	}
	reference, err := url.Parse(completion.DownloadURL)
	if err != nil {
		return "", fmt.Errorf("report: parse download URL %q: %s", completion.DownloadURL, err)
	}
	resolved := base.ResolveReference(reference)
	if resolved.Scheme != "http" && resolved.Scheme != "https" {
		return "", fmt.Errorf("report: download URL %q does not use http or https", resolved.String())
	}
	return resolved.String(), nil
}

// blockedByLogin is a conservative check that the browser is still on a page
// that can serve the export rather than on a login or error screen. It costs a
// full page-source fetch, so polling uses blockedByLoginURL between
// transitions and reserves this for the start of a job and its completion.
func blockedByLogin(page Page) bool {
	if blockedByLoginURL(page) {
		return true
	}
	html, err := page.HTML()
	if err != nil {
		return false
	}
	lowerHTML := strings.ToLower(html)
	if strings.Contains(lowerHTML, `type="password"`) || strings.Contains(lowerHTML, "name=\"password\"") {
		return strings.Contains(lowerHTML, "<form")
	}
	return false
}

// blockedByLoginURL checks only the page URL, which is cheap enough to run on
// every poll.
func blockedByLoginURL(page Page) bool {
	if page == nil {
		return true
	}
	pageURL, err := page.URL()
	if err != nil {
		return false
	}
	lower := strings.ToLower(pageURL)
	if parsed, err := url.Parse(pageURL); err == nil {
		lower = strings.ToLower(parsed.Path)
	}
	for _, segment := range strings.Split(lower, "/") {
		switch segment {
		case "login", "signin", "sign-in", "signout", "logout", "unauthorized", "session-expired", "auth":
			return true
		}
	}
	return false
}

func decodeBridgeJSON(raw string, target interface{}) error {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" || raw == "undefined" {
		return errors.New("page bridge returned no value")
	}
	if err := json.Unmarshal([]byte(raw), target); err != nil {
		return fmt.Errorf("page bridge returned invalid JSON: %s", err)
	}
	return nil
}

func validIdentifier(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		letter := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_' || r == '$'
		digit := r >= '0' && r <= '9'
		if !letter && !(i > 0 && digit) {
			return false
		}
	}
	return true
}

func mustJSON(value interface{}) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(data)
}

// jsString encodes s as a JavaScript string literal. encoding/json produces a
// subset of JavaScript string syntax that is safe to embed.
func jsString(s string) string {
	data, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func copyFilters(filters map[string]string) map[string]string {
	copied := make(map[string]string, len(filters))
	for key, value := range filters {
		copied[key] = value
	}
	return copied
}
