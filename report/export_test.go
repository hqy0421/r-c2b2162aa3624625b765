package report

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func mustJSONString(t *testing.T, value interface{}) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %s", err)
	}
	return string(data)
}

func completionJSON(t *testing.T, jobID, snapshotID, packageID, url string, manifest *Manifest) string {
	t.Helper()
	return mustJSONString(t, map[string]interface{}{
		"state":        "complete",
		"job_id":       jobID,
		"snapshot_id":  snapshotID,
		"package_id":   packageID,
		"download_url": url,
		"manifest":     manifest,
	})
}

func TestStartExportBindsReportedIdentity(t *testing.T) {
	page := &fakePage{scripts: []scriptResponse{{value: `{"job_id":"job-42","snapshot_id":"snap-9","package_id":"pkg-7","download_url":"/exports/42.zip"}`}}}
	job, err := StartExport(page, Site{}, map[string]string{"status": "paid", "region": "emea"})
	if err != nil {
		t.Fatalf("start: %s", err)
	}
	if job.JobID != "job-42" || job.SnapshotID != "snap-9" || job.PackageID != "pkg-7" {
		t.Fatalf("identity not bound: %+v", job)
	}
	if job.Filters["status"] != "paid" || job.Filters["region"] != "emea" {
		t.Fatalf("filters not preserved: %+v", job.Filters)
	}
	if len(page.ScriptCalls()) != 1 ||
		!strings.Contains(page.ScriptCalls()[0], "status") ||
		!strings.Contains(page.ScriptCalls()[0], "paid") ||
		!strings.Contains(page.ScriptCalls()[0], "emea") {
		t.Fatalf("filters were not passed to the page bridge: %v", page.ScriptCalls())
	}
}

func TestStartExportRejectsMissingJobID(t *testing.T) {
	page := &fakePage{scripts: []scriptResponse{{value: `{"snapshot_id":"snap-9"}`}}}
	if _, err := StartExport(page, Site{}, nil); err == nil {
		t.Fatal("expected missing job ID to be rejected")
	}
}

func TestStartExportRejectsUnsafeJobID(t *testing.T) {
	page := &fakePage{scripts: []scriptResponse{{value: `{"job_id":"../../etc","snapshot_id":"snap"}`}}}
	if _, err := StartExport(page, Site{}, nil); err == nil {
		t.Fatal("expected unsafe job ID to be rejected")
	}
}

func TestStartExportRejectsLoginRedirect(t *testing.T) {
	page := &fakePage{pageURL: "https://app.example.com/login?next=/exports"}
	if _, err := StartExport(page, Site{}, nil); err == nil {
		t.Fatal("expected login redirect to be rejected")
	}
}

func TestStartExportRejectsPasswordForm(t *testing.T) {
	page := &fakePage{
		scripts: []scriptResponse{{value: `{"job_id":"job-1","snapshot_id":"snap"}`}},
		html:    `<html><form action="/session"><input type="password" name="password"></form></html>`,
	}
	if _, err := StartExport(page, Site{}, nil); err == nil {
		t.Fatal("expected password form to be treated as a login page")
	}
}

func TestWaitForCompletionPollsUntilDone(t *testing.T) {
	spec := canonicalSpec()
	spec.jobID = "job-1"
	manifest := spec.manifest(t)
	manifest.Size = 10
	manifest.SHA256 = strings.Repeat("a", 64)
	page := &fakePage{scripts: []scriptResponse{
		{value: `{"state":"running","job_id":"job-1","snapshot_id":"snap-1"}`},
		{value: `{"state":"pending","job_id":"job-1","snapshot_id":"snap-1"}`},
		{value: completionJSON(t, "job-1", "snap-1", "pkg-1", "https://app.example.com/exports/1.zip", manifest)},
	}}
	job := &Job{JobID: "job-1", SnapshotID: "snap-1", PackageID: "pkg-1"}
	completion, err := WaitForCompletion(page, Site{PollInterval: time.Millisecond, PollTimeout: time.Second}, job)
	if err != nil {
		t.Fatalf("wait: %s", err)
	}
	if completion.Manifest.JobID != "job-1" {
		t.Fatalf("unexpected completion: %+v", completion)
	}
	if !strings.Contains(completion.DownloadURL, "1.zip") {
		t.Fatalf("download URL not bound: %q", completion.DownloadURL)
	}
	if len(page.ScriptCalls()) != 3 {
		t.Fatalf("expected 3 polls, got %d", len(page.ScriptCalls()))
	}
}

func TestWaitForCompletionRejectsFailureState(t *testing.T) {
	page := &fakePage{scripts: []scriptResponse{{value: `{"state":"failed","job_id":"job-1","error":"quota exceeded"}`}}}
	job := &Job{JobID: "job-1", SnapshotID: "snap-1"}
	if _, err := WaitForCompletion(page, Site{PollInterval: time.Millisecond, PollTimeout: time.Second}, job); err == nil {
		t.Fatal("expected failed job to be reported")
	} else if !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("failure message not surfaced: %s", err)
	}
}

func TestWaitForCompletionRejectsSnapshotChange(t *testing.T) {
	manifest := canonicalSpec().manifest(t)
	manifest.Size = 1
	manifest.SHA256 = strings.Repeat("a", 64)
	page := &fakePage{scripts: []scriptResponse{{value: completionJSON(t, "job-1", "snap-2", "pkg-1", "https://x/1.zip", manifest)}}}
	job := &Job{JobID: "job-1", SnapshotID: "snap-1"}
	if _, err := WaitForCompletion(page, Site{PollInterval: time.Millisecond, PollTimeout: time.Second}, job); err == nil {
		t.Fatal("expected snapshot change to be rejected")
	}
}

func TestWaitForCompletionRejectsWrongManifestJob(t *testing.T) {
	manifest := canonicalSpec().manifest(t)
	manifest.JobID = "job-other"
	manifest.Version = 1
	manifest.Size = 1
	manifest.SHA256 = strings.Repeat("a", 64)
	page := &fakePage{scripts: []scriptResponse{{value: completionJSON(t, "job-1", "snap-1", "pkg-1", "https://x/1.zip", manifest)}}}
	job := &Job{JobID: "job-1", SnapshotID: "snap-1"}
	if _, err := WaitForCompletion(page, Site{PollInterval: time.Millisecond, PollTimeout: time.Second}, job); err == nil {
		t.Fatal("expected a manifest for another job to be rejected")
	}
}

func TestWaitForCompletionRejectsCompleteWithoutManifest(t *testing.T) {
	page := &fakePage{scripts: []scriptResponse{{value: `{"state":"complete","job_id":"job-1","snapshot_id":"snap-1"}`}}}
	job := &Job{JobID: "job-1", SnapshotID: "snap-1"}
	if _, err := WaitForCompletion(page, Site{PollInterval: time.Millisecond, PollTimeout: time.Second}, job); err == nil {
		t.Fatal("expected completion without a manifest to be rejected")
	}
}

func TestWaitForCompletionTimesOut(t *testing.T) {
	page := &fakePage{scripts: []scriptResponse{
		{value: `{"state":"running","job_id":"job-1"}`},
		{value: `{"state":"running","job_id":"job-1"}`},
		{value: `{"state":"running","job_id":"job-1"}`},
		{value: `{"state":"running","job_id":"job-1"}`},
		{value: `{"state":"running","job_id":"job-1"}`},
	}}
	job := &Job{JobID: "job-1", SnapshotID: "snap-1"}
	site := Site{PollInterval: time.Millisecond, PollTimeout: 5 * time.Millisecond}
	// The fake page returns "" once its script queue is exhausted, and an
	// empty bridge value is itself an error. Give the queue a looping status
	// so only the deadline can stop the loop.
	page.scripts = nil
	page.repeat = `{"state":"running","job_id":"job-1","snapshot_id":"snap-1"}`
	_, err := WaitForCompletion(page, site, job)
	if err == nil || !strings.Contains(err.Error(), "did not complete") {
		t.Fatalf("expected timeout, got %v", err)
	}
}

func TestResolveDownloadURL(t *testing.T) {
	page := &fakePage{pageURL: "https://app.example.com/exports/report"}
	cases := map[string]string{
		"/exports/1.zip":        "https://app.example.com/exports/1.zip",
		"1.zip":                 "https://app.example.com/exports/1.zip",
		"https://cdn.example/1": "https://cdn.example/1",
	}
	for input, want := range cases {
		got, err := ResolveDownloadURL(page, &Completion{DownloadURL: input})
		if err != nil {
			t.Fatalf("%q: %s", input, err)
		}
		if got != want {
			t.Errorf("%q resolved to %q, want %q", input, got, want)
		}
	}
	if _, err := ResolveDownloadURL(page, &Completion{DownloadURL: "javascript:alert(1)"}); err == nil {
		t.Fatal("expected a non-HTTP scheme to be rejected")
	}
}

// fakeDownloader returns a fixed body and records calls.
type fakeDownloader struct {
	body     []byte
	err      error
	attempts int
	calls    int
}

func (d *fakeDownloader) Download(downloadURL string, manifest *Manifest, attempts int) ([]byte, error) {
	d.calls++
	d.attempts = attempts
	return d.body, d.err
}

func TestDelivererEndToEnd(t *testing.T) {
	data, manifest := buildCanonical(t)
	store := newTestStore(t)
	page := &fakePage{scripts: []scriptResponse{
		{value: `{"job_id":"job-A","snapshot_id":"snap-1","package_id":"pkg-1","download_url":"/exports/A.zip"}`},
		{value: completionJSON(t, "job-A", "snap-1", "pkg-1", "https://app.example.com/exports/A.zip", manifest)},
	}}
	downloader := &fakeDownloader{body: data}
	deliverer := &Deliverer{Page: page, Client: downloader, Store: store, Attempts: 2}

	result, err := deliverer.Deliver(map[string]string{"status": "paid"}, canonicalSchema())
	if err != nil {
		t.Fatalf("deliver: %s", err)
	}
	if result.Job.JobID != "job-A" || result.Package.SnapshotID != "snap-1" {
		t.Fatalf("result identity: %+v", result)
	}
	if downloader.attempts != 2 {
		t.Errorf("retry budget not passed: %d", downloader.attempts)
	}
	opened, err := store.Open("job-A")
	if err != nil {
		t.Fatalf("open: %s", err)
	}
	verifyCanonical(t, opened)
}

func TestDelivererKeepsPreviousPackageWhenDownloadFails(t *testing.T) {
	store := newTestStore(t)
	data, manifest := buildCanonical(t)
	if _, err := store.Deliver("job-A", "", data, manifest, canonicalSchema()); err != nil {
		t.Fatalf("seed: %s", err)
	}
	before, _ := store.Current("job-A")

	page := &fakePage{scripts: []scriptResponse{
		{value: `{"job_id":"job-A","snapshot_id":"snap-1","package_id":"pkg-2","download_url":"/exports/A2.zip"}`},
		{value: completionJSON(t, "job-A", "snap-1", "pkg-2", "https://app.example.com/exports/A2.zip", manifest)},
	}}
	deliverer := &Deliverer{Page: page, Client: &fakeDownloader{err: errors.New("network down")}, Store: store}
	if _, err := deliverer.Deliver(nil, canonicalSchema()); err == nil {
		t.Fatal("expected download failure")
	}
	after, _ := store.Current("job-A")
	if before != after {
		t.Fatalf("current version changed after a failed delivery: %q -> %q", before, after)
	}
	if _, err := store.Open("job-A"); err != nil {
		t.Fatalf("previous package must remain usable: %s", err)
	}
}

func TestDelivererRequiresSchema(t *testing.T) {
	deliverer := &Deliverer{Page: &fakePage{}, Client: &fakeDownloader{}, Store: newTestStore(t)}
	if _, err := deliverer.Deliver(nil, nil); err == nil {
		t.Fatal("expected nil schema to be rejected")
	}
}

func TestDelivererRejectsInvalidSchema(t *testing.T) {
	schema := canonicalSchema()
	schema.Files = nil
	deliverer := &Deliverer{Page: &fakePage{}, Client: &fakeDownloader{}, Store: newTestStore(t)}
	if _, err := deliverer.Deliver(nil, schema); err == nil {
		t.Fatal("expected invalid schema to be rejected before any browser work")
	}
	if len(deliverer.Page.(*fakePage).ScriptCalls()) != 0 {
		t.Fatal("schema must be validated before starting an export")
	}
}

// The full HTTP path: a real server serves the ZIP, and the deliverer uses the
// shared authenticated client to fetch it.
func TestDelivererWithRealHTTPDownload(t *testing.T) {
	data, manifest := buildCanonical(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cookie, err := r.Cookie("session"); err != nil || cookie.Value != "token" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Write(data)
	}))
	defer server.Close()

	session, err := NewSessionForPage(&fakePage{
		pageURL: server.URL + "/exports",
		cookies: []*http.Cookie{{Name: "session", Value: "token"}},
	})
	if err != nil {
		t.Fatalf("session: %s", err)
	}
	store := newTestStore(t)
	page := &fakePage{
		pageURL: "https://app.example.com/exports",
		scripts: []scriptResponse{
			{value: `{"job_id":"job-A","snapshot_id":"snap-1","package_id":"pkg-1","download_url":"/exports/A.zip"}`},
			{value: completionJSON(t, "job-A", "snap-1", "pkg-1", server.URL+"/exports/A.zip", manifest)},
		},
	}
	deliverer := &Deliverer{Page: page, Client: session, Store: store, Attempts: 2}
	result, err := deliverer.Deliver(nil, canonicalSchema())
	if err != nil {
		t.Fatalf("deliver: %s", err)
	}
	if result.Package.JobID != "job-A" {
		t.Fatalf("unexpected result: %+v", result)
	}
	verifyCanonical(t, result.Package)
}
