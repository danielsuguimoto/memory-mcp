package gitstore

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/transport"

	"memory-mcp/internal/config"
	"memory-mcp/internal/memory"
)

func fixture(t *testing.T) *Store {
	return fixtureWithDroppedPush(t, "")
}

func fixtureWithDroppedPush(t *testing.T, drop string, prepare ...func(string)) *Store {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "memory.git")
	work := filepath.Join(root, "work")
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		for _, value := range os.Environ() {
			if !strings.HasPrefix(value, "GIT_") && !strings.HasPrefix(value, "HOME=") && !strings.HasPrefix(value, "XDG_CONFIG_HOME=") {
				cmd.Env = append(cmd.Env, value)
			}
		}
		cmd.Env = append(cmd.Env, "HOME="+root, "XDG_CONFIG_HOME="+root, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "--bare", "--initial-branch=main", repo)
	run("--git-dir", repo, "config", "http.receivepack", "true")
	run("init", "--initial-branch=main", work)
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("unrelated\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if len(prepare) > 0 {
		prepare[0](work)
	}
	run("-C", work, "add", "--all")
	run("-C", work, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "initial")
	run("-C", work, "push", repo, "main")
	backend := filepath.Join(run("--exec-path"), "git-http-backend")
	h := &cgi.Handler{Path: backend, Root: "/", Env: []string{"HOME=" + root, "XDG_CONFIG_HOME=" + root, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}, Stderr: io.Discard}
	var dropped atomic.Bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if drop != "" && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-receive-pack") && dropped.CompareAndSwap(false, true) {
			if drop == "after" {
				recorder := httptest.NewRecorder()
				h.ServeHTTP(recorder, r)
			}
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.(*net.TCPConn).SetLinger(0)
				_ = conn.Close()
			}
			return
		}
		h.ServeHTTP(w, r)
	})
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	cfg := config.Config{URL: httpServer.URL + "/memory.git", Branch: "main", AuthorName: "Test", AuthorEmail: "test@example.invalid"}
	return New(cfg, func(context.Context) (transport.AuthMethod, error) { return nil, nil })
}

func TestRejectsManagedSymlinks(t *testing.T) {
	store := fixtureWithDroppedPush(t, "", func(work string) {
		dir := filepath.Join(work, "memories")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/etc/passwd", filepath.Join(dir, "unsafe.md")); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := store.Snapshot(context.Background()); err == nil || !strings.Contains(err.Error(), "INVALID_MEMORY") {
		t.Fatalf("expected managed symlink rejection, got %v", err)
	}
}

func TestLostPushResponseIsConfirmed(t *testing.T) {
	store := fixtureWithDroppedPush(t, "after")
	result, err := store.Mutate(context.Background(), memory.Mutation{Kind: "create", Name: "confirmed", Description: "Confirmed", Body: "body"})
	if err != nil || result.Status != "pushed" {
		t.Fatalf("expected successful remote confirmation: %+v %v", result, err)
	}
}

func TestLostPushWithoutPublicationIsUnknown(t *testing.T) {
	store := fixtureWithDroppedPush(t, "before")
	_, err := store.Mutate(context.Background(), memory.Mutation{Kind: "create", Name: "unknown", Description: "Unknown", Body: "body"})
	if err == nil || !strings.Contains(err.Error(), "OUTCOME_UNKNOWN") {
		t.Fatalf("expected unknown outcome, got %v", err)
	}
	snapshot, err := store.Snapshot(context.Background())
	if err != nil || len(snapshot.State.Entries) != 0 {
		t.Fatalf("unexpected remote state: %+v %v", snapshot, err)
	}
}

func TestConcurrentConflictingUpdates(t *testing.T) {
	store := fixture(t)
	created, err := store.Mutate(context.Background(), memory.Mutation{Kind: "create", Name: "same", Description: "Same", Body: "initial"})
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	for _, body := range []string{"first", "second"} {
		go func(body string) {
			_, err := store.Mutate(context.Background(), memory.Mutation{Kind: "update", Name: "same", ExpectedRevision: created.Revision, Description: "Same", Body: body})
			errs <- err
		}(body)
	}
	success, conflicts := 0, 0
	for i := 0; i < 2; i++ {
		err := <-errs
		if err == nil {
			success++
		} else if strings.Contains(err.Error(), "REVISION_CONFLICT") {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d", success, conflicts)
	}
}

func TestSmartHTTPMutationLifecycle(t *testing.T) {
	store := fixture(t)
	ctx := context.Background()
	created, err := store.Mutate(ctx, memory.Mutation{Kind: "create", Name: "project/auth", Description: "Authentication", Body: "Original"})
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != "pushed" || created.Revision == "" {
		t.Fatalf("unexpected create: %+v", created)
	}
	snapshot, err := store.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := snapshot.State.Get("project/auth", "")
	if err != nil || entry.Body != "Original" || snapshot.State.IndexStatus() != "current" {
		t.Fatalf("unexpected snapshot: %+v %v", entry, err)
	}
	updated, err := store.Mutate(ctx, memory.Mutation{Kind: "update", Name: entry.Name, ExpectedRevision: entry.Revision, Description: entry.Description, Body: "Updated"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Mutate(ctx, memory.Mutation{Kind: "delete", Name: entry.Name, ExpectedRevision: entry.Revision}); err == nil || !strings.Contains(err.Error(), "REVISION_CONFLICT") {
		t.Fatalf("expected revision conflict, got %v", err)
	}
	if _, err := store.Mutate(ctx, memory.Mutation{Kind: "delete", Name: entry.Name, ExpectedRevision: updated.Revision}); err != nil {
		t.Fatal(err)
	}
	found, err := store.CheckCommit(ctx, created.Commit)
	if err != nil || !found {
		t.Fatalf("commit not found: %v %v", found, err)
	}
}

func TestConcurrentIndependentWrites(t *testing.T) {
	store := fixture(t)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, name := range []string{"one", "two"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			_, err := store.Mutate(context.Background(), memory.Mutation{Kind: "create", Name: name, Description: name, Body: "body"})
			errs <- err
		}(name)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := store.Snapshot(context.Background())
	if err != nil || len(snapshot.State.Entries) != 2 {
		t.Fatalf("lost a concurrent write: %+v %v", snapshot, err)
	}
}
