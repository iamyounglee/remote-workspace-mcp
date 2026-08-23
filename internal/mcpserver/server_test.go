package mcpserver

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/iamyounglee/remote-workspace-mcp/internal/auth"
	"github.com/iamyounglee/remote-workspace-mcp/internal/config"
	"github.com/iamyounglee/remote-workspace-mcp/internal/workspace"
)

// TestAppendExistingReadOnlyBinds 验证 Bubblewrap 只绑定实际存在的只读路径。
func TestAppendExistingReadOnlyBinds(t *testing.T) {
	existing := filepath.Join(t.TempDir(), "existing")
	if err := os.WriteFile(existing, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	got := appendExistingReadOnlyBinds([]string{"base"}, []string{existing, missing})
	want := []string{"base", "--ro-bind", existing, existing}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bind args = %v, want %v", got, want)
	}
}

// TestBubblewrapArgsAppliesNetworkPolicy 验证 Bubblewrap 根据联网开关绑定网络配置或隔离网络。
func TestBubblewrapArgsAppliesNetworkPolicy(t *testing.T) {
	server, _ := testServer(t)
	networkConfig := filepath.Join(t.TempDir(), "network.conf")
	if err := os.WriteFile(networkConfig, []byte("config"), 0o600); err != nil {
		t.Fatal(err)
	}
	contains := func(args []string, values ...string) bool {
		for i := 0; i+len(values) <= len(args); i++ {
			if reflect.DeepEqual(args[i:i+len(values)], values) {
				return true
			}
		}
		return false
	}

	server.cfg.Bash.Sandbox.AllowNetwork = true
	connected := server.bubblewrapArgsWithNetworkPaths(server.resolver.Workspace(), []string{"true"}, []string{networkConfig})
	if !contains(connected, "--ro-bind", networkConfig, networkConfig) {
		t.Fatalf("network configuration is not bound: %v", connected)
	}
	if contains(connected, "--unshare-net") {
		t.Fatalf("connected sandbox unexpectedly isolates network: %v", connected)
	}

	server.cfg.Bash.Sandbox.AllowNetwork = false
	isolated := server.bubblewrapArgsWithNetworkPaths(server.resolver.Workspace(), []string{"true"}, []string{networkConfig})
	if !contains(isolated, "--unshare-net") {
		t.Fatalf("isolated sandbox does not unshare network: %v", isolated)
	}
	if contains(isolated, "--ro-bind", networkConfig, networkConfig) {
		t.Fatalf("isolated sandbox unexpectedly binds network configuration: %v", isolated)
	}
}

// TestBashPreservesServiceEnvironmentWithoutSandbox 验证非 Bubblewrap 模式完整保留服务环境。
func TestBashPreservesServiceEnvironmentWithoutSandbox(t *testing.T) {
	// Windows 上 bash 来自 Git Bash，其 env 输出与 PATH 规范化行为不同于原生Unix，
	// 且 /usr/bin/env 为 Unix 路径，环境继承语义不在 Windows 上断言。
	if runtime.GOOS == "windows" {
		t.Skip("service environment inheritance is asserted on Unix; Windows bash env differs")
	}
	t.Setenv("PATH", "/test/service/bin")
	t.Setenv("HOME", "/test/service/home")
	server, _ := testServer(t)
	server.cfg.Bash.Enabled = true
	server.cfg.Bash.Sandbox = config.SandboxConfig{Mode: "none"}
	result, err := server.bash(context.Background(), json.RawMessage(`{"command":"/usr/bin/env"}`))
	if err != nil {
		t.Fatal(err)
	}
	output := []byte("\n" + result.(map[string]any)["stdout"].(string) + "\n")
	if !bytes.Contains(output, []byte("\nPATH=/test/service/bin\n")) {
		t.Fatal("command environment does not preserve the service PATH")
	}
	if !bytes.Contains(output, []byte("\nHOME=/test/service/home\n")) {
		t.Fatal("command environment does not preserve the service HOME")
	}
}

// TestBashTimeoutReturnsPromptly 验证 bash 超时后工具及时返回，而非挂起至
// 命令自然结束。回归保护：bash 被 SIGKILL 后其子进程仍持有 stdout/stderr
// 管道写端，若不设置 exec.Cmd.WaitDelay，Wait 会阻塞到管道关闭（表现为
// duration 等于命令完整执行时长）。
func TestBashTimeoutReturnsPromptly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process group/pipe semantics differ on Windows")
	}
	server, _ := testServer(t)
	server.cfg.Bash.Enabled = true
	server.cfg.Bash.TimeoutSeconds = 5
	server.cfg.Bash.Sandbox = config.SandboxConfig{Mode: "none"}

	start := time.Now()
	result, err := server.bash(context.Background(), json.RawMessage(`{
		"command": "echo start; sleep 8; echo end",
		"timeout_seconds": 1
	}`))
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	r := result.(map[string]any)
	if r["timed_out"] != true {
		t.Fatalf("expected timed_out=true, got %v", r["timed_out"])
	}
	if got := r["stdout"].(string); got != "start\n" {
		t.Fatalf("stdout = %q, want %q (command must be interrupted)", got, "start\n")
	}
	// 修复前 Wait 会挂起至 sleep 8 结束（约 8s）；修复后应约 1.5s 内返回。
	if elapsed > 5*time.Second {
		t.Fatalf("bash timeout returned too slowly: %v", elapsed)
	}
}

// TestApplyByteEditPreservesNonUTF8 验证字节编辑不会破坏非 UTF-8 内容。
func TestApplyByteEditPreservesNonUTF8(t *testing.T) {
	original := append([]byte{0xff, 0xfe}, []byte("before target after")...)
	updated, count, err := applyByteEdit(original, patchEdit{OldText: "target", NewText: "done"}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || !bytes.Equal(updated, append([]byte{0xff, 0xfe}, []byte("before done after")...)) {
		t.Fatalf("unexpected edited bytes: %v", updated)
	}
}

// TestApplyByteEditRejectsEmptyMatch 验证空匹配文本会被拒绝。
func TestApplyByteEditRejectsEmptyMatch(t *testing.T) {
	if _, _, err := applyByteEdit([]byte("content"), patchEdit{}, 1024); err == nil {
		t.Fatal("expected empty old_text error")
	}
}

// TestApplyByteEditRequiresUniqueMatch 验证默认编辑拒绝不唯一的匹配文本。
func TestApplyByteEditRequiresUniqueMatch(t *testing.T) {
	if _, _, err := applyByteEdit([]byte("x x"), patchEdit{OldText: "x", NewText: "y"}, 1024); err == nil {
		t.Fatal("expected ambiguous old_text error")
	}
}

// TestApplyByteEditReplacesAllMatches 验证 replace_all 会替换全部字节匹配。
func TestApplyByteEditReplacesAllMatches(t *testing.T) {
	updated, count, err := applyByteEdit([]byte("x x"), patchEdit{OldText: "x", NewText: "yz", ReplaceAll: true}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 || string(updated) != "yz yz" {
		t.Fatalf("updated = %q, count = %d", updated, count)
	}
}

// TestApplyByteEditEnforcesWriteLimit 验证编辑结果不能超过最大写入字节数。
func TestApplyByteEditEnforcesWriteLimit(t *testing.T) {
	if _, _, err := applyByteEdit([]byte("x"), patchEdit{OldText: "x", NewText: "too large"}, 4); err == nil {
		t.Fatal("expected max_write_bytes error")
	}
}

// TestApplyPatchAccumulatesEditsForSameFile 验证同一文件的编辑按请求顺序累计并仅返回一个文件结果。
func TestApplyPatchAccumulatesEditsForSameFile(t *testing.T) {
	server, _ := testServer(t)
	result, err := server.applyPatch(json.RawMessage(`{
		"edits": [
			{"path":"hello.txt","old_text":"hello","new_text":"greetings"},
			{"path":"hello.txt","old_text":"greetings\nworld","new_text":"done"}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	path, err := server.resolver.Resolve("hello.txt", workspace.Read)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "done\n" {
		t.Fatalf("content = %q", content)
	}
	response := result.(map[string]any)
	if response["file_count"] != 1 || response["edit_count"] != 2 || response["committed"] != true {
		t.Fatalf("unexpected result: %#v", response)
	}
	files := response["files"].([]map[string]any)
	if len(files) != 1 || files[0]["replacements"] != 2 {
		t.Fatalf("unexpected files: %#v", files)
	}
}

// TestApplyPatchDryRunUsesOrderedPlan 验证 dry_run 使用相同的顺序规划但不写盘。
func TestApplyPatchDryRunUsesOrderedPlan(t *testing.T) {
	server, _ := testServer(t)
	result, err := server.applyPatch(json.RawMessage(`{
		"dry_run": true,
		"edits": [
			{"path":"hello.txt","old_text":"hello","new_text":"greetings"},
			{"path":"hello.txt","old_text":"greetings","new_text":"done"}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	path, _ := server.resolver.Resolve("hello.txt", workspace.Read)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "hello\nworld\n" {
		t.Fatalf("dry run changed file: %q", content)
	}
	response := result.(map[string]any)
	if response["committed"] != false || response["file_count"] != 1 {
		t.Fatalf("unexpected result: %#v", response)
	}
}

// TestApplyPatchValidatesExpectedHashPerFile 验证批次哈希针对文件原始内容校验并带编辑索引报错。
func TestApplyPatchValidatesExpectedHashPerFile(t *testing.T) {
	server, _ := testServer(t)
	_, err := server.applyPatch(json.RawMessage(`{
		"edits": [{
			"path":"hello.txt",
			"old_text":"hello",
			"new_text":"done",
			"expected_sha256":"0000000000000000000000000000000000000000000000000000000000000000"
		}]
	}`))
	if err == nil || !strings.Contains(err.Error(), `edit 0 path "hello.txt" stage expected_sha256`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestApplyPatchRejectsUnknownArguments 验证运行时参数校验拒绝 Schema 之外的字段。
func TestApplyPatchRejectsUnknownArguments(t *testing.T) {
	server, _ := testServer(t)
	_, err := server.applyPatch(json.RawMessage(`{
		"edits": [{"path":"hello.txt","old_text":"hello","new_text":"done","unknown":true}]
	}`))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestRollbackPatchFilesRestoresOriginalContent 验证补偿回滚按原始字节恢复已提交文件。
func TestRollbackPatchFilesRestoresOriginalContent(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.txt")
	second := filepath.Join(dir, "second.txt")
	if err := os.WriteFile(first, []byte("changed first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("changed second"), 0o600); err != nil {
		t.Fatal(err)
	}
	files := []*pendingPatchFile{
		{path: first, displayPath: "first.txt", original: []byte("first")},
		{path: second, displayPath: "second.txt", original: []byte("second")},
	}
	if err := rollbackPatchFiles(files); err != nil {
		t.Fatal(err)
	}
	firstContent, _ := os.ReadFile(first)
	secondContent, _ := os.ReadFile(second)
	if string(firstContent) != "first" || string(secondContent) != "second" {
		t.Fatalf("rollback content = %q, %q", firstContent, secondContent)
	}
}

// TestAtomicWritePreservesMode 验证原子替换保留已有文件权限。
func TestAtomicWritePreservesMode(t *testing.T) {
	// Windows 使用 ACL 而非 POSIX 权限位，文件权限模式无法保证，跳过该断言。
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file permissions are not enforced on Windows")
	}
	path := filepath.Join(t.TempDir(), "mode.txt")
	if err := os.WriteFile(path, []byte("old"), 0o754); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(path, []byte("new"), false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o754 {
		t.Fatalf("mode %o, want 754", got)
	}
}

// testServer 创建用于测试的 MCP 服务。
func testServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello\nworld\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Workspace.Root = dir
	cfg.Auth.TokenFile = filepath.Join(dir, ".state", "token")
	resolver, err := workspace.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	store, _, err := auth.Open(cfg.Auth.TokenFile)
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, resolver, store), store.Token()
}

// rpcPost 向测试服务发送 JSON-RPC 请求并返回记录的响应。
func rpcPost(t *testing.T, server http.Handler, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json, text/event-stream")
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)
	return w
}

// TestToolDefinitionsSchemaRequiredIsNeverNull 验证工具定义中 inputSchema 的
// required 字段在缺少必填项时被省略（而非序列化为 null），符合 MCP 客户端对
// 工具描述的校验要求（例如避免 "Invalid MCP tool descriptor"）。
func TestToolDefinitionsSchemaRequiredIsNeverNull(t *testing.T) {
	server, _ := testServer(t)
	tools := server.toolDefinitions()

	// 期望省略 required 字段的工具（无必填参数）。
	omitRequired := map[string]bool{
		"list":                        true,
		"describe_transfer_endpoints": true,
	}

	// asStringSlice 将任意字符串切片（[]string 或 []any）安全地转为 []string。
	asStringSlice := func(v any) ([]string, bool) {
		switch s := v.(type) {
		case []string:
			return s, true
		case []any:
			out := make([]string, 0, len(s))
			for _, item := range s {
				str, ok := item.(string)
				if !ok {
					return nil, false
				}
				out = append(out, str)
			}
			return out, true
		}
		return nil, false
	}

	if len(tools) == 0 {
		t.Fatal("toolDefinitions returned no tools")
	}
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		schemaVal, ok := tool["inputSchema"].(map[string]any)
		if !ok {
			t.Fatalf("tool %q: inputSchema missing or wrong type", name)
		}
		required, present := schemaVal["required"]
		if !present {
			// 字段省略：仅允许在明确无必填项的工具上。
			if !omitRequired[name] {
				t.Errorf("tool %q: required field unexpectedly omitted", name)
			}
			continue
		}
		// 字段存在：必须是字符串数组，绝不能是非数组或 null。
		arr, isArr := asStringSlice(required)
		if !isArr {
			t.Fatalf("tool %q: required is %T (%v), want string array or omitted", name, required, required)
		}
		if omitRequired[name] {
			t.Errorf("tool %q: expected required to be omitted, but got %v", name, arr)
		}
	}
}

// TestToolsListResponseHasNoNullRequired 通过真实 HTTP 路径（tools/list）验证
// 序列化后的响应中不存在 "required":null，防止 MCP 客户端拒绝工具描述。
func TestToolsListResponseHasNoNullRequired(t *testing.T) {
	server, token := testServer(t)
	resp := rpcPost(t, server, token, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	if resp.Code != http.StatusOK {
		t.Fatalf("tools/list status %d: %s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	if strings.Contains(body, `"required":null`) {
		t.Fatalf("tools/list response contains \"required\":null: %s", body)
	}
}

// TestInitializeListAndRead 验证初始化、工具列表和文件读取的完整调用流程。
func TestInitializeListAndRead(t *testing.T) {
	server, token := testServer(t)
	initBody := `{
		"jsonrpc":"2.0",
		"id":1,
		"method":"initialize",
		"params":{
			"protocolVersion":"2025-06-18",
			"capabilities":{},
			"clientInfo":{"name":"test","version":"1"}
		}
	}`
	init := rpcPost(t, server, token, initBody)
	if init.Code != http.StatusOK {
		t.Fatalf("initialize status %d: %s", init.Code, init.Body.String())
	}
	list := rpcPost(t, server, token, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	if list.Code != http.StatusOK {
		t.Fatalf("tools/list status %d: %s", list.Code, list.Body.String())
	}
	var listResponse map[string]any
	if err := json.Unmarshal(list.Body.Bytes(), &listResponse); err != nil {
		t.Fatal(err)
	}
	readBody := `{
		"jsonrpc":"2.0",
		"id":3,
		"method":"tools/call",
		"params":{
			"name":"read",
			"arguments":{"path":"hello.txt","offset":1,"limit":10}
		}
	}`
	read := rpcPost(t, server, token, readBody)
	if read.Code != http.StatusOK {
		t.Fatalf("read status %d: %s", read.Code, read.Body.String())
	}
	if !bytes.Contains(read.Body.Bytes(), []byte("hello")) {
		t.Fatalf("read result missing content: %s", read.Body.String())
	}
}

// TestAuthenticationRequired 验证请求必须通过 Bearer 认证，但不要求会话 ID。
func TestAuthenticationRequired(t *testing.T) {
	server, token := testServer(t)
	unauthorized := rpcPost(t, server, "wrong", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", unauthorized.Code)
	}
	// 无会话 ID 时，已认证的请求应当正常处理。
	noSession := rpcPost(t, server, token, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	if noSession.Code != http.StatusOK {
		t.Fatalf("tools/list status %d, want 200: %s", noSession.Code, noSession.Body.String())
	}
}

// TestPostRejectsTrailingJSON 验证 POST 请求会拒绝尾随的 JSON 数据。
func TestPostRejectsTrailingJSON(t *testing.T) {
	server, token := testServer(t)
	response := rpcPost(t, server, token, `{"jsonrpc":"2.0","id":1,"method":"initialize"}{}`)
	if !bytes.Contains(response.Body.Bytes(), []byte("invalid JSON")) {
		t.Fatalf("expected parse error: %s", response.Body.String())
	}
}

// TestFileAndDirectoryTransfers 验证文件与目录的上传和下载流程。
func TestFileAndDirectoryTransfers(t *testing.T) {
	server, token := testServer(t)
	put := httptest.NewRequest(http.MethodPut, "/files?path=uploaded.txt", bytes.NewBufferString("uploaded content"))
	put.Header.Set("Authorization", "Bearer "+token)
	putResponse := httptest.NewRecorder()
	server.ServeHTTP(putResponse, put)
	if putResponse.Code != http.StatusCreated {
		t.Fatalf("file upload status %d: %s", putResponse.Code, putResponse.Body.String())
	}
	get := httptest.NewRequest(http.MethodGet, "/files?path=uploaded.txt", nil)
	get.Header.Set("Authorization", "Bearer "+token)
	getResponse := httptest.NewRecorder()
	server.ServeHTTP(getResponse, get)
	if getResponse.Code != http.StatusOK || getResponse.Body.String() != "uploaded content" {
		t.Fatalf("file download = %d %q", getResponse.Code, getResponse.Body.String())
	}

	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	content := []byte("hello directory")
	if err := tw.WriteHeader(&tar.Header{Name: "nested/hello.txt", Mode: 0o600, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	dirPut := httptest.NewRequest(http.MethodPut, "/directories?path=uploaded-dir", bytes.NewReader(archive.Bytes()))
	dirPut.Header.Set("Authorization", "Bearer "+token)
	dirPutResponse := httptest.NewRecorder()
	server.ServeHTTP(dirPutResponse, dirPut)
	if dirPutResponse.Code != http.StatusCreated {
		t.Fatalf("directory upload status %d: %s", dirPutResponse.Code, dirPutResponse.Body.String())
	}
	extractedPath := filepath.Join(
		server.resolver.Workspace(), "uploaded-dir", "nested", "hello.txt",
	)
	if data, err := os.ReadFile(extractedPath); err != nil || string(data) != "hello directory" {
		t.Fatalf("extracted file = %q, err=%v", data, err)
	}

	dirGet := httptest.NewRequest(http.MethodGet, "/directories?path=uploaded-dir", nil)
	dirGet.Header.Set("Authorization", "Bearer "+token)
	dirGetResponse := httptest.NewRecorder()
	server.ServeHTTP(dirGetResponse, dirGet)
	if dirGetResponse.Code != http.StatusOK {
		t.Fatalf("directory download status %d: %s", dirGetResponse.Code, dirGetResponse.Body.String())
	}
	gzReader, err := gzip.NewReader(bytes.NewReader(dirGetResponse.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	defer gzReader.Close()
	tarReader := tar.NewReader(gzReader)
	var downloaded []byte
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == "nested/hello.txt" {
			downloaded, err = io.ReadAll(tarReader)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if string(downloaded) != "hello directory" {
		t.Fatalf("downloaded archive content = %q", downloaded)
	}
}

// TestDirectoryUploadRejectsPathTraversal 验证目录上传会拒绝包含路径穿越的归档。
func TestDirectoryUploadRejectsPathTraversal(t *testing.T) {
	server, token := testServer(t)
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	content := []byte("must not escape")
	if err := tw.WriteHeader(&tar.Header{Name: "../escape.txt", Mode: 0o600, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write(content)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, "/directories?path=unsafe-dir", bytes.NewReader(archive.Bytes()))
	req.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, req)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", response.Code)
	}
	if _, err := os.Stat(filepath.Join(server.resolver.Workspace(), "escape.txt")); !os.IsNotExist(err) {
		t.Fatal("archive traversal created an escaped file")
	}
}

// TestGlobDoubleStar 验证双星号通配符可跨目录层级匹配路径。
func TestGlobDoubleStar(t *testing.T) {
	if !globMatch("**/*.go", "main.go") || !globMatch("**/*.go", "cmd/main.go") || globMatch("**/*.go", "README.md") {
		t.Fatal("double-star glob matching is incorrect")
	}
}

// TestReadChineseBoundaryTruncation 验证 read 按 rune 边界截断中文，不出现乱码。
func TestReadChineseBoundaryTruncation(t *testing.T) {
	server, _ := testServer(t)
	path := filepath.Join(server.resolver.Workspace(), "cn.txt")
	content := []byte("中文测试内容")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	server.cfg.Files.MaxReadBytes = 9
	result, err := server.read(json.RawMessage(`{"path":"cn.txt","offset":1,"limit":200}`))
	if err != nil {
		t.Fatal(err)
	}
	resp := result.(map[string]any)
	if !resp["truncated"].(bool) {
		t.Fatalf("expected truncated=true, got %v", resp["truncated"])
	}
	// MaxReadBytes=9 时按 rune 边界截断：前 4 个汉字（8 字节）保留，第 5 字被截断，不应出现半个汉字或乱码。
	got := resp["content"].(string)
	if !strings.Contains(got, "中文测") || strings.Contains(got, "试") || strings.Contains(got, "？") {
		t.Fatalf("truncated content = %q", got)
	}
}

// TestReadTruncatedFlagExact 验证 read 的 truncated 标志仅在真正超限时为 true。
func TestReadTruncatedFlagExact(t *testing.T) {
	server, _ := testServer(t)
	path := filepath.Join(server.resolver.Workspace(), "exact.txt")
	if err := os.WriteFile(path, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	server.cfg.Files.MaxReadBytes = 10
	r1, err := server.read(json.RawMessage(`{"path":"exact.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	if r1.(map[string]any)["truncated"].(bool) {
		t.Fatal("exactly-at-limit read must not be marked truncated")
	}
	server.cfg.Files.MaxReadBytes = 9
	r2, err := server.read(json.RawMessage(`{"path":"exact.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !r2.(map[string]any)["truncated"].(bool) {
		t.Fatal("over-limit read must be marked truncated")
	}
}

// TestEditRejectsOversizedFile 验证编辑超过 max_read_bytes 的文件时明确报错而非静默读全量。
func TestEditRejectsOversizedFile(t *testing.T) {
	server, _ := testServer(t)
	path := filepath.Join(server.resolver.Workspace(), "big.txt")
	if err := os.WriteFile(path, bytes.Repeat([]byte("a"), 100), 0o600); err != nil {
		t.Fatal(err)
	}
	server.cfg.Files.MaxReadBytes = 50
	_, err := server.edit(json.RawMessage(`{"path":"big.txt","old_text":"a","new_text":"b"}`))
	if err == nil || !strings.Contains(err.Error(), "exceeds max_read_bytes") {
		t.Fatalf("expected max_read_bytes error, got %v", err)
	}
}

// TestWriteWithoutExpectedUsesStatOnly 验证无 expected_sha256 时 write 仅做存在性判断。
func TestWriteWithoutExpectedUsesStatOnly(t *testing.T) {
	server, _ := testServer(t)
	if _, err := server.write(json.RawMessage(`{"path":"new.txt","content":"hello"}`)); err != nil {
		t.Fatalf("write without expected_sha256 failed: %v", err)
	}
	_, err := server.write(json.RawMessage(`{"path":"new.txt","content":"world"}`))
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected already-exists error, got %v", err)
	}
}

// TestApplyPatchCommitsWithoutGlobalLock 验证多文件 apply_patch 在分片锁下正常提交。
func TestApplyPatchCommitsWithoutGlobalLock(t *testing.T) {
	server, _ := testServer(t)
	first := filepath.Join(server.resolver.Workspace(), "a.txt")
	second := filepath.Join(server.resolver.Workspace(), "b.txt")
	if err := os.WriteFile(first, []byte("aaa"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("bbb"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := server.applyPatch(json.RawMessage(`{
		"edits": [
			{"path":"a.txt","old_text":"aaa","new_text":"AAA"},
			{"path":"b.txt","old_text":"bbb","new_text":"BBB"}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	resp := result.(map[string]any)
	if resp["file_count"].(int) != 2 || !resp["committed"].(bool) {
		t.Fatalf("unexpected apply_patch result: %#v", resp)
	}
	aData, _ := os.ReadFile(first)
	bData, _ := os.ReadFile(second)
	if string(aData) != "AAA" || string(bData) != "BBB" {
		t.Fatalf("committed content mismatch: a=%q b=%q", aData, bData)
	}
}
