package main

import (
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

const (
	refreshEvery = 60 * time.Second
	panelWidth   = 60 // same column as gh-view
)

var (
	dim    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	bold   = lipgloss.NewStyle().Bold(true)
	red    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	yellow = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	cursor = lipgloss.NewStyle().Foreground(lipgloss.Color("4")).Bold(true)
)

type mode int

const (
	normal mode = iota
	adding
	pasting
	deleting
)

type (
	tickMsg   struct{}
	usageMsg  map[string]Service
	loginDone struct {
		acct Account
		err  error
	}
)

type model struct {
	accounts      []Account
	services      map[string]Service
	sel           int
	mode          mode
	pending       Account // account being added by paste
	input         string
	flash         string
	loading       bool
	width, height int
}

func (m *model) fetch(force bool) tea.Cmd {
	m.loading = true
	accts := m.accounts
	return func() tea.Msg {
		out := usageMsg{}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, a := range accts {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s := usage(a, force)
				mu.Lock()
				out[a.ID] = s
				mu.Unlock()
			}()
		}
		wg.Wait()
		return out
	}
}

func tick() tea.Cmd {
	return tea.Tick(refreshEvery, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m *model) Init() tea.Cmd { return tea.Batch(m.fetch(false), tick()) }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tickMsg:
		return m, tea.Batch(m.fetch(false), tick())
	case usageMsg:
		m.loading = false
		m.services = msg
	case loginDone:
		if !loggedIn(msg.acct) {
			if msg.err != nil {
				removeAccount(msg.acct)
				m.flash = "login cancelled"
				return m, nil
			}
			// The CLI says it succeeded but we can't find the creds; keep the
			// account visible so the error shows and `d` cleans it up.
			m.flash = "login finished but credentials not found"
		}
		m.accounts = sortAccounts(append(m.accounts, msg.acct))
		m.sel = slices.Index(m.accounts, msg.acct)
		if err := saveManaged(m.accounts); err != nil {
			m.flash = err.Error()
		}
		return m, m.fetch(true)
	case tea.KeyMsg:
		m.flash = ""
		k := msg.String()
		if k == "ctrl+c" {
			return m, tea.Quit
		}
		switch m.mode {
		case adding:
			m.mode = normal
			for _, id := range providerOrder {
				if providers[id].key != k {
					continue
				}
				a := newManaged(id)
				if providers[id].login == nil {
					m.mode, m.pending, m.input = pasting, a, ""
					return m, nil
				}
				return m, tea.ExecProcess(loginCmd(a), func(err error) tea.Msg { return loginDone{a, err} })
			}
			return m, nil
		case pasting:
			switch msg.Type {
			case tea.KeyEsc:
				m.mode = normal
			case tea.KeyEnter:
				m.mode = normal
				if err := saveToken(m.pending, m.input); err != nil {
					m.flash = err.Error()
					return m, nil
				}
				return m, func() tea.Msg { return loginDone{m.pending, nil} }
			case tea.KeyBackspace:
				if r := []rune(m.input); len(r) > 0 {
					m.input = string(r[:len(r)-1])
				}
			case tea.KeyRunes, tea.KeySpace:
				m.input += string(msg.Runes)
			}
			return m, nil
		case deleting:
			m.mode = normal
			if k == "y" {
				removeAccount(m.accounts[m.sel])
				m.accounts = append(m.accounts[:m.sel], m.accounts[m.sel+1:]...)
				m.sel = max(min(m.sel, len(m.accounts)-1), 0)
				saveManaged(m.accounts)
			}
			return m, nil
		}
		switch k {
		case "q":
			return m, tea.Quit
		case "j", "down":
			m.sel = min(m.sel+1, max(len(m.accounts)-1, 0))
		case "k", "up":
			m.sel = max(m.sel-1, 0)
		case "a":
			m.mode = adding
		case "d":
			if m.sel < len(m.accounts) && m.accounts[m.sel].managed() {
				m.mode = deleting
			} else {
				m.flash = "default logins are managed by the CLI"
			}
		case "r":
			return m, m.fetch(true)
		}
	}
	return m, nil
}

func until(t time.Time) string {
	d := time.Until(t).Round(time.Minute)
	switch {
	case d <= 0:
		return "now"
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

// limitRow renders "Weekly  ██████░░░░  60% left  ↻ 3d4h". The bar is drawn
// in the service color until it nears the limit.
func limitRow(l Limit, w int, color lipgloss.Style) string {
	used := l.Used
	reset := "↻ " + until(l.ResetsAt)
	if !l.ResetsAt.IsZero() && l.ResetsAt.Before(time.Now()) {
		used, reset = 0, "↻ reset" // window rolled over since it was observed
	}
	left := math.Max(100-used, 0)
	style := color
	switch {
	case used >= 90:
		style = red
	case used >= 70:
		style = yellow
	}
	name := fmt.Sprintf("  %-13s", ansi.Truncate(l.Name, 13, "…"))
	pct := fmt.Sprintf(" %3.0f%% left ", left)
	barW := max(w-lipgloss.Width(name)-lipgloss.Width(pct)-lipgloss.Width(reset), 4)
	fill := min(max(int(math.Round(used/100*float64(barW))), 0), barW)
	bar := style.Render(strings.Repeat("█", fill)) + dim.Render(strings.Repeat("░", barW-fill))
	return name + bar + style.Render(pct) + dim.Render(reset)
}

// ticketRow renders "Resets       ●●   ⌛ 4d20h · 12d0h", aligned with
// limitRow; an expiry turns yellow within 3 days and red within 1.
func ticketRow(t Tickets, color lipgloss.Style) string {
	n := min(t.Count, 5)
	row := fmt.Sprintf("  %-13s", "Resets") + color.Render(strings.Repeat("●", n)) + strings.Repeat(" ", 5-n)
	if t.Count > 5 {
		row = fmt.Sprintf("  %-13s", "Resets") + color.Render(fmt.Sprintf("●×%-3d", t.Count))
	}
	if len(t.Expires) > 0 {
		var es []string
		for _, e := range t.Expires {
			style := dim
			switch d := time.Until(e); {
			case d < 24*time.Hour:
				style = red
			case d < 72*time.Hour:
				style = yellow
			}
			es = append(es, style.Render(until(e)))
		}
		row += dim.Render("⌛ ") + strings.Join(es, dim.Render(" · "))
	}
	if t.Usable > 0 {
		row += yellow.Render(fmt.Sprintf(" · %d usable now", t.Usable))
	}
	return row
}

func (m *model) View() string {
	w := max(m.width-3, 20) // 2 columns of gutter for the selection marker
	var rows []string
	add := func(selected bool, s string) {
		mark := "  "
		if selected {
			mark = cursor.Render("▌ ")
		}
		rows = append(rows, mark+ansi.Truncate(s, w, "…"))
	}
	if len(m.accounts) == 0 {
		add(false, dim.Render("No logins found. Press a to add one."))
	}
	for i, a := range m.accounts {
		s, ok := m.services[a.ID]
		p := providers[a.Provider]
		color := lipgloss.NewStyle().Foreground(lipgloss.Color(p.color))
		if i == 0 || m.accounts[i-1].Provider != a.Provider {
			if i > 0 {
				add(false, "")
			}
			add(false, color.Bold(true).Render("● "+p.title))
		}
		head := s.Email
		switch {
		case head != "":
		case a.managed():
			head = dim.Render(a.ID[strings.LastIndex(a.ID, "-")+1:])
		default:
			head = dim.Render("default")
		}
		if s.Plan != "" {
			// "self_serve_business_prolite" → "business prolite"
			plan := strings.ReplaceAll(strings.TrimPrefix(s.Plan, "self_serve_"), "_", " ")
			head += dim.Render(" · " + plan)
		}
		if !a.managed() {
			head += dim.Render(" · cli")
		}
		add(i == m.sel, head)
		switch {
		case !ok:
			add(false, dim.Render("  Loading…"))
		case s.Err == "rate limited":
			add(false, yellow.Render("  ⚠ rate limited · retry in "+until(s.RetryUntil)))
		case s.Err != "":
			add(false, red.Render("  ⚠ "+s.Err))
		case len(s.Limits) == 0:
			add(false, dim.Render("  no usage reported yet"))
		}
		for _, l := range s.Limits {
			add(false, limitRow(l, w, color))
		}
		if t := s.Tickets; t != nil && t.Count > 0 {
			add(false, ticketRow(*t, color))
		}
	}

	body := max(m.height-1, 1)
	if len(rows) > body {
		rows = rows[:body] // ponytail: no scrolling; add it if people link many accounts
	}
	for len(rows) < body {
		rows = append(rows, "")
	}
	help := "j/k · a add · d delete · r refresh · q close"
	switch {
	case m.mode == adding:
		var opts []string
		for _, id := range providerOrder {
			opts = append(opts, providers[id].key+" "+id)
		}
		help = "add: " + strings.Join(opts, " · ")
	case m.mode == pasting:
		// Masked: the token is a credential.
		help = "paste " + providers[m.pending.Provider].title + " session token, ⏎ save, esc cancel: " +
			strings.Repeat("•", min(len(m.input), 8))
	case m.mode == deleting:
		help = "delete this account? y / n"
	case m.flash != "":
		help = m.flash
	case m.loading:
		help = "⟳ " + help
	}
	return strings.Join(rows, "\n") + "\n" + dim.Render(ansi.Truncate(help, max(m.width, 1), "…"))
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "open" {
		if err := openPanel(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if self := os.Getenv("HERDR_PANE_ID"); self != "" {
		go fitWidth(self, panelWidth)
	}
	loadColors()
	m := &model{accounts: loadAccounts()}
	if _, err := tea.NewProgram(m, tea.WithAltScreen()).Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
