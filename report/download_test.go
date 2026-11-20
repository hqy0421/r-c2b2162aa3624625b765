package report

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func newTestSession(t *testing.T) *HttpSession {
	t.Helper()
	session, err := NewHttpSession(nil)
	if err != nil {
		t.Fatalf("new session: %s", err)
	}
	return session
}

func TestDownloadSucceedsAndSendsSessionCookies(t *testing.T) {
	data, manifest := buildCanonical(t)
	var sawCookie atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cookie, err := r.Cookie("session"); err == nil {
			sawCookie.Store(cookie.Value)
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, manifest.FileName))
		w.Write(data)
	}))
	defer server.Close()

	session, err := NewHttpSession([]*http.Cookie{{Name: "session", Value: "abc123", Domain: "127.0.0.1", Path: "/"}})
	if err != nil {
		t.Fatalf("new session: %s", err)
	}
	got, err := session.Download(server.URL+"/report.zip", manifest, 1)
	if err != nil {
		t.Fatalf("download: %s", err)
	}
	if string(got) != string(data) {
		t.Fatal("downloaded bytes differ")
	}
	if value, _ := sawCookie.Load().(string); value != "abc123" {
		t.Errorf("session cookie was not shared: %q", value)
	}
}

func TestDownloadRejectsLoginPage(t *testing.T) {
	_, manifest := buildCanonical(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte("<html><form><input type=password></form></html>"))
	}))
	defer server.Close()
	if _, err := newTestSession(t).Download(server.URL, manifest, 1); err == nil {
		t.Fatal("expected login page to be rejected")
	}
}

func TestDownloadRejectsTruncatedStream(t *testing.T) {
	data, manifest := buildCanonical(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		w.Write(data[:len(data)/2])
	}))
	defer server.Close()
	if _, err := newTestSession(t).Download(server.URL, manifest, 1); err == nil {
		t.Fatal("expected truncated stream to be rejected")
	}
}

func TestDownloadRejectsWrongDigest(t *testing.T) {
	data, manifest := buildCanonical(t)
	tampered := append([]byte{}, data...)
	tampered[len(tampered)-1] ^= 0xff
	// Keep the length so only the digest check can catch it.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		w.Write(tampered)
	}))
	defer server.Close()
	if _, err := newTestSession(t).Download(server.URL, manifest, 1); err == nil {
		t.Fatal("expected wrong digest to be rejected")
	}
}

func TestDownloadRejectsOversizedBody(t *testing.T) {
	data, manifest := buildCanonical(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(appendedTamper(data))
	}))
	defer server.Close()
	if _, err := newTestSession(t).Download(server.URL, manifest, 1); err == nil {
		t.Fatal("expected oversized body to be rejected")
	}
}

func appendedTamper(data []byte) []byte {
	return append(append([]byte{}, data...), []byte("trailing")...)
}

func TestDownloadRejectsIdentityHeaderMismatch(t *testing.T) {
	data, manifest := buildCanonical(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Report-Job-Id", "job-other")
		w.Write(data)
	}))
	defer server.Close()
	if _, err := newTestSession(t).Download(server.URL, manifest, 1); err == nil {
		t.Fatal("expected identity header mismatch to be rejected")
	}
}

func TestDownloadRejectsWrongFileName(t *testing.T) {
	data, manifest := buildCanonical(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="other.zip"`)
		w.Write(data)
	}))
	defer server.Close()
	if _, err := newTestSession(t).Download(server.URL, manifest, 1); err == nil {
		t.Fatal("expected wrong file name to be rejected")
	}
}

func TestDownloadRetriesWholePackageFromZero(t *testing.T) {
	data, manifest := buildCanonical(t)
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := atomic.AddInt32(&requests, 1)
		if r.Header.Get("Range") != "" {
			t.Errorf("download must never use a range request, got %q", r.Header.Get("Range"))
		}
		if attempt == 1 {
			// First attempt is a truncated stream.
			w.Header().Set("Content-Length", fmt.Sprint(len(data)))
			w.Write(data[:len(data)/3])
			return
		}
		w.Write(data)
	}))
	defer server.Close()

	got, err := newTestSession(t).Download(server.URL, manifest, 3)
	if err != nil {
		t.Fatalf("download should retry from zero: %s", err)
	}
	if string(got) != string(data) {
		t.Fatal("retried download bytes differ")
	}
	if atomic.LoadInt32(&requests) != 2 {
		t.Errorf("expected exactly 2 requests, got %d", requests)
	}
}

func TestDownloadRetriesAmbiguousLengthThenSucceeds(t *testing.T) {
	data, manifest := buildCanonical(t)
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&requests, 1) == 1 {
			w.Write([]byte("short"))
			return
		}
		w.Write(data)
	}))
	defer server.Close()
	if _, err := newTestSession(t).Download(server.URL, manifest, 3); err != nil {
		t.Fatalf("expected retry to succeed: %s", err)
	}
}

func TestDownloadRetriesDigestMismatchAsWholePackage(t *testing.T) {
	data, manifest := buildCanonical(t)
	tampered := append([]byte{}, data...)
	tampered[len(tampered)-1] ^= 0x01
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		if r.Header.Get("Range") != "" {
			t.Errorf("a retry must restart from zero, saw Range %q", r.Header.Get("Range"))
		}
		w.Write(tampered)
	}))
	defer server.Close()
	if _, err := newTestSession(t).Download(server.URL, manifest, 3); err == nil {
		t.Fatal("expected digest mismatch")
	}
	// A corrupt body is retried as a whole package (never a byte range), and
	// the caller still never receives it because verification fails.
	if atomic.LoadInt32(&requests) != 3 {
		t.Errorf("expected 3 whole-package attempts, saw %d", requests)
	}
}

func TestDownloadRejectsNonOKStatus(t *testing.T) {
	_, manifest := buildCanonical(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	if _, err := newTestSession(t).Download(server.URL, manifest, 1); err == nil {
		t.Fatal("expected non-200 to be rejected")
	}
}

func TestNewSessionForPageReadsCookies(t *testing.T) {
	page := &fakePage{cookies: []*http.Cookie{{Name: "session", Value: "value"}}}
	session, err := NewSessionForPage(page)
	if err != nil {
		t.Fatalf("session: %s", err)
	}
	if session == nil || session.client == nil {
		t.Fatal("session not initialised")
	}
	noCookies := &fakePage{}
	if _, err := NewSessionForPage(noCookies); err != nil {
		t.Fatalf("a page with no cookies is still a valid session: %s", err)
	}
}

func TestDownloadRejectsHTMLContentTypeRegardlessOfBody(t *testing.T) {
	data, manifest := buildCanonical(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write(data)
	}))
	defer server.Close()
	_, err := newTestSession(t).Download(server.URL, manifest, 1)
	if err == nil || !strings.Contains(err.Error(), "HTML") {
		t.Fatalf("expected HTML rejection, got %v", err)
	}
}
