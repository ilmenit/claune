package main

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"time"
)

// TrackerStats is what we distill from marginlab's daily series. Any field may
// be NaN if we couldn't determine it; callers treat NaN as "unknown".
type TrackerStats struct {
	TodayPassRate    float64 // latest daily passRate (%)
	ThirtyDayPassRate float64 // arithmetic mean of the last ≤30 daily passRates
	Baseline         float64 // NaN if the tracker hasn't published one
	TodayRuntimeSec  float64
	ThirtyDayRuntimeSec float64
	LatestDate       string
}

func newEmptyStats() TrackerStats {
	n := math.NaN()
	return TrackerStats{n, n, n, n, n, ""}
}

// Regexes are compiled once. The tracker embeds its data as inline JSON
// literals (Next.js-style server-rendered page), so we don't need a full
// HTML parser — match the shapes directly.
var (
	// {"date":"YYYY-MM-DD","passRate":NN.NN, ...}
	dailyPassRateRE = regexp.MustCompile(
		`\{"date":"(\d{4}-\d{2}-\d{2})","passRate":([0-9.]+)`)
	// {"date":"YYYY-MM-DD", ... ,"avgRuntimeSeconds":NN.NN, ...}
	dailyRuntimeRE = regexp.MustCompile(
		`\{"date":"(\d{4}-\d{2}-\d{2})"[^}]*"avgRuntimeSeconds":([0-9.]+)`)
	// Optional: "baselinePassRate":NN.NN — only present once the tracker has
	// collected enough baseline data for the current model.
	baselineRE = regexp.MustCompile(`"baselinePassRate":([0-9.]+)`)
)

// FetchTrackerStats downloads the tracker page and extracts pass-rate and
// runtime stats. On any failure it returns (emptyStats, err); callers should
// treat this as "signal unavailable" and not fail the launch.
func FetchTrackerStats(url string, timeout time.Duration) (TrackerStats, error) {
	stats := newEmptyStats()
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return stats, err
	}
	// Some hosts reject the default Go UA; a browser-ish UA sidesteps that
	// without pretending to be anything in particular.
	req.Header.Set("User-Agent", programName+"/"+version+" (+go-http)")
	resp, err := client.Do(req)
	if err != nil {
		return stats, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return stats, fmt.Errorf("tracker: HTTP %d", resp.StatusCode)
	}
	// Cap read at 4 MiB — the page is ~90 KiB, but we guard against runaway.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return stats, err
	}

	type dated struct {
		date  string
		value float64
	}
	dedupLatest := func(all [][]string) []dated {
		// Multiple matches per date can appear (the page embeds related
		// series). Keep the last occurrence per date, then sort ascending.
		m := map[string]float64{}
		for _, m2 := range all {
			if v, err := strconv.ParseFloat(m2[2], 64); err == nil {
				m[m2[1]] = v
			}
		}
		out := make([]dated, 0, len(m))
		for d, v := range m {
			out = append(out, dated{d, v})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].date < out[j].date })
		return out
	}

	pass := dedupLatest(dailyPassRateRE.FindAllStringSubmatch(string(body), -1))
	run := dedupLatest(dailyRuntimeRE.FindAllStringSubmatch(string(body), -1))

	if len(pass) > 0 {
		last := pass[len(pass)-1]
		stats.TodayPassRate = last.value
		stats.LatestDate = last.date
		start := len(pass) - 30
		if start < 0 {
			start = 0
		}
		var sum float64
		for _, d := range pass[start:] {
			sum += d.value
		}
		stats.ThirtyDayPassRate = sum / float64(len(pass[start:]))
	}
	if len(run) > 0 {
		stats.TodayRuntimeSec = run[len(run)-1].value
		start := len(run) - 30
		if start < 0 {
			start = 0
		}
		var sum float64
		for _, d := range run[start:] {
			sum += d.value
		}
		stats.ThirtyDayRuntimeSec = sum / float64(len(run[start:]))
	}
	if m := baselineRE.FindStringSubmatch(string(body)); len(m) == 2 {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			stats.Baseline = v
		}
	}
	return stats, nil
}
