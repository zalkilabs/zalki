package bus_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"

	agentapi "github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// The characterization tests use the exported bus API and persisted journal
// format rather than private Go identifiers. They pin observable behavior
// across package moves; public aliases can bridge later type migrations.

// fakeAdapter is a minimal client adapter with scripted transport behavior.
type fakeAdapter struct {
	kind string
	caps model.ReceiveCapabilities

	mu      sync.Mutex
	calls   int
	deliver func(context.Context, bus.Message, bus.Session) bus.Receipt
}

func (a *fakeAdapter) Descriptor() agentapi.AdapterDescriptor {
	return agentapi.AdapterDescriptor{Kind: a.kind, Label: a.kind, ProcessNames: []string{a.kind}, OwnerRole: "client", Executable: a.kind}
}

func (a *fakeAdapter) ReceiveCapabilities(bus.Session) model.ReceiveCapabilities { return a.caps }

func (a *fakeAdapter) Deliver(ctx context.Context, msg bus.Message, target bus.Session) bus.Receipt {
	a.mu.Lock()
	a.calls++
	deliver := a.deliver
	a.mu.Unlock()
	if deliver == nil {
		return bus.NewReceipt(model.StatusSubmitted)
	}
	return deliver(ctx, msg, target)
}

func (a *fakeAdapter) transportCalls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

func (a *fakeAdapter) script(deliver func(context.Context, bus.Message, bus.Session) bus.Receipt) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.deliver = deliver
}

// nativeAdapter delivers through an in-process native inbox.
func nativeAdapter(kind string) *fakeAdapter {
	return &fakeAdapter{kind: kind, caps: model.ReceiveCapabilities{
		IdleWake: true, Delivery: "fake_native", Inbox: model.InboxNative, BusyDelivery: model.DeliveryNextTurn,
	}}
}

// listenerAdapter needs a native listener, like a client that wakes through a
// background process.
func listenerAdapter(kind string) *fakeAdapter {
	return &fakeAdapter{kind: kind, caps: model.ReceiveCapabilities{
		Delivery: "fake_listener", Inbox: model.InboxJournal, BusyDelivery: model.DeliveryNextHook, RequiresListener: true,
	}}
}

// newStore returns a Store in a private temporary directory.
func newStore(t *testing.T, adapters ...agentapi.Adapter) bus.Store {
	t.Helper()
	registry, err := agentapi.NewRegistry(adapters...)
	if err != nil {
		t.Fatal(err)
	}
	store, err := bus.NewStore(filepath.Join(t.TempDir(), "state"), registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// restart returns a Store on the same directory, as a new process would see it.
func restart(t *testing.T, store bus.Store) bus.Store {
	t.Helper()
	fresh, err := bus.NewStore(store.Dir, store.Registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	return fresh
}

func newUUID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	b[6], b[8] = b[6]&15|0x40, b[8]&63|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// agent is a registered session and the capability that proves it.
type agent struct {
	bus.Session
	Key string
}

func register(t *testing.T, store bus.Store, kind string) agent {
	t.Helper()
	session, key, err := store.Register(kind, newUUID(t), "/workspace", nil)
	if err != nil {
		t.Fatal(err)
	}
	return agent{Session: session, Key: key}
}

// readJSONFile decodes one journal file into a generic map.
func readJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(b, &record); err != nil {
		t.Fatalf("%s is not a JSON object: %v", path, err)
	}
	return record
}

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}

// sessionFile is where the journal keeps a registration: "kind:uuid" is stored
// as sessions/kind_uuid.json.
func sessionFile(store bus.Store, a agent) string {
	return filepath.Join(store.Dir, "sessions", strings.Replace(a.ID, ":", "_", 1)+".json")
}

// snapshot returns every regular file under dir keyed by relative path.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !entry.Type().IsRegular() {
			return err
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		rel, _ := filepath.Rel(dir, path)
		files[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// listenSocket binds a private Unix socket so a lease can arm a real endpoint.
func listenSocket(t *testing.T) *model.Endpoint {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "zalki-char-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "inbox.sock"), Net: "unix"})
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		t.Skipf("execution sandbox forbids binding a Unix socket: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	endpoint, err := bus.InspectEndpoint(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return endpoint
}

var (
	uuidPattern = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	timePattern = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T[0-9:.]+(?:Z|[+-]\d{2}:\d{2})`)
	hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// normalize replaces values that change on every run: identifiers,
// timestamps and key hashes. Everything else stays exact.
func normalize(text string) string {
	if hashPattern.MatchString(text) {
		return "<hash>"
	}
	text = timePattern.ReplaceAllString(text, "<time>")
	return uuidPattern.ReplaceAllString(text, "<uuid>")
}

// flatten renders a decoded JSON value as sorted "path = value" lines. When
// quotaShapeOnly is set, token balances are reduced to their type so quota
// tuning and elapsed time do not change a layout test. Schema versions stay exact.
func flatten(prefix string, value any, quotaShapeOnly bool, lines *[]string) {
	switch v := value.(type) {
	case map[string]any:
		if len(v) == 0 {
			*lines = append(*lines, prefix+" = {}")
		}
		for key, child := range v {
			flatten(prefix+"."+normalize(key), child, quotaShapeOnly, lines)
		}
	case []any:
		if len(v) == 0 {
			*lines = append(*lines, prefix+" = []")
		}
		for i, child := range v {
			flatten(fmt.Sprintf("%s[%d]", prefix, i), child, quotaShapeOnly, lines)
		}
	case string:
		*lines = append(*lines, fmt.Sprintf("%s = %q", prefix, normalize(v)))
	case float64:
		if quotaShapeOnly && strings.HasSuffix(prefix, ".tokens") {
			*lines = append(*lines, prefix+" = <number>")
		} else {
			*lines = append(*lines, fmt.Sprintf("%s = %v", prefix, v))
		}
	default:
		*lines = append(*lines, fmt.Sprintf("%s = %v", prefix, v))
	}
}

// describeJournal lists every directory and file of a state directory and the
// flattened, normalized content of the JSON files whose relative path starts
// with one of the given prefixes. Flood token balances are reduced to their
// type, and lines containing any skip fragment are left out. Entries are sorted
// after normalizing, so random identifiers cannot change the order. JSON key
// order and platform-specific temporary paths are not part of the comparison.
func describeJournal(t *testing.T, dir string, contentPrefixes []string, skip ...string) string {
	t.Helper()
	var entries []string
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if rel == "." {
			return nil
		}
		name := normalize(rel)
		switch {
		case entry.IsDir():
			entries = append(entries, "dir  "+name+"\n")
		case !entry.Type().IsRegular():
			entries = append(entries, "other "+name+"\n")
		default:
			block := "file " + name + "\n"
			for _, prefix := range contentPrefixes {
				if !strings.HasPrefix(rel, prefix) {
					continue
				}
				var lines []string
				flatten("", readJSONFile(t, path), strings.HasPrefix(rel, "flood"), &lines)
				sort.Strings(lines)
			lines:
				for _, line := range lines {
					for _, fragment := range skip {
						if strings.Contains(line, fragment) {
							continue lines
						}
					}
					block += "       " + line + "\n"
				}
			}
			entries = append(entries, block)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(entries)
	return strings.Join(entries, "")
}

// copyDir copies a state directory, keeping the private permissions that the
// journal requires.
func copyDir(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, path)
		target := filepath.Join(to, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0600)
	})
	if err != nil {
		t.Fatal(err)
	}
}
