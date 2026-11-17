package integration

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"go/version"
	"io"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestStdioMemoryLifecycle(t *testing.T) {
	if runtime.GOOS == "darwin" && version.Compare(runtime.Version(), "go1.27") < 0 {
		t.Skip("macOS Go versions before 1.27 cannot use the fixture CA through SSL_CERT_FILE")
	}
	root := t.TempDir()
	repo, work := filepath.Join(root, "memory.git"), filepath.Join(root, "work")
	runGit := func(args ...string) string {
		t.Helper()
		c := exec.Command("git", args...)
		for _, value := range os.Environ() {
			if !strings.HasPrefix(value, "GIT_") && !strings.HasPrefix(value, "HOME=") && !strings.HasPrefix(value, "XDG_CONFIG_HOME=") {
				c.Env = append(c.Env, value)
			}
		}
		c.Env = append(c.Env, "HOME="+root, "XDG_CONFIG_HOME="+root, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
		output, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, output, err)
		}
		return strings.TrimSpace(string(output))
	}
	runGit("init", "--bare", "--initial-branch=main", repo)
	runGit("--git-dir", repo, "config", "http.receivepack", "true")
	runGit("init", "--initial-branch=main", work)
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("unrelated\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("-C", work, "add", "README.md")
	runGit("-C", work, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "initial")
	runGit("-C", work, "push", repo, "main")
	handler := &cgi.Handler{
		Path:   filepath.Join(runGit("--exec-path"), "git-http-backend"),
		Root:   "/",
		Env:    []string{"HOME=" + root, "XDG_CONFIG_HOME=" + root, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"},
		Stderr: io.Discard,
	}
	remote := httptest.NewTLSServer(handler)
	defer remote.Close()
	certPath := filepath.Join(root, "fixture-cert.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: remote.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "memory-mcp")
	build := exec.Command("go", "build", "-mod=readonly", "-o", binary, "../cmd/memory-mcp")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build server: %s: %v", out, err)
	}
	runDir := filepath.Join(root, "runtime")
	if err := os.Mkdir(runDir, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(runDir, 0700)
	command := exec.Command(binary)
	command.Dir = runDir
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "MEMORY_") && !strings.HasPrefix(value, "GIT_") && !strings.HasPrefix(value, "HOME=") && !strings.HasPrefix(value, "TMPDIR=") && !strings.HasPrefix(value, "SSL_CERT_FILE=") && !strings.HasPrefix(value, "GODEBUG=") {
			command.Env = append(command.Env, value)
		}
	}
	command.Env = append(command.Env,
		"MEMORY_GIT_URL="+remote.URL+"/memory.git",
		"MEMORY_GIT_BRANCH=main",
		"HOME="+runDir,
		"TMPDIR="+runDir,
		"SSL_CERT_FILE="+certPath,
		"GODEBUG=x509sslcertoverrideplatform=1",
	)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "integration-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: command}, nil)
	if err != nil {
		t.Fatalf("connect: %v (%s)", err, stderr.String())
	}
	defer session.Close()
	tools, err := session.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 13 {
		t.Fatalf("tools: %v %v", tools, err)
	}
	call := func(name string, args map[string]any, wantError bool) map[string]any {
		t.Helper()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || result.IsError != wantError || len(result.Content) == 0 {
			t.Fatalf("%s: %+v %v (%s)", name, result, err, stderr.String())
		}
		text, ok := result.Content[0].(*mcp.TextContent)
		if !ok {
			t.Fatalf("%s: unexpected content %T", name, result.Content[0])
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(text.Text), &out); err != nil {
			t.Fatalf("%s: invalid JSON result: %v", name, err)
		}
		return out
	}
	first := call("create_memory", map[string]any{"name": "example", "description": "Example repository", "keywords": []string{"offline", "empty repository"}, "body": "feature: first\nNetwork failures do not fall back to stale data.\nThe repository and branch must already exist and contain a commit."}, false)
	firstRev := first["revision"].(string)
	call("create_memory", map[string]any{"name": "links", "description": "Links", "body": "feature: `mem:example`\nTests must not leave the driver empty."}, false)
	index := call("read_index", map[string]any{}, false)
	if index["index_status"] != "current" || !strings.Contains(index["index"].(string), "`mem:example`") {
		t.Fatalf("unexpected index: %v", index)
	}
	for _, query := range []string{"offline", "OFFLINE", "empty repository"} {
		args := map[string]any{"query": query}
		result := call("search_memories", args, false)
		items := result["items"].([]any)
		if len(items) == 0 || items[0].(map[string]any)["name"] != "example" {
			t.Fatalf("search %q did not find the relevant memory first: %v", query, items)
		}
	}
	result := call("search_memories", map[string]any{"query": "feature: first"}, false)
	if items := result["items"].([]any); len(items) != 1 || items[0].(map[string]any)["name"] != "example" {
		t.Fatalf("literal search returned unexpected matches: %v", result)
	}
	if items := call("search_memories", map[string]any{"query": "astronaut telescope"}, false)["items"].([]any); len(items) != 0 {
		t.Fatalf("unrelated query returned matches: %v", items)
	}
	if items := call("search_memories", map[string]any{"query": "offline", "topic": "links"}, false)["items"].([]any); len(items) != 0 {
		t.Fatalf("search ignored topic filter: %v", items)
	}
	page := call("search_memories", map[string]any{"query": "feature:", "limit": 1}, false)
	cursor := page["next_cursor"].(string)
	if cursor == "" {
		t.Fatal("expected search cursor")
	}
	call("search_memories", map[string]any{"query": "feature:", "limit": 1, "cursor": cursor}, false)
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		t.Fatal(err)
	}
	for _, replacement := range []string{`"other":1`, `"offset":null`, `"offset":0`, `"offset":1,"offset":1`} {
		malformed := strings.Replace(string(raw), `"offset":1`, replacement, 1)
		badCursor := base64.RawURLEncoding.EncodeToString([]byte(malformed))
		result := call("search_memories", map[string]any{"query": "feature:", "limit": 1, "cursor": badCursor}, true)
		if result["code"] != "CURSOR_CONFLICT" {
			t.Fatalf("invalid cursor accepted: %v", result)
		}
	}
	oldCursor := strings.Replace(string(raw), `,"version":3`, "", 1)
	result = call("search_memories", map[string]any{"query": "feature:", "limit": 1, "cursor": base64.RawURLEncoding.EncodeToString([]byte(oldCursor))}, true)
	if result["code"] != "CURSOR_CONFLICT" {
		t.Fatalf("old cursor version accepted: %v", result)
	}
	entry := call("read_memory", map[string]any{"name": "example"}, false)
	keywords := entry["keywords"].([]any)
	if len(keywords) != 2 || keywords[0] != "offline" {
		t.Fatalf("keywords were not persisted: %v", entry)
	}
	updateArgs := map[string]any{"name": "example", "expected_revision": firstRev, "description": entry["description"], "body": entry["body"]}
	unchanged := call("update_memory", updateArgs, false)
	if unchanged["revision"] != firstRev {
		t.Fatal("omitting keywords changed the memory")
	}
	updateArgs["keywords"] = []string{}
	cleared := call("update_memory", updateArgs, false)
	if items := call("search_memories", map[string]any{"query": "offline"}, false)["items"].([]any); len(items) != 0 {
		t.Fatalf("cleared keyword still matched: %v", items)
	}
	updateArgs["expected_revision"] = cleared["revision"]
	updateArgs["keywords"] = []string{"offline", "empty repository"}
	firstRev = call("update_memory", updateArgs, false)["revision"].(string)
	blocked := call("delete_memory", map[string]any{"name": "example", "expected_revision": firstRev}, true)
	if blocked["code"] != "REFERENCED" {
		t.Fatalf("delete was not blocked: %v", blocked)
	}
	renamed := call("rename_memory", map[string]any{"name": "example", "new_name": "renamed", "expected_revision": firstRev}, false)
	links := call("read_memory", map[string]any{"name": "links"}, false)
	if !strings.Contains(links["body"].(string), "`mem:renamed`") {
		t.Fatal("rename did not update reference")
	}
	call("search_memories", map[string]any{"query": "feature:", "limit": 1, "cursor": cursor}, true)
	call("update_memory", map[string]any{"name": "links", "expected_revision": links["revision"], "description": "Links", "body": "no references"}, false)
	call("delete_memory", map[string]any{"name": "renamed", "expected_revision": renamed["revision"]}, false)
	call("repair_index", map[string]any{}, false)
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(runDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("server wrote runtime files: %v %v", entries, err)
	}
	if data := runGit("--git-dir", repo, "show", "main:README.md"); data != "unrelated" {
		t.Fatal("unrelated file changed")
	}
}
