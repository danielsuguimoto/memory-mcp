package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	"memory-mcp/internal/config"
	"memory-mcp/internal/memory"
)

type Manager struct {
	mu          sync.Mutex
	cfg         config.Config
	http        *http.Client
	base        string
	device      string
	expiry      time.Time
	nextPoll    time.Time
	interval    time.Duration
	token       string
	refresh     string
	tokenExpiry time.Time
}

type StartResult struct {
	VerificationURI string `json:"verification_uri"`
	UserCode        string `json:"user_code"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

type Status struct {
	Mode          string `json:"mode"`
	Authenticated bool   `json:"authenticated"`
}

type deviceResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
	Error           string `json:"error"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
}

func New(c config.Config) *Manager {
	return &Manager{cfg: c, base: "https://github.com", http: &http.Client{
		Timeout: 20 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 || len(via) > 0 && (req.URL.Scheme != via[0].URL.Scheme || req.URL.Host != via[0].URL.Host) {
				return errors.New("cross-origin authentication redirect rejected")
			}
			return nil
		},
	}}
}

func (a *Manager) mode() string {
	if a.cfg.Token != "" {
		return "environment"
	}
	if a.cfg.GitHub() && a.cfg.ClientID != "" {
		return "github_device"
	}
	return "anonymous"
}

func (a *Manager) Status() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	valid := a.cfg.Token != "" || a.token != "" && (a.tokenExpiry.IsZero() || time.Now().Before(a.tokenExpiry) || a.refresh != "")
	return Status{Mode: a.mode(), Authenticated: valid}
}

func (a *Manager) Start(ctx context.Context) (StartResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.mode() != "github_device" {
		return StartResult{}, memory.Fail("AUTH_UNAVAILABLE", "GitHub device login is not configured")
	}
	var response deviceResponse
	err := a.post(ctx, "/login/device/code", url.Values{"client_id": {a.cfg.ClientID}}, &response)
	if err != nil || response.Error != "" || response.DeviceCode == "" || response.UserCode == "" || response.ExpiresIn <= 0 {
		return StartResult{}, memory.Fail("AUTH_ERROR", "could not start GitHub device authorization")
	}
	if response.VerificationURI != "https://github.com/login/device" {
		return StartResult{}, memory.Fail("AUTH_ERROR", "invalid GitHub device verification URL")
	}
	if response.Interval < 1 {
		response.Interval = 5
	}
	a.device = response.DeviceCode
	a.expiry = time.Now().Add(time.Duration(response.ExpiresIn) * time.Second)
	a.interval = time.Duration(response.Interval) * time.Second
	a.nextPoll = time.Now().Add(a.interval)
	return StartResult{VerificationURI: response.VerificationURI, UserCode: response.UserCode, ExpiresIn: response.ExpiresIn, Interval: response.Interval}, nil
}

func (a *Manager) Poll(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.mode() != "github_device" || a.device == "" {
		return "", memory.Fail("AUTH_UNAVAILABLE", "no GitHub device authorization is pending")
	}
	now := time.Now()
	if !now.Before(a.expiry) {
		a.device = ""
		return "expired", nil
	}
	if now.Before(a.nextPoll) {
		return "pending", nil
	}
	a.nextPoll = now.Add(a.interval)
	var response tokenResponse
	err := a.post(ctx, "/login/oauth/access_token", url.Values{
		"client_id": {a.cfg.ClientID}, "device_code": {a.device},
		"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"},
	}, &response)
	if err != nil {
		return "", memory.Fail("AUTH_ERROR", "could not poll GitHub authorization")
	}
	switch response.Error {
	case "authorization_pending":
		return "pending", nil
	case "slow_down":
		a.interval += 5 * time.Second
		a.nextPoll = time.Now().Add(a.interval)
		return "pending", nil
	case "access_denied":
		a.device = ""
		return "denied", nil
	case "expired_token":
		a.device = ""
		return "expired", nil
	case "":
		if response.AccessToken == "" {
			return "", memory.Fail("AUTH_ERROR", "GitHub returned no access token")
		}
		a.setToken(response)
		a.device = ""
		return "authorized", nil
	default:
		return "", memory.Fail("AUTH_ERROR", "GitHub authorization failed")
	}
}

func (a *Manager) setToken(r tokenResponse) {
	a.token, a.refresh = r.AccessToken, r.RefreshToken
	a.tokenExpiry = time.Time{}
	if r.ExpiresIn > 0 {
		a.tokenExpiry = time.Now().Add(time.Duration(r.ExpiresIn) * time.Second)
	}
}

func (a *Manager) Credentials(ctx context.Context) (transport.AuthMethod, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cfg.Token != "" {
		return &githttp.BasicAuth{Username: a.cfg.Username, Password: a.cfg.Token}, nil
	}
	if a.token != "" && !a.tokenExpiry.IsZero() && time.Now().After(a.tokenExpiry.Add(-30*time.Second)) {
		if a.refresh == "" {
			a.token = ""
			return nil, memory.Fail("AUTH_REQUIRED", "GitHub authorization expired")
		}
		var response tokenResponse
		err := a.post(ctx, "/login/oauth/access_token", url.Values{
			"client_id": {a.cfg.ClientID}, "grant_type": {"refresh_token"}, "refresh_token": {a.refresh},
		}, &response)
		if err != nil || response.Error != "" || response.AccessToken == "" {
			a.token, a.refresh = "", ""
			return nil, memory.Fail("AUTH_REQUIRED", "GitHub authorization refresh failed; log in again")
		}
		a.setToken(response)
	}
	if a.token == "" {
		return nil, nil
	}
	return &githttp.BasicAuth{Username: "x-access-token", Password: a.token}, nil
}

func (a *Manager) post(ctx context.Context, endpoint string, form url.Values, result any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("authentication endpoint rejected request")
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(result)
}
