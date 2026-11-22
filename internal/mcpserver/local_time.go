package mcpserver

import (
	"regexp"
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
