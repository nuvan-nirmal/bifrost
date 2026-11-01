package chaosmock_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/maximhq/bifrost/tests/cmd/mcpconsole/chaosmock"
)

// Prompts, resources and templates are answered per caller, so what a read
// returns names who read it. These tests pin the edges of that on the raw
// wire, where a client library would otherwise smooth them over.

// rpcResult posts one JSON-RPC message and decodes its result, failing on a
// JSON-RPC error.
func rpcResult(t *testing.T, url, session, body string) map[string]any {
	t.Helper()
	status, _, raw := rawPost(t, url, session, body)
	if status != http.StatusOK {
		t.Fatalf("%s returned %d: %s", body, status, raw)
	}
	var out struct {
		Result map[string]any `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Error != nil {
		t.Fatalf("%s did not succeed: %s", body, raw)
	}
	return out.Result
}

func TestCatalogAnswersAnAnonymousCallerOnTheRawWire(t *testing.T) {
	fleet := chaosmock.NewFleet()
	fleet.AddSeeded("alpha", chaosmock.Seed{
		Prompts: []chaosmock.Prompt{{Name: "summarize"}},
		Resources: []chaosmock.Resource{
			{URI: "notes://pinned"}, {URI: "file:///b.txt"}, {URI: "file:///a.txt"},
		},
		Templates: []chaosmock.ResourceTemplate{{URITemplate: "notes://{id}"}, {URITemplate: "static://only"}},
	})
	fleet.Start()
	t.Cleanup(fleet.Stop)
	url := fleet.ServerURL("alpha")

	_, session, raw := rawPost(t, url, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	var init struct {
		Result struct {
			Capabilities map[string]json.RawMessage `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &init); err != nil {
		t.Fatalf("bad initialize: %s", raw)
	}
	// A server with no tools still advertises them, since tools/list always
	// answers; prompts and resources are advertised because they are held.
	caps := init.Result.Capabilities
	if string(caps["prompts"]) != "{}" || string(caps["resources"]) != "{}" || caps["tools"] == nil {
		t.Errorf("expected tools, prompts {} and resources {}, got %s", raw)
	}

	listed := rpcResult(t, url, session, `{"jsonrpc":"2.0","id":2,"method":"resources/list"}`)
	if at(listed, "resources", 0, "uri") != "file:///a.txt" || at(listed, "resources", 2, "uri") != "notes://pinned" {
		t.Errorf("expected resources sorted by uri, got %v", listed)
	}

	got := rpcResult(t, url, session, `{"jsonrpc":"2.0","id":3,"method":"prompts/get","params":{"name":"summarize"}}`)
	if at(got, "messages", 0, "content", "text") != "summarize: as anonymous" {
		t.Errorf("expected an anonymous prompt with no arguments, got %v", got)
	}

	for uri, want := range map[string]string{
		// A listed resource wins over a template that also matches it.
		"notes://pinned": "notes://pinned as anonymous",
		// Captured values are percent-decoded.
		"notes://a%20b": `notes://a%20b from notes://{id} {"id":"a b"} as anonymous`,
		// A template with no expressions matches only itself.
		"static://only": `static://only from static://only {} as anonymous`,
	} {
		read := rpcResult(t, url, session, `{"jsonrpc":"2.0","id":4,"method":"resources/read","params":{"uri":"`+uri+`"}}`)
		if text := at(read, "contents", 0, "text"); text != want {
			t.Errorf("%s: expected %q, got %v", uri, want, text)
		}
		if at(read, "contents", 0, "uri") != uri || at(read, "contents", 0, "mimeType") != "text/plain" {
			t.Errorf("%s: expected the uri and a text/plain default, got %v", uri, read)
		}
	}

	_, _, raw = rawPost(t, url, session, `{"jsonrpc":"2.0","id":5,"method":"resources/read","params":{"uri":"static://only/more"}}`)
	if !json.Valid(raw) || at(decode(t, string(raw)), "error", "code") != float64(-32002) {
		t.Errorf("expected a uri past a literal template to be not found, got %s", raw)
	}
}

func TestCatalogFollowsTheSessionIdentityOverSSE(t *testing.T) {
	fleet := chaosmock.NewFleet()
	fleet.AddSeeded("alpha", chaosmock.Seed{
		Auth:        &chaosmock.Auth{Bearer: map[string]string{"token-a": "alice"}},
		PromptViews: map[string][]chaosmock.Prompt{"alice": {{Name: "alice_only"}}},
	})
	fleet.Start()
	t.Cleanup(fleet.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := client.NewSSEMCPClient(fleet.SSEURL("alpha"), transport.WithHeaders(map[string]string{"Authorization": "Bearer token-a"}))
	if err != nil {
		t.Fatalf("failed to create SSE client: %v", err)
	}
	defer c.Close()
	initClient(t, c, ctx)

	var get mcp.GetPromptRequest
	get.Params.Name = "alice_only"
	res, err := c.GetPrompt(ctx, get)
	if err != nil {
		t.Fatalf("prompts/get over SSE failed: %v", err)
	}
	if text, ok := mcp.AsTextContent(res.Messages[0].Content); !ok || text.Text != "alice_only: as alice" {
		t.Errorf("expected the prompt to name alice, got %+v", res.Messages)
	}
}
