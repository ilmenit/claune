package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// StatusReport tells us whether Anthropic's own dashboard is happy. Missing
// is true when we couldn't reach the status page or none of the configured
// components were found — in that case callers treat it as "no signal".
type StatusReport struct {
	Missing    bool
	Degraded   bool              // any tracked component not "operational"
	Components map[string]string // component name → status value
}

type statuspagePayload struct {
	Components []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"components"`
}

// FetchStatus queries Anthropic's Statuspage summary JSON. `watch` is the
// list of component names we care about (case-insensitive, substring match
// so users can put "Claude Code" or the full "Claude Code (...)" name).
func FetchStatus(url string, watch []string, timeout time.Duration) (StatusReport, error) {
	r := StatusReport{Missing: true, Components: map[string]string{}}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return r, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return r, fmt.Errorf("status: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return r, err
	}
	var p statuspagePayload
	if err := json.Unmarshal(body, &p); err != nil {
		return r, err
	}
	// For each requested component, find the first page component whose name
	// contains it (case-insensitive). Case matters on Statuspage — "Claude
	// Code" is exact, "Claude API (api.anthropic.com)" is the full name.
	anyMatched := false
	for _, w := range watch {
		needle := strings.ToLower(strings.TrimSpace(w))
		if needle == "" {
			continue
		}
		for _, c := range p.Components {
			if strings.Contains(strings.ToLower(c.Name), needle) {
				r.Components[c.Name] = c.Status
				anyMatched = true
				if c.Status != "operational" {
					r.Degraded = true
				}
				break
			}
		}
	}
	r.Missing = !anyMatched
	return r, nil
}
