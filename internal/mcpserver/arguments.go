package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type argumentAlias struct {
	alias, canonical string
}

// Run before SDK schema validation so a typo teaches the caller the contract.
// Normalize declared aliases before validation; all other unknown fields are
// rejected. Built-in alias and field errors omit argument values; validator
// errors are returned verbatim.
func argumentHints(definitions []toolSpec) mcp.Middleware {
	accepted := map[string][]string{}
	specs := map[string]toolSpec{}
	for _, definition := range definitions {
		specs[definition.name] = definition
		accepted[definition.name] = acceptedFields(definition)
	}
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			params, ok := request.GetParams().(*mcp.CallToolParamsRaw)
			if method != "tools/call" || !ok {
				return next(ctx, method, request)
			}
			definition, known := specs[params.Name]
			var arguments map[string]json.RawMessage
			if !known || json.Unmarshal(params.Arguments, &arguments) != nil {
				return next(ctx, method, request)
			}
			changed, refusal := checkToolArguments(definition, accepted[params.Name], arguments)
			if refusal != nil {
				return refusal, nil
			}
			if changed {
				params.Arguments, _ = json.Marshal(arguments)
			}
			return next(ctx, method, request)
		}
	}
}

// acceptedFields lists the canonical field names of a tool in sorted order.
func acceptedFields(definition toolSpec) []string {
	fields := make([]string, 0, len(definition.input.Properties))
	for field := range definition.input.Properties {
		fields = append(fields, field)
	}
	slices.Sort(fields)
	return fields
}

// checkToolArguments runs the argument checks in this order: aliases,
// unknown fields, then the tool's own validator. It returns the first
// refusal as a tool error. On success it reports whether an alias was
// rewritten in arguments.
func checkToolArguments(definition toolSpec, fields []string, arguments map[string]json.RawMessage) (changed bool, refusal *mcp.CallToolResult) {
	changed, err := normalizeAliases(definition.aliases, arguments)
	if err != nil {
		return false, argumentError(err)
	}
	if unknown := unknownFields(arguments, fields); len(unknown) != 0 {
		return false, argumentError(fmt.Errorf("unknown fields %s; %s accepts %s.%s", strings.Join(unknown, ", "), definition.name, strings.Join(fields, ", "), definition.hint))
	}
	if definition.validate != nil {
		if err := definition.validate(arguments); err != nil {
			return false, argumentError(err)
		}
	}
	return changed, nil
}

// unknownFields returns the quoted names of the arguments that are not
// accepted, sorted.
func unknownFields(arguments map[string]json.RawMessage, fields []string) []string {
	var unknown []string
	for field := range arguments {
		if !slices.Contains(fields, field) {
			unknown = append(unknown, fmt.Sprintf("%q", field))
		}
	}
	slices.Sort(unknown)
	return unknown
}

func argumentError(err error) *mcp.CallToolResult {
	result := &mcp.CallToolResult{}
	result.SetError(err)
	return result
}

// normalizeAliases leaves schemas canonical while accepting declared spellings.
// String comparison is semantic, so JSON escape differences are not conflicts.
func normalizeAliases(aliases []argumentAlias, arguments map[string]json.RawMessage) (bool, error) {
	changed := false
	for _, pair := range aliases {
		alias, canonical := pair.alias, pair.canonical
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
