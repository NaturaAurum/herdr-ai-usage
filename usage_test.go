package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseClaude(t *testing.T) {
	s, err := parseClaude([]byte(`{"five_hour":{"utilization":1.0},"limits":[
		{"kind":"session","percent":12,"resets_at":"2026-10-25T05:40:00.288449+00:00","scope":null},
		{"kind":"weekly_all","percent":3,"resets_at":"2026-10-29T07:00:00+00:00","scope":null},
		{"kind":"weekly_scoped","percent":0,"resets_at":"2026-10-29T07:00:00+00:00","scope":{"model":{"id":null,"display_name":"Fable"}}}]}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ls := s.Limits
	if len(ls) != 3 || ls[0].Name != "5h" || ls[0].Used != 12 || ls[2].Name != "Weekly Fable" {
		t.Fatalf("got %+v", ls)
	}
	if want := time.Date(2026, 10, 25, 5, 40, 0, 288449000, time.UTC); !ls[0].ResetsAt.Equal(want) {
		t.Fatalf("resets_at %v", ls[0].ResetsAt)
	}
}

func TestClaudeTickets(t *testing.T) {
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		body string
	}{
		{"absent", `{}`},
		{"null", `{"cedar_ember":null}`},
		{"ineligible", `{"cedar_ember":{"eligible":false,"grants":[{"resets_left":1,"starts_at":"2026-11-01T00:00:00Z","ends_at":"2026-12-01T00:00:00Z","usable_now":true}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := parseClaude([]byte(tc.body), now)
			if err != nil || s.Tickets != nil {
				t.Fatalf("tickets = %+v, err = %v", s.Tickets, err)
			}
		})
	}
	s, err := parseClaude([]byte(`{"cedar_ember":{"eligible":true,"grants":[
		{"resets_left":2,"starts_at":"2026-11-01T00:00:00Z","ends_at":"2026-11-30T00:00:00Z","usable_now":false,"use_requires_limit":true},
		{"resets_left":3,"starts_at":"2026-11-10T00:00:00Z","ends_at":"2026-11-20T00:00:00Z","usable_now":true},
		{"resets_left":7,"starts_at":"2026-11-01T00:00:00Z","ends_at":"2026-11-10T00:00:00Z","usable_now":true},
		{"resets_left":7,"starts_at":"2026-11-11T00:00:00Z","ends_at":"2026-11-30T00:00:00Z","usable_now":true},
		{"resets_left":7,"starts_at":"2026-11-01T00:00:00Z","ends_at":"2026-11-30T00:00:00Z","paused":true,"usable_now":true},
		{"resets_left":0,"starts_at":"2026-11-01T00:00:00Z","ends_at":"2026-11-30T00:00:00Z","usable_now":true},
		{"resets_left":-1,"starts_at":"2026-11-01T00:00:00Z","ends_at":"2026-11-30T00:00:00Z","usable_now":true},
		{"resets_left":7,"ends_at":"2026-11-30T00:00:00Z","usable_now":true},
		{"resets_left":7,"starts_at":"2026-11-01T00:00:00Z","usable_now":true}
	]}}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if s.Tickets == nil || s.Tickets.Count != 5 || s.Tickets.Usable != 3 {
		t.Fatalf("tickets = %+v", s.Tickets)
	}
	if es := s.Tickets.Expires; len(es) != 2 || es[0].Day() != 20 || es[1].Day() != 30 {
		t.Fatalf("expiry = %v", es)
	}
}

func TestParseWham(t *testing.T) {
	s, err := parseWham([]byte(`{"email":"a@b.c","plan_type":"pro","rate_limit":{
		"primary_window":{"used_percent":40,"limit_window_seconds":604800,"reset_at":1793383594},
		"secondary_window":{"used_percent":7,"limit_window_seconds":18000,"reset_at":1793300000}},
		"rate_limit_reset_credits":{"available_count":2,"applicable_available_count":0}}`))
	if err != nil {
		t.Fatal(err)
	}
	if s.Email != "a@b.c" || s.Plan != "pro" || len(s.Limits) != 2 ||
		s.Limits[0].Name != "Weekly" || s.Limits[0].Used != 40 || s.Limits[1].Name != "5h" ||
		s.Limits[0].ResetsAt.Unix() != 1793383594 || s.Tickets == nil || s.Tickets.Count != 2 {
		t.Fatalf("got %+v", s)
	}
}

func TestClaudeService(t *testing.T) {
	// sha256("/tmp/x")[:8], as Claude Code names the keychain item.
	if got := claudeService("/tmp/x"); got != "Claude Code-credentials-2e56aa36" {
		t.Fatal(got)
	}
	if got := claudeService(""); got != "Claude Code-credentials" {
		t.Fatal(got)
	}
}

// A 429 must keep the last limits on screen and hold off until Retry-After.
func TestCached429(t *testing.T) {
	cfgDir = t.TempDir()
	a := Account{ID: "t", Provider: "claude"}
	ok := func() (Service, error) { return Service{Limits: []Limit{{Name: "5h", Used: 10}}}, nil }
	limited := func() (Service, error) {
		return Service{}, &httpError{http.StatusTooManyRequests, 10 * time.Minute}
	}
	if s := cachedUsage(a, false, ok); len(s.Limits) != 1 {
		t.Fatalf("first fetch %+v", s)
	}
	if s := cachedUsage(a, false, limited); s.FetchedAt.IsZero() || len(s.Limits) != 1 {
		t.Fatal("TTL not honored")
	}
	s := cachedUsage(a, true, limited)
	if len(s.Limits) != 1 || s.Err != "rate limited" || time.Until(s.RetryUntil) < 9*time.Minute {
		t.Fatalf("after 429 %+v", s)
	}
	if s := cachedUsage(a, true, func() (Service, error) { return Service{}, errors.New("called") }); s.Err != "rate limited" {
		t.Fatal("Retry-After not honored")
	}
}

func TestCodexRefreshDecode(t *testing.T) {
	var r codexTokens
	if err := json.Unmarshal([]byte(`{"access_token":"a","refresh_token":"b","token_type":"Bearer","expires_in":864000}`), &r); err != nil {
		t.Fatal(err)
	}
	tokens := map[string]any{"id_token": "old", "access_token": "old", "account_id": "acc"}
	r.apply(tokens)
	if tokens["access_token"] != "a" || tokens["refresh_token"] != "b" || tokens["id_token"] != "old" || tokens["account_id"] != "acc" {
		t.Fatalf("%v", tokens)
	}
}

func TestParseOpencode(t *testing.T) {
	s, err := parseOpencode([]byte(`{"usage":{"rolling":{"percent":0.5,"resetsAt":"2026-08-12T02:00:00.000Z"},"weekly":{"percent":30,"resetsAt":"2026-08-15T00:00:00.000Z"}}}`))
	if err != nil || len(s.Limits) != 2 || s.Limits[0].Name != "5h" || s.Limits[0].Used != 0.5 || s.Limits[1].Name != "Weekly" || s.Limits[0].ResetsAt.IsZero() {
		t.Fatalf("%+v %v", s, err)
	}
}

func TestParseCursor(t *testing.T) {
	s, _ := parseCursor([]byte(`{"membershipType":"pro","billingCycleEnd":"2026-11-01T00:00:00.000Z","individualUsage":{"plan":{"used":500,"limit":2000}}}`))
	if s.Plan != "pro" || len(s.Limits) != 1 || s.Limits[0].Used != 25 || s.Limits[0].ResetsAt.IsZero() {
		t.Fatalf("%+v", s)
	}
	s, _ = parseCursor([]byte(`{"individualUsage":{"plan":{"used":500,"limit":2000,"totalPercentUsed":0.36}}}`))
	if s.Limits[0].Used != 0.36 {
		t.Fatalf("%+v", s)
	}
}

func TestCursorSessionToken(t *testing.T) {
	// header.{"sub":"auth0|user_abc"}.sig
	jwt := "e30.eyJzdWIiOiJhdXRoMHx1c2VyX2FiYyJ9.x"
	if got, _ := cursorSessionToken(jwt); got != "user_abc%3A%3A"+jwt {
		t.Fatal(got)
	}
	if got, _ := cursorSessionToken("WorkosCursorSessionToken=user_abc%3A%3A" + jwt); got != "user_abc%3A%3A"+jwt {
		t.Fatal(got)
	}
	// "\"tok\"" as UTF-16LE
	if got := decodeDBValue("220074006F006B002200"); got != "tok" {
		t.Fatal(got)
	}
}

func TestParseGrok(t *testing.T) {
	s, _ := parseGrok([]byte(`{"config":{"creditUsagePercent":42,"currentPeriod":{"start":"2026-10-20T00:00:00Z","end":"2026-10-27T00:00:00Z"}}}`))
	if len(s.Limits) != 1 || s.Limits[0].Used != 42 || s.Limits[0].Name != "Weekly" {
		t.Fatalf("%+v", s)
	}
	s, _ = parseGrok([]byte(`{"config":{"onDemandUsed":{"val":"250"},"onDemandCap":{"val":"1000"},"billingPeriodEnd":"2026-11-01T00:00:00Z"}}`))
	if len(s.Limits) != 1 || s.Limits[0].Used != 25 || s.Limits[0].Name != "Credits" {
		t.Fatalf("%+v", s)
	}
	if s, _ = parseGrok([]byte(`{"config":{}}`)); len(s.Limits) != 0 {
		t.Fatal("missing percent must be unknown, not 0%")
	}
}

func TestTicketExpiry(t *testing.T) {
	now := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	got := ticketExpiry(availableCredits([]byte(`{"credits":[
		{"status":"available","expires_at":"2026-11-06T23:31:23.834369Z"},
		{"status":"redeemed","expires_at":"2026-10-26T00:00:00Z"},
		{"status":"available","expires_at":"2026-10-20T00:00:00Z"},
		{"status":"available","expires_at":"2026-10-29T19:34:17.072133Z"}]}`), now))
	if len(got) != 2 || got[0].Day() != 29 || got[1].Day() != 6 {
		t.Fatalf("got %v", got)
	}
}

func TestPickTicket(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { t := now.Add(d); return &t }
	cs := []credit{{ID: "soon", ExpiresAt: at(time.Hour)}, {ID: "late", ExpiresAt: at(48 * time.Hour)}}
	low := Service{Limits: []Limit{{Name: "5h", Used: 99.7}, {Name: "Weekly", Used: 40}}}
	full := Service{Limits: []Limit{{Name: "5h", Used: 20}, {Name: "Weekly", Used: 99.8}}}
	for _, c := range []struct {
		s    Service
		cs   []credit
		want string
	}{
		{low, cs, ""},
		{full, cs, "soon"},
		{full, nil, ""},
		{low, []credit{{ID: "x", ExpiresAt: at(5 * time.Minute)}}, "x"},
		{low, []credit{{ID: "x", ExpiresAt: at(6 * time.Minute)}}, ""},
		{low, []credit{{ID: "forever"}}, ""},
	} {
		if id, why := pickTicket(c.s, c.cs, now); id != c.want {
			t.Errorf("pickTicket(%v) = %q (%s), want %q", c.s.Limits, id, why, c.want)
		}
	}
}

// Never talks to ChatGPT: consume is a fake, so no real ticket is spent.
func TestAutoResetPause(t *testing.T) {
	cfgDir = t.TempDir()
	os.MkdirAll(filepath.Join(cfgDir, "cache"), 0o700)
	a := Account{ID: "codex-test"}
	calls := 0
	consume := func(code string) func(in, out any) error {
		return func(in, out any) error {
			calls++
			return json.Unmarshal([]byte(`{"code":"`+code+`","windows_reset":1}`), out)
		}
	}
	if autoReset(a, "c1", "test", consume("nothing_to_reset")) || calls != 1 {
		t.Fatal("nothing_to_reset counted as a reset")
	}
	if !autoReset(a, "c1", "test", consume("reset")) || calls != 2 {
		t.Fatal("reset not reported")
	}
	if autoReset(a, "c2", "test", consume("reset")) || calls != 2 {
		t.Fatal("second ticket spent inside the pause")
	}
}
