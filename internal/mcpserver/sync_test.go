package mcpserver

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestSyncPlanAndApply 验证同步规划与应用能够正确创建和更新文件。
func TestSyncPlanAndApply(t *testing.T) {
	server, token := testServer(t)
	root := filepath.Join(server.resolver.Workspace(), "sync-root")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatal(err)
	}
	remote := map[string]string{
		"same.txt":    "same content",
		"changed.txt": "old content",
		"extra.txt":   "keep me",
	}
	for name, content := range remote {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	local := syncManifest{Files: []syncFile{
		{Path: "same.txt", Size: int64(len("same content")), SHA256: testHash("same content")},
		{Path: "changed.txt", Size: int64(len("new content")), SHA256: testHash("new content")},
		{Path: "missing.txt", Size: int64(len("new file")), SHA256: testHash("new file")},
	}}
	body, err := json.Marshal(local)
	if err != nil {
		t.Fatal(err)
	}
	planRequest := httptest.NewRequest(http.MethodPost, "/sync/plan?path=sync-root", bytes.NewReader(body))
	planRequest.Header.Set("Authorization", "Bearer "+token)
	planResponse := httptest.NewRecorder()
	server.ServeHTTP(planResponse, planRequest)
	if planResponse.Code != http.StatusOK {
		t.Fatalf("sync plan status %d: %s", planResponse.Code, planResponse.Body.String())
	}
	var plan syncPlan
	if err := json.Unmarshal(planResponse.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Missing, []string{"missing.txt"}) ||
		!reflect.DeepEqual(plan.Changed, []string{"changed.txt"}) ||
		!reflect.DeepEqual(plan.Unchanged, []string{"same.txt"}) ||
		!reflect.DeepEqual(plan.Extra, []string{"extra.txt"}) {
		t.Fatalf("unexpected sync plan: %#v", plan)
	}

	applyManifest := syncManifest{Files: []syncFile{
		{Path: "changed.txt", Size: int64(len("new content")), SHA256: testHash("new content"), ExpectedSHA256: testHash("old content")},
		{Path: "missing.txt", Size: int64(len("new file")), SHA256: testHash("new file")},
	}}
	archive := makeSyncArchive(t, applyManifest, map[string]string{"changed.txt": "new content", "missing.txt": "new file"})
	applyRequest := httptest.NewRequest(http.MethodPut, "/sync/apply?path=sync-root", bytes.NewReader(archive))
	applyRequest.Header.Set("Authorization", "Bearer "+token)
	applyResponse := httptest.NewRecorder()
	server.ServeHTTP(applyResponse, applyRequest)
	if applyResponse.Code != http.StatusOK {
		t.Fatalf("sync apply status %d: %s", applyResponse.Code, applyResponse.Body.String())
	}
	assertFileContent(t, filepath.Join(root, "changed.txt"), "new content")
	assertFileContent(t, filepath.Join(root, "missing.txt"), "new file")
	assertFileContent(t, filepath.Join(root, "extra.txt"), "keep me")
}

// TestSyncApplyRejectsManifestMismatch 验证同步应用会拒绝内容与清单不一致的归档。
func TestSyncApplyRejectsManifestMismatch(t *testing.T) {
	server, token := testServer(t)
	manifest := syncManifest{Files: []syncFile{{Path: "bad.txt", Size: 5, SHA256: testHash("right")}}}
	archive := makeSyncArchive(t, manifest, map[string]string{"bad.txt": "wrong"})
	request := httptest.NewRequest(http.MethodPut, "/sync/apply?path=sync-root", bytes.NewReader(archive))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("sync mismatch status %d, want 400", response.Code)
	}
	if _, err := os.Stat(filepath.Join(server.resolver.Workspace(), "sync-root", "bad.txt")); !os.IsNotExist(err) {
		t.Fatal("mismatched file was applied")
	}
}

// makeSyncArchive 为测试构造包含清单和文件内容的同步归档。
func makeSyncArchive(t *testing.T, manifest syncManifest, files map[string]string) []byte {
	t.Helper()
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	tw := tar.NewWriter(gz)
	writeTarEntry(t, tw, syncManifestFile, manifestData)
	for name, content := range files {
		writeTarEntry(t, tw, name, []byte(content))
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// writeTarEntry 向测试 tar 归档写入指定名称和内容的普通文件。
func writeTarEntry(t *testing.T, writer *tar.Writer, name string, content []byte) {
	t.Helper()
	header := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(content)), Typeflag: tar.TypeReg}
	if err := writer.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(content); err != nil {
		t.Fatal(err)
	}
}

// testHash 计算测试内容的 SHA-256 十六进制摘要。
func testHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// assertFileContent 断言指定文件的内容符合预期。
func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("file %s = %q, want %q", path, data, want)
	}
}
