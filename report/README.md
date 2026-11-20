# report: verified, recoverable report delivery

`github.com/sclevine/agouti/report` turns an export request made from a
logged-in browser page into a locally readable report package, and refuses to
publish anything short of a fully verified result.

It is **additive**: no existing Agouti API or behaviour changes. The package
depends only on the standard library at run time, and on `agouti` to prove that
a real `*agouti.Page` satisfies the page interface.

## Why

A browser page saying "export complete" proves only that some script finished
running. It does not prove that a file exists, that the file is the one the
server intended to send, or that it is intact. `report` treats everything after
the export request as untrusted:

- the job identity reported by the page is bound to the completion manifest;
- the ZIP is downloaded in full by a shared authenticated client and is never
  resumed from a byte offset;
- the archive's size and SHA-256 must match the manifest, and so must every
  member's name, uncompressed size and SHA-256;
- every declared CSV member is decoded against the declared schema before
  anything is published;
- only then does the store switch the job's current version.

A failure at any point leaves the previous usable package in place.

## Page contract

The page must expose a small bridge. By default `report` calls:

```js
window.agoutiReport.start(filters)  // -> {job_id, snapshot_id, package_id, download_url}
window.agoutiReport.status(jobID)   // -> {state, snapshot_id, package_id, download_url, manifest}
```

Both must return JSON-serialisable values; the default scripts wrap them in
`JSON.stringify`. `manifest` is the completion manifest:

```json
{
  "version": 1,
  "job_id": "job-123",
  "snapshot_id": "snap-9",
  "package_id": "pkg-4",
  "file_name": "report.zip",
  "size": 12345,
  "sha256": "<64 lowercase hex characters>",
  "files": [
    {"name": "orders.csv", "size": 2345, "sha256": "<64 hex>"}
  ]
}
```

The archive must contain `manifest.json` whose identity and file list match the
completion manifest; it deliberately does not carry the archive's own size and
digest.

Sites that use a different bridge set `Site.StartScript` / `Site.StatusScript`.

## Schema

A schema declares every data member and its columns:

```go
schema := &report.Schema{Files: []report.FileSchema{{
    Name:   "orders.csv",
    Header: true,
    Columns: []report.Column{
        {Name: "order_id", Type: report.IntegerType},
        {Name: "note",     Type: report.TextType, EmptyIsText: true},
        {Name: "paid",     Type: report.BooleanType},
    },
}}}
```

Decoding preserves:

- **quoted newlines** inside a logical record;
- **duplicate rows** in source order;
- **integer zero** as `0`, distinct from NULL;
- **boolean FALSE** as `false`, distinct from NULL;
- **NULL vs empty text**: an unquoted empty field is NULL unless the column
  sets `EmptyIsText`; a quoted empty field (`""`) is always empty text; a
  declared `NullToken` (for example `\N`) is NULL only when unquoted.

Malformed CSV (bare quotes, text after a closing quote, unterminated quotes,
stray carriage returns, wrong field counts, invalid UTF-8) and values that do
not decode to their declared type are errors. Invalid delivery, never data.

## Delivery

```go
store := &report.Store{Root: "/var/lib/reports"}

session, err := report.NewSessionForPage(page) // shares the page's cookies
if err != nil { ... }

deliverer := &report.Deliverer{
    Page: page, Site: site, Client: session, Store: store, Attempts: 3,
}
result, err := deliverer.Deliver(map[string]string{"status": "paid"}, schema)
if err != nil {
    // The previous package is still current and openable.
    return err
}
```

`Deliver` runs: start export → bind real job identity → wait for completion
manifest → download whole ZIP → verify everything → decode every file → switch
the current version.

## Reading

```go
report, err := store.OpenReport("job-123")
if err != nil { ... }

for _, file := range report.Files {
    for _, record := range file.Records {
        if value, ok := record.Value("order_id"); ok && !value.Null {
            _ = value.Int
        }
    }
    // file.Summary counts NULLs, zeroes and FALSEs separately.
}
```

`OpenReport` re-verifies the files on disk - sizes, digests, archive members
and CSV decoding - rather than trusting that they exist. If the current
version no longer verifies, it falls back to the newest older version that
does, repairs the `CURRENT` pointer, and leaves the damaged version in place
for inspection.

## On-disk layout

```
<root>/<job_id>/CURRENT                 # content-addressed version token
<root>/<job_id>/versions/<token>/       # immutable, per delivery
    package.zip
    manifest.json                       # completion manifest
    schema.json                         # declared schema
    meta.json                           # source URL, delivery time
```

Versions are named after the job and package digest, never after the archive's
file name, so two jobs that both export `report.zip` cannot overwrite each
other.
