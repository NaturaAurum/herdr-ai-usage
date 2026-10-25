package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Limit is one rate-limit window. Used is 0–100.
type Limit struct {
	Name     string    `json:"name"`
	Used     float64   `json:"used"`
	ResetsAt time.Time `json:"resets_at"`
}

// Service is the last known state of one account, cached on disk so every
// panel instance shares one fetch per TTL.
type Service struct {
	Email      string    `json:"email,omitempty"`
	Plan       string    `json:"plan,omitempty"`
	Limits     []Limit   `json:"limits,omitempty"`
	Tickets    *Tickets  `json:"tickets,omitempty"` // ChatGPT rate limit reset credits
	FetchedAt  time.Time `json:"fetched_at"`
	RetryUntil time.Time `json:"retry_until"`
	Err        string    `json:"err,omitempty"`
}

// Tickets are ChatGPT's rate limit reset credits; Usable is how many apply
// right now (only when a limit is hit).
type Tickets struct {
	Count  int `json:"available_count"`
	Usable int `json:"applicable_available_count"`
}

type httpError struct {
	status     int
	retryAfter time.Duration
}

func (e *httpError) Error() string { return "HTTP " + strconv.Itoa(e.status) }

var errRelogin = errors.New("login expired")

var client = &http.Client{Timeout: 15 * time.Second}

func cacheFile(a Account) string { return filepath.Join(cfgDir, "cache", a.ID+".json") }

// usage returns the account's cached state, fetching first when it is stale.
// force skips the TTL but never the server's Retry-After.
func usage(a Account, force bool) Service {
	return cachedUsage(a, force, func() (Service, error) { return providers[a.Provider].fetch(a) })
}

func cachedUsage(a Account, force bool, fetch func() (Service, error)) Service {
	path := cacheFile(a)
	os.MkdirAll(filepath.Dir(path), 0o700)
	// The lock also serializes token refresh: two panels refreshing with the
	// same rotating refresh token would log the account out.
	if lf, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600); err == nil {
		defer lf.Close()
		syscall.Flock(int(lf.Fd()), syscall.LOCK_EX)
	}

	var s Service
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &s)
	}
	now := time.Now()
	if now.Before(s.RetryUntil) || (!force && s.Err == "" && now.Sub(s.FetchedAt) < providers[a.Provider].ttl) {
		return s
	}

	fresh, err := fetch()
	var he *httpError
	switch {
	case err == nil:
		s = fresh
		s.FetchedAt = now
	case errors.As(err, &he) && he.status == http.StatusTooManyRequests:
		// Keep the last known limits on screen.
		s.RetryUntil = now.Add(max(he.retryAfter, providers[a.Provider].ttl))
		s.Err = "rate limited"
	default:
		s.RetryUntil = now.Add(time.Minute)
		s.Err = err.Error()
	}
	b, _ := json.Marshal(s)
	os.WriteFile(path, b, 0o600)
	return s
}

func getJSON(req *http.Request, v any) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return errRelogin
	}
	if resp.StatusCode != http.StatusOK {
		ra, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		return &httpError{resp.StatusCode, time.Duration(ra) * time.Second}
	}
	return json.Unmarshal(body, v)
}

func postJSON(url string, in, out any) error {
	b, _ := json.Marshal(in)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	return getJSON(req, out)
}

// ---- Claude ----

const claudeClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

// claudeService mirrors Claude Code's keychain naming: a non-default
// CLAUDE_CONFIG_DIR gets the first 8 hex chars of its sha256 appended.
func claudeService(dir string) string {
	if dir == "" {
		return "Claude Code-credentials"
	}
	h := sha256.Sum256([]byte(dir))
	return "Claude Code-credentials-" + hex.EncodeToString(h[:])[:8]
}

func keychainUser() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "claude-code-user"
}

func claudeHome(dir string) string {
	if dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

type claudeCreds struct {
	raw     map[string]any // whole blob, rewritten as-is on refresh
	oauth   map[string]any
	fromKey bool
}

func (c claudeCreds) str(k string) string { s, _ := c.oauth[k].(string); return s }

func readClaudeCreds(dir string) (claudeCreds, error) {
	c := claudeCreds{fromKey: true}
	args := []string{"find-generic-password", "-s", claudeService(dir), "-w"}
	if dir != "" {
		args = append(args, "-a", keychainUser())
	}
	b, err := exec.Command("security", args...).Output()
	if err != nil {
		c.fromKey = false
		if b, err = os.ReadFile(filepath.Join(claudeHome(dir), ".credentials.json")); err != nil {
			return c, errors.New("not logged in")
		}
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(&c.raw); err != nil {
		return c, errors.New("unreadable credentials")
	}
	c.oauth, _ = c.raw["claudeAiOauth"].(map[string]any)
	if c.oauth == nil || c.str("accessToken") == "" {
		return c, errors.New("not logged in")
	}
	return c, nil
}

func (c claudeCreds) expired() bool {
	n, _ := c.oauth["expiresAt"].(json.Number)
	ms, _ := n.Int64()
	return ms > 0 && time.Now().Add(time.Minute).After(time.UnixMilli(ms))
}

func writeClaudeCreds(dir string, c claudeCreds) error {
	b, _ := json.Marshal(c.raw)
	if !c.fromKey {
		return os.WriteFile(filepath.Join(claudeHome(dir), ".credentials.json"), b, 0o600)
	}
	// `security -i` reads the command from stdin so the secret stays out of argv.
	cmd := exec.Command("security", "-i")
	cmd.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -U -a %s -s '%s' -X %s\n",
		keychainUser(), claudeService(dir), hex.EncodeToString(b)))
	return cmd.Run()
}

func refreshClaude(dir string, c claudeCreds) error {
	var r struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := postJSON("https://platform.claude.com/v1/oauth/token", map[string]string{
		"grant_type": "refresh_token", "refresh_token": c.str("refreshToken"), "client_id": claudeClientID,
	}, &r); err != nil || r.AccessToken == "" {
		return errRelogin
	}
	c.oauth["accessToken"] = r.AccessToken
	if r.RefreshToken != "" {
		c.oauth["refreshToken"] = r.RefreshToken
	}
	c.oauth["expiresAt"] = time.Now().Add(time.Duration(r.ExpiresIn) * time.Second).UnixMilli()
	return writeClaudeCreds(dir, c)
}

func claudeEmail(dir string) string {
	path := filepath.Join(claudeHome(dir), ".claude.json")
	if dir == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".claude.json")
	}
	var cfg struct {
		OAuthAccount struct {
			EmailAddress string `json:"emailAddress"`
		} `json:"oauthAccount"`
	}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &cfg)
	}
	return cfg.OAuthAccount.EmailAddress
}

func fetchClaude(a Account) (Service, error) {
	c, err := readClaudeCreds(a.Dir)
	if err != nil {
		return Service{}, err
	}
	if c.expired() {
		if !a.managed() {
			return Service{}, errors.New("login expired, run claude to refresh")
		}
		if err := refreshClaude(a.Dir, c); err != nil {
			return Service{}, err
		}
	}
	req, _ := http.NewRequest("GET", "https://api.anthropic.com/api/oauth/usage", nil)
	req.Header.Set("Authorization", "Bearer "+c.str("accessToken"))
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	var body json.RawMessage
	if err := getJSON(req, &body); err != nil {
		return Service{}, err
	}
	ls, err := parseClaude(body)
	return Service{Email: claudeEmail(a.Dir), Plan: c.str("subscriptionType"), Limits: ls}, err
}

func parseClaude(body []byte) ([]Limit, error) {
	var r struct {
		Limits []struct {
			Kind     string    `json:"kind"`
			Percent  float64   `json:"percent"`
			ResetsAt time.Time `json:"resets_at"`
			Scope    *struct {
				Model *struct {
					DisplayName string `json:"display_name"`
				} `json:"model"`
			} `json:"scope"`
		} `json:"limits"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	var ls []Limit
	for _, l := range r.Limits {
		name := l.Kind
		switch l.Kind {
		case "session":
			name = "5h"
		case "weekly_all":
			name = "Weekly"
		case "weekly_scoped":
			name = "Weekly"
			if l.Scope != nil && l.Scope.Model != nil && l.Scope.Model.DisplayName != "" {
				name += " " + l.Scope.Model.DisplayName
			}
		}
		ls = append(ls, Limit{name, l.Percent, l.ResetsAt})
	}
	return ls, nil
}

// ---- Codex ----

const codexClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

func codexAuthFile(dir string) string {
	return filepath.Join(homeDir(dir, "CODEX_HOME", ".codex"), "auth.json")
}

func jwtExpired(tok string) bool {
	var claims struct {
		Exp int64 `json:"exp"`
	}
	jwtClaims(tok, &claims)
	return claims.Exp > 0 && time.Now().Add(time.Minute).After(time.Unix(claims.Exp, 0))
}

// codexTokens is the refresh response; other fields (expires_in, …) are ignored.
type codexTokens struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

func (r codexTokens) apply(tokens map[string]any) {
	for k, v := range map[string]string{"id_token": r.IDToken, "access_token": r.AccessToken, "refresh_token": r.RefreshToken} {
		if v != "" {
			tokens[k] = v
		}
	}
}

func fetchCodex(a Account) (Service, error) {
	path := codexAuthFile(a.Dir)
	b, err := os.ReadFile(path)
	if err != nil {
		return Service{}, errors.New("not logged in")
	}
	var auth map[string]any
	if err := json.Unmarshal(b, &auth); err != nil {
		return Service{}, errors.New("unreadable auth.json")
	}
	tokens, _ := auth["tokens"].(map[string]any)
	str := func(k string) string { s, _ := tokens[k].(string); return s }
	if str("access_token") == "" {
		return Service{}, errors.New("not logged in with ChatGPT")
	}
	if jwtExpired(str("access_token")) {
		if !a.managed() {
			return Service{}, errors.New("login expired, run codex to refresh")
		}
		var r codexTokens
		if err := postJSON("https://auth.openai.com/oauth/token", map[string]string{
			"client_id": codexClientID, "grant_type": "refresh_token",
			"refresh_token": str("refresh_token"), "scope": "openid profile email",
		}, &r); err != nil || r.AccessToken == "" {
			return Service{}, errRelogin
		}
		r.apply(tokens)
		auth["last_refresh"] = time.Now().UTC().Format(time.RFC3339Nano)
		nb, _ := json.MarshalIndent(auth, "", "  ")
		if err := os.WriteFile(path, nb, 0o600); err != nil {
			return Service{}, err
		}
	}
	req, _ := http.NewRequest("GET", "https://chatgpt.com/backend-api/wham/usage", nil)
	req.Header.Set("Authorization", "Bearer "+str("access_token"))
	req.Header.Set("ChatGPT-Account-Id", str("account_id"))
	req.Header.Set("User-Agent", "codex-cli")
	var body json.RawMessage
	if err := getJSON(req, &body); err != nil {
		return Service{}, err
	}
	return parseWham(body)
}

func parseWham(body []byte) (Service, error) {
	type window struct {
		UsedPercent float64 `json:"used_percent"`
		WindowSecs  int     `json:"limit_window_seconds"`
		ResetAt     int64   `json:"reset_at"`
	}
	var r struct {
		Email     string `json:"email"`
		PlanType  string `json:"plan_type"`
		RateLimit struct {
			Primary   *window `json:"primary_window"`
			Secondary *window `json:"secondary_window"`
		} `json:"rate_limit"`
		Tickets *Tickets `json:"rate_limit_reset_credits"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return Service{}, err
	}
	s := Service{Email: r.Email, Plan: r.PlanType, Tickets: r.Tickets}
	for _, w := range []*window{r.RateLimit.Primary, r.RateLimit.Secondary} {
		if w != nil {
			s.Limits = append(s.Limits, Limit{windowName(w.WindowSecs / 60), w.UsedPercent, time.Unix(w.ResetAt, 0)})
		}
	}
	return s, nil
}

func windowName(minutes int) string {
	switch {
	case minutes == 10080:
		return "Weekly"
	case minutes%1440 == 0:
		return fmt.Sprintf("%dd", minutes/1440)
	case minutes%60 == 0:
		return fmt.Sprintf("%dh", minutes/60)
	}
	return fmt.Sprintf("%dm", minutes)
}
