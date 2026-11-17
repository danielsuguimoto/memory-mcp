package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	"memory-mcp/internal/config"
)

func TestDeviceFlowAndRefresh(t *testing.T) {
	var mu sync.Mutex
	polls := 0
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/login/device/code" {
			_, _ = w.Write([]byte(`{"device_code":"private-device-code","user_code":"ABCD-EFGH","verification_uri":"https://github.com/login/device","expires_in":900,"interval":5}`))
			return
		}
		if r.FormValue("grant_type") == "refresh_token" {
			_, _ = w.Write([]byte(`{"access_token":"new-token","refresh_token":"new-refresh","expires_in":3600}`))
			return
		}
		polls++
		switch polls {
		case 1:
			_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
		case 2:
			_, _ = w.Write([]byte(`{"error":"slow_down"}`))
		default:
			_, _ = w.Write([]byte(`{"access_token":"private-token","refresh_token":"private-refresh","expires_in":3600}`))
		}
	}))
	defer fixture.Close()
	a := New(config.Config{URL: "https://github.com/example/memory.git", ClientID: "public-id"})
	a.base = fixture.URL
	ctx := context.Background()
	start, err := a.Start(ctx)
	if err != nil || start.UserCode != "ABCD-EFGH" {
		t.Fatalf("start: %+v %v", start, err)
	}
	encoded, _ := json.Marshal(start)
	if strings.Contains(string(encoded), "private-device-code") {
		t.Fatal("device secret was exposed")
	}
	if status, err := a.Poll(ctx); err != nil || status != "pending" || polls != 0 {
		t.Fatalf("poll ignored interval: %s %v", status, err)
	}
	a.nextPoll = time.Time{}
	if status, err := a.Poll(ctx); err != nil || status != "pending" {
		t.Fatalf("pending: %s %v", status, err)
	}
	a.nextPoll = time.Time{}
	if status, err := a.Poll(ctx); err != nil || status != "pending" || a.interval != 10*time.Second {
		t.Fatalf("slow down: %s %v", status, err)
	}
	a.nextPoll = time.Time{}
	if status, err := a.Poll(ctx); err != nil || status != "authorized" || !a.Status().Authenticated {
		t.Fatalf("authorized: %s %v", status, err)
	}
	a.tokenExpiry = time.Now().Add(-time.Second)
	credentials, err := a.Credentials(ctx)
	if err != nil || credentials.(*githttp.BasicAuth).Password != "new-token" {
		t.Fatalf("refresh failed: %v", err)
	}
}

func TestTokenPrecedenceAndUnavailableLogin(t *testing.T) {
	a := New(config.Config{URL: "https://github.com/example/memory.git", ClientID: "public-id", Username: "user", Token: "private-token"})
	if _, err := a.Start(context.Background()); err == nil {
		t.Fatal("device login must not replace an environment token")
	}
	credentials, err := a.Credentials(context.Background())
	if err != nil || credentials.(*githttp.BasicAuth).Password != "private-token" {
		t.Fatal("environment credentials unavailable")
	}
	data, _ := json.Marshal(a.Status())
	if strings.Contains(string(data), "private-token") {
		t.Fatal("status exposed the token")
	}
}

func TestExpiredAndDeniedDeviceFlow(t *testing.T) {
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"error":"access_denied"}`))
	}))
	defer fixture.Close()
	a := New(config.Config{URL: "https://github.com/example/memory.git", ClientID: "public-id"})
	a.base = fixture.URL
	a.device = "test-device"
	a.expiry = time.Now().Add(-time.Second)
	if status, err := a.Poll(context.Background()); err != nil || status != "expired" {
		t.Fatalf("expired flow: %s %v", status, err)
	}
	a.device = "test-device"
	a.expiry = time.Now().Add(time.Minute)
	if status, err := a.Poll(context.Background()); err != nil || status != "denied" {
		t.Fatalf("denied flow: %s %v", status, err)
	}
}
