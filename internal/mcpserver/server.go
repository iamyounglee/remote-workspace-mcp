package mcpserver

// Package mcpserver 提供面向编码 Agent 的 MCP 服务：文件读写、搜索、命令执行、
// 文件/目录传输、目录增量同步工具，以及对应的 Streamable HTTP 路由与鉴权。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"sync"

	"github.com/iamyounglee/remote-workspace-mcp/internal/auth"
	"github.com/iamyounglee/remote-workspace-mcp/internal/config"
	"github.com/iamyounglee/remote-workspace-mcp/internal/workspace"
)

// Version 由构建时的 -ldflags 注入，例如：
//
//	-X github.com/iamyounglee/remote-workspace-mcp/internal/mcpserver.Version=v0.1.0
//
// 默认值 "dev" 表示非发布构建。
var Version = "dev"

// fileLockShards 为文件编辑锁使用的分片数量。分片锁按归一化绝对路径取模，
// 使不同文件的写操作可以并行，同时避免全局锁的串行瓶颈与 map 锁的内存开销。
const fileLockShards = 64

// Server 提供带认证、会话管理和工作区工具调用能力的 MCP HTTP 服务
type Server struct {
	cfg      config.Config
	resolver *workspace.Resolver
	tokens   *auth.Store
	bashSem  chan struct{}
	// fileMu 为按路径分片的互斥锁，保证同一文件编辑互斥、不同文件可并行。
	// 取代原先全局串行化的 mutationMu。
	// 注意：bash 与文件写之间不再加互斥锁。bash 执行无法预知其会修改哪些文件，
	// 服务器也无从对它做精确的文件级隔离；该并发安全性交由调用方（agent）通过
	// 顺序调用 + re-read 保证，与 Claude Code 等主流 agent 的做法一致。fileMu 仅
	// 用于串行化“同一文件的写工具之间”（write/edit/apply_patch/transfer/sync）的
	// read-check-then-write，避免跨会话的同文件写竞争。
	fileMu [fileLockShards]sync.Mutex
	// toolsOnce 确保工具定义仅构建一次（见 toolDefinitions）。
	toolsOnce sync.Once
	tools     []map[string]any
}

// New 根据配置、工作区解析器和令牌存储创建 MCP 服务。
func New(cfg config.Config, resolver *workspace.Resolver, tokens *auth.Store) *Server {
	return &Server{cfg: cfg, resolver: resolver, tokens: tokens, bashSem: make(chan struct{}, cfg.Bash.MaxConcurrent)}
}

// lockFiles 按分片索引去重并升序取分片锁，避免同一请求对同一分片重复加锁（自死锁），
// 也保证不同请求以一致顺序加锁（避免交叉死锁）。仅串行化“同一文件的写工具之间”，
// 不涉及 bash（bash 与写操作之间不互斥）。调用方应调用 unlockFiles 释放。
func (s *Server) lockFiles(paths []string) {
	indices := s.uniqueLockIndices(paths)
	for _, idx := range indices {
		s.fileMu[idx].Lock()
	}
}

// unlockFiles 按 lockFiles 的逆序释放一组路径对应的分片锁。
func (s *Server) unlockFiles(paths []string) {
	indices := s.uniqueLockIndices(paths)
	for i := len(indices) - 1; i >= 0; i-- {
		s.fileMu[indices[i]].Unlock()
	}
}

// uniqueLockIndices 返回去重后升序排列的分片索引。
func (s *Server) uniqueLockIndices(paths []string) []uint32 {
	seen := make(map[uint32]struct{}, len(paths))
	for _, p := range paths {
		seen[s.fileLockIndex(p)] = struct{}{}
	}
	indices := make([]uint32, 0, len(seen))
	for idx := range seen {
		indices = append(indices, idx)
	}
	sort.Slice(indices, func(i, j int) bool { return indices[i] < indices[j] })
	return indices
}

// fileLockIndex 返回给定路径对应的分片索引，供按分片去重使用。
func (s *Server) fileLockIndex(path string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(filepath.Clean(path)))
	return h.Sum32() % fileLockShards
}

// ServeHTTP 对文件传输、同步和 MCP 请求进行路由与认证。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/files" || r.URL.Path == "/directories" || r.URL.Path == "/sync/plan" || r.URL.Path == "/sync/apply" {
		s.transferHTTP(w, r)
		return
	}
	if r.URL.Path != s.cfg.Server.MCPPath {
		http.NotFound(w, r)
		return
	}
	if token, ok := auth.Bearer(r.Header.Get("Authorization")); !ok || !s.tokens.Validate(token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodPost:
		s.post(w, r)
	case http.MethodGet:
		s.get(w, r)
	case http.MethodDelete:
		s.delete(w, r)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// post 处理 MCP JSON-RPC 请求。本服务为无状态模式，不维护会话。
func (s *Server) post(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength > 10<<20 {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	var req rpcRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 10<<20))
	if err := dec.Decode(&req); err != nil {
		writeRPCError(w, nil, -32700, "invalid JSON")
		return
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); err != io.EOF {
		writeRPCError(w, req.ID, -32700, "invalid JSON")
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeRPCError(w, req.ID, -32600, "invalid JSON-RPC request")
		return
	}
	if req.ID == nil || string(req.ID) == "null" {
		if req.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}
	result, code, msg := s.dispatch(r.Context(), req.Method, req.Params)
	if code != 0 {
		writeRPCError(w, req.ID, code, msg)
		return
	}
	writeRPCResult(w, req.ID, result)
}

// get 返回 405。本服务未启用 server-to-client 的 SSE 事件流。
func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", "POST, DELETE")
	http.Error(w, "server-to-client streaming is not enabled", http.StatusMethodNotAllowed)
}

// delete 返回 204。本服务为无状态模式，不维护会话，因此无需终止会话。
func (s *Server) delete(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// dispatch 按 JSON-RPC 方法分派请求并生成协议级结果或错误。
func (s *Server) dispatch(ctx context.Context, method string, params json.RawMessage) (any, int, string) {
	switch method {
	case "initialize":
		return map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities": map[string]any{
				"tools": map[string]any{"listChanged": false},
			},
			"serverInfo": map[string]any{
				"name": s.cfg.Server.Name, "version": Version,
			},
		}, 0, ""
	case "tools/list":
		return map[string]any{"tools": s.toolDefinitions()}, 0, ""
	case "tools/call":
		var call struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(params, &call); err != nil || call.Name == "" {
			return nil, -32602, "invalid tools/call arguments"
		}
		result, err := s.callTool(ctx, call.Name, call.Arguments)
		if err != nil {
			return nil, -32603, err.Error()
		}
		return result, 0, ""
	case "ping":
		return map[string]any{}, 0, ""
	default:
		return nil, -32601, "method not found"
	}
}

// rpcRequest 表示客户端发送的 JSON-RPC 请求
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// rpcResponse 表示服务端返回的 JSON-RPC 响应
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError 描述 JSON-RPC 协议错误
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// writeRPCResult 写入成功的 JSON-RPC 响应。
func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

// writeRPCError 写入失败的 JSON-RPC 响应。
func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

// toolDefinitions 返回当前服务支持的 MCP 工具及其输入模式。
func (s *Server) toolDefinitions() []map[string]any {
	s.toolsOnce.Do(func() {
		s.tools = []map[string]any{
			{
				"name": "read",
				"description": "Read a text file by workspace-relative or permitted absolute path. " +
					"Supports line offset and limit. Returns the file's sha256 only when the content fits within the read limit (truncated reads omit it).",
				"inputSchema": schema(
					map[string]any{
						"path": stringProp(), "offset": numberProp(), "limit": numberProp(),
					},
					[]string{"path"},
				),
			},
			{
				"name":        "write",
				"description": "Create or atomically replace a text file. The path must be in the workspace or writable roots.",
				"inputSchema": schema(
					map[string]any{
						"path": stringProp(), "content": stringProp(), "overwrite": boolProp(),
						"expected_sha256": stringProp(),
					},
					[]string{"path", "content"},
				),
			},
			{
				"name":        "edit",
				"description": "Replace exact text in a file. By default old_text must match exactly once.",
				"inputSchema": schema(
					map[string]any{
						"path": stringProp(), "old_text": stringProp(), "new_text": stringProp(),
						"replace_all": boolProp(), "expected_sha256": stringProp(),
					},
					[]string{"path", "old_text", "new_text"},
				),
			},
			{
				"name":        "apply_patch",
				"description": "Apply ordered exact edits, grouping edits for the same file into one atomic write.",
				"inputSchema": schema(
					map[string]any{
						"edits": map[string]any{
							"type":     "array",
							"minItems": 1,
							"maxItems": patchMaxEdits,
							"items": schema(
								map[string]any{
									"path": stringProp(), "old_text": stringProp(), "new_text": stringProp(),
									"replace_all": boolProp(), "expected_sha256": stringProp(),
								},
								[]string{"path", "old_text", "new_text"},
							),
						},
						"dry_run":           boolProp(),
						"rollback_on_error": boolProp(),
					},
					[]string{"edits"},
				),
			},
			{
				"name":        "grep",
				"description": "Search text in permitted files with regex or fixed-string matching and bounded context.",
				"inputSchema": schema(
					map[string]any{
						"path": stringProp(), "pattern": stringProp(), "glob": stringProp(),
						"fixed_string": boolProp(), "case_sensitive": boolProp(),
						"context_before": numberProp(), "context_after": numberProp(),
						"max_results": numberProp(),
					},
					[]string{"pattern"},
				),
			},
			{
				"name":        "glob",
				"description": "Find permitted file paths using a glob pattern. Results are stable and bounded.",
				"inputSchema": schema(
					map[string]any{
						"path": stringProp(), "pattern": stringProp(), "max_results": numberProp(),
					},
					[]string{"pattern"},
				),
			},
			{
				"name":        "list",
				"description": "List entries in a permitted directory.",
				"inputSchema": schema(
					map[string]any{
						"path": stringProp(), "recursive": boolProp(), "max_entries": numberProp(),
					},
					nil,
				),
			},
			{
				"name": "bash",
				"description": "Run a command in the workspace via bash -c. Execution is bounded by timeout, " +
					"output, concurrency, and optional Bubblewrap sandbox isolation.",
				"inputSchema": schema(
					map[string]any{
						"command": stringProp(), "cwd": stringProp(),
						"timeout_seconds": numberProp(), "max_output_bytes": numberProp(),
					},
					[]string{"command"},
				),
			},
			{
				"name": "describe_transfer_endpoints",
				"description": "Return reference documentation (URL template, HTTP method, headers, query " +
					"parameters, and body format) for the file/directory transfer and incremental sync REST " +
					"endpoints exposed alongside the MCP endpoint (/files, /directories, /sync/plan, " +
					"/sync/apply). This tool performs no network calls itself; callers capable of issuing " +
					"their own HTTP requests can use the returned documentation to call those endpoints " +
					"directly.",
				"inputSchema": schema(
					map[string]any{
						"endpoint": stringProp(),
					},
					nil,
				),
			},
		}
	})
	return s.tools
}

// schema 构造对象类型的 JSON Schema。required 为 nil 时省略该字段，
// 避免被序列化为 null（部分 MCP 客户端会因此拒绝工具描述）。
func schema(props map[string]any, required []string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if required != nil {
		s["required"] = required
	}
	return s
}

// stringProp 构造字符串类型的 JSON Schema 属性。
func stringProp() map[string]any { return map[string]any{"type": "string"} }

// numberProp 构造非负整数类型的 JSON Schema 属性。
func numberProp() map[string]any { return map[string]any{"type": "integer", "minimum": 0} }

// boolProp 构造布尔类型的 JSON Schema 属性。
func boolProp() map[string]any { return map[string]any{"type": "boolean"} }

// decodeStrict 拒绝未知字段和参数对象后的额外 JSON 值。
func decodeStrict(raw json.RawMessage, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("arguments contain multiple JSON values")
		}
		return err
	}
	return nil
}
