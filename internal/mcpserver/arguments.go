package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Run before SDK schema validation so a typo teaches the caller the contract.
// The schema still rejects unknown fields; argument values are never echoed.
func argumentHints(definitions []map[string]any) mcp.Middleware {
	accepted := map[string][]string{}
	for _, definition := range definitions {
		name := definition["name"].(string)
		properties := definition["inputSchema"].(map[string]any)["properties"].(map[string]any)
		accepted[name] = []string{}
		for field := range properties {
			accepted[name] = append(accepted[name], field)
		}
		slices.Sort(accepted[name])
	}
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			params, ok := request.GetParams().(*mcp.CallToolParamsRaw)
			if method != "tools/call" || !ok {
				return next(ctx, method, request)
			}
			fields, known := accepted[params.Name]
			var arguments map[string]json.RawMessage
			if !known || json.Unmarshal(params.Arguments, &arguments) != nil {
				return next(ctx, method, request)
			}
			var unknown []string
			for field := range arguments {
				if !slices.Contains(fields, field) {
					unknown = append(unknown, fmt.Sprintf("%q", field))
				}
			}
			if len(unknown) == 0 {
				return next(ctx, method, request)
			}
			slices.Sort(unknown)
			hint := fmt.Sprintf("unknown fields %s; %s accepts %s.", strings.Join(unknown, ", "), params.Name, strings.Join(fields, ", "))
			switch params.Name {
			case "list_agents":
				hint = "list_agents takes no arguments; omit actor and capability."
			case "send_message":
				hint += " Use body for message text. message_id is optional; pass it to make retries safe."
			case "reply_message":
				hint += " message_id is the incoming message you are replying to; body is the reply. The outgoing ID is generated."
			}
			result := &mcp.CallToolResult{}
			result.SetError(fmt.Errorf("%s", hint))
			return result, nil
		}
	}
}
