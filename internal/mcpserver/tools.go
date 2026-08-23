package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/iamyounglee/remote-workspace-mcp/internal/workspace"
)

// callTool 解析工具调用参数并执行对应的工作区操作。
func (s *Server) callTool(ctx context.Context, name string, raw json.RawMessage) (map[string]any, error) {
	slog.Info("mcp tool call", "tool", name)
	var result any
	var err error
	switch name {
	case "read":
		result, err = s.read(raw)
	case "write":
		result, err = s.write(raw)
	case "edit":
		result, err = s.edit(raw)
	case "apply_patch":
		result, err = s.applyPatch(raw)
	case "grep":
		result, err = s.grep(ctx, raw)
	case "glob":
		result, err = s.glob(ctx, raw)
	case "list":
		result, err = s.list(ctx, raw)
	case "bash":
		result, err = s.bash(ctx, raw)
	case "describe_transfer_endpoints":
		result, err = s.describeTransferEndpoints(raw)
	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}
	if err != nil {
		return toolError(err), nil
	}
	data, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	// 对工具结果大小施加上限保护，避免超大结果耗尽客户端或传输层
	// （MaxResultBytes 此前定义却未被消费）。
	if int64(len(data)) > s.cfg.Files.MaxResultBytes {
		return nil, fmt.Errorf("tool result exceeds max_result_bytes (%d)", s.cfg.Files.MaxResultBytes)
	}
	return map[string]any{"content": []map[string]any{{"type": "text", "text": string(data)}}, "isError": false}, nil
}

// toolError 将工具执行错误转换为 MCP 工具错误结果。
func toolError(err error) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": err.Error()}}, "isError": true}
}

// describeTransferEndpointsArgs 定义传输端点说明工具的请求参数
type describeTransferEndpointsArgs struct {
	Endpoint string `json:"endpoint"`
}

// describeTransferEndpoints 返回文件/目录传输与增量同步 REST 端点的调用说明文档。
// 该工具本身不发起任何网络请求或转发，仅返回静态的接口元数据，
// 供具备独立 HTTP 请求能力的调用方按标准 REST 方式直接调用这些端点。
func (s *Server) describeTransferEndpoints(raw json.RawMessage) (any, error) {
	var a describeTransferEndpointsArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, err
		}
	}
	endpoints := []map[string]any{
		{
			"name":        "files",
			"description": "单文件上传（PUT）或下载（GET），请求/响应体均为原始文件字节，不做 JSON 封装。",
			"methods": []map[string]any{
				{
					"method":       "PUT",
					"url_template": "{base_url}/files?path={workspace_relative_path}",
					"headers":      map[string]any{"Authorization": "Bearer <token>"},
					"query_params": map[string]any{"path": "工作区内目标文件的相对路径（必需）"},
					"request_body": "原始文件字节（不是 JSON），Content-Length 需符合服务端 max_upload_bytes 限制",
					"response":     "201 Created，无响应体；失败时返回 4xx/5xx 及错误文本",
					"example": "curl -H \"Authorization: Bearer <token>\" --upload-file ./app " +
						"\"{base_url}/files?path=bin/app\"",
				},
				{
					"method":       "GET",
					"url_template": "{base_url}/files?path={workspace_relative_path}",
					"headers":      map[string]any{"Authorization": "Bearer <token>"},
					"query_params": map[string]any{"path": "工作区内目标文件的相对路径（必需）"},
					"response":     "200 OK，Content-Type: application/octet-stream，body 为原始文件字节",
					"example": "curl -H \"Authorization: Bearer <token>\" -o ./app " +
						"\"{base_url}/files?path=bin/app\"",
				},
			},
		},
		{
			"name":        "directories",
			"description": "整个目录以 tar 或 tar.gz 归档形式上传（PUT）或下载（GET）。",
			"methods": []map[string]any{
				{
					"method":       "PUT",
					"url_template": "{base_url}/directories?path={workspace_relative_dir}&format={tar|tar.gz}",
					"headers":      map[string]any{"Authorization": "Bearer <token>"},
					"query_params": map[string]any{
						"path":   "工作区内目标目录的相对路径（必需，目录不能已存在）",
						"format": "tar 或 tar.gz，默认 tar.gz（可选）",
					},
					"request_body": "tar 或 tar.gz 格式的归档字节流",
					"response":     "201 Created，无响应体",
				},
				{
					"method":       "GET",
					"url_template": "{base_url}/directories?path={workspace_relative_dir}&format={tar|tar.gz}",
					"headers":      map[string]any{"Authorization": "Bearer <token>"},
					"query_params": map[string]any{
						"path":   "工作区内目标目录的相对路径（必需）",
						"format": "tar 或 tar.gz，默认 tar.gz（可选）",
					},
					"response": "200 OK，body 为 tar 或 tar.gz 格式的归档字节流",
				},
			},
		},
		{
			"name":        "sync",
			"description": "基于 manifest 和 SHA-256 的目录增量同步，分两步：先 plan 比对差异，再 apply 提交变更。",
			"methods": []map[string]any{
				{
					"method":       "POST",
					"url_template": "{base_url}/sync/plan?path={workspace_relative_dir}",
					"headers":      map[string]any{"Authorization": "Bearer <token>", "Content-Type": "application/json"},
					"query_params": map[string]any{"path": "远端目标目录的相对路径（必需）"},
					"request_body": "JSON 格式的本地文件清单：" +
						`{"files":[{"path":"cmd/main.go","size":1234,"sha256":"<hex>"}]}`,
					"response": "200 OK，返回 JSON 格式的差异计划：" +
						`{"missing":[...],"changed":[...],"unchanged":[...],"extra":[...]}` +
						"（missing=远端缺失需上传，changed=内容不同需上传，unchanged=无需处理，extra=远端多余可能需清理）",
				},
				{
					"method":       "PUT",
					"url_template": "{base_url}/sync/apply?path={workspace_relative_dir}&format={tar|tar.gz}",
					"headers":      map[string]any{"Authorization": "Bearer <token>"},
					"query_params": map[string]any{
						"path":   "远端目标目录的相对路径（必需）",
						"format": "tar 或 tar.gz，默认 tar.gz（可选）",
					},
					"request_body": "tar/tar.gz 归档，必须且只能包含 plan 中 missing+changed 的文件，" +
						"并在归档根目录额外附带一个名为 .remote-workspace-mcp-sync-manifest.json 的条目，" +
						"内容为与本次提交文件严格匹配的清单 JSON（path/size/sha256 需与归档内实际文件一致）",
					"response": "200 OK，返回 JSON 格式的应用结果（created/updated 文件列表）；" +
						"若归档内容与清单不匹配或目标已被修改导致冲突，返回 409 Conflict",
				},
			},
		},
	}
	if a.Endpoint != "" {
		for _, ep := range endpoints {
			if ep["name"] == a.Endpoint {
				return map[string]any{"endpoint": ep}, nil
			}
		}
		return nil, fmt.Errorf("unknown endpoint %q, expected one of: files, directories, sync", a.Endpoint)
	}
	return map[string]any{
		"note": "以下端点与 MCP 协议端点并列提供，不通过 tools/call 调用，需由具备独立 HTTP 请求能力的" +
			"调用方直接向 {base_url}（即本服务的基础 URL）发起标准 REST 请求。本工具仅返回说明文档，不代为转发。",
		"endpoints": endpoints,
	}, nil
}

// readArgs 定义文件读取工具的请求参数
type readArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

// read 按行读取受限大小的工作区文件并返回内容摘要。
func (s *Server) read(raw json.RawMessage) (any, error) {
	var a readArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	if a.Offset < 1 {
		a.Offset = 1
	}
	if a.Limit < 1 {
		a.Limit = 200
	}
	path, err := s.resolver.Resolve(a.Path, workspace.Read)
	if err != nil {
		return nil, err
	}
	data, truncated, err := readLimited(path, s.cfg.Files.MaxReadBytes)
	if err != nil {
		return nil, err
	}
	// 方案 B：read 端不做 sha256 的 CAS 校验。readLimited 对超大文件会截断，
	// 若用截断内容算哈希，会与调用方基于“全量内容”算出的 expected_sha256 误判为 "file changed"。
	// “确认未被改动”交由调用方通过 re-read + write/edit/apply_patch 的 expected_sha256
	// （这些路径基于整文件哈希）来保证，与 Claude Code / MCP 参考实现一致。
	// 返回的 sha256 仅在未截断（data 即完整内容）时计算，避免回传截断内容的错误摘要。
	sum := ""
	if !truncated {
		sum = sha256Hex(data)
	}
	// 按字节扫描换行，仅将需返回的行区间转换为字符串，避免对整个文件做 string(data) 复制。
	bounds := lineBoundaries(data)
	// 真实内容行数：以换行符计数，文件以 \n 结尾时不把末行空串计入。
	nl := strings.Count(string(data), "\n")
	totalLines := nl
	if len(data) > 0 && data[len(data)-1] != '\n' {
		totalLines = nl + 1
	}
	start := a.Offset - 1
	if start < 0 {
		start = 0
	}
	if start > totalLines {
		start = totalLines
	}
	end := start + a.Limit
	if end > totalLines {
		end = totalLines
	}
	var b strings.Builder
	for i := start; i < end; i++ {
		// bounds 含末尾哨兵，bounds[i+1] 始终有效。
		seg := data[bounds[i]:bounds[i+1]]
		// 去掉行尾的 \n（最后一行若无换行则保留原样）。
		if n := len(seg); n > 0 && seg[n-1] == '\n' {
			seg = seg[:n-1]
		}
		fmt.Fprintf(&b, "%d\t%s\n", i+1, seg)
	}
	return map[string]any{
		"path":        s.resolver.Display(path),
		"content":     b.String(),
		"offset":      a.Offset,
		"limit":       a.Limit,
		"total_lines": totalLines,
		"sha256":      sum,
		"truncated":   truncated,
	}, nil
}

// lineBoundaries 返回 data 中每一行起始字节偏移的切片，并在末尾追加 len(data) 作为哨兵，
// 便于直接用 [bounds[i]:bounds[i+1]] 切片而无需越界判断。
func lineBoundaries(data []byte) []int {
	bounds := []int{0}
	for i, c := range data {
		if c == '\n' {
			bounds = append(bounds, i+1)
		}
	}
	bounds = append(bounds, len(data))
	return bounds
}

// write 在校验预期哈希后原子写入工作区文件。
func (s *Server) write(raw json.RawMessage) (any, error) {
	var a struct {
		Path      string `json:"path"`
		Content   string `json:"content"`
		Overwrite bool   `json:"overwrite"`
		Expected  string `json:"expected_sha256"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	if int64(len(a.Content)) > s.cfg.Files.MaxWriteBytes {
		return nil, errors.New("content exceeds max_write_bytes")
	}
	path, err := s.resolver.Resolve(a.Path, workspace.Write)
	if err != nil {
		return nil, err
	}
	// 按路径分片取锁，使不同文件可并行写入（取代全局 mutationMu）。
	// bash 与文件写之间不再互斥（见 bash 工具设计），并发安全性由调用方保证。
	s.lockFiles([]string{path})
	defer s.unlockFiles([]string{path})

	// 无预期哈希时仅做存在性判断（os.Stat），不读取文件内容，避免无谓的全量读盘。
	if a.Expected == "" {
		exists, statErr := fileExists(path)
		if statErr != nil {
			return nil, statErr
		}
		if exists && !a.Overwrite {
			return nil, errors.New("file already exists and overwrite is false")
		}
	} else {
		// 提供 expected_sha256 即视为 CAS：以哈希匹配为唯一放行/拒绝依据，
		// 不再受 overwrite/存在性守卫影响（哈希匹配即允许安全原地更新）。
		if err := validateExpectedSHA(a.Expected); err != nil {
			return nil, err
		}
		old, exists, err := existingContent(path, s.cfg.Files.MaxReadBytes)
		if err != nil {
			return nil, err
		}
		if !exists || !strings.EqualFold(a.Expected, sha256Hex(old)) {
			return nil, errors.New("file changed since expected_sha256 was calculated")
		}
	}
	if err := atomicWrite(path, []byte(a.Content), s.cfg.Files.CreateParentDirs); err != nil {
		return nil, err
	}
	return map[string]any{"path": s.resolver.Display(path), "sha256": sha256Hex([]byte(a.Content)), "bytes": len(a.Content)}, nil
}

// patchEdit 定义单个文件精确替换操作
type patchEdit struct {
	Path       string `json:"path"`
	OldText    string `json:"old_text"`
	NewText    string `json:"new_text"`
	ReplaceAll bool   `json:"replace_all"`
	Expected   string `json:"expected_sha256"`
}

// edit 对文件执行字节级精确替换并保留未修改的原始字节。
func (s *Server) edit(raw json.RawMessage) (any, error) {
	var a patchEdit
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	path, err := s.resolver.Resolve(a.Path, workspace.Write)
	if err != nil {
		return nil, err
	}
	// 按路径分片取锁，使不同文件可并行编辑（取代全局 mutationMu）。
	// bash 与文件写之间不再互斥（见 bash 工具设计），并发安全性由调用方保证。
	s.lockFiles([]string{path})
	defer s.unlockFiles([]string{path})

	data, err := readEditableFile(path, s.cfg.Files.MaxReadBytes)
	if err != nil {
		return nil, err
	}
	oldSum := sha256Hex(data)
	if a.Expected != "" {
		if err := validateExpectedSHA(a.Expected); err != nil {
			return nil, err
		}
		if !strings.EqualFold(a.Expected, oldSum) {
			return nil, errors.New("file changed since expected_sha256 was calculated")
		}
	}
	updated, count, err := applyByteEdit(data, a, s.cfg.Files.MaxWriteBytes)
	if err != nil {
		return nil, err
	}
	if err := atomicWrite(path, updated, false); err != nil {
		return nil, err
	}
	return map[string]any{"path": s.resolver.Display(path), "replacements": count, "old_sha256": oldSum, "new_sha256": sha256Hex(updated)}, nil
}
