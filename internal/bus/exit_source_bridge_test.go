package bus

import (
	"errors"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/procwatch"
)

type rawExitSourceStub struct {
	events           chan procwatch.Event
	watch, unwatch   procwatch.Identity
	directory        string
	watchErr, endErr error
	closes           int
}

func (s *rawExitSourceStub) WatchProcess(ref procwatch.Identity) error {
	s.watch = ref
	return s.watchErr
}
func (s *rawExitSourceStub) Unwatch(ref procwatch.Identity) { s.unwatch = ref }
func (s *rawExitSourceStub) WatchDir(path string) error {
	s.directory = path
	return s.watchErr
}
func (s *rawExitSourceStub) Events() <-chan procwatch.Event { return s.events }
func (s *rawExitSourceStub) Close() error {
	s.closes++
	return s.endErr
}

func TestExitSourceBridgeProjectsProcessIdentity(t *testing.T) {
	raw := &rawExitSourceStub{events: make(chan procwatch.Event), watchErr: errors.New("inspect denied")}
	source := adaptExitSource(raw)
	defer source.Close()
	ref := ProcessRef{PID: 42, Start: "first-incarnation", Role: "backend"}
	want := procwatch.Identity{PID: ref.PID, Start: ref.Start}
	if err := source.WatchProcess(ref); !errors.Is(err, raw.watchErr) || raw.watch != want {
		t.Fatalf("WatchProcess = %+v, %v; want %+v, %v", raw.watch, err, want, raw.watchErr)
	}
	source.Unwatch(ref)
	if raw.unwatch != want {
		t.Fatalf("Unwatch = %+v, want %+v", raw.unwatch, want)
	}
	if err := source.WatchDir("/watched"); !errors.Is(err, raw.watchErr) || raw.directory != "/watched" {
		t.Fatalf("WatchDir = %q, %v", raw.directory, err)
	}
}

func TestExitSourceBridgeConvertsEvents(t *testing.T) {
	raw := &rawExitSourceStub{events: make(chan procwatch.Event)}
	source := adaptExitSource(raw)
	defer source.Close()
	for _, test := range []struct {
		name string
		raw  procwatch.EventKind
		want exitEventKind
	}{
		{"process", procwatch.ProcessExited, exitProcess},
		{"directory", procwatch.DirectoryChanged, exitDirChanged},
		{"failure", procwatch.SourceFailed, exitSourceFailed},
		{"unknown", procwatch.EventKind(99), 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := procwatch.Event{Kind: test.raw, Process: procwatch.Identity{PID: 42, Start: "one"}}
			select {
			case raw.events <- input:
			case <-time.After(time.Second):
				t.Fatal("bridge did not receive raw event")
			}
			select {
			case event := <-source.Events():
				if event.Kind != test.want || event.Process != (ProcessRef{PID: 42, Start: "one"}) {
					t.Fatalf("converted event = %+v", event)
				}
			case <-time.After(time.Second):
				t.Fatal("bridge did not forward event")
			}
		})
	}
}

func TestExitSourceBridgeCloseStopsBlockedForwarding(t *testing.T) {
	raw := &rawExitSourceStub{events: make(chan procwatch.Event), endErr: errors.New("source close")}
	source := adaptExitSource(raw)
	select {
	case raw.events <- procwatch.Event{Kind: procwatch.DirectoryChanged}:
	case <-time.After(time.Second):
		t.Fatal("bridge did not receive raw event")
	}
	// Nobody consumes the converted event. Close must still stop both sources.
	done := make(chan error, 1)
	go func() { done <- source.Close() }()
	select {
	case err := <-done:
		if !errors.Is(err, raw.endErr) {
			t.Fatalf("Close = %v, want %v", err, raw.endErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Close blocked behind event consumer")
	}
	if err := source.Close(); err != nil || raw.closes != 1 {
		t.Fatalf("repeated Close = %v; raw closes = %d", err, raw.closes)
	}
	select {
	case _, ok := <-source.Events():
		if !ok {
			t.Fatal("bridge changed the source's open-channel close contract")
		}
	default:
	}
}
