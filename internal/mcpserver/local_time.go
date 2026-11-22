package mcpserver

import (
	"regexp"
	"strconv"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// localTime shows a stored UTC time in this machine's zone, with its offset, in
// MCP output only. Stored records and the plugin feed keep their UTC text. A
// value that is not an RFC 3339 time is returned as it is.
func localTime(value string) string {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return value
	}
	return parsed.Local().Format(time.RFC3339Nano)
}

// localWatch shows the times of a watch result as local time with an offset.
func localWatch(result bus.WatchResult, err error) (bus.WatchResult, error) {
	result.At, result.ExpiresAt = localTime(result.At), localTime(result.ExpiresAt)
	return result, err
}

// localAgents shows each updated_at as local time with an offset. It copies the
// rows and keeps a nil list nil.
func localAgents(agents []bus.AgentInfo) []bus.AgentInfo {
	if agents == nil {
		return nil
	}
	shown := make([]bus.AgentInfo, len(agents))
	for i, agent := range agents {
		agent.UpdatedAt = localTime(agent.UpdatedAt)
		if since, err := time.Parse(time.RFC3339Nano, agent.ApprovalSince); err == nil {
			agent.ApprovalSince = since.Local().Format(time.RFC3339)
		}
		shown[i] = agent
	}
	return shown
}

// retryAt finds the UTC retry time of a rate limit message: "(at <time>)".
var retryAt = regexp.MustCompile(`\(at (\d{4}-\d{2}-\d{2}T[0-9:.]+Z)\)`)

// localErrorText is the error text for MCP output. A rate limit names its retry
// time in UTC; here it is shown as local time with an offset. The same text
// stays UTC in the plugin feed.
func localErrorText(err error) string {
	text := err.Error()
	if model.ErrorCodeOf(err) != model.CodeRateLimited {
		return text
	}
	return retryAt.ReplaceAllStringFunc(text, func(match string) string {
		return "(at " + localTime(retryAt.FindStringSubmatch(match)[1]) + ")"
	})
}

// clock is the time now. Tests replace it.
var clock = time.Now

// waitingSince says since when a message held for a permission prompt has been
// waiting: the start as local time with an offset, and for a minute or more how
// long. It is built from the stored receipt time when the answer is built, and
// stores nothing. It returns "" when the message has no usable hold time.
func waitingSince(msg model.Message) string {
	if msg.Receipt == nil {
		return ""
	}
	start, err := time.Parse(time.RFC3339Nano, msg.Receipt.At)
	if err != nil {
		return ""
	}
	text := "since " + start.Local().Format(time.RFC3339)
	if age := clock().Sub(start); age >= time.Minute {
		text += " (" + waitingAge(age) + ")"
	}
	return text
}

// waitingAge shows a wait in whole minutes, or hours and minutes: "5 min",
// "16 h 3 min", "40 h".
func waitingAge(age time.Duration) string {
	minutes := int(age / time.Minute)
	if minutes < 60 {
		return strconv.Itoa(minutes) + " min"
	}
	if minutes%60 == 0 {
		return strconv.Itoa(minutes/60) + " h"
	}
	return strconv.Itoa(minutes/60) + " h " + strconv.Itoa(minutes%60) + " min"
}

// heldNote is the note of a send result for a message held for a permission
// prompt, with since when it waits.
func heldNote(msg model.Message) string {
	const note = "Queued until the target's permission prompt is answered"
	if since := waitingSince(msg); since != "" {
		return note + "; waiting " + since + "."
	}
	return note + "."
}

// heldHistoryText is the text of a held message in a list, with since when it waits.
func heldHistoryText(msg model.Message) string {
	if since := waitingSince(msg); since != "" {
		return "Waiting for the target's permission prompt " + since + "."
	}
	return ""
}
