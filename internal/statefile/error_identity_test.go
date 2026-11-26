package statefile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/fsutil"
)

// These tests pin what callers outside the package can observe: the error
// identities they test with errors.Is, the error text, and the exact bytes of
// a record. They were written before the record IO moved here from the
// journal package and pass with only the package and sentinel names changed.

func TestSentinelErrorsKeepTextAndStayDistinct(t *testing.T) {
	if got := ErrInvalidFile.Error(); got != "invalid state file" {
		t.Fatalf("ErrInvalidFile text %q", got)
	}
	if got := ErrInvalidData.Error(); got != "invalid journal batch" {
		t.Fatalf("ErrInvalidData text %q", got)
	}
	if errors.Is(ErrInvalidFile, ErrInvalidData) || errors.Is(ErrInvalidData, ErrInvalidFile) {
		t.Fatal("the two sentinels must stay separate identities")
	}
}

// ReadJSON and ReadBytes report unsafe files as invalid-file and never as
// invalid-data; a missing file stays a not-exist error.
func TestUnsafeRecordFilesAreInvalidFileOnly(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	if err := os.WriteFile(target, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(dir, "big.json")
	if err := os.WriteFile(big, []byte(`{"a":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"symbolic link": link, "directory": dir} {
		var value map[string]any
		for _, err := range []error{ReadJSON(path, 1024, &value), readBytesError(path, 1024)} {
			if !errors.Is(err, ErrInvalidFile) || errors.Is(err, ErrInvalidData) {
				t.Fatalf("%s: error = %v, want only ErrInvalidFile", name, err)
			}
		}
	}
	var value map[string]any
	for _, err := range []error{ReadJSON(big, 3, &value), readBytesError(big, 3)} {
		if !errors.Is(err, ErrInvalidFile) || errors.Is(err, ErrInvalidData) {
			t.Fatalf("oversize: error = %v, want only ErrInvalidFile", err)
		}
	}
	missing := filepath.Join(dir, "missing.json")
	for _, err := range []error{ReadJSON(missing, 1024, &value), readBytesError(missing, 1024)} {
		if !errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrInvalidFile) || errors.Is(err, ErrInvalidData) {
			t.Fatalf("missing: error = %v, want only os.ErrNotExist", err)
		}
	}
}

func readBytesError(path string, limit int) error {
	_, err := ReadBytes(path, limit)
	return err
}

type goldenRecord struct {
	Name    string            `json:"name"`
	Text    string            `json:"text,omitempty"`
	Count   int               `json:"count"`
	Ratio   float64           `json:"ratio"`
	Items   []string          `json:"items"`
	None    []string          `json:"none"`
	Labels  map[string]string `json:"labels"`
	Nested  *goldenRecord     `json:"nested,omitempty"`
	Skipped string            `json:"-"`
}

// The exact bytes a writer publishes: field order of the struct, sorted map
// keys, two-space indent, HTML characters escaped, and one final newline.
func TestEncodeJSONExactBytes(t *testing.T) {
	value := goldenRecord{
		Name:   "a<b>&é ",
		Count:  3,
		Ratio:  0.5,
		Items:  []string{},
		Labels: map[string]string{"z": "1", "a": "2"},
		Nested: &goldenRecord{Name: "inner", Text: "t", Ratio: 1e21},
	}
	want := "{\n" +
		"  \"name\": \"a\\u003cb\\u003e\\u0026é\\u2028\",\n" +
		"  \"count\": 3,\n" +
		"  \"ratio\": 0.5,\n" +
		"  \"items\": [],\n" +
		"  \"none\": null,\n" +
		"  \"labels\": {\n" +
		"    \"a\": \"2\",\n" +
		"    \"z\": \"1\"\n" +
		"  },\n" +
		"  \"nested\": {\n" +
		"    \"name\": \"inner\",\n" +
		"    \"text\": \"t\",\n" +
		"    \"count\": 0,\n" +
		"    \"ratio\": 1e+21,\n" +
		"    \"items\": null,\n" +
		"    \"none\": null,\n" +
		"    \"labels\": null\n" +
		"  }\n" +
		"}\n"
	got, err := EncodeJSON(value, 1<<20)
	if err != nil || string(got) != want {
		t.Fatalf("got %q, %v\nwant %q", got, err, want)
	}
}

// What EncodeJSON measures is exactly what WriteAtomic publishes and what
// ReadBytes and ReadJSON return later.
func TestEncodedRecordRoundTripsThroughTheFile(t *testing.T) {
	value := goldenRecord{Name: "round", Count: 7, Items: []string{"x"}, Labels: map[string]string{"k": "v"}}
	encoded, err := EncodeJSON(value, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "r.json")
	if err := fsutil.WriteAtomic(path, encoded); err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil || string(onDisk) != string(encoded) {
		t.Fatalf("file %q, %v", onDisk, err)
	}
	if got, err := ReadBytes(path, len(encoded)); err != nil || string(got) != string(encoded) {
		t.Fatalf("ReadBytes %q, %v", got, err)
	}
	var back goldenRecord
	if err := ReadJSON(path, len(encoded), &back); err != nil || back.Name != "round" || back.Count != 7 {
		t.Fatalf("ReadJSON %+v, %v", back, err)
	}
	again, err := EncodeJSON(back, 1<<20)
	if err != nil || string(again) != string(encoded) {
		t.Fatalf("second encoding %q, %v", again, err)
	}
}
