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
// Normalize supported aliases before validation; all other unknown fields are
// rejected. Argument values are never echoed.
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
			changed, err := normalizeAliases(params.Name, arguments)
			if err != nil {
				result := &mcp.CallToolResult{}
				result.SetError(err)
				return result, nil
			}
			var unknown []string
			for field := range arguments {
				if !slices.Contains(fields, field) {
					unknown = append(unknown, fmt.Sprintf("%q", field))
				}
			}
			if len(unknown) == 0 {
				if params.Name == "list_agents" {
					_, actorGiven := arguments["actor"]
					_, keyGiven := arguments["capability"]
					if actorGiven || keyGiven {
						var actor, key string
						_ = json.Unmarshal(arguments["actor"], &actor)
						_ = json.Unmarshal(arguments["capability"], &key)
						if actor == "" || key == "" {
							result := &mcp.CallToolResult{}
							result.SetError(fmt.Errorf("actor and capability must both be nonempty when provided"))
							return result, nil
						}
					}
				}
				if changed {
					params.Arguments, _ = json.Marshal(arguments)
				}
				return next(ctx, method, request)
			}
			slices.Sort(unknown)
			hint := fmt.Sprintf("unknown fields %s; %s accepts %s.", strings.Join(unknown, ", "), params.Name, strings.Join(fields, ", "))
			switch params.Name {
			case "list_agents":
				hint += " Omit credentials or provide actor and capability together to mark self."
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

// normalizeAliases leaves schemas canonical while accepting common call spellings.
// String comparison is semantic, so JSON escape differences are not conflicts.
func normalizeAliases(tool string, arguments map[string]json.RawMessage) (bool, error) {
	if tool != "send_message" && tool != "reply_message" {
		return false, nil
	}
	aliases := [][2]string{{"message", "body"}}
	if tool == "reply_message" {
		aliases = append(aliases, [2]string{"reply_to", "message_id"})
	}
	changed := false
	for _, pair := range aliases {
		alias, canonical := pair[0], pair[1]
		raw, exists := arguments[alias]
		if !exists {
			continue
		}
		var value string
		if string(raw) == "null" || json.Unmarshal(raw, &value) != nil {
			return false, fmt.Errorf("%s must be a string", alias)
		}
		if other, exists := arguments[canonical]; exists {
			var canonicalValue string
			if string(other) == "null" || json.Unmarshal(other, &canonicalValue) != nil {
				return false, fmt.Errorf("%s must be a string", canonical)
			}
			if canonicalValue != value {
				return false, fmt.Errorf("%s and %s must match when both are provided", alias, canonical)
			}
		} else {
			arguments[canonical] = raw
		}
		delete(arguments, alias)
		changed = true
	}
	return changed, nil
}
