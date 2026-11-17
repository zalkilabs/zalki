package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/install"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestStateArchiveBackgroundEvidenceIsConservative(t *testing.T) {
	for _, test := range []struct {
		name string
		report install.Report
		err error
		allowed bool
	}{
		{"absent", install.Report{ManagedState: "missing", ServiceState: "not_loaded"}, nil, true},
		{"no platform service", install.Report{ManagedState: "missing", ServiceState: "unsupported"}, nil, true},
		{"may restart", install.Report{ManagedState: "managed", ServiceState: "not_loaded"}, nil, false},
		{"loaded", install.Report{ManagedState: "missing", ServiceState: "loaded"}, nil, false},
		{"unknown", install.Report{ManagedState: "missing", ServiceState: "unknown"}, nil, false},
		{"disabled", install.Report{ManagedState: "managed", ServiceState: "disabled"}, nil, false},
		{"pending", install.Report{ManagedState: "missing", ServiceState: "not_loaded", OperationPending: true}, nil, false},
		{"inspection failure", install.Report{ManagedState: "missing", ServiceState: "not_loaded"}, errors.New("fixture query error"), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := archiveBackgroundReport(test.report, test.err) == nil; got != test.allowed {
				t.Fatalf("ARCHIVE_SERVICE_NOT_PROVEN_STOPPED: allowed=%v want=%v", got, test.allowed)
			}
		})
	}
}

func TestStateArchiveUnsafeRootsAndIdentityProbesRefuse(t *testing.T) {
	f := newArchiveFixture(t)
	for _, root := range []string{"/", f.request.home, f.request.config, filepath.Join(f.request.home, ".codex")} {
		if err := archiveProtectedRoot(root, f.request); err == nil {
			t.Fatal("ARCHIVE_ACCEPTED_PROTECTED_ROOT", root)
		}
	}
	if err := archiveProcessStopped(&model.ProcessRef{PID: os.Getpid()}); err == nil {
		t.Fatal("ARCHIVE_ACCEPTED_UNKNOWN_PROCESS_IDENTITY")
	}
	path := filepath.Join(f.request.source, "not-a-socket")
	archiveWrite(t, path, []byte("opaque"))
	if err := archiveSocketStopped(path); err == nil {
		t.Fatal("ARCHIVE_ACCEPTED_UNKNOWN_SOCKET_IDENTITY")
	}
	link := f.request.source + "-link"
	if err := os.Symlink(f.request.source, link); err != nil {
		t.Fatal(err)
	}
	request := f.request
	request.source = link
	if _, err := prepareStateArchive(request, f.ops); err == nil {
		t.Fatal("ARCHIVE_ACCEPTED_LINKED_ROOT")
	}
}
