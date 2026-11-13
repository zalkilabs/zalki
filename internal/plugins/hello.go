package plugins

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

// hostCapabilities are the capability names this host implements, in the
// order the hello lists them.
var hostCapabilities = []string{"feed", "inbox", "reply", "send", "read"}

// maxCapabilityNames bounds each list in a plugin's hello.
const maxCapabilityNames = 32

type hostHello struct {
	Wire string   `json:"wire"`
	Host hostInfo `json:"host"`
}

type hostInfo struct {
	Version      string   `json:"version"`
	Capabilities []string `json:"capabilities"`
}

type helloOK struct {
	Enabled         []string `json:"enabled"`
	MissingOptional []string `json:"missing_optional"`
}

// grantedCapabilities is what the host offers one installation: the names
// it implements AND its access allows. feed needs status access; inbox and
// reply need receive; send and read need a named session each.
func grantedCapabilities(status *pluginfeed.PluginFeedStatus) []string {
	a := status.Access
	offered := []string{}
	if a.Status {
		offered = append(offered, "feed")
	}
	if a.Receive {
		offered = append(offered, "inbox", "reply")
	}
	if len(a.Send) > 0 {
		offered = append(offered, "send")
	}
	if len(a.Read) > 0 {
		offered = append(offered, "read")
	}
	return offered
}

// oldWires are the wires before zalki-plugin/1. Only these exact values are
// named back to the owner; any other version is plugin text and never shown.
var oldWires = []string{"v1", "v1-read", "v2"}

func oldWireHello(version string) error {
	if slices.Contains(oldWires, version) {
		return oldWireError{version}
	}
	return errHelloVersion
}

// oldWireError names a plugin that still speaks a wire before zalki-plugin/1.
type oldWireError struct{ version string }

func (e oldWireError) Error() string { return e.version }

// missingCapabilityError names what a plugin requires and the host lacks.
type missingCapabilityError struct{ missing []string }

func (e missingCapabilityError) Error() string { return strings.Join(e.missing, ",") }

// greet runs the hello: the host's hello first, then the plugin's answer
// within helloTimeout, then hello_ok or a terminal frame. It returns the
// enabled capability names.
func greet(c *child, offered []string) ([]string, error) {
	if err := c.input.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return nil, c.deadlineError(c.input, err)
	}
	if err := c.pipeError(writeFrame(c.input, map[string]hostHello{"hello": {Wire: wireName, Host: hostInfo{Version: bus.Version, Capabilities: offered}}})); err != nil {
		return nil, err
	}
	header, err := c.receive(helloTimeout)
	if err != nil {
		return nil, err
	}
	enabled, missingOptional, err := negotiate(header, offered)
	var terminal any = map[string]string{"code": "protocol_error"}
	var old oldWireError
	var missing missingCapabilityError
	switch {
	case err == nil:
		terminal = map[string]helloOK{"hello_ok": {Enabled: enabled, MissingOptional: missingOptional}}
	case errors.As(err, &old):
		terminal = map[string]string{"code": "unsupported_wire"}
	case errors.As(err, &missing):
		terminal = map[string]any{"code": "missing_capability", "missing": missing.missing}
	}
	if werr := c.input.SetWriteDeadline(time.Now().Add(time.Second)); werr != nil {
		return nil, errors.Join(err, c.deadlineError(c.input, werr))
	}
	return enabled, errors.Join(err, c.pipeError(writeFrame(c.input, terminal)))
}

// negotiate checks a plugin hello against the offered names. A name in
// requires that is not offered stops the start; an optional one that is not
// offered (or not known at all) is only reported back.
func negotiate(header frame, offered []string) (enabled, missingOptional []string, err error) {
	h := header.Hello
	if h == nil || header.ID != "" || header.Ack != "" || header.Gap != "" || header.Event != nil || header.isRequest() {
		return nil, nil, errHelloVersion
	}
	if h.Version != "" {
		return nil, nil, oldWireHello(h.Version)
	}
	if h.Wire != wireName || !validNames(h.Requires) || !validNames(h.Optional) {
		return nil, nil, errHelloVersion
	}
	if missing := notIn(h.Requires, offered); len(missing) > 0 {
		return nil, nil, missingCapabilityError{missing}
	}
	enabled = []string{}
	for _, name := range offered {
		if slices.Contains(h.Requires, name) || slices.Contains(h.Optional, name) {
			enabled = append(enabled, name)
		}
	}
	return enabled, notIn(h.Optional, offered), nil
}

// notIn returns the names that are not offered, never nil.
func notIn(names, offered []string) []string {
	out := []string{}
	for _, name := range names {
		if !slices.Contains(offered, name) {
			out = append(out, name)
		}
	}
	return out
}

// validNames accepts at most 32 names, each [a-z][a-z0-9_]{0,31}, no repeats.
func validNames(names []string) bool {
	if len(names) > maxCapabilityNames {
		return false
	}
	for i, name := range names {
		if name == "" || len(name) > 32 || name[0] < 'a' || name[0] > 'z' || slices.Contains(names[:i], name) {
			return false
		}
		for _, r := range name {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
				return false
			}
		}
	}
	return true
}

// helloError turns a failed hello into the owner's text.
func helloError(name string, err error) error {
	var old oldWireError
	var missing missingCapabilityError
	switch {
	case errors.As(err, &old):
		return hostHintError{fmt.Sprintf("%s uses the old plugin wire (%s). Zalki now speaks only zalki-plugin/1; rebuild the plugin from its updated source and install it again", name, old.version)}
	case errors.As(err, &missing):
		return hostHintError{fmt.Sprintf("%s needs %s, which this Zalki does not offer (or which is not granted). Update Zalki or grant it", name, quoteNames(missing.missing))}
	case errors.Is(err, errHelloVersion):
		return hostHintError{"plugin hello failed: " + errHelloVersion.Error()}
	}
	return fmt.Errorf("plugin hello failed: %w", err)
}

// hostHintError is owner text the host wrote from fixed words, known
// literals and checked names, never from plugin bytes. It is shown even
// when the plugin's stderr is withheld.
type hostHintError struct{ text string }

func (e hostHintError) Error() string { return e.text }

func quoteNames(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = fmt.Sprintf("%q", name)
	}
	return strings.Join(quoted, ", ")
}
