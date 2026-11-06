package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zalkilabs/zalki/internal/bus"
)

// These tests pin the order of the checks in argumentHints (arguments.go) with
// a hand-made tool, because no real tool has both aliases and a validator.
// The middleware does this, in this order:
//
//  1. A request that is not tools/call, or whose params are not a raw tool
//     call, goes to the next handler untouched.
//  2. An unknown tool name, or arguments that cannot decode into a JSON map,
//     go to the next handler untouched. JSON null decodes to a nil map and
//     continues through the checks, including the validator.
//  3. Aliases are normalized. A bad or conflicting alias is returned as a tool
//     error ("<name> must be a string" / "<a> and <b> must match ...").
//  4. Unknown fields are refused, sorted, with the accepted fields and the hint.
//  5. The tool's validator runs, and its error is returned as a tool error.
//  6. Only when no check failed, and an alias was rewritten, params.Arguments
//     is replaced by the canonical encoding. Then the next handler runs.
//
// Each refusal is a tool result (IsError), not a protocol error, and the next
// handler is not called. These fixtures use literal validator errors; arbitrary
// validator errors are returned verbatim.

var errInvalidCombination = errors.New("validator refused the arguments")

// hintedTool has one alias (nick -> name), an optional field, a hint and a
// validator that refuses "optional":"bad".
func hintedTool() toolSpec {
	return toolSpec{toolDefinition: toolDefinition{
		name:    "hinted",
		input:   inputSchema{Properties: map[string]any{"name": 1, "optional": 1}},
		aliases: []argumentAlias{{"nick", "name"}},
		hint:    " Use name.",
		validate: func(arguments map[string]json.RawMessage) error {
			if string(arguments["optional"]) == `"bad"` {
				return errInvalidCombination
			}
			return nil
		},
	}}
}

// callHinted runs the middleware for one request and reports what reached the
// next handler. nextArguments is nil when the next handler was not called.
func callHinted(t *testing.T, tool string, arguments string) (result mcp.Result, nextArguments *string, err error) {
	t.Helper()
	params := &mcp.CallToolParamsRaw{Name: tool, Arguments: json.RawMessage(arguments)}
	request := &mcp.ServerRequest[*mcp.CallToolParamsRaw]{Params: params}
	next := func(_ context.Context, _ string, r mcp.Request) (mcp.Result, error) {
		seen := string(r.GetParams().(*mcp.CallToolParamsRaw).Arguments)
		nextArguments = &seen
		return &mcp.CallToolResult{}, nil
	}
	handler := argumentHints([]toolSpec{hintedTool()})(next)
	result, err = handler(context.Background(), "tools/call", request)
	return result, nextArguments, err
}

func toolErrorText(t *testing.T, result mcp.Result) string {
	t.Helper()
	call, ok := result.(*mcp.CallToolResult)
	if !ok || !call.IsError || len(call.Content) != 1 {
		t.Fatalf("result is not a single tool error: %#v", result)
	}
	text, ok := call.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("tool error content is %T", call.Content[0])
	}
	return text.Text
}

func TestArgumentHintsPassesThroughWhatItDoesNotOwn(t *testing.T) {
	for _, test := range []struct{ name, tool, arguments string }{
		{"an unknown tool name", "other", `{"anything":"goes"}`},
		{"arguments that are a list", "hinted", `[1,2]`},
		{"arguments that are text", "hinted", `"text"`},
		{"missing arguments", "hinted", ``},
		{"unknown fields on an unknown tool", "other", `{"nick":"x","unexpected":1}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, seen, err := callHinted(t, test.tool, test.arguments)
			if err != nil || seen == nil || *seen != test.arguments {
				t.Fatalf("request was not passed through untouched: seen=%v err=%v", seen, err)
			}
			if call, ok := result.(*mcp.CallToolResult); !ok || call.IsError {
				t.Fatalf("pass-through result changed: %#v", result)
			}
		})
	}
	t.Run("another method", func(t *testing.T) {
		called := false
		next := func(context.Context, string, mcp.Request) (mcp.Result, error) {
			called = true
			return &mcp.CallToolResult{}, nil
		}
		request := &mcp.ServerRequest[*mcp.CallToolParamsRaw]{Params: &mcp.CallToolParamsRaw{Name: "hinted", Arguments: json.RawMessage(`{"unexpected":1}`)}}
		if _, err := argumentHints([]toolSpec{hintedTool()})(next)(context.Background(), "tools/list", request); err != nil || !called {
			t.Fatalf("a method other than tools/call was not passed through: called=%v err=%v", called, err)
		}
	})
}

func TestArgumentHintsPassesThroughNonRawParams(t *testing.T) {
	params := &mcp.CallToolParams{Name: "hinted", Arguments: map[string]any{"unexpected": true}}
	request := &mcp.ServerRequest[*mcp.CallToolParams]{Params: params}
	called := false
	next := func(_ context.Context, _ string, r mcp.Request) (mcp.Result, error) {
		called = true
		if r != request || r.GetParams() != params {
			t.Fatal("non-raw request was replaced")
		}
		return &mcp.CallToolResult{}, nil
	}
	if _, err := argumentHints([]toolSpec{hintedTool()})(next)(t.Context(), "tools/call", request); err != nil || !called {
		t.Fatalf("non-raw request was not passed through: called=%v err=%v", called, err)
	}
}

func TestArgumentHintsValidatesNullAsANilMap(t *testing.T) {
	definition := hintedTool()
	validated := false
	definition.validate = func(arguments map[string]json.RawMessage) error {
		validated = true
		if arguments != nil {
			t.Fatalf("null decoded to a non-nil map: %v", arguments)
		}
		return errInvalidCombination
	}
	params := &mcp.CallToolParamsRaw{Name: "hinted", Arguments: json.RawMessage(`null`)}
	request := &mcp.ServerRequest[*mcp.CallToolParamsRaw]{Params: params}
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) {
		t.Fatal("null bypassed its validator refusal")
		return nil, nil
	}
	result, err := argumentHints([]toolSpec{definition})(next)(t.Context(), "tools/call", request)
	if err != nil || !validated || toolErrorText(t, result) != errInvalidCombination.Error() {
		t.Fatalf("null validation: validated=%v result=%v err=%v", validated, result, err)
	}
	if string(params.Arguments) != "null" {
		t.Fatalf("refused null was rewritten: %s", params.Arguments)
	}
}

func TestArgumentHintsReportsTheFirstFailingCheck(t *testing.T) {
	const (
		unknownFields = `unknown fields "unexpected"; hinted accepts name, optional. Use name.`
		conflict      = "nick and name must match when both are provided"
		notString     = "nick must be a string"
	)
	tests := []struct{ name, arguments, want string }{
		// One fault each.
		{"an alias conflict", `{"nick":"a","name":"b"}`, conflict},
		{"an alias that is not text", `{"nick":7}`, notString},
		{"an unknown field", `{"name":"a","unexpected":1}`, unknownFields},
		{"several unknown fields are sorted", `{"zeta":1,"name":"a","alpha":2}`, `unknown fields "alpha", "zeta"; hinted accepts name, optional. Use name.`},
		{"the validator", `{"name":"a","optional":"bad"}`, errInvalidCombination.Error()},
		// Two faults at once: the earlier check wins.
		{"alias conflict and unknown field", `{"nick":"a","name":"b","unexpected":1}`, conflict},
		{"alias not text and unknown field", `{"nick":7,"unexpected":1}`, notString},
		{"alias conflict and validator", `{"nick":"a","name":"b","optional":"bad"}`, conflict},
		{"unknown field and validator", `{"name":"a","optional":"bad","unexpected":1}`, unknownFields},
		{"all three", `{"nick":"a","name":"b","optional":"bad","unexpected":1}`, conflict},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, seen, err := callHinted(t, "hinted", test.arguments)
			if err != nil || seen != nil {
				t.Fatalf("a refused call must be a tool error and must not reach the next handler: seen=%v err=%v", seen, err)
			}
			if got := toolErrorText(t, result); got != test.want {
				t.Fatalf("error = %q, want %q", got, test.want)
			}
		})
	}
}

func TestArgumentHintsRewritesAliasesOnlyForAcceptedCalls(t *testing.T) {
	t.Run("an alias is rewritten to the canonical field", func(t *testing.T) {
		_, seen, err := callHinted(t, "hinted", `{"nick":"a","optional":"ok"}`)
		if err != nil || seen == nil {
			t.Fatalf("accepted call did not reach the next handler: %v", err)
		}
		var got map[string]string
		if json.Unmarshal([]byte(*seen), &got) != nil || len(got) != 2 || got["name"] != "a" || got["optional"] != "ok" {
			t.Fatalf("next handler got %s, want name and optional only", *seen)
		}
	})
	t.Run("an alias equal to the canonical field is dropped", func(t *testing.T) {
		_, seen, err := callHinted(t, "hinted", `{"nick":"\u0061","name":"a"}`)
		var got map[string]string
		if err != nil || seen == nil || json.Unmarshal([]byte(*seen), &got) != nil || len(got) != 1 || got["name"] != "a" {
			t.Fatalf("next handler got %v, err %v", seen, err)
		}
	})
	t.Run("canonical arguments keep their exact bytes", func(t *testing.T) {
		const raw = `{ "name" : "a" }`
		_, seen, err := callHinted(t, "hinted", raw)
		if err != nil || seen == nil || *seen != raw {
			t.Fatalf("arguments without aliases were rewritten: %v %v", seen, err)
		}
	})
}

func TestArgumentHintsLeaveARefusedRequestUnrewritten(t *testing.T) {
	params := &mcp.CallToolParamsRaw{Name: "hinted", Arguments: json.RawMessage(`{"nick":"a","optional":"bad"}`)}
	request := &mcp.ServerRequest[*mcp.CallToolParamsRaw]{Params: params}
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) {
		t.Fatal("next handler ran for a refused call")
		return nil, nil
	}
	result, err := argumentHints([]toolSpec{hintedTool()})(next)(context.Background(), "tools/call", request)
	if err != nil || toolErrorText(t, result) != errInvalidCombination.Error() {
		t.Fatalf("validator refusal not returned: %v %v", result, err)
	}
	if string(params.Arguments) != `{"nick":"a","optional":"bad"}` {
		t.Fatalf("a refused call had its arguments rewritten: %s", params.Arguments)
	}
}

// Through a real MCP session an unknown tool is reported by the SDK, not by
// the argument hints, so the caller sees no list of accepted fields.
func TestArgumentHintsLeaveUnknownToolsToTheSDK(t *testing.T) {
	client, ctx := testClient(t, &bus.Service{Store: newTestStore(t)})
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "no_such_tool", Arguments: map[string]any{"unexpected": "secret-value"}})
	text := sdkErrorText(t, result, err)
	if !strings.Contains(text, "no_such_tool") || strings.Contains(text, "accepts") || strings.Contains(text, "secret-value") {
		t.Fatalf("unknown tool error = %q", text)
	}
}

// sdkErrorText returns the text of a refused call, whether the SDK reported it
// as a protocol error or as a tool error.
func sdkErrorText(t *testing.T, result *mcp.CallToolResult, err error) string {
	t.Helper()
	switch {
	case err != nil:
		return err.Error()
	case result != nil && result.IsError:
		var text strings.Builder
		for _, block := range result.Content {
			if content, ok := block.(*mcp.TextContent); ok {
				text.WriteString(content.Text)
			}
		}
		return text.String()
	default:
		t.Fatalf("a refused call was accepted: %+v", result)
		return ""
	}
}
