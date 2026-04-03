// Package mcp implements a Model Context Protocol (MCP) server over stdio.
// The server exposes Semantica's playbook search, attribution, and explain
// functionality as native tools that AI coding agents can call.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/semanticash/cli/internal/version"
)

const serverName = "semantica"

// jsonrpcRequest is a JSON-RPC 2.0 request.
type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// jsonrpcResponse is a JSON-RPC 2.0 response.
type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Server handles MCP protocol communication over stdio.
type Server struct {
	repoPath string
}

// NewServer creates a new MCP server for the given repository.
func NewServer(repoPath string) *Server {
	return &Server{repoPath: repoPath}
}

// Serve reads JSON-RPC requests from stdin and writes responses to stdout.
// It blocks until stdin is closed or ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	scanner := bufio.NewScanner(os.Stdin)
	// MCP messages can be large (e.g. search results).
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req jsonrpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			s.writeError(os.Stdout, nil, -32700, "parse error")
			continue
		}

		resp := s.handleRequest(ctx, &req)
		if resp != nil {
			s.writeResponse(os.Stdout, resp)
		}
	}

	return scanner.Err()
}

func (s *Server) handleRequest(ctx context.Context, req *jsonrpcRequest) *jsonrpcResponse {
	switch req.Method {
	case "initialize":
		return s.handleInitialize(req)
	case "notifications/initialized":
		// Client acknowledgement - no response needed.
		return nil
	case "tools/list":
		return s.handleToolsList(req)
	case "tools/call":
		return s.handleToolsCall(ctx, req)
	default:
		// Notifications (no ID) don't get responses.
		if req.ID == nil {
			return nil
		}
		return &jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &jsonrpcError{Code: -32601, Message: fmt.Sprintf("method not found: %s", req.Method)},
		}
	}
}

func (s *Server) handleInitialize(req *jsonrpcRequest) *jsonrpcResponse {
	return &jsonrpcResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result: map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    serverName,
				"version": version.Version,
			},
		},
	}
}

func (s *Server) handleToolsList(req *jsonrpcRequest) *jsonrpcResponse {
	return &jsonrpcResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result: map[string]any{
			"tools": toolDefinitions(),
		},
	}
}

func (s *Server) handleToolsCall(ctx context.Context, req *jsonrpcRequest) *jsonrpcResponse {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return &jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &jsonrpcError{Code: -32602, Message: "invalid params"},
		}
	}

	result, toolErr := callTool(ctx, s.repoPath, params.Name, params.Arguments)
	if toolErr != nil {
		return &jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": toolErr.Error()},
				},
				"isError": true,
			},
		}
	}

	return &jsonrpcResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  result,
	}
}

func (s *Server) writeResponse(w io.Writer, resp *jsonrpcResponse) {
	data, err := json.Marshal(resp)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcp: marshal response: %v\n", err)
		return
	}
	if _, err := fmt.Fprintf(w, "%s\n", data); err != nil {
		fmt.Fprintf(os.Stderr, "mcp: write response: %v\n", err)
	}
}

func (s *Server) writeError(w io.Writer, id json.RawMessage, code int, msg string) {
	resp := &jsonrpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &jsonrpcError{Code: code, Message: msg},
	}
	s.writeResponse(w, resp)
}
