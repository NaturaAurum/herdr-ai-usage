package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func antigravityCLI() (string, error) {
	if path, err := exec.LookPath("agy"); err == nil {
		return path, nil
	}
	home, _ := os.UserHomeDir()
	for _, path := range []string{filepath.Join(home, ".local", "bin", "agy"), "/opt/homebrew/bin/agy", "/usr/local/bin/agy"} {
		if path, err := exec.LookPath(path); err == nil {
			return path, nil
		}
	}
	return "", errors.New("Antigravity CLI (agy) not found")
}

func fetchAntigravity(_ Account) (Service, error) {
	path, err := antigravityCLI()
	if err != nil {
		return Service{}, err
	}
	dir, err := os.MkdirTemp("", "ai-usage-agy-")
	if err != nil {
		return Service{}, err
	}
	defer os.RemoveAll(dir)

	// Run outside the workspace and prevent passive refreshes from opening sign-in.
	browser := filepath.Join(dir, "open")
	if err := os.Symlink("/usr/bin/false", browser); err != nil {
		return Service{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	run := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, path, args...)
		cmd.Dir = dir
		cmd.Env = append(cmd.Environ(), "AGY_CLI_DISABLE_AUTO_UPDATE=true", "BROWSER="+browser,
			"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		cmd.WaitDelay = time.Second
		return cmd.Output()
	}

	version, err := run("--version")
	if err != nil {
		return Service{}, errors.New("could not check Antigravity CLI version")
	}
	// Older versions could treat /usage as a model prompt in print mode.
	var major, minor, patch int
	if n, _ := fmt.Sscanf(string(version), "%d.%d.%d", &major, &minor, &patch); n != 3 ||
		major < 1 || major == 1 && (minor < 1 || minor == 1 && patch < 11) {
		return Service{}, errors.New("Antigravity CLI 1.1.11 or later required")
	}
	body, err := run("-p", "/usage", "--output-format", "json")
	if err != nil {
		if ctx.Err() != nil {
			return Service{}, errors.New("Antigravity quota timed out; check sign-in with agy")
		}
		return Service{}, errors.New("could not read Antigravity quota; check sign-in with agy")
	}
	return parseAntigravity(body)
}

func parseAntigravity(body []byte) (Service, error) {
	var r struct {
		Status  string
		Command struct {
			Name string
			Data struct {
				Groups []struct {
					Name    string
					Buckets []struct {
						Window   string
						Disabled bool
						Left     *float64  `json:"remaining_fraction"`
						Reset    time.Time `json:"reset_time"`
					}
				}
			}
		}
	}
	if err := json.Unmarshal(body, &r); err != nil || r.Status != "SUCCESS" || r.Command.Name != "usage" {
		return Service{}, errors.New("no Antigravity usage report; check sign-in with agy")
	}
	var s Service
	for _, group := range r.Command.Data.Groups {
		name := strings.TrimSpace(group.Name)
		switch name {
		case "":
			continue
		case "Gemini Models":
			name = "Gemini"
		case "Claude and GPT models":
			name = "Claude/GPT"
		}
		for _, b := range group.Buckets {
			if b.Disabled || b.Left == nil || *b.Left < 0 || *b.Left > 1 {
				continue
			}
			window := b.Window
			switch window {
			case "weekly":
				window = "1w"
			case "5h":
			default:
				continue
			}
			s.Limits = append(s.Limits, Limit{name + " " + window, 100 - *b.Left*100, b.Reset})
		}
	}
	if len(s.Limits) == 0 {
		return Service{}, errors.New("no measured Antigravity quota")
	}
	return s, nil
}
