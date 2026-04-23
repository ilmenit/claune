package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Config captures user preferences that outlive a single invocation. It is
// stored at $XDG_CONFIG_HOME/claune/config.json (falling back to
// ~/.config/claune/config.json). Every field has a sensible default, so a
// missing or partial file is never fatal.
type Config struct {
	// Enabled turns the whole "react to Claude status" behaviour on/off. When
	// false, claune behaves like a plain terminal launcher driven only by
	// the static config values.
	Enabled bool `json:"enabled"`

	// Command is the program to launch inside the new terminal.
	Command string `json:"command"`

	// Terminal forces a specific emulator (Linux / Windows). Empty means
	// "first one that's installed, in claune's preferred order".
	Terminal string `json:"terminal"`

	// Bold makes every glyph render bold — the existing Comic Sans tradition.
	Bold bool `json:"bold"`

	// TrackerURL is the marginlab.ai page we scrape for pass rate + runtime.
	TrackerURL string `json:"tracker_url"`

	// StatusURL is Anthropic's Statuspage summary JSON.
	StatusURL string `json:"status_url"`

	// StatusComponents are the component names we look for on the status
	// page. If any is not "operational", we flag the environment as degraded.
	StatusComponents []string `json:"status_components"`

	// HTTPTimeoutSeconds bounds each external fetch; on timeout we treat the
	// signal as unavailable (never a hard failure).
	HTTPTimeoutSeconds int `json:"http_timeout_seconds"`

	// NormalFont is used when comic mode is NOT triggered. "monospace" is a
	// fontconfig alias that resolves to the user's default monospace on any
	// Linux terminal; macOS / Windows substitute their own equivalent.
	NormalFont string `json:"normal_font"`

	// FontSearchPaths are directories scanned (in order) for the files listed
	// in FallbackFonts. Entries support ~ and $VAR expansion. Directories
	// that don't exist are skipped silently.
	FontSearchPaths []string `json:"font_search_paths"`

	// FallbackFonts are tried, in order, when comic mode triggers. Each
	// entry lists candidate filenames (we'll use the first that exists on
	// any search path) and the family name the terminal should request.
	FallbackFonts []FontEntry `json:"fallback_fonts"`

	// PassRateDropPct is the threshold (in percentage points) at which a drop
	// from baseline — or, if baseline is unavailable, from the 30-day pass
	// rate — flips us into comic mode.
	PassRateDropPct float64 `json:"pass_rate_drop_pct"`

	// BgNormal / BgDegraded / Fg are terminal colors as #RRGGBB.
	BgNormal   string `json:"bg_normal"`
	BgDegraded string `json:"bg_degraded"`
	Fg         string `json:"fg"`

	// BaseSize is the reference font size (points). Actual size may be
	// scaled by the runtime signal.
	BaseSize int `json:"base_size"`

	// RuntimeScale* controls the "funny" mapping from today's avg runtime to
	// font size. Slower Claude → larger font (more dramatic). Scale factor
	// is clamped to [RuntimeMinScale, RuntimeMaxScale].
	RuntimeScaleEnabled bool    `json:"runtime_scale_enabled"`
	RuntimeMinScale     float64 `json:"runtime_min_scale"`
	RuntimeMaxScale     float64 `json:"runtime_max_scale"`

}

// FontEntry is "the family named X, which may be on disk at any of these
// filenames". Leave Files empty to rely on the system font registry only.
type FontEntry struct {
	Family string   `json:"family"`
	Files  []string `json:"files,omitempty"`
}

func defaultConfig() Config {
	return Config{
		Enabled:            true,
		Command:            "claude",
		Terminal:           "",
		Bold:               true,
		TrackerURL:         "https://marginlab.ai/trackers/claude-code/",
		StatusURL:          "https://status.claude.com/api/v2/summary.json",
		StatusComponents:   []string{"Claude Code", "Claude API (api.anthropic.com)"},
		HTTPTimeoutSeconds: 5,
		NormalFont:         defaultNormalFont,
		FontSearchPaths:    defaultFontSearchPaths(),
		FallbackFonts: []FontEntry{
			{
				Family: "Comic Sans MS",
				Files: []string{
					"Comic Sans MS.ttf",
					"ComicSansMS.ttf",
					"comic.ttf",
					"Comic Sans MS Bold.ttf",
					"comicbd.ttf",
				},
			},
			{
				Family: "Caveat",
				Files: []string{
					"Caveat.ttf",
					"Caveat-Regular.ttf",
					"Caveat-VariableFont_wght.ttf",
					"Caveat/Caveat-VariableFont_wght.ttf",
					"Caveat/static/Caveat-Regular.ttf",
				},
			},
		},
		PassRateDropPct:     10,
		BgNormal:            "#000000",
		BgDegraded:          "#4A1D3A",
		Fg:                  "#FFE8F3",
		BaseSize:            defaultSize,
		RuntimeScaleEnabled: true,
		RuntimeMinScale:     0.85,
		RuntimeMaxScale:     1.6,
	}
}

// defaultFontSearchPaths returns a cross-platform list of places to look for
// TTF/OTF files. Relative entries are resolved against (in order) the config
// directory, CWD, and the binary's directory when the font is actually
// looked up — see resolveFontPath.
func defaultFontSearchPaths() []string {
	common := []string{
		"fonts",
		"~/.fonts",
		"~/.local/share/fonts",
	}
	switch runtime.GOOS {
	case "linux":
		return append(common,
			"/usr/share/fonts",
			"/usr/local/share/fonts",
		)
	case "darwin":
		return append(common,
			"~/Library/Fonts",
			"/Library/Fonts",
			"/System/Library/Fonts",
			"/System/Library/Fonts/Supplemental",
		)
	case "windows":
		return append(common,
			"%LOCALAPPDATA%/Microsoft/Windows/Fonts",
			"%WINDIR%/Fonts",
		)
	}
	return common
}

// configPath returns the absolute path to the config file, creating parent
// directories if needed. It respects XDG_CONFIG_HOME.
func configPath() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	dir := filepath.Join(base, programName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// loadOrCreateConfig reads the user's config, writing out defaults on first
// run. Unknown keys are ignored; missing keys inherit from defaults.
func loadOrCreateConfig() (Config, string, error) {
	path, err := configPath()
	if err != nil {
		return defaultConfig(), "", err
	}
	return readConfigAt(path, true)
}

// readConfigAt loads config from an explicit path. When createIfMissing is
// true, a defaults file is written on first read.
func readConfigAt(path string, createIfMissing bool) (Config, string, error) {
	cfg := defaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) || !createIfMissing {
			return cfg, path, err
		}
		out, _ := json.MarshalIndent(cfg, "", "  ")
		if werr := os.WriteFile(path, out, 0o644); werr != nil {
			return cfg, path, werr
		}
		infof("wrote default config to %s", path)
		return cfg, path, nil
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return defaultConfig(), path, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, path, nil
}

// expandPath replaces leading ~ with the user's home directory and expands
// %VAR% (Windows-style) as well as $VAR (Unix-style) occurrences.
func expandPath(p string) string {
	// $VAR / ${VAR}
	p = os.ExpandEnv(p)
	// %VAR% (for cross-platform configs that use Windows-style vars)
	for {
		i := strings.Index(p, "%")
		if i < 0 {
			break
		}
		j := strings.Index(p[i+1:], "%")
		if j < 0 {
			break
		}
		name := p[i+1 : i+1+j]
		val := os.Getenv(name)
		// Even if the env var is empty, consume the %VAR% pattern so we
		// don't loop forever.
		p = p[:i] + val + p[i+1+j+1:]
	}
	// Leading ~
	if strings.HasPrefix(p, "~/") || p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			if p == "~" {
				p = home
			} else {
				p = filepath.Join(home, p[2:])
			}
		}
	}
	return p
}

// resolveFontPath walks every configured search path looking for `name`. If
// `name` is absolute, it is used as-is. Missing directories are skipped
// without complaint.
func resolveFontPath(name string, searchPaths []string, cfgDir string) (string, bool) {
	if name == "" {
		return "", false
	}
	name = expandPath(name)
	if filepath.IsAbs(name) {
		if _, err := os.Stat(name); err == nil {
			return name, true
		}
		return "", false
	}

	// Built-in default roots (tried after user-configured ones): config dir,
	// CWD, binary directory. These let "fonts/Caveat.ttf" work out of the
	// box for anyone running from the repo or a portable bundle.
	extras := []string{}
	if cfgDir != "" {
		extras = append(extras, cfgDir)
	}
	if cwd, err := os.Getwd(); err == nil {
		extras = append(extras, cwd)
	}
	if exe, err := os.Executable(); err == nil {
		extras = append(extras, filepath.Dir(exe))
	}

	tryDir := func(d string) (string, bool) {
		if d == "" {
			return "", false
		}
		d = expandPath(d)
		// Skip entries that don't resolve to a real directory.
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			return "", false
		}
		candidate := filepath.Join(d, name)
		if _, err := os.Stat(candidate); err == nil {
			abs, _ := filepath.Abs(candidate)
			return abs, true
		}
		return "", false
	}

	for _, d := range searchPaths {
		if p, ok := tryDir(d); ok {
			return p, true
		}
	}
	for _, d := range extras {
		if p, ok := tryDir(d); ok {
			return p, true
		}
	}
	return "", false
}
