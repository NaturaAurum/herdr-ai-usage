package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

func herdrBin() string {
	if b := os.Getenv("HERDR_BIN_PATH"); b != "" {
		return b
	}
	return "herdr"
}

// sidebarLabels are right-side panels this one stacks under.
var sidebarLabels = map[string]bool{"GitHub PR": true}

type paneInfo struct {
	PaneID      string `json:"pane_id"`
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
}

func herdrJSON(v any, args ...string) error {
	out, err := exec.Command(herdrBin(), args...).Output()
	if err != nil {
		return err
	}
	return json.Unmarshal(out, v)
}

// sidebarPane returns the bottom-most sidebar panel in focused's tab, or "".
func sidebarPane(focused string) string {
	var get struct {
		Result struct{ Pane paneInfo } `json:"result"`
	}
	if herdrJSON(&get, "pane", "get", focused) != nil {
		return ""
	}
	me := get.Result.Pane
	var list struct {
		Result struct{ Panes []paneInfo } `json:"result"`
	}
	if herdrJSON(&list, "pane", "list", "--workspace", me.WorkspaceID) != nil {
		return ""
	}
	found := ""
	for _, p := range list.Result.Panes {
		if p.TabID == me.TabID && sidebarLabels[p.Label] {
			found = p.PaneID
		}
	}
	return found
}

// openPanel stacks the panel under an existing sidebar panel (top/bottom
// split), or opens it to the right of the focused pane.
func openPanel() error {
	var ctx struct {
		FocusedPaneID string `json:"focused_pane_id"`
	}
	json.Unmarshal([]byte(os.Getenv("HERDR_PLUGIN_CONTEXT_JSON")), &ctx)
	focused := ctx.FocusedPaneID
	if focused == "" {
		focused = os.Getenv("HERDR_PANE_ID")
	}
	target, dir := focused, "right"
	if focused != "" {
		if p := sidebarPane(focused); p != "" {
			target, dir = p, "down"
		}
	}
	args := []string{"plugin", "pane", "open", "--plugin", "ai-usage", "--entrypoint", "panel",
		"--placement", "split", "--direction", dir, "--no-focus"}
	if target != "" {
		args = append(args, "--target-pane", target)
	}
	cmd := exec.Command(herdrBin(), args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

type rect struct{ X, Y, Width, Height int }

// fitWidth shrinks the panel to cols by moving the split it sits on the right
// side of. `pane resize --amount` shifts that split's ratio by the given delta.
func fitWidth(paneID string, cols int) {
	out, err := exec.Command(herdrBin(), "pane", "layout", "--pane", paneID).Output()
	if err != nil {
		return
	}
	var resp struct {
		Result struct {
			Layout struct {
				Panes []struct {
					PaneID string `json:"pane_id"`
					Rect   rect   `json:"rect"`
				} `json:"panes"`
				Splits []struct {
					Direction string `json:"direction"`
					Rect      rect   `json:"rect"`
				} `json:"splits"`
			} `json:"layout"`
		} `json:"result"`
	}
	if json.Unmarshal(out, &resp) != nil {
		return
	}
	var me *rect
	for _, p := range resp.Result.Layout.Panes {
		if p.PaneID == paneID {
			me = &p.Rect
		}
	}
	if me == nil || me.Width <= cols {
		return
	}
	// The parent is the narrowest right-split that shares our right edge and contains us.
	var parent *rect
	for _, s := range resp.Result.Layout.Splits {
		r := s.Rect
		if s.Direction == "right" && r.X < me.X && r.X+r.Width == me.X+me.Width &&
			r.Y <= me.Y && r.Y+r.Height >= me.Y+me.Height && (parent == nil || r.Width < parent.Width) {
			parent = &r
		}
	}
	if parent == nil {
		return
	}
	delta := float64(me.Width-cols) / float64(parent.Width)
	exec.Command(herdrBin(), "pane", "resize", "--pane", paneID, "--direction", "right",
		"--amount", fmt.Sprintf("%.3f", delta)).Run()
}
