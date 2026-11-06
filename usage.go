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
	"slices"
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
	Count   int         `json:"available_count"`
	Usable  int         `json:"applicable_available_count"`
	Expires []time.Time `json:"expires,omitempty"` // soonest first
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

// refreshClaude rotates the tokens the way Claude Code does (same scopes) and
// writes them back, so a running Claude Code session picks them up on its next
// keychain read.
func refreshClaude(dir string, c claudeCreds) error {
	var r struct {
		AccessToken    string `json:"access_token"`
		RefreshToken   string `json:"refresh_token"`
		ExpiresIn      int64  `json:"expires_in"`
		RefreshExpires int64  `json:"refresh_token_expires_in"`
	}
	body := map[string]string{
		"grant_type": "refresh_token", "refresh_token": c.str("refreshToken"), "client_id": claudeClientID,
	}
	if sc, ok := c.oauth["scopes"].([]any); ok {
		var ss []string
		for _, x := range sc {
			if x, ok := x.(string); ok {
				ss = append(ss, x)
			}
		}
		body["scope"] = strings.Join(ss, " ")
	}
	if err := postJSON("https://platform.claude.com/v1/oauth/token", body, &r); err != nil || r.AccessToken == "" {
		return errRelogin
	}
	now := time.Now()
	c.oauth["accessToken"] = r.AccessToken
	if r.RefreshToken != "" {
		c.oauth["refreshToken"] = r.RefreshToken
	}
	c.oauth["expiresAt"] = now.Add(time.Duration(r.ExpiresIn) * time.Second).UnixMilli()
	if r.RefreshExpires > 0 {
		c.oauth["refreshTokenExpiresAt"] = now.Add(time.Duration(r.RefreshExpires) * time.Second).UnixMilli()
	}
	if err := writeClaudeCreds(dir, c); err != nil {
		// The old refresh token is already spent, so this is a logout.
		return fmt.Errorf("refreshed but could not save Claude login (%v), run claude auth login", err)
	}
	return nil
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
		if err := refreshClaude(a.Dir, c); err != nil {
			// A Claude Code session may have rotated the token at the same time.
			if c2, e := readClaudeCreds(a.Dir); e == nil && !c2.expired() {
				c = c2
			} else {
				return Service{}, err
			}
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
	call := func(method, path string, in, v any) error {
		var body io.Reader
		if in != nil {
			b, _ := json.Marshal(in)
			body = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, "https://chatgpt.com/backend-api/wham/"+path, body)
		req.Header.Set("Authorization", "Bearer "+str("access_token"))
		req.Header.Set("ChatGPT-Account-Id", str("account_id"))
		req.Header.Set("User-Agent", "codex-cli")
		req.Header.Set("Content-Type", "application/json")
		return getJSON(req, v)
	}
	var body json.RawMessage
	if err := call("GET", "usage", nil, &body); err != nil {
		return Service{}, err
	}
	s, err := parseWham(body)
	// Best effort: the ticket list adds expiry dates and lets auto reset pick one.
	var rc json.RawMessage
	if err == nil && s.Tickets != nil && s.Tickets.Count > 0 && call("GET", "rate-limit-reset-credits", nil, &rc) == nil {
		now := time.Now()
		cs := availableCredits(rc, now)
		s.Tickets.Expires = ticketExpiry(cs)
		if id, why := pickTicket(s, cs, now); id != "" && autoReset(a, id, why, func(in, out any) error {
			return call("POST", "rate-limit-reset-credits/consume", in, out)
		}) {
			return fetchCodex(a) // show the reset; autoReset's pause stops a second spend
		}
	}
	return s, err
}

// Auto reset spends a ChatGPT reset ticket when a limit is all but used up,
// or when a ticket is about to expire unused.
const (
	autoResetUsed   = 99.8
	autoResetExpiry = 5 * time.Minute
	autoResetPause  = 10 * time.Minute
)

type credit struct {
	ID        string     `json:"id"`
	Status    string     `json:"status"`
	ExpiresAt *time.Time `json:"expires_at"`
}

func (c credit) expiry() time.Time {
	if c.ExpiresAt == nil {
		return time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return *c.ExpiresAt
}

// availableCredits returns the unspent, unexpired reset tickets, soonest expiry first.
func availableCredits(body []byte, now time.Time) []credit {
	var r struct{ Credits []credit }
	json.Unmarshal(body, &r)
	var out []credit
	for _, c := range r.Credits {
		if c.Status == "available" && c.expiry().After(now) {
			out = append(out, c)
		}
	}
	slices.SortStableFunc(out, func(a, b credit) int { return a.expiry().Compare(b.expiry()) })
	return out
}

func ticketExpiry(cs []credit) []time.Time {
	var out []time.Time
	for _, c := range cs {
		if c.ExpiresAt != nil {
			out = append(out, *c.ExpiresAt)
		}
	}
	return out
}

// pickTicket returns the ticket to spend now and why, or "" to keep them all.
func pickTicket(s Service, cs []credit, now time.Time) (id, why string) {
	if len(cs) == 0 {
		return "", ""
	}
	for _, l := range s.Limits {
		if l.Used >= autoResetUsed {
			return cs[0].ID, fmt.Sprintf("%s %.1f%% used", l.Name, l.Used)
		}
	}
	if left := cs[0].expiry().Sub(now); left <= autoResetExpiry {
		return cs[0].ID, fmt.Sprintf("expires in %s", left.Round(time.Second))
	}
	return "", ""
}

// autoReset spends ticket id and logs the outcome to autoreset.log. After a
// reset, or an error that may hide one, it pauses autoResetPause per account so
// a usage read that lags behind the reset can't burn a second ticket.
func autoReset(a Account, id, why string, consume func(in, out any) error) bool {
	mark := filepath.Join(cfgDir, "cache", a.ID+".autoreset")
	if fi, err := os.Stat(mark); err == nil && time.Since(fi.ModTime()) < autoResetPause {
		return false
	}
	var r struct {
		Code string `json:"code"`
	}
	err := consume(map[string]string{
		"redeem_request_id": fmt.Sprintf("herdr-ai-usage-%s-%d", id, time.Now().Unix()),
		"credit_id":         id,
	}, &r)
	outcome := r.Code
	if err != nil {
		outcome = err.Error()
	}
	paused := os.WriteFile(mark, []byte(outcome), 0o600) == nil
	if err == nil && r.Code != "reset" {
		os.Remove(mark) // nothing spent: try again on the next fetch
	}
	if f, ferr := os.OpenFile(filepath.Join(cfgDir, "autoreset.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); ferr == nil {
		fmt.Fprintf(f, "%s %s ticket %s (%s): %s\n", time.Now().Format(time.RFC3339), a.ID, id, why, outcome)
		f.Close()
	}
	// Without the pause file, fetchCodex's re-read could spend another ticket.
	return paused && err == nil && r.Code == "reset"
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
