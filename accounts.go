package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// Account is one login to show. Dir empty means the CLI's own default login
// (~/.claude, ~/.codex, …), which is read-only to us: refreshing it would rotate
// the token out from under the CLI. Managed accounts live in their own dir,
// created by `a` in the panel, and we keep their tokens fresh.
type Account struct {
	ID       string `json:"id"`
	Provider string `json:"provider"` // key of providers
	Dir      string `json:"dir,omitempty"`
}

func (a Account) managed() bool { return a.Dir != "" }

var cfgDir = func() string {
	if out, err := exec.Command(herdrBin(), "plugin", "config-dir", "ai-usage").Output(); err == nil {
		if d := strings.TrimSpace(string(out)); d != "" {
			return d
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "herdr", "plugins", "config", "ai-usage")
}()

func accountsFile() string { return filepath.Join(cfgDir, "accounts.json") }

// loadAccounts returns the default logins that exist, then managed ones.
func loadAccounts() []Account {
	var as []Account
	for _, id := range providerOrder {
		if providers[id].creds("") == nil {
			as = append(as, Account{ID: id + "-default", Provider: id})
		}
	}
	var managed []Account
	if b, err := os.ReadFile(accountsFile()); err == nil {
		json.Unmarshal(b, &managed)
	}
	return sortAccounts(append(as, managed...))
}

// sortAccounts groups accounts by service in providerOrder, keeping the
// default login first within a group.
func sortAccounts(as []Account) []Account {
	slices.SortStableFunc(as, func(a, b Account) int {
		return slices.Index(providerOrder, a.Provider) - slices.Index(providerOrder, b.Provider)
	})
	return as
}

func saveManaged(as []Account) error {
	var managed []Account
	for _, a := range as {
		if a.managed() {
			managed = append(managed, a)
		}
	}
	b, _ := json.MarshalIndent(managed, "", "  ")
	os.MkdirAll(cfgDir, 0o700)
	return os.WriteFile(accountsFile(), b, 0o600)
}

func newManaged(provider string) Account {
	b := make([]byte, 4)
	rand.Read(b)
	id := provider + "-" + hex.EncodeToString(b)
	return Account{ID: id, Provider: provider, Dir: filepath.Join(cfgDir, "accounts", id)}
}

// loginCmd runs the provider's own login against the account's dir, so the
// result is also usable from a shell, e.g. `CLAUDE_CONFIG_DIR=<dir> claude`.
func loginCmd(a Account) *exec.Cmd {
	os.MkdirAll(a.Dir, 0o700)
	return providers[a.Provider].login(a.Dir)
}

func loggedIn(a Account) bool { return providers[a.Provider].creds(a.Dir) == nil }

// saveToken stores a pasted token for providers without a CLI login.
func saveToken(a Account, tok string) error {
	os.MkdirAll(a.Dir, 0o700)
	return os.WriteFile(filepath.Join(a.Dir, "token"), []byte(strings.TrimSpace(tok)), 0o600)
}

func removeAccount(a Account) {
	if a.Provider == "claude" {
		exec.Command("security", "delete-generic-password", "-s", claudeService(a.Dir), "-a", keychainUser()).Run()
	}
	os.RemoveAll(a.Dir)
	os.Remove(cacheFile(a))
	os.Remove(cacheFile(a) + ".lock")
}
