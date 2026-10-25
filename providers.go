package main

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// provider is one AI service. creds checks a login exists in dir ("" = the
// CLI's default location). login runs the service's own CLI login against a
// managed dir; nil means the account is added by pasting a token instead.
type provider struct {
	title string
	key   string // key in the panel's add prompt
	color string // lipgloss color; overridable in colors.json
	ttl   time.Duration
	creds func(dir string) error
	fetch func(Account) (Service, error)
	login func(dir string) *exec.Cmd
}

var providerOrder = []string{"claude", "codex", "opencode", "cursor", "grok"}

var providers = map[string]provider{
	// The Claude usage endpoint is rate limited per token, and other tools
	// (CodexBar, quota plugins) poll it with the same token.
	"claude": {title: "Claude", key: "c", color: "#D97757", ttl: 5 * time.Minute,
		creds: func(dir string) error { _, err := readClaudeCreds(dir); return err },
		fetch: fetchClaude,
		login: func(dir string) *exec.Cmd {
			return envCmd("CLAUDE_CONFIG_DIR="+dir, "claude", "auth", "login")
		}},
	"codex": {title: "Codex", key: "x", color: "#10A37F", ttl: time.Minute,
		creds: fileExists(codexAuthFile),
		fetch: fetchCodex,
		login: func(dir string) *exec.Cmd {
			return envCmd("CODEX_HOME="+dir, "codex", "login", "-c", `cli_auth_credentials_store="file"`)
		}},
	"opencode": {title: "OpenCode Go", key: "o", color: "#FAB283", ttl: time.Minute,
		creds: func(dir string) error { _, err := opencodeKey(dir); return err },
		fetch: fetchOpencode,
		login: func(dir string) *exec.Cmd {
			return envCmd("XDG_DATA_HOME="+dir, "opencode", "auth", "login")
		}},
	"cursor": {title: "Cursor", key: "u", color: "#8B9CF7", ttl: 5 * time.Minute,
		creds: func(dir string) error { _, err := cursorCookie(dir); return err },
		fetch: fetchCursor},
	"grok": {title: "Grok", key: "g", color: "#E5E5E5", ttl: 5 * time.Minute,
		creds: func(dir string) error { _, err := grokAuth(dir); return err },
		fetch: fetchGrok,
		login: func(dir string) *exec.Cmd { return envCmd("GROK_HOME="+dir, "grok", "login") }},
}

// loadColors applies <cfgDir>/colors.json, e.g. {"claude": "#ff8800", "codex": "6"}
// (hex or ANSI 0-255).
func loadColors() {
	var cs map[string]string
	if b, err := os.ReadFile(filepath.Join(cfgDir, "colors.json")); err == nil {
		json.Unmarshal(b, &cs)
	}
	for id, c := range cs {
		if p, ok := providers[id]; ok && c != "" {
			p.color = c
			providers[id] = p
		}
	}
}

func envCmd(env, name string, args ...string) *exec.Cmd {
	c := exec.Command(name, args...)
	c.Env = append(os.Environ(), env)
	return c
}

func fileExists(path func(string) string) func(string) error {
	return func(dir string) error {
		_, err := os.Stat(path(dir))
		return err
	}
}

func homeDir(dir, env string, def ...string) string {
	if dir != "" {
		return dir
	}
	if d := os.Getenv(env); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(append([]string{home}, def...)...)
}

func jwtClaims(tok string, v any) {
	if parts := strings.Split(tok, "."); len(parts) == 3 {
		b, _ := base64.RawURLEncoding.DecodeString(parts[1])
		json.Unmarshal(b, v)
	}
}

func parseFloat(raw json.RawMessage) (float64, bool) {
	f, err := strconv.ParseFloat(strings.Trim(string(raw), `"`), 64)
	return f, err == nil
}

// ---- OpenCode Go ----

func opencodeKey(dir string) (string, error) {
	var auth map[string]struct {
		Key string `json:"key"`
	}
	b, err := os.ReadFile(filepath.Join(homeDir(dir, "XDG_DATA_HOME", ".local", "share"), "opencode", "auth.json"))
	if err != nil || json.Unmarshal(b, &auth) != nil || auth["opencode-go"].Key == "" {
		return "", errors.New("no OpenCode Go API key")
	}
	return auth["opencode-go"].Key, nil
}

func fetchOpencode(a Account) (Service, error) {
	key, err := opencodeKey(a.Dir)
	if err != nil {
		return Service{}, err
	}
	req, _ := http.NewRequest("GET", "https://opencode.ai/zen/go/v1/usage", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	var body json.RawMessage
	if err := getJSON(req, &body); err != nil {
		if errors.Is(err, errRelogin) {
			err = errors.New("API key expired, create a new one in OpenCode Console")
		}
		return Service{}, err
	}
	return parseOpencode(body)
}

func parseOpencode(body []byte) (Service, error) {
	type window struct {
		Percent  float64   `json:"percent"`
		ResetsAt time.Time `json:"resetsAt"`
	}
	var r struct {
		Usage struct {
			Rolling, Weekly, Monthly *window
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return Service{}, err
	}
	var s Service
	for _, w := range []struct {
		name string
		w    *window
	}{{"5h", r.Usage.Rolling}, {"Weekly", r.Usage.Weekly}, {"Monthly", r.Usage.Monthly}} {
		if w.w != nil {
			s.Limits = append(s.Limits, Limit{w.name, w.w.Percent, w.w.ResetsAt})
		}
	}
	return s, nil
}

// ---- Cursor ----

// cursorCookie returns the WorkosCursorSessionToken value. Default: the token
// Cursor.app keeps in its state DB. Managed: the token pasted into the panel.
func cursorCookie(dir string) (string, error) {
	var tok string
	if dir != "" {
		b, err := os.ReadFile(filepath.Join(dir, "token"))
		if err != nil {
			return "", errors.New("no token")
		}
		tok = strings.TrimSpace(string(b))
	} else {
		home, _ := os.UserHomeDir()
		db := filepath.Join(home, "Library", "Application Support", "Cursor", "User", "globalStorage", "state.vscdb")
		if _, err := os.Stat(db); err != nil {
			return "", errors.New("Cursor.app not signed in")
		}
		out, err := exec.Command("sqlite3", "-readonly", db,
			"SELECT hex(value) FROM ItemTable WHERE key='cursorAuth/accessToken'").Output()
		if err != nil {
			return "", errors.New("unreadable Cursor state DB")
		}
		tok = decodeDBValue(strings.TrimSpace(string(out)))
	}
	return cursorSessionToken(tok)
}

// decodeDBValue undoes VS Code's storage: values may be UTF-16LE blobs.
func decodeDBValue(hexStr string) string {
	b, _ := hex.DecodeString(hexStr)
	if len(b) >= 2 && len(b)%2 == 0 && b[1] == 0 {
		u := make([]uint16, len(b)/2)
		for i := range u {
			u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
		}
		return strings.Trim(string(utf16.Decode(u)), `"`)
	}
	return strings.Trim(string(b), `"`)
}

// cursorSessionToken accepts a cookie value ("userId::jwt", maybe
// URL-encoded) or a bare JWT, and returns the cookie value.
func cursorSessionToken(tok string) (string, error) {
	tok = strings.TrimPrefix(tok, "WorkosCursorSessionToken=")
	if dec, err := url.QueryUnescape(tok); err == nil {
		tok = dec
	}
	if tok == "" {
		return "", errors.New("not signed in")
	}
	if strings.Contains(tok, "::") {
		return url.QueryEscape(tok), nil
	}
	var c struct {
		Sub string `json:"sub"`
	}
	jwtClaims(tok, &c)
	if c.Sub == "" {
		return "", errors.New("unrecognized Cursor token")
	}
	user := c.Sub[strings.LastIndex(c.Sub, "|")+1:]
	return url.QueryEscape(user + "::" + tok), nil
}

func fetchCursor(a Account) (Service, error) {
	cookie, err := cursorCookie(a.Dir)
	if err != nil {
		return Service{}, err
	}
	dec, _ := url.QueryUnescape(cookie)
	jwt := dec[strings.Index(dec, "::")+2:]
	if jwtExpired(jwt) {
		return Service{}, errors.New("login expired, sign in to Cursor again")
	}
	var claims struct {
		Email string `json:"email"`
	}
	jwtClaims(jwt, &claims)
	req, _ := http.NewRequest("GET", "https://cursor.com/api/usage-summary", nil)
	req.Header.Set("Cookie", "WorkosCursorSessionToken="+cookie)
	req.Header.Set("Accept", "application/json")
	var body json.RawMessage
	if err := getJSON(req, &body); err != nil {
		return Service{}, err
	}
	s, err := parseCursor(body)
	s.Email = claims.Email
	return s, err
}

func parseCursor(body []byte) (Service, error) {
	var r struct {
		MembershipType  string    `json:"membershipType"`
		BillingCycleEnd time.Time `json:"billingCycleEnd"`
		IndividualUsage struct {
			Plan struct {
				Used             float64  `json:"used"`
				Limit            float64  `json:"limit"`
				TotalPercentUsed *float64 `json:"totalPercentUsed"`
			} `json:"plan"`
		} `json:"individualUsage"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return Service{}, err
	}
	s := Service{Plan: r.MembershipType}
	p := r.IndividualUsage.Plan
	switch {
	case p.TotalPercentUsed != nil:
		s.Limits = []Limit{{"Monthly", *p.TotalPercentUsed, r.BillingCycleEnd}}
	case p.Limit > 0:
		s.Limits = []Limit{{"Monthly", p.Used / p.Limit * 100, r.BillingCycleEnd}}
	}
	// ponytail: legacy 500-request plans (/api/usage) not shown; add if someone is on one
	return s, nil
}

// ---- Grok ----

type grokEntry struct {
	Key       string    `json:"key"`
	ExpiresAt time.Time `json:"expires_at"`
	Email     string    `json:"email"`
}

func grokAuth(dir string) (grokEntry, error) {
	var auth map[string]grokEntry
	b, err := os.ReadFile(filepath.Join(homeDir(dir, "GROK_HOME", ".grok"), "auth.json"))
	if err != nil || json.Unmarshal(b, &auth) != nil {
		return grokEntry{}, errors.New("not logged in")
	}
	var fallback grokEntry
	for k, e := range auth {
		switch {
		case strings.HasPrefix(k, "https://auth.x.ai::") && e.Key != "":
			return e, nil
		case strings.HasPrefix(k, "https://accounts.x.ai/sign-in") && e.Key != "":
			fallback = e
		}
	}
	if fallback.Key == "" {
		return fallback, errors.New("not logged in")
	}
	return fallback, nil
}

func fetchGrok(a Account) (Service, error) {
	e, err := grokAuth(a.Dir)
	if err != nil {
		return Service{}, err
	}
	// The grok CLI refreshes its own token (~7 days); we only read it.
	if !e.ExpiresAt.IsZero() && e.ExpiresAt.Before(time.Now()) {
		return Service{}, errors.New("login expired, run grok to refresh")
	}
	req, _ := http.NewRequest("GET", "https://cli-chat-proxy.grok.com/v1/billing?format=credits", nil)
	req.Header.Set("Authorization", "Bearer "+e.Key)
	req.Header.Set("x-xai-token-auth", "xai-grok-cli")
	req.Header.Set("Accept", "application/json")
	var body json.RawMessage
	if err := getJSON(req, &body); err != nil {
		return Service{}, err
	}
	s, err := parseGrok(body)
	s.Email = e.Email
	return s, err
}

func parseGrok(body []byte) (Service, error) {
	type val struct {
		Val json.RawMessage `json:"val"`
	}
	var r struct {
		Config struct {
			CreditUsagePercent json.RawMessage `json:"creditUsagePercent"`
			OnDemandUsed       val             `json:"onDemandUsed"`
			OnDemandCap        val             `json:"onDemandCap"`
			CurrentPeriod      struct {
				Start time.Time `json:"start"`
				End   time.Time `json:"end"`
			} `json:"currentPeriod"`
			BillingPeriodEnd time.Time `json:"billingPeriodEnd"`
		} `json:"config"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return Service{}, err
	}
	c := r.Config
	used, ok := parseFloat(c.CreditUsagePercent)
	if !ok {
		u, ok1 := parseFloat(c.OnDemandUsed.Val)
		cp, ok2 := parseFloat(c.OnDemandCap.Val)
		if !ok1 || !ok2 || cp == 0 {
			return Service{}, nil // not reported yet this period; unknown, not 0%
		}
		used = u / cp * 100
	}
	end, name := c.CurrentPeriod.End, "Credits"
	if end.IsZero() {
		end = c.BillingPeriodEnd
	} else if !c.CurrentPeriod.Start.IsZero() {
		name = windowName(int(end.Sub(c.CurrentPeriod.Start).Minutes()))
	}
	return Service{Limits: []Limit{{name, used, end}}}, nil
}
