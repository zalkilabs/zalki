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

func TestReadJSON(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string) string
		limit int
		check func(t *testing.T, got sample, err error)
	}{
		{
			name: "decodes a regular file",
			setup: func(t *testing.T, dir string) string {
				t.Helper()
				return writeFile(t, dir, "r.json", `{"name":"a","count":2}`)
			},
			limit: 1024,
			check: func(t *testing.T, got sample, err error) {
				if err != nil || got != (sample{Name: "a", Count: 2}) {
					t.Fatalf("got %+v, %v", got, err)
				}
			},
		},
		{
			name: "a file of exactly the limit is accepted",
			setup: func(t *testing.T, dir string) string {
				t.Helper()
				return writeFile(t, dir, "r.json", `{"name":"abc"}`)
			},
			limit: len(`{"name":"abc"}`),
			check: func(t *testing.T, got sample, err error) {
				if err != nil || got.Name != "abc" {
					t.Fatalf("got %+v, %v", got, err)
				}
			},
		},
		{
			name: "one byte over the limit is invalid",
			setup: func(t *testing.T, dir string) string {
				t.Helper()
				return writeFile(t, dir, "r.json", `{"name":"abc"}`)
			},
			limit: len(`{"name":"abc"}`) - 1,
			check: func(t *testing.T, _ sample, err error) {
				if !errors.Is(err, journal.ErrInvalidFile) {
					t.Fatalf("error = %v, want ErrInvalidFile", err)
				}
			},
		},
		{
			name:  "a missing file is not-exist",
			setup: func(_ *testing.T, dir string) string { return filepath.Join(dir, "missing.json") },
			limit: 1024,
			check: func(t *testing.T, _ sample, err error) {
				if !errors.Is(err, os.ErrNotExist) || errors.Is(err, journal.ErrInvalidFile) {
					t.Fatalf("error = %v, want os.ErrNotExist", err)
				}
			},
		},
		{
			name: "a directory is invalid",
			setup: func(t *testing.T, dir string) string {
				t.Helper()
				path := filepath.Join(dir, "d.json")
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
				return path
			},
			limit: 1024,
			check: func(t *testing.T, _ sample, err error) {
				if !errors.Is(err, journal.ErrInvalidFile) {
					t.Fatalf("error = %v, want ErrInvalidFile", err)
				}
			},
		},
		{
			name: "a symbolic link is invalid even when its target is fine",
			setup: func(t *testing.T, dir string) string {
				t.Helper()
				target := writeFile(t, dir, "target.json", `{"name":"a"}`)
				link := filepath.Join(dir, "link.json")
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
				return link
			},
			limit: 1024,
			check: func(t *testing.T, _ sample, err error) {
				if !errors.Is(err, journal.ErrInvalidFile) {
					t.Fatalf("error = %v, want ErrInvalidFile", err)
				}
			},
		},
		{
			name: "malformed JSON reports the decoder's syntax error",
			setup: func(t *testing.T, dir string) string {
				t.Helper()
				return writeFile(t, dir, "r.json", `{"name":`)
			},
			limit: 1024,
			check: func(t *testing.T, _ sample, err error) {
				var syntax *json.SyntaxError
				if !errors.As(err, &syntax) {
					t.Fatalf("error = %v (%T), want a JSON syntax error", err, err)
				}
			},
		},
		{
			name: "a value of the wrong type reports the decoder's type error",
			setup: func(t *testing.T, dir string) string {
				t.Helper()
				return writeFile(t, dir, "r.json", `{"name":5}`)
			},
			limit: 1024,
			check: func(t *testing.T, _ sample, err error) {
				var typeErr *json.UnmarshalTypeError
				if !errors.As(err, &typeErr) {
					t.Fatalf("error = %v (%T), want an UnmarshalTypeError", err, err)
				}
			},
		},
		{
			name: "an unreadable file reports a permission error",
			setup: func(t *testing.T, dir string) string {
				t.Helper()
				skipIfRoot(t)
				path := writeFile(t, dir, "r.json", `{"name":"a"}`)
				if err := os.Chmod(path, 0); err != nil {
					t.Fatal(err)
				}
				return path
			},
			limit: 1024,
			check: func(t *testing.T, _ sample, err error) {
				if !errors.Is(err, os.ErrPermission) {
					t.Fatalf("error = %v, want os.ErrPermission", err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := test.setup(t, t.TempDir())
			var got sample
			err := journal.ReadJSON(path, test.limit, &got)
			test.check(t, got, err)
		})
	}
}

func TestEncodeJSON(t *testing.T) {
	t.Run("two-space indent and a final newline", func(t *testing.T) {
		got, err := journal.EncodeJSON(sample{Name: "a", Count: 2}, 1024)
		want := "{\n  \"name\": \"a\",\n  \"count\": 2\n}\n"
		if err != nil || string(got) != want {
			t.Fatalf("got %q, %v; want %q", got, err, want)
		}
	})

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
		t.Run(test.name, func(t *testing.T) {
			value := sample{Name: strings.Repeat("x", 100)}
			encoded, err := journal.EncodeJSON(value, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			limit := test.limitFor(len(encoded))
			got, err := journal.EncodeJSON(value, limit)
			if !test.oversize {
				if err != nil || string(got) != string(encoded) {
					t.Fatalf("got %d bytes, %v", len(got), err)
				}
				return
			}
			var size *journal.SizeError
			if !errors.As(err, &size) || size.Size != len(encoded) || size.Limit != limit || got != nil {
				t.Fatalf("got %v, %v; want a SizeError with size %d limit %d", got, err, len(encoded), limit)
			}
		})
	}

	t.Run("a value that cannot be encoded is not a size error", func(t *testing.T) {
		_, err := journal.EncodeJSON(make(chan int), 1024)
		var size *journal.SizeError
		if err == nil || errors.As(err, &size) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestWriteAtomic(t *testing.T) {
	t.Run("creates and replaces with private permissions and no leftovers", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "r.json")
		for _, content := range []string{"first", "second"} {
			if err := journal.WriteAtomic(path, []byte(content)); err != nil {
				t.Fatal(err)
			}
			if b, err := os.ReadFile(path); err != nil || string(b) != content {
				t.Fatalf("content %q, %v", b, err)
			}
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("mode %v, want 0600", info.Mode())
		}
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
			t.Fatalf("directory holds %d entries, %v; want only the record", len(entries), err)
		}
	})

	t.Run("an unwritable directory fails and keeps the old record", func(t *testing.T) {
		skipIfRoot(t)
		dir := t.TempDir()
		path := writeFile(t, dir, "r.json", "old")
		if err := os.Chmod(dir, 0500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
		if err := journal.WriteAtomic(path, []byte("new")); !errors.Is(err, os.ErrPermission) {
			t.Fatalf("error = %v, want os.ErrPermission", err)
		}
		if b, _ := os.ReadFile(path); string(b) != "old" {
			t.Fatalf("record is %q after a failed write", b)
		}
	})

	t.Run("a failed rename leaves no temporary file", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "r.json")
		if err := os.Mkdir(target, 0700); err != nil { // Renaming a file over a directory fails.
			t.Fatal(err)
		}
		if err := journal.WriteAtomic(target, []byte("new")); err == nil {
			t.Fatal("expected an error")
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 1 {
			t.Fatalf("directory holds %d entries after a failed write", len(entries))
		}
	})

	t.Run("a missing directory is an error", func(t *testing.T) {
		err := journal.WriteAtomic(filepath.Join(t.TempDir(), "missing", "r.json"), []byte("x"))
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("error = %v, want os.ErrNotExist", err)
		}
	})
}

// A SizeError and SchemaError describe themselves without leaking a record.
func TestTypedErrorsDescribeLimits(t *testing.T) {
	if got := (&journal.SizeError{Size: 7, Limit: 5}).Error(); got != "encoded record is 7 bytes; limit is 5 bytes" {
		t.Fatalf("SizeError text %q", got)
	}
	if got := (&journal.SchemaError{Version: 3, Supported: 2}).Error(); got != "unsupported record schema 3; supported up to 2" {
		t.Fatalf("SchemaError text %q", got)
	}
}
