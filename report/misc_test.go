package report

import (
	"io/ioutil"
	"strings"
	"testing"
	"time"
)

func TestDecodeTimestampDeclaredAndDefaultLayouts(t *testing.T) {
	schema := &FileSchema{
		Name: "times.csv",
		Columns: []Column{
			{Name: "custom", Type: TimestampType, Layouts: []string{"02/01/2006 15:04"}},
			{Name: "default", Type: TimestampType},
		},
	}
	records, err := decodeCSV([]byte("31/12/2024 23:59,2024-12-31T23:59:00Z\n"), schema)
	if err != nil {
		t.Fatalf("decode: %s", err)
	}
	custom := records[0].Values[0]
	if custom.Null || !custom.Time.Equal(time.Date(2024, 12, 31, 23, 59, 0, 0, time.UTC)) {
		t.Errorf("custom layout: %+v", custom)
	}
	if records[0].Values[1].Time.IsZero() {
		t.Errorf("default layout not applied: %+v", records[0].Values[1])
	}
	if _, err := decodeCSV([]byte("not-a-date,2024-12-31\n"), schema); err == nil {
		t.Fatal("expected an invalid timestamp to be rejected")
	}
}

func TestDecodeValueAllTypes(t *testing.T) {
	if v, err := decodeValue(Field{Value: "42", Quoted: true}, &Column{Name: "n", Type: IntegerType}); err != nil || v.Int != 42 {
		t.Errorf("integer: %+v %v", v, err)
	}
	// Stray whitespace is not silently trimmed: a padded integer is a type
	// error, because trimming could hide a real data problem.
	if _, err := decodeValue(Field{Value: " 42 "}, &Column{Name: "n", Type: IntegerType}); err == nil {
		t.Error("padded integer should be rejected")
	}
	if v, err := decodeValue(Field{Value: "yes", Quoted: true}, &Column{Name: "b", Type: BooleanType, TrueTokens: []string{"yes"}, FalseTokens: []string{"no"}}); err != nil || !v.Bool {
		t.Errorf("boolean tokens: %+v %v", v, err)
	}
	if v, err := decodeValue(Field{Value: "plain"}, &Column{Name: "t"}); err != nil || v.Type != TextType || v.Text != "plain" {
		t.Errorf("default text type: %+v %v", v, err)
	}
	if _, err := decodeValue(Field{Value: "x"}, &Column{Name: "u", Type: ColumnType("unknown")}); err == nil {
		t.Error("unknown type should fail")
	}
}

func TestRecordValueLookupMissingColumn(t *testing.T) {
	record := Record{Values: []Value{{Column: "a", Text: "x"}}}
	if _, ok := record.Value("b"); ok {
		t.Fatal("missing column should not be found")
	}
}

func TestCSVErrorFormatting(t *testing.T) {
	err := &CSVError{File: "f.csv", Record: 2, Line: 7, Column: "c", Message: "bad"}
	text := err.Error()
	for _, want := range []string{"f.csv", "record 2", "line 7", `"c"`, "bad"} {
		if !strings.Contains(text, want) {
			t.Errorf("error %q missing %q", text, want)
		}
	}
}

func TestParseManifestErrors(t *testing.T) {
	cases := []string{
		"",
		"not json",
		`{"version":1}`,
	}
	for _, input := range cases {
		if _, err := ParseManifest([]byte(input)); err == nil {
			t.Errorf("%q: expected an error", input)
		}
	}
}

func TestManifestNegativeSizeRejected(t *testing.T) {
	_, manifest := buildCanonical(t)
	manifest.Size = -1
	if err := manifest.Validate(); err == nil {
		t.Fatal("expected negative size to be rejected")
	}
}

func TestResolveDownloadURLRequiresPageURL(t *testing.T) {
	page := &fakePage{urlErr: errFake}
	if _, err := ResolveDownloadURL(page, &Completion{DownloadURL: "1.zip"}); err == nil {
		t.Fatal("expected a page URL error to propagate")
	}
}

var errFake = &fakeError{"fake"}

type fakeError struct{ message string }

func (e *fakeError) Error() string { return e.message }

func TestSiteDefaults(t *testing.T) {
	site := Site{}.Normalize()
	if site.FilterArgument != "filters" || site.StartScript == "" || site.StatusScript == "" {
		t.Fatalf("defaults not applied: %+v", site)
	}
	if site.PollInterval <= 0 || site.PollTimeout <= 0 {
		t.Fatalf("poll defaults not applied: %+v", site)
	}
	// A custom filter argument must be used.
	custom := Site{FilterArgument: "params"}.Normalize()
	script, err := custom.startScript(map[string]string{"a": "b"})
	if err != nil {
		t.Fatalf("script: %s", err)
	}
	if !strings.Contains(script, "function(params)") {
		t.Fatalf("custom argument not used: %s", script)
	}
	// An invalid identifier is refused rather than injected into the script.
	bad := Site{FilterArgument: "a b); alert(1"}.Normalize()
	if _, err := bad.startScript(nil); err == nil {
		t.Fatal("expected an invalid filter argument to be rejected")
	}
}

func TestDecodeBridgeJSONRejectsEmpty(t *testing.T) {
	for _, raw := range []string{"", "null", "undefined", "not json"} {
		if err := decodeBridgeJSON(raw, &startResponse{}); err == nil {
			t.Errorf("%q: expected an error", raw)
		}
	}
}

func TestNewHttpSessionFromClientRejectsNil(t *testing.T) {
	if _, err := NewHttpSessionFromClient(nil); err == nil {
		t.Fatal("expected nil client to be rejected")
	}
}

func TestContentDispositionName(t *testing.T) {
	cases := map[string]string{
		`attachment; filename="report.zip"`: "report.zip",
		`attachment; filename=report.zip`:   "report.zip",
		`attachment`:                        "",
		`attachment; filename='r.zip'; x=y`: "r.zip",
	}
	for input, want := range cases {
		if got := contentDispositionName(input); got != want {
			t.Errorf("%q -> %q, want %q", input, got, want)
		}
	}
}

func TestPathMatches(t *testing.T) {
	cases := []struct {
		cookiePath string
		request    string
		want       bool
	}{
		{"", "/a", true},
		{"/", "/a/b", true},
		{"/exports", "/exports/1.zip", true},
		{"/exports", "/exports", true},
		{"/exports", "/exportsx", false},
		{"/exports", "/other", false},
	}
	for _, c := range cases {
		if got := pathMatches(c.cookiePath, c.request); got != c.want {
			t.Errorf("pathMatches(%q,%q) = %v, want %v", c.cookiePath, c.request, got, c.want)
		}
	}
}

func TestStoreCurrentMissingIsAnError(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Current("job-none"); err == nil {
		t.Fatal("expected a missing CURRENT to be an error")
	}
}

func TestStoreRejectsCorruptCurrent(t *testing.T) {
	store := newTestStore(t)
	data, manifest := buildCanonical(t)
	if _, err := store.Deliver("job-A", "", data, manifest, canonicalSchema()); err != nil {
		t.Fatalf("deliver: %s", err)
	}
	// Overwrite CURRENT with a traversal-shaped value.
	currentPath := store.Root + "/job-A/" + currentFileName
	if err := ioutil.WriteFile(currentPath, []byte("../../etc"), 0644); err != nil {
		t.Fatalf("write: %s", err)
	}
	if _, err := store.Current("job-A"); err == nil {
		t.Fatal("expected an invalid CURRENT to be rejected")
	}
	// Open should still recover by scanning the versions.
	if _, err := store.Open("job-A"); err != nil {
		t.Fatalf("open should recover from a bad pointer: %s", err)
	}
}

func TestDelivererDeliverRejectsMissingDependencies(t *testing.T) {
	cases := []*Deliverer{
		{},
		{Page: &fakePage{}},
		{Page: &fakePage{}, Client: &fakeDownloader{}},
	}
	for i, d := range cases {
		if _, err := d.Deliver(nil, canonicalSchema()); err == nil {
			t.Errorf("case %d: expected a missing-dependency error", i)
		}
	}
}
