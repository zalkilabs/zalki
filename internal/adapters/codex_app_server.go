package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/zalkilabs/zalki/internal/bus"
)

const (
	codexToolOutputVersion = "0.164.0"
	codexSocketName        = "app-server-control.sock"
	codexControlDirName    = "app-server-control"
	codexFrameLimit        = 1 << 20
)

var (
	errCodexUnavailable = errors.New("codex app-server unavailable")
	errCodexUnsupported = errors.New("codex app-server does not support tool output")
	errCodexNotLoaded   = errors.New("codex thread is not loaded")
	errCodexIdentity    = errors.New("codex thread identity did not match")
	errCodexApproval    = errors.New("codex thread is waiting for approval")
	errCodexUncertain   = errors.New("codex app-server did not confirm acceptance")
)

type codexSupport int

const (
	codexSupportUnknown codexSupport = iota
	codexSupportVerified
	codexSupportUnavailable
)

type codexSupportKey struct {
	device, inode uint64
	home          string
}

var (
	codexSupportMu    sync.Mutex
	codexSupportCache = map[codexSupportKey]codexSupport{}
)

type codexSocket struct {
	Path          string
	Device, Inode uint64
}

func codexConfigRoot(target bus.Session) string {
	if filepath.IsAbs(target.NativeConfigDir) {
		return filepath.Clean(target.NativeConfigDir)
	}
	return (codexAdapter{}).ConfigDir()
}

func inspectCodexControl(root string) (codexSocket, error) {
	if !filepath.IsAbs(root) {
		return codexSocket{}, errCodexUnavailable
	}
	control := filepath.Join(root, codexControlDirName)
	if err := privateDirectory(control); err != nil {
		return codexSocket{}, err
	}
	link := filepath.Join(control, codexSocketName)
	info, err := os.Lstat(link)
	if err != nil || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) { //nolint:gosec // G115: getuid returns a Unix uid_t, which fits uint32.
		return codexSocket{}, errCodexUnavailable
	}
	resolved := link
	if info.Mode()&os.ModeSymlink != 0 {
		dest, err := os.Readlink(link)
		if err != nil {
			return codexSocket{}, errCodexUnavailable
		}
		if !filepath.IsAbs(dest) {
			dest = filepath.Join(control, dest)
		}
		resolved, err = filepath.EvalSymlinks(dest)
		if err != nil {
			return codexSocket{}, errCodexUnavailable
		}
	} else if info.Mode()&os.ModeSocket == 0 {
		return codexSocket{}, errCodexUnavailable
	}
	return statCodexSocket(resolved)
}

func statCodexSocket(path string) (codexSocket, error) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 {
		return codexSocket{}, errCodexUnavailable
	}
	stat := info.Sys().(*syscall.Stat_t)
	if stat.Uid != uint32(os.Getuid()) || info.Mode().Perm()&0077 != 0 { //nolint:gosec // G115: getuid returns a Unix uid_t, which fits uint32.
		return codexSocket{}, errCodexUnavailable
	}
	if err := privateDirectory(filepath.Dir(path)); err != nil {
		return codexSocket{}, err
	}
	return codexSocket{Path: path, Device: uint64(stat.Dev), Inode: stat.Ino}, nil // #nosec G115 -- Preserve opaque device identity; Darwin's dev_t is signed, Linux's is unsigned.
}

func privateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errCodexUnavailable
	}
	if info.Mode().Perm()&0077 != 0 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) { //nolint:gosec // G115: getuid returns a Unix uid_t, which fits uint32.
		return errCodexUnavailable
	}
	return nil
}

func (s codexSocket) unchanged() error {
	current, err := statCodexSocket(s.Path)
	if err != nil || current != s {
		return errCodexUnavailable
	}
	return nil
}

func deliverCodex(ctx context.Context, msg bus.Message, target bus.Session) bus.Receipt {
	id, err := bus.UUID(bus.NativeID(target))
	if err != nil {
		return failedReceipt(bus.StatusNotSent, err)
	}
	receipt, fallback := deliverCodexAppServer(ctx, msg, target, id)
	if !fallback {
		return receipt
	}
	if ctx.Err() != nil {
		receipt := bus.NewReceipt(bus.StatusNotSent)
		receipt.Error = errCodexUnavailable.Error()
		return receipt.WithCause(ctx.Err())
	}
	return queueCodex(ctx, msg, target)
}

func deliverCodexAppServer(ctx context.Context, msg bus.Message, target bus.Session, threadID string) (bus.Receipt, bool) {
	root := codexConfigRoot(target)
	endpoint, err := inspectCodexControl(root)
	if err != nil {
		return bus.Receipt{}, true
	}
	attempt, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	joined, err := submitCodexToolOutput(attempt, endpoint, root, threadID, bus.Envelope(msg))
	if err == nil {
		output := "started_idle_turn"
		if joined {
			output = "joined_active_turn"
		}
		receipt := bus.NewReceipt(bus.StatusSubmitted)
		receipt.Output = output
		return receipt, false
	}
	if errors.Is(err, errCodexApproval) {
		return failedReceipt(bus.StatusApprovalBlocked, err), false
	}
	if errors.Is(err, errCodexUncertain) {
		status := bus.StatusUnknownWrite
		if errors.Is(attempt.Err(), context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			status = bus.StatusUnknownTimeout
		}
		return failedReceipt(status, err), false
	}
	return bus.Receipt{}, true
}

func submitCodexToolOutput(ctx context.Context, endpoint codexSocket, root, threadID, output string) (joined bool, err error) {
	if err := endpoint.unchanged(); err != nil {
		return false, err
	}
	client, err := newCodexClient(ctx, endpoint)
	if err != nil {
		return false, err
	}
	defer client.Close()
	var initialized struct {
		CodexHome string `json:"codexHome"`
		UserAgent string `json:"userAgent"`
	}
	if err := client.call(ctx, "initialize", map[string]any{
		"clientInfo": map[string]string{"name": "zalki", "version": bus.Version},
	}, &initialized); err != nil {
		if errors.Is(err, errCodexUnsupported) {
			rememberCodexSupport(endpoint, root, codexSupportUnavailable)
		}
		return false, classifyCodexSetupError(err)
	}
	if !codexPathsMatch(initialized.CodexHome, root) || !codexVersionAtLeast(initialized.UserAgent, codexToolOutputVersion) {
		rememberCodexSupport(endpoint, root, codexSupportUnavailable)
		return false, errCodexUnsupported
	}
	if err := client.notify(ctx, "initialized", map[string]any{}); err != nil {
		return false, classifyCodexSetupError(err)
	}
	if err := endpoint.unchanged(); err != nil {
		return false, err
	}
	loaded, err := codexThreadLoaded(ctx, client, threadID)
	if err != nil {
		return false, classifyCodexSetupError(err)
	}
	if !loaded {
		return false, errCodexNotLoaded
	}
	var read struct {
		Thread struct {
			ID     string `json:"id"`
			Status struct {
				Type  string   `json:"type"`
				Flags []string `json:"activeFlags"`
			} `json:"status"`
		} `json:"thread"`
	}
	if err := client.call(ctx, "thread/read", map[string]any{"threadId": threadID, "includeTurns": false}, &read); err != nil {
		return false, classifyCodexSetupError(err)
	}
	if !strings.EqualFold(read.Thread.ID, threadID) {
		return false, errCodexIdentity
	}
	for _, flag := range read.Thread.Status.Flags {
		if flag == "waitingOnApproval" {
			return false, errCodexApproval
		}
	}
	switch read.Thread.Status.Type {
	case "idle":
	case "active":
		joined = true
	default:
		return false, errCodexNotLoaded
	}
	if err := endpoint.unchanged(); err != nil {
		return false, err
	}
	var started struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	params := struct {
		ThreadID   string         `json:"threadId"`
		Input      []struct{}     `json:"input"`
		ToolOutput map[string]any `json:"toolOutput"`
	}{
		ThreadID: threadID,
		Input:    []struct{}{},
		ToolOutput: map[string]any{
			"name": "zalki", "namespace": "zalki", "output": output, "retain": true,
		},
	}
	err = client.callSubmitted(ctx, "turn/start", params, &started)
	if err != nil {
		if errors.Is(err, errCodexUnsupported) {
			rememberCodexSupport(endpoint, root, codexSupportUnavailable)
		}
		return false, err
	}
	if started.Turn.ID == "" {
		return false, errCodexUncertain
	}
	rememberCodexSupport(endpoint, root, codexSupportVerified)
	return joined, nil
}

func classifyCodexSetupError(err error) error {
	if errors.Is(err, errCodexApproval) {
		return err
	}
	if errors.Is(err, errCodexUncertain) || errors.Is(err, errCodexUnsupported) {
		return errCodexUnsupported
	}
	return err
}

func cachedCodexSupport(endpoint codexSocket, root string) codexSupport {
	codexSupportMu.Lock()
	defer codexSupportMu.Unlock()
	return codexSupportCache[codexSupportKey{endpoint.Device, endpoint.Inode, codexSupportHome(root)}]
}

func rememberCodexSupport(endpoint codexSocket, root string, support codexSupport) {
	codexSupportMu.Lock()
	defer codexSupportMu.Unlock()
	codexSupportCache[codexSupportKey{endpoint.Device, endpoint.Inode, codexSupportHome(root)}] = support
}

func codexSupportHome(root string) string {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return filepath.Clean(root)
	}
	return filepath.Clean(resolved)
}

func codexThreadLoaded(ctx context.Context, client *codexClient, threadID string) (bool, error) {
	var cursor string
	for page := 0; page < 16; page++ {
		params := map[string]any{"limit": 200}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var listed struct {
			Data       []string `json:"data"`
			NextCursor *string  `json:"nextCursor"`
		}
		if err := client.call(ctx, "thread/loaded/list", params, &listed); err != nil {
			return false, err
		}
		for _, id := range listed.Data {
			if strings.EqualFold(id, threadID) {
				return true, nil
			}
		}
		if listed.NextCursor == nil || *listed.NextCursor == "" || *listed.NextCursor == cursor {
			return false, nil
		}
		cursor = *listed.NextCursor
	}
	return false, nil
}

func codexPathsMatch(reported, root string) bool {
	reported, err := filepath.EvalSymlinks(reported)
	if err != nil {
		return false
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	return filepath.Clean(reported) == filepath.Clean(root)
}

func codexVersionAtLeast(userAgent, minimum string) bool {
	found := clientVersionPattern.FindString(userAgent)
	if found == "" {
		return false
	}
	parse := func(v string) ([3]int, bool) {
		var out [3]int
		parts := strings.Split(strings.SplitN(v, "-", 2)[0], ".")
		if len(parts) < 3 {
			return out, false
		}
		for i := 0; i < 3; i++ {
			n, err := strconv.Atoi(parts[i])
			if err != nil {
				return out, false
			}
			out[i] = n
		}
		return out, true
	}
	got, ok := parse(found)
	want, okMin := parse(minimum)
	if !ok || !okMin {
		return false
	}
	for i := 0; i < 3; i++ {
		if got[i] != want[i] {
			return got[i] > want[i]
		}
	}
	return true
}

type codexClient struct {
	conn   *websocket.Conn
	nextID int
	sent   bool
}

func newCodexClient(ctx context.Context, endpoint codexSocket) (*codexClient, error) {
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		if err := endpoint.unchanged(); err != nil {
			return nil, err
		}
		return dialer.DialContext(ctx, "unix", endpoint.Path)
	}}
	conn, response, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}})
	if response != nil && response.Body != nil {
		_ = response.Body.Close() // Cleanup after the WebSocket handshake.
	}
	if err != nil {
		if response != nil && response.StatusCode != http.StatusSwitchingProtocols {
			return nil, errCodexUnsupported
		}
		return nil, errCodexUnavailable
	}
	conn.SetReadLimit(codexFrameLimit)
	return &codexClient{conn: conn, nextID: 1}, nil
}

func (c *codexClient) Close() {
	if c == nil || c.conn == nil {
		return
	}
	if c.sent {
		_ = c.conn.Close(websocket.StatusNormalClosure, "")
		return
	}
	_ = c.conn.CloseNow() // No submission occurred; there is no delivery result to update.
}

func (c *codexClient) call(ctx context.Context, method string, params, dest any) error {
	return c.roundTrip(ctx, method, params, dest, false)
}

func (c *codexClient) callSubmitted(ctx context.Context, method string, params, dest any) error {
	return c.roundTrip(ctx, method, params, dest, true)
}

func (c *codexClient) notify(ctx context.Context, method string, params any) error {
	return c.write(ctx, map[string]any{"method": method, "params": params}, false)
}

func (c *codexClient) roundTrip(ctx context.Context, method string, params, dest any, submission bool) error {
	id := c.nextID
	c.nextID++
	if err := c.write(ctx, map[string]any{"id": id, "method": method, "params": params}, submission); err != nil {
		return err
	}
	raw, err := c.readResult(ctx, id, submission)
	if err != nil {
		return err
	}
	if dest == nil || len(raw) == 0 || string(raw) == "null" {
		if submission {
			return errCodexUncertain
		}
		return errCodexUnsupported
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		if submission {
			return errCodexUncertain
		}
		return errCodexUnsupported
	}
	return nil
}

func (c *codexClient) write(ctx context.Context, payload any, submission bool) error {
	err := wsjson.Write(ctx, c.conn, payload)
	if err != nil {
		if submission {
			c.sent = true
			return errCodexUncertain
		}
		return errCodexUnavailable
	}
	if submission {
		c.sent = true
	}
	return nil
}

func (c *codexClient) readResult(ctx context.Context, id int, submission bool) (json.RawMessage, error) {
	want := strconv.Itoa(id)
	for range 1000 {
		var message struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := wsjson.Read(ctx, c.conn, &message); err != nil {
			if submission && c.sent {
				return nil, errCodexUncertain
			}
			return nil, errCodexUnavailable
		}
		if len(message.ID) == 0 || string(message.ID) == "null" || string(message.ID) != want {
			continue
		}
		if failure := codexRPCFailure(message.Error, submission && c.sent); failure != nil {
			return nil, failure
		}
		return message.Result, nil
	}
	if submission && c.sent {
		return nil, errCodexUncertain
	}
	return nil, errCodexUnsupported
}

// Documented JSON-RPC rejections prove the request was not accepted.
// Internal, server, and malformed errors after turn/start do not.
func codexRPCFailure(raw json.RawMessage, submitted bool) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var body struct {
		Code json.RawMessage `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	codeText := ""
	if json.Unmarshal(raw, &body) == nil {
		codeText = strings.TrimSpace(string(body.Code))
	}
	code, codeErr := strconv.Atoi(codeText)
	if codeErr != nil || strings.ContainsAny(codeText, ".eE+") {
		if submitted {
			return errCodexUncertain
		}
		return errCodexUnsupported
	}
	if codexApprovalData(body.Data) {
		return errCodexApproval
	}
	switch code {
	case -32700, -32600, -32601, -32602:
		return errCodexUnsupported
	default:
		if submitted {
			return errCodexUncertain
		}
		return errCodexUnsupported
	}
}

func codexApprovalData(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	return approvalValue(value)
}

func approvalValue(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == "waitingOnApproval" || key == "activeTurnNotSteerable" || key == "approvalDenied" {
				return true
			}
			if approvalValue(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if approvalValue(child) {
				return true
			}
		}
	case string:
		return typed == "waitingOnApproval"
	}
	return false
}
