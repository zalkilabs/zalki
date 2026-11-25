package journal_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/journal"
)

type sample struct {
	Name  string `json:"name"`
	Count int    `json:"count,omitempty"`
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permission checks do not apply to root")
	}
}

// readJSONCase is one ReadJSON case: it prepares a path, then checks the result.
type readJSONCase struct {
	name  string
	setup func(t *testing.T, dir string) string
	limit int
	check func(t *testing.T, got sample, err error)
}

func TestReadJSON(t *testing.T) {
	for _, test := range readJSONCases() {
		t.Run(test.name, func(t *testing.T) {
			path := test.setup(t, t.TempDir())
			var got sample
			err := journal.ReadJSON(path, test.limit, &got)
			test.check(t, got, err)
		})
	}
}

func readJSONCases() []readJSONCase {
	const abc = `{"name":"abc"}`
	return []readJSONCase{
		{"decodes a regular file", setupJSONFile(`{"name":"a","count":2}`), 1024, wantSample(sample{Name: "a", Count: 2})},
		{"a file of exactly the limit is accepted", setupJSONFile(abc), len(abc), wantSample(sample{Name: "abc"})},
		{"one byte over the limit is invalid", setupJSONFile(abc), len(abc) - 1, wantInvalidFile},
		{"a missing file is not-exist", setupMissingFile, 1024, wantNotExist},
		{"a directory is invalid", setupDirectory, 1024, wantInvalidFile},
		{"a symbolic link is invalid even when its target is fine", setupSymlink, 1024, wantInvalidFile},
		{"malformed JSON reports the decoder's syntax error", setupJSONFile(`{"name":`), 1024, wantSyntaxError},
		{"a value of the wrong type reports the decoder's type error", setupJSONFile(`{"name":5}`), 1024, wantTypeError},
		{"an unreadable file reports a permission error", setupUnreadableFile, 1024, wantPermissionError},
	}
}

func setupJSONFile(content string) func(t *testing.T, dir string) string {
	return func(t *testing.T, dir string) string {
		t.Helper()
		return writeFile(t, dir, "r.json", content)
	}
}

func setupMissingFile(_ *testing.T, dir string) string { return filepath.Join(dir, "missing.json") }

func setupDirectory(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "d.json")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func setupSymlink(t *testing.T, dir string) string {
	t.Helper()
	target := writeFile(t, dir, "target.json", `{"name":"a"}`)
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	return link
}

func setupUnreadableFile(t *testing.T, dir string) string {
	t.Helper()
	skipIfRoot(t)
	path := writeFile(t, dir, "r.json", `{"name":"a"}`)
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	return path
}

func wantSample(want sample) func(t *testing.T, got sample, err error) {
	return func(t *testing.T, got sample, err error) {
		t.Helper()
		if err != nil || got != want {
			t.Fatalf("got %+v, %v", got, err)
		}
	}
}

func wantInvalidFile(t *testing.T, _ sample, err error) {
	t.Helper()
	if !errors.Is(err, journal.ErrInvalidFile) {
		t.Fatalf("error = %v, want ErrInvalidFile", err)
	}
}

func wantNotExist(t *testing.T, _ sample, err error) {
	t.Helper()
	if !errors.Is(err, os.ErrNotExist) || errors.Is(err, journal.ErrInvalidFile) {
		t.Fatalf("error = %v, want os.ErrNotExist", err)
	}
}

func wantSyntaxError(t *testing.T, _ sample, err error) {
	t.Helper()
	var syntax *json.SyntaxError
	if !errors.As(err, &syntax) {
		t.Fatalf("error = %v (%T), want a JSON syntax error", err, err)
	}
}

func wantTypeError(t *testing.T, _ sample, err error) {
	t.Helper()
	var typeErr *json.UnmarshalTypeError
	if !errors.As(err, &typeErr) {
		t.Fatalf("error = %v (%T), want an UnmarshalTypeError", err, err)
	}
}

func wantPermissionError(t *testing.T, _ sample, err error) {
	t.Helper()
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("error = %v, want os.ErrPermission", err)
	}
}

func TestEncodeJSON(t *testing.T) {
	t.Run("two-space indent and a final newline", checkEncodeIndent)
	tests := []struct {
		name     string
		limitFor func(encoded int) int
		oversize bool
	}{
		{"exactly the limit is accepted", func(n int) int { return n }, false},
		{"one byte over the limit is refused", func(n int) int { return n - 1 }, true},
		{"far over the limit is refused", func(n int) int { return 1 }, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) { checkEncodeLimit(t, test.limitFor, test.oversize) })
	}
	t.Run("a value that cannot be encoded is not a size error", checkEncodeUnencodable)
}

func checkEncodeIndent(t *testing.T) {
	got, err := journal.EncodeJSON(sample{Name: "a", Count: 2}, 1024)
	want := "{\n  \"name\": \"a\",\n  \"count\": 2\n}\n"
	if err != nil || string(got) != want {
		t.Fatalf("got %q, %v; want %q", got, err, want)
	}
}

func checkEncodeLimit(t *testing.T, limitFor func(int) int, oversize bool) {
	t.Helper()
	value := sample{Name: strings.Repeat("x", 100)}
	encoded, err := journal.EncodeJSON(value, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	limit := limitFor(len(encoded))
	got, err := journal.EncodeJSON(value, limit)
	if !oversize {
		if err != nil || string(got) != string(encoded) {
			t.Fatalf("got %d bytes, %v", len(got), err)
		}
		return
	}
	var size *journal.SizeError
	if !errors.As(err, &size) || size.Size != len(encoded) || size.Limit != limit || got != nil {
		t.Fatalf("got %v, %v; want a SizeError with size %d limit %d", got, err, len(encoded), limit)
	}
}

func checkEncodeUnencodable(t *testing.T) {
	_, err := journal.EncodeJSON(make(chan int), 1024)
	var size *journal.SizeError
	if err == nil || errors.As(err, &size) {
		t.Fatalf("error = %v", err)
	}
}

// A SizeError and SchemaError describe themselves without leaking a record.
func TestTypedErrorsDescribeLimits(t *testing.T) {
	if got := (&journal.SizeError{Size: 7, Limit: 5}).Error(); got != "encoded record is 7 bytes; limit is 5 bytes" {
		t.Fatalf("SizeError text %q", got)
	}
	if got := (&journal.SchemaError{Version: 3, Supported: 2}).Error(); got != "unsupported record schema 3; expected 2" {
		t.Fatalf("SchemaError text %q", got)
	}
}
