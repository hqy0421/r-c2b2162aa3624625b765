package report

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

// maxPackageBytes bounds a download. It is deliberately larger than any real
// report but small enough that a hostile endpoint cannot fill the disk or
// memory before the manifest check rejects the result.
const maxPackageBytes = 1 << 30 // 1 GiB

// HttpSession is the shared, authenticated HTTP client used to download a
// report package. It carries cookies from the logged-in browser page so that
// the download is served exactly as it would be to the browser, but it never
// forwards the page's HTML or its credentials anywhere else.
type HttpSession struct {
	client    *http.Client
	jar       http.CookieJar
	transport *http.Transport
	fallbacks []*http.Cookie
	host      string // authenticated host, used to scope host-only cookies
}

// NewHttpSession builds an HTTP session whose cookie jar is seeded with the
// cookies of an already-logged-in page. Cookies are copied by value, so later
// changes in the browser do not silently alter a download that is in flight.
//
// Populating the jar is best effort. Some pages expose cookies scoped to a
// host that a standard cookie jar refuses (for example an IP literal in older
// Go versions); those cookies are skipped rather than failing the whole
// session, because the package itself is verified independently of the
// transport that carried it.
func NewHttpSession(cookies []*http.Cookie) (*HttpSession, error) {
	return newHttpSession(cookies, "")
}

// newHttpSession is NewHttpSession with an authoritative host. Cookies that
// carry no domain are host-only, so when a page's host is known they are
// scoped to it and are never sent anywhere else.
func newHttpSession(cookies []*http.Cookie, host string) (*HttpSession, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("report: create cookie jar: %s", err)
	}
	fallbacks := make([]*http.Cookie, 0, len(cookies))
	for _, cookie := range cookies {
		if cookie == nil {
			continue
		}
		copied := *cookie
		if copied.Domain == "" && host != "" {
			copied.Domain = host
		}
		jar.SetCookies(cookieURL(&copied), []*http.Cookie{&copied})
		if copied.Domain == "" || copied.Domain == "localhost" || isIPHost(trimLeadingDot(copied.Domain)) {
			// A standard jar may reject an IP-literal domain, so keep the
			// cookie for the explicit Cookie header fallback.
			fallbacks = append(fallbacks, &copied)
		}
	}
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		TLSClientConfig:     &tls.Config{},
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     30 * time.Second,
	}
	return &HttpSession{
		client:    &http.Client{Jar: jar, Transport: transport},
		jar:       jar,
		transport: transport,
		fallbacks: fallbacks,
		host:      host,
	}, nil
}

// NewHttpSessionFromClient wraps a caller-provided client that already carries
// authentication (for example a cookie jar or a bearer-token transport). The
// caller remains responsible for not following redirects into credentials
// that should not be used.
func NewHttpSessionFromClient(client *http.Client) (*HttpSession, error) {
	if client == nil {
		return nil, errors.New("report: HTTP client is nil")
	}
	return &HttpSession{client: client, jar: client.Jar}, nil
}

func cookieURL(cookie *http.Cookie) *url.URL {
	scheme := "http"
	if cookie.Secure {
		scheme = "https"
	}
	domain := cookie.Domain
	if domain == "" {
		domain = "localhost"
	}
	domain = trimLeadingDot(domain)
	return &url.URL{Scheme: scheme, Host: domain, Path: "/"}
}

func trimLeadingDot(domain string) string {
	for len(domain) > 0 && domain[0] == '.' {
		domain = domain[1:]
	}
	return domain
}

// addFallbackCookies adds cookies that a standard jar would not store, and
// only when the host matches the cookie's domain, so credentials are never
// sent to an unrelated host.
func (s *HttpSession) addFallbackCookies(request *http.Request) {
	if len(s.fallbacks) == 0 || request.URL == nil {
		return
	}
	host := request.URL.Hostname()
	for _, cookie := range s.fallbacks {
		domain := trimLeadingDot(cookie.Domain)
		if domain == "" {
			// A host-only cookie is only sent back to the host it came from.
			if s.host == "" || s.host != host {
				continue
			}
		} else if domain != host {
			continue
		}
		if !pathMatches(cookie.Path, request.URL.Path) {
			continue
		}
		request.AddCookie(cookie)
	}
}

func isIPHost(host string) bool {
	return net.ParseIP(host) != nil
}

func pathMatches(cookiePath, requestPath string) bool {
	if cookiePath == "" || cookiePath == "/" {
		return true
	}
	if !strings.HasPrefix(requestPath, cookiePath) {
		return false
	}
	// A cookie path ending in "/" matches any continuation; otherwise it must
	// match a full path segment.
	return strings.HasSuffix(cookiePath, "/") || len(requestPath) == len(cookiePath) || requestPath[len(cookiePath)] == '/'
}

// Download fetches the package at downloadURL in full, from byte zero, and
// returns the bytes. A download is never resumed: a partial transfer is
// discarded and retried as a whole.
//
// The response is accepted only when it is recognisably the declared package:
// a successful status, a ZIP content type or body, the exact declared byte
// count, and the declared SHA-256. A login page, an HTML error page, a
// truncated body, or a body that disagrees with the manifest is an error. When
// RetryDownload is set the whole download is retried, never a byte range.
func (s *HttpSession) Download(downloadURL string, manifest *Manifest, attempts int) ([]byte, error) {
	if s == nil || s.client == nil {
		return nil, errors.New("report: HTTP session is not initialised")
	}
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	if attempts <= 0 {
		attempts = 1
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		data, err := s.downloadOnce(downloadURL, manifest)
		if err == nil {
			return data, nil
		}
		lastErr = err
		if !retryable(err) {
			break
		}
	}
	return nil, lastErr
}

type retryableError struct{ error }

func retryable(err error) bool {
	_, ok := err.(retryableError)
	return ok
}

func (s *HttpSession) downloadOnce(downloadURL string, manifest *Manifest) ([]byte, error) {
	request, err := http.NewRequest("GET", downloadURL, nil)
	if err != nil {
		return nil, fmt.Errorf("report: build download request: %s", err)
	}
	request.Header.Set("Accept", "application/zip, application/octet-stream;q=0.9, */*;q=0.1")
	request.Header.Set("Accept-Encoding", "identity")
	s.addFallbackCookies(request)

	response, err := s.client.Do(request)
	if err != nil {
		return nil, retryableError{fmt.Errorf("report: download %s: %s", downloadURL, err)}
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		io.Copy(ioutil.Discard, io.LimitReader(response.Body, 64*1024))
		return nil, fmt.Errorf("report: download %s returned HTTP %s", downloadURL, response.Status)
	}

	contentType := response.Header.Get("Content-Type")
	if looksLikeHTML(contentType) {
		return nil, fmt.Errorf("report: download %s returned an HTML document (%s), not a ZIP package", downloadURL, contentType)
	}

	// The declared size is a hard bound: read at most one byte more so that an
	// oversized body is detected rather than silently truncated.
	limit := manifest.Size
	if limit > maxPackageBytes {
		return nil, fmt.Errorf("report: declared package size %d exceeds the %d byte limit", limit, int64(maxPackageBytes))
	}
	body, err := ioutil.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, retryableError{fmt.Errorf("report: read download %s: %s", downloadURL, err)}
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("report: download %s is larger than the declared %d bytes", downloadURL, manifest.Size)
	}
	if int64(len(body)) != manifest.Size {
		return nil, retryableError{fmt.Errorf("report: download %s is %d bytes, completion manifest declares %d (truncated or partial)", downloadURL, len(body), manifest.Size)}
	}
	if dig := digestBytes(body); dig != manifest.SHA256 {
		// A whole-package integrity failure is retryable: the next attempt
		// starts from byte zero and must pass every check before it can be
		// published, so retrying can never let wrong bytes through.
		return nil, retryableError{fmt.Errorf("report: download %s sha256 is %s, completion manifest declares %s", downloadURL, dig, manifest.SHA256)}
	}
	if !isZIP(body) {
		return nil, fmt.Errorf("report: download %s is not a ZIP package", downloadURL)
	}

	// The package identity header, when present, must agree with the manifest.
	if err := checkIdentityHeaders(response, manifest); err != nil {
		return nil, err
	}
	return body, nil
}

// checkIdentityHeaders rejects a package whose transport identity disagrees
// with the completion manifest. Absent headers are not an error because not
// every site emits them; present-but-different headers are.
func checkIdentityHeaders(response *http.Response, manifest *Manifest) error {
	for header, expected := range map[string]string{
		"X-Report-Job-Id":      manifest.JobID,
		"X-Report-Snapshot-Id": manifest.SnapshotID,
		"X-Report-Package-Id":  manifest.PackageID,
		"X-Report-Sha256":      manifest.SHA256,
	} {
		actual := response.Header.Get(header)
		if actual != "" && actual != expected {
			return fmt.Errorf("report: response header %s is %q, completion manifest declares %q", header, actual, expected)
		}
	}
	if content := response.Header.Get("Content-Disposition"); content != "" {
		name := contentDispositionName(content)
		if name != "" && name != manifest.FileName {
			return fmt.Errorf("report: response names the package %q, completion manifest declares %q", name, manifest.FileName)
		}
	}
	return nil
}

func looksLikeHTML(contentType string) bool {
	lower := strings.ToLower(contentType)
	return strings.Contains(lower, "text/html") || strings.Contains(lower, "application/xhtml")
}

func isZIP(data []byte) bool {
	return len(data) >= 4 && data[0] == 'P' && data[1] == 'K' && data[2] == 3 && data[3] == 4
}

func contentDispositionName(value string) string {
	// Only the simple "filename=" form is trusted for this check; a malformed
	// header is ignored rather than treated as an identity claim.
	const marker = "filename="
	index := strings.Index(strings.ToLower(value), marker)
	if index < 0 {
		return ""
	}
	name := value[index+len(marker):]
	if len(name) > 0 && (name[0] == '"' || name[0] == '\'') {
		quote := name[0]
		name = name[1:]
		if end := strings.IndexByte(name, quote); end >= 0 {
			name = name[:end]
		}
	} else if end := strings.IndexByte(name, ';'); end >= 0 {
		name = name[:end]
	}
	return strings.TrimSpace(name)
}

// Close releases idle connections.
func (s *HttpSession) Close() {
	if s != nil && s.transport != nil {
		s.transport.CloseIdleConnections()
	}
}
