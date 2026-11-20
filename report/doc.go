// Package report delivers exported tabular reports from a logged-in web
// application into a verified, locally readable package.
//
// A web page saying "export complete" is not evidence that a usable report
// exists. This package treats the browser as a place to *ask for* an export
// and treats every byte that arrives afterwards as untrusted input. Nothing
// becomes locally readable until the whole package has been checked, every
// declared CSV file has been decoded according to its declared schema, and
// the result has been switched into place atomically.
//
// # Contract with the exporting site
//
// The browser page must expose a small bridge that this package calls through
// agouti's Page.RunScript. The bridge starts an export from the declared
// filters, reports the real job ID assigned by the server, and reports the
// completion manifest:
//
//	window.agoutiReport = {
//	  start: function (filters) {
//	    return {
//	      job_id: "job-123",
//	      snapshot_id: "snap-9",
//	      package_id: "pkg-4",
//	      download_url: "/exports/job-123/report.zip"
//	    };
//	  },
//	  status: function (jobID) {
//	    return {
//	      state: "complete",             // pending|running|complete|failed
//	      snapshot_id: "snap-9",
//	      package_id: "pkg-4",
//	      download_url: "/exports/job-123/report.zip",
//	      manifest: {
//	        version: 1,
//	        job_id: "job-123",
//	        snapshot_id: "snap-9",
//	        package_id: "pkg-4",
//	        file_name: "report.zip",
//	        size: 12345,
//	        sha256: "<64 hex characters>",
//	        files: [
//	          {"name":"orders.csv","size":2345,"sha256":"<64 hex>"}
//	        ]
//	      }
//	    };
//	  }
//	};
//
// Both functions must return JSON serialisable values; PageSite wraps them in
// JSON.stringify and the scripts are configurable for sites that use a
// different bridge.
//
// # Delivery guarantees
//
// Deliver performs the following steps, in order:
//
//  1. Start an export on the already-logged-in page using the exact declared
//     filters.
//  2. Bind the returned job ID, snapshot ID and package ID to the completion
//     manifest and reject any mismatch.
//  3. Download the whole ZIP with a shared authenticated HTTP client. A
//     download that is truncated, is an HTML login page, is not a ZIP, or
//     whose identity headers disagree with the manifest is discarded.
//  4. Verify the ZIP's byte count and SHA-256, and every member's name,
//     uncompressed size and SHA-256 against the completion manifest. Missing
//     members, extra members, duplicate members, unsafe names and digest
//     mismatches are fatal.
//  5. Decode every declared CSV file according to its declared schema,
//     preserving quoted newlines, duplicate rows, integer zero, boolean FALSE,
//     and the distinction between NULL and empty text.
//  6. Only if every step succeeded, switch the job's CURRENT pointer to the
//     new immutable version. A failure at any point leaves the previously
//     usable version in place. Downloads are always restarted from byte zero,
//     never resumed.
//
// Opening a previously delivered package re-verifies the files that are on
// disk (sizes and digests, archive members, and CSV decoding) instead of
// merely checking that a file exists. If the newest version no longer verifies,
// Open falls back to the newest older version that does, repairs the CURRENT
// pointer, and leaves the damaged version untouched for inspection.
//
// # Backwards compatibility
//
// The package is additive. It does not change any existing Agouti API or
// behaviour; the existing WebDriver, Page, Selection and matcher APIs continue
// to work exactly as before.
package report
