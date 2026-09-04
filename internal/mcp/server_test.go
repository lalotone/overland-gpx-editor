package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestStreamableHTTPServer(t *testing.T) {
	bridge, err := NewBridge()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bridge.Close)
	putBrowserView(t, bridge, "view-one", true, `{"screen":"creation","planner":{"routePoints":[]}}`)

	mux := http.NewServeMux()
	mux.Handle("/mcp", http.StripPrefix("/mcp", NewHandler(bridge, "test-version")))
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test-client", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &sdkmcp.StreamableClientTransport{
		Endpoint: httpServer.URL + "/mcp",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != len(toolDefinitions) {
		t.Fatalf("tool count = %d, want %d", len(tools.Tools), len(toolDefinitions))
	}
	for _, tool := range tools.Tools {
		if tool.OutputSchema == nil {
			t.Fatalf("tool %q has no output schema", tool.Name)
		}
	}
	view, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "get_view", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if view.IsError || len(view.Content) != 1 {
		t.Fatalf("get_view result = %#v", view)
	}
	resource, err := session.ReadResource(ctx, &sdkmcp.ReadResourceParams{URI: "overland://view"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resource.Contents) != 1 || resource.Contents[0].MIMEType != "application/json" {
		t.Fatalf("resource result = %#v", resource)
	}
}

func TestHandlerRejectsMCPAliases(t *testing.T) {
	bridge, err := NewBridge()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bridge.Close)
	handler := NewHandler(bridge, "test-version")
	for _, path := range []string{"/", "/not-real", "/browser"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest("POST", "http://127.0.0.1"+path, nil)
		handler.ServeHTTP(recorder, request)
		if recorder.Code != 404 {
			t.Errorf("path %q status = %d, want 404", path, recorder.Code)
		}
	}
}

func TestStreamableHTTPServerRejectsRemoteRequests(t *testing.T) {
	bridge, err := NewBridge()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bridge.Close)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "http://127.0.0.1/mcp", nil)
	request.URL.Path = ""
	request.RemoteAddr = "192.0.2.10:32000"
	NewHandler(bridge, "test-version").ServeHTTP(recorder, request)
	if recorder.Code != 403 {
		t.Fatalf("remote MCP status = %d, want 403", recorder.Code)
	}
}
