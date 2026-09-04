package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const toolCallTimeout = 2 * time.Minute

// NewHandler exposes the standard MCP endpoint for local agents. Mount it at
// /mcp on a loopback-only listener.
//
// The browser bridge is deliberately not served here: the page has to reach it
// same-origin, so it stays on the main listener while this endpoint stays off
// any reverse proxy.
func NewHandler(bridge *Bridge, version string) http.Handler {
	server := newSDKServer(bridge, version)
	streamable := localOnly(sdkmcp.NewStreamableHTTPHandler(
		func(*http.Request) *sdkmcp.Server { return server },
		&sdkmcp.StreamableHTTPOptions{
			Stateless:                    true,
			JSONResponse:                 true,
			MaxRequestBodyBytes:          maxBridgeBodyBytes,
			PropagateRequestCancellation: true,
		},
	))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "" {
			http.NotFound(w, r)
			return
		}
		copy := r.Clone(r.Context())
		urlCopy := *r.URL
		urlCopy.Path = "/"
		copy.URL = &urlCopy
		streamable.ServeHTTP(w, copy)
	})
}

func newSDKServer(bridge *Bridge, version string) *sdkmcp.Server {
	if strings.TrimSpace(version) == "" {
		version = "dev"
	}
	server := sdkmcp.NewServer(
		&sdkmcp.Implementation{Name: "overland", Version: version},
		&sdkmcp.ServerOptions{
			Instructions: serverInstructions,
			Capabilities: &sdkmcp.ServerCapabilities{},
		},
	)
	for _, definition := range toolDefinitions {
		server.AddTool(sdkTool(definition), func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			return callTool(ctx, bridge, definition.Name, request.Params.Arguments), nil
		})
	}
	server.AddResource(&sdkmcp.Resource{
		URI:         "overland://view",
		Name:        "Active Overland view",
		Description: "Current map, session overlays, marker catalog, route, track, waypoints, POIs, library, terrain, and planning status.",
		MIMEType:    "application/json",
	}, func(context.Context, *sdkmcp.ReadResourceRequest) (*sdkmcp.ReadResourceResult, error) {
		snapshot, err := bridge.ActiveSnapshot()
		if err != nil {
			return nil, err
		}
		return &sdkmcp.ReadResourceResult{Contents: []*sdkmcp.ResourceContents{{
			URI: "overland://view", MIMEType: "application/json", Text: string(snapshot),
		}}}, nil
	})
	return server
}

func sdkTool(definition toolDefinition) *sdkmcp.Tool {
	annotations := &sdkmcp.ToolAnnotations{
		ReadOnlyHint:   annotationBool(definition.Annotations, "readOnlyHint"),
		IdempotentHint: annotationBool(definition.Annotations, "idempotentHint"),
		OpenWorldHint:  boolPointer(false),
	}
	if destructive, ok := definition.Annotations["destructiveHint"].(bool); ok {
		annotations.DestructiveHint = boolPointer(destructive)
	}
	return &sdkmcp.Tool{
		Name:         definition.Name,
		Title:        definition.Title,
		Description:  definition.Description,
		InputSchema:  definition.InputSchema,
		OutputSchema: map[string]any{"type": "object"},
		Annotations:  annotations,
	}
}

func callTool(ctx context.Context, bridge *Bridge, name string, raw json.RawMessage) *sdkmcp.CallToolResult {
	arguments, err := validateToolArguments(name, raw)
	if err != nil {
		return toolError(err)
	}

	var value json.RawMessage
	if name == "get_view" {
		value, err = bridge.ActiveSnapshot()
	} else {
		callCtx, cancel := context.WithTimeout(ctx, toolCallTimeout)
		defer cancel()
		value, err = bridge.Call(callCtx, name, arguments)
	}
	if err != nil {
		return toolError(err)
	}

	result := &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: string(value)}}}
	var structured any
	if json.Unmarshal(value, &structured) == nil {
		result.StructuredContent = structured
	}
	return result
}

func toolError(err error) *sdkmcp.CallToolResult {
	result := &sdkmcp.CallToolResult{}
	result.SetError(err)
	return result
}

func annotationBool(values map[string]any, key string) bool {
	value, _ := values[key].(bool)
	return value
}

func boolPointer(value bool) *bool {
	return &value
}

func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackBrowserRequest(r) {
			http.Error(w, "MCP is available only to loopback clients", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

const serverInstructions = "Use get_view or overland://view before editing. Use switch_mode before mode-specific tools. draw_map_track and set_map_markers are session-only and work on any active map without switching modes. Route controls affect routing; GPX waypoints only mark places. draw_track changes the open track in memory and leaves saving to the user."
