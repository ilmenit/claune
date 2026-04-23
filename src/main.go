// claune: launch a new terminal window with Comic Sans MS (if available)
// and start the `claude` CLI inside it. Best-effort across Linux, macOS, Windows.
package main

import (
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	programName = "claune"
	version     = "0.8.0"
	defaultSize = 14
	// Fallback when no config / no auto-signal picks a font. "monospace" is
	// a fontconfig alias every Linux terminal understands; macOS Terminal and
	// Windows Terminal substitute their own monospace equivalent.
	defaultNormalFont = "monospace"
)

// fontName is the font family to apply; overridable via -font. bold is whether
// text should render bold. bgHex is the terminal background color as #RRGGBB.
// All are read (never written) from the launchers.
var (
	fontName = defaultNormalFont
	bold     = true
	bgHex    = "#000000"
	fgHex    = "#FFFFFF"
)

// Exit codes — distinct per failure class so scripts can branch.
const (
	exitOK       = 0
	exitUsage    = 2
	exitNoTarget = 3 // the command we'd run (e.g. `claude`) isn't on PATH
	exitNoTerm   = 4 // no supported terminal emulator on this host
	exitLaunch   = 5 // spawn failed or the launcher exited immediately
)

// options are what the launcher paths ultimately need. With the slim CLI in
// place, nearly every field is derived from config.json — the CLI only
// selects WHICH config, whether to hit the network, and how to simulate
// degraded states for local testing.
type options struct {
	command  string
	font     string
	fontFile string
	size     int
	bold     bool
	bg       string
	fg       string
	terminal string
	skipFont bool

	dryRun      bool
	showVersion bool
	noAuto      bool
	configPath  string
	testMode    string // "", "is-stupid", "is-down", "both"
}

func main() {
	opts := parseFlags()

	if opts.showVersion {
		fmt.Printf("%s %s (%s/%s, %s)\n", programName, version, runtime.GOOS, runtime.GOARCH, runtime.Version())
		return
	}

	cfg, cfgPath := loadConfigWithOverride(opts.configPath)
	applyConfigAndSignals(&opts, cfg, cfgPath)

	// Validate after config+signals have populated everything. Any bad value
	// at this point came from config.json (or is a bug), so point the user
	// at the file they'd edit to fix it.
	if opts.size < 6 || opts.size > 72 {
		fail(exitUsage, "font size %d is outside 6..72 (check %s)", opts.size, cfgPath)
	}
	if _, _, _, err := parseHexColor(opts.bg); err != nil {
		fail(exitUsage, "invalid background %q: %v (check %s)", opts.bg, err, cfgPath)
	}
	if _, _, _, err := parseHexColor(opts.fg); err != nil {
		fail(exitUsage, "invalid foreground %q: %v (check %s)", opts.fg, err, cfgPath)
	}
	fontName = opts.font
	bold = opts.bold
	bgHex = normalizeHexColor(opts.bg)
	fgHex = normalizeHexColor(opts.fg)

	// Resolve the target up-front so failures surface here instead of flashing
	// and closing inside the new terminal.
	cmdPath, err := exec.LookPath(opts.command)
	if err != nil {
		fail(exitNoTarget,
			"cannot find %q on PATH: %v\n  → install it, or change \"command\" in %s",
			opts.command, err, cfgPath)
	}
	infof("target: %s → %s", opts.command, cmdPath)

	// Font file resolution: applyConfigAndSignals may have pre-filled
	// opts.fontFile (comic-mode pick). We just need to register it with the
	// child terminal's fontconfig so the family resolves.
	var fontEnv []string
	if opts.fontFile != "" {
		abs, err := filepath.Abs(opts.fontFile)
		if err == nil {
			if _, err := os.Stat(abs); err == nil {
				infof("font-file: %s", abs)
				if env, err := registerFontFile(abs); err != nil {
					warnf("could not register font file: %v", err)
				} else {
					fontEnv = env
					opts.skipFont = true
				}
			}
		}
	}

	if !opts.skipFont {
		if ok, detail := fontAvailable(); ok {
			infof("font %q available (%s)", fontName, detail)
		} else {
			warnf("could not confirm %q is installed (%s)", fontName, detail)
			warnf("continuing anyway — terminals usually substitute a default font")
		}
	}

	launchCmd := cmdPath

	var launchErr error
	switch runtime.GOOS {
	case "linux":
		launchErr = launchLinux(opts, launchCmd, fontEnv)
	case "darwin":
		launchErr = launchDarwin(opts, launchCmd)
	case "windows":
		launchErr = launchWindows(opts, launchCmd)
	default:
		fail(exitNoTerm, "unsupported OS: %s", runtime.GOOS)
	}
	if launchErr != nil {
		fail(exitLaunch, "%v", launchErr)
	}
	infof("launched; new terminal should run %q in %s on %s", opts.command, fontName, bgHex)
}

func parseFlags() options {
	var opts options
	flag.StringVar(&opts.configPath, "config", "", "path to config.json (default ~/.config/claune/config.json)")
	flag.StringVar(&opts.testMode, "test", "",
		`simulate a Claude health signal and skip network fetches; one of "is-stupid" (pass-rate drop), "is-down" (status outage), or "both"`)
	flag.BoolVar(&opts.dryRun, "dry-run", false, "print the launch plan without executing")
	flag.BoolVar(&opts.noAuto, "no-auto", false, "skip live fetches; just use config values")
	flag.BoolVar(&opts.showVersion, "version", false, "print version and exit")
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprintf(out, "%s %s — launch a terminal that reacts to Claude's pass-rate + status.\n\n",
			programName, version)
		fmt.Fprintf(out, "Usage:\n  %s [flags]\n\nFlags:\n", programName)
		flag.PrintDefaults()
		fmt.Fprintf(out, "\nEverything else (fonts, colors, command, terminal, bold, size, URLs, ...)\n")
		fmt.Fprintf(out, "lives in config.json. Edit it to change behaviour.\n\n")
		fmt.Fprintf(out, "Examples:\n")
		fmt.Fprintf(out, "  %s                            # normal run: check Claude's health, launch accordingly\n", programName)
		fmt.Fprintf(out, "  %s -no-auto                   # skip network, use config defaults only\n", programName)
		fmt.Fprintf(out, "  %s -test is-stupid            # force comic-mode font (pretend pass rate dropped)\n", programName)
		fmt.Fprintf(out, "  %s -test is-down              # force pink background (pretend status page is red)\n", programName)
		fmt.Fprintf(out, "  %s -test both -dry-run        # both, and print the command without launching\n", programName)
		fmt.Fprintf(out, "  %s -config ./my-claune.json   # use an alternate config file\n", programName)
	}
	flag.Parse()
	switch opts.testMode {
	case "", "is-stupid", "is-down", "both":
	default:
		fail(exitUsage, "invalid -test %q (expected is-stupid | is-down | both)", opts.testMode)
	}
	return opts
}

// parseHexColor accepts "#RRGGBB" or "RRGGBB" and returns (r,g,b) in 0..255.
func parseHexColor(s string) (r, g, b uint8, err error) {
	h := strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(h) != 6 {
		return 0, 0, 0, fmt.Errorf("need 6 hex digits")
	}
	var v uint32
	if _, err := fmt.Sscanf(h, "%x", &v); err != nil {
		return 0, 0, 0, err
	}
	return uint8(v >> 16), uint8(v >> 8), uint8(v), nil
}

func normalizeHexColor(s string) string {
	r, g, b, _ := parseHexColor(s)
	return fmt.Sprintf("#%02X%02X%02X", r, g, b)
}

// ---------- Config + live signals ----------

func loadConfigWithOverride(override string) (Config, string) {
	if override != "" {
		cfg, path, err := readConfigAt(expandPath(override), false)
		if err != nil {
			warnf("config %s: %v (using defaults)", path, err)
		}
		return cfg, path
	}
	cfg, path, err := loadOrCreateConfig()
	if err != nil {
		warnf("config: %v (using defaults)", err)
	}
	return cfg, path
}

// applyConfigAndSignals fills opts from config.json, then either runs the
// live signal fetch (normal path) or forces a simulated state when -test is
// set. Nothing here ever fails the launch on a network error.
func applyConfigAndSignals(opts *options, cfg Config, cfgPath string) {
	cfgDir := filepath.Dir(cfgPath)

	// 1. Copy static settings from config into opts.
	opts.command = nonEmpty(cfg.Command, "claude")
	opts.terminal = cfg.Terminal
	opts.bold = cfg.Bold
	opts.bg = nonEmpty(cfg.BgNormal, "#000000")
	opts.fg = nonEmpty(cfg.Fg, "#FFFFFF")
	opts.size = cfg.BaseSize
	if opts.size == 0 {
		opts.size = defaultSize
	}
	opts.font = nonEmpty(cfg.NormalFont, defaultNormalFont)

	// 2. Decide whether to consult the network.
	degraded := false
	comicMode := false
	runtimeRatio := 1.0

	switch opts.testMode {
	case "is-stupid":
		comicMode = true
		infof("test mode is-stupid: forcing comic font, skipping network")
	case "is-down":
		degraded = true
		infof("test mode is-down: forcing pink background, skipping network")
	case "both":
		comicMode, degraded = true, true
		infof("test mode both: forcing comic + pink, skipping network")
	case "":
		if opts.noAuto || !cfg.Enabled {
			infof("auto-mode off: using config values as-is")
		} else {
			comicMode, degraded, runtimeRatio = fetchSignals(cfg)
		}
	}

	// 3. Apply decisions. Explicit CLI flags no longer exist for these, so
	// signal-derived values always win.
	if degraded {
		opts.bg = nonEmpty(cfg.BgDegraded, "#4A1D3A")
	}
	if cfg.RuntimeScaleEnabled && runtimeRatio != 1.0 {
		scaled := int(float64(opts.size)*runtimeRatio + 0.5)
		if scaled < 6 {
			scaled = 6
		} else if scaled > 72 {
			scaled = 72
		}
		if scaled != opts.size {
			infof("runtime scale %.2fx → size %d (was %d)", runtimeRatio, scaled, opts.size)
			opts.size = scaled
		}
	}
	if comicMode {
		chosen := pickComicFont(cfg, cfgDir)
		if chosen.family != "" {
			opts.font = chosen.family
			opts.fontFile = chosen.file
			if chosen.file != "" {
				infof("comic font: %q from %s", chosen.family, chosen.file)
			} else {
				infof("comic font: %q (system-installed)", chosen.family)
			}
		} else {
			warnf("comic mode triggered but no fallback font could be resolved — keeping %q", opts.font)
		}
	}
}

// fetchSignals runs the tracker + status probes in parallel and returns
// (comicMode, degraded, runtimeRatio). Any error on either side disables
// only that signal.
func fetchSignals(cfg Config) (bool, bool, float64) {
	timeout := time.Duration(cfg.HTTPTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	type trackerResult struct {
		stats TrackerStats
		err   error
	}
	type statusResult struct {
		rep StatusReport
		err error
	}
	trackerCh := make(chan trackerResult, 1)
	statusCh := make(chan statusResult, 1)
	go func() {
		s, err := FetchTrackerStats(cfg.TrackerURL, timeout)
		trackerCh <- trackerResult{s, err}
	}()
	go func() {
		r, err := FetchStatus(cfg.StatusURL, cfg.StatusComponents, timeout)
		statusCh <- statusResult{r, err}
	}()
	tr := <-trackerCh
	st := <-statusCh

	degraded := false
	if st.err != nil {
		warnf("status: %v (not adjusting background)", st.err)
	} else if st.rep.Missing {
		warnf("status: no matching components found (not adjusting background)")
	} else {
		for name, s := range st.rep.Components {
			infof("status: %s = %s", name, s)
		}
		degraded = st.rep.Degraded
	}

	comicMode := false
	ratio := 1.0
	if tr.err != nil {
		warnf("tracker: %v (not adjusting font/size)", tr.err)
		return comicMode, degraded, ratio
	}

	reference := math.NaN()
	refLabel := ""
	if !math.IsNaN(tr.stats.Baseline) {
		reference, refLabel = tr.stats.Baseline, "baseline"
	} else if !math.IsNaN(tr.stats.ThirtyDayPassRate) {
		reference, refLabel = tr.stats.ThirtyDayPassRate, "30-day mean"
	}
	infof("tracker (%s): today=%.1f%% %s=%.1f%% avg-runtime=%.0fs",
		tr.stats.LatestDate, tr.stats.TodayPassRate, refLabel, reference, tr.stats.TodayRuntimeSec)

	if !math.IsNaN(tr.stats.TodayPassRate) && !math.IsNaN(reference) {
		drop := reference - tr.stats.TodayPassRate
		if drop >= cfg.PassRateDropPct {
			comicMode = true
			infof("pass rate dropped %.1fpp vs %s (threshold %.1f) → comic mode",
				drop, refLabel, cfg.PassRateDropPct)
		}
	}
	if cfg.RuntimeScaleEnabled &&
		!math.IsNaN(tr.stats.TodayRuntimeSec) && !math.IsNaN(tr.stats.ThirtyDayRuntimeSec) &&
		tr.stats.ThirtyDayRuntimeSec > 0 {
		r := tr.stats.TodayRuntimeSec / tr.stats.ThirtyDayRuntimeSec
		if r < cfg.RuntimeMinScale {
			r = cfg.RuntimeMinScale
		} else if r > cfg.RuntimeMaxScale {
			r = cfg.RuntimeMaxScale
		}
		ratio = r
	}
	return comicMode, degraded, ratio
}

type pickedFont struct{ family, file string }

// pickComicFont walks the user's fallback list. For each entry it tries
// every configured file candidate across every search path, then falls back
// to a fontconfig probe for the family name. The first match wins.
func pickComicFont(cfg Config, cfgDir string) pickedFont {
	for _, e := range cfg.FallbackFonts {
		for _, name := range e.Files {
			if path, ok := resolveFontPath(name, cfg.FontSearchPaths, cfgDir); ok {
				family := e.Family
				if family == "" {
					if q, _ := queryFontFamily(path); q != "" {
						family = q
					}
				}
				return pickedFont{family, path}
			}
		}
		if e.Family != "" && familyInstalled(e.Family) {
			return pickedFont{e.Family, ""}
		}
	}
	return pickedFont{}
}

func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func familyInstalled(family string) bool {
	if runtime.GOOS != "linux" {
		// Best-effort on macOS/Windows — the fontAvailable probe already runs
		// later for the chosen family; return true so we at least try.
		return true
	}
	fc, err := exec.LookPath("fc-list")
	if err != nil {
		return false
	}
	out, err := exec.Command(fc, ":family").Output()
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(out)), strings.ToLower(family))
}

// ---------- Logging helpers ----------

func infof(f string, a ...any) { fmt.Fprintf(os.Stderr, "["+programName+"] "+f+"\n", a...) }
func warnf(f string, a ...any) { fmt.Fprintf(os.Stderr, "["+programName+"] warning: "+f+"\n", a...) }
func fail(code int, f string, a ...any) {
	fmt.Fprintf(os.Stderr, "["+programName+"] error: "+f+"\n", a...)
	os.Exit(code)
}

// ---------- Font availability detection ----------

func fontAvailable() (bool, string) {
	switch runtime.GOOS {
	case "linux":
		fc, err := exec.LookPath("fc-list")
		if err != nil {
			return false, "fc-list not found; cannot probe"
		}
		out, err := exec.Command(fc, ":family").Output()
		if err != nil {
			return false, "fc-list failed: " + err.Error()
		}
		if strings.Contains(strings.ToLower(string(out)), strings.ToLower(fontName)) {
			return true, "matched via fc-list"
		}
		return false, "fc-list did not list the font (install ttf-mscorefonts-installer, " +
			"msttcore-fonts-installer, or copy comic.ttf into ~/.fonts)"
	case "darwin":
		candidates := []string{
			"/Library/Fonts/Comic Sans MS.ttf",
			"/System/Library/Fonts/Supplemental/Comic Sans MS.ttf",
		}
		if home, err := os.UserHomeDir(); err == nil {
			candidates = append(candidates, filepath.Join(home, "Library/Fonts/Comic Sans MS.ttf"))
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				return true, c
			}
		}
		return false, "no Comic Sans MS.ttf in system or user Fonts directories"
	case "windows":
		winDir := os.Getenv("WINDIR")
		if winDir == "" {
			winDir = `C:\Windows`
		}
		for _, name := range []string{"comic.ttf", "comicbd.ttf"} {
			p := filepath.Join(winDir, "Fonts", name)
			if _, err := os.Stat(p); err == nil {
				return true, p
			}
		}
		return false, `no comic.ttf in %WINDIR%\Fonts`
	}
	return false, "unknown OS"
}

// ---------- Linux ----------

type linuxTerminal struct {
	name     string
	supports bool // set at detection time
	// build returns the command to exec plus extra env vars (KEY=VAL).
	// If build returns nil, the terminal is not usable right now.
	build func(cmdPath string, size int) (cmd *exec.Cmd, extraEnv []string, note string)
}

func linuxTerminals() []*linuxTerminal {
	return []*linuxTerminal{
		{name: "alacritty", build: func(c string, s int) (*exec.Cmd, []string, string) {
			args := []string{
				"-o", fmt.Sprintf(`font.normal.family=%q`, fontName),
				"-o", fmt.Sprintf("font.size=%d", s),
				"-o", fmt.Sprintf(`colors.primary.background="%s"`, bgHex),
				"-o", fmt.Sprintf(`colors.primary.foreground="%s"`, fgHex),
			}
			if bold {
				// Force the "normal" face to resolve to the Bold face so all
				// text renders bold (not just text the app explicitly bolds).
				args = append(args,
					"-o", `font.normal.style="Bold"`,
					"-o", `font.italic.style="Bold Italic"`)
			}
			args = append(args, "-e", c)
			return exec.Command("alacritty", args...), nil, ""
		}},
		{name: "kitty", build: func(c string, s int) (*exec.Cmd, []string, string) {
			// kitty takes a single family string; append "Bold" so fontconfig
			// resolves it to the Bold face.
			family := fontName
			if bold {
				family = fontName + " Bold"
			}
			return exec.Command("kitty",
				"-o", "font_family="+family,
				"-o", fmt.Sprintf("font_size=%d", s),
				"-o", "background="+bgHex,
				"-o", "foreground="+fgHex,
				c), nil, ""
		}},
		{name: "wezterm", build: func(c string, s int) (*exec.Cmd, []string, string) {
			// `--config KEY=VALUE` is a global option that must precede the subcommand,
			// and each setting needs its own flag. VALUE is a Lua expression.
			fontExpr := fmt.Sprintf(`wezterm.font("%s")`, fontName)
			if bold {
				fontExpr = fmt.Sprintf(`wezterm.font("%s", {weight="Bold"})`, fontName)
			}
			return exec.Command("wezterm",
				"--config", "font="+fontExpr,
				"--config", fmt.Sprintf("font_size=%d", s),
				"--config", fmt.Sprintf(`colors={background="%s",foreground="%s"}`, bgHex, fgHex),
				"start", "--always-new-process", "--", c), nil, ""
		}},
		{name: "xfce4-terminal", build: func(c string, s int) (*exec.Cmd, []string, string) {
			// xfce4-terminal uses a Pango font string: "Family [Style] Size".
			pango := fmt.Sprintf("%s %d", fontName, s)
			if bold {
				pango = fmt.Sprintf("%s Bold %d", fontName, s)
			}
			return exec.Command("xfce4-terminal",
				"--font="+pango,
				"--color-bg="+bgHex,
				"--color-fg="+fgHex,
				"--command="+c,
				"--hold"), nil, ""
		}},
		{name: "konsole", build: func(c string, s int) (*exec.Cmd, []string, string) {
			env, err := prepareKonsoleProfile(s)
			if err != nil {
				warnf("konsole profile setup failed: %v", err)
				return nil, nil, ""
			}
			return exec.Command("konsole", "--profile", "Claune", "-e", c),
				env,
				"profile written to a temp XDG_DATA_HOME (auto-discarded on reboot)"
		}},
		{name: "gnome-terminal", build: func(c string, s int) (*exec.Cmd, []string, string) {
			// gnome-terminal exposes no CLI flag for font family. We refuse to
			// mutate the user's default profile via dconf, so we launch
			// unstyled and tell the user plainly.
			return exec.Command("gnome-terminal", "--", c), nil,
				"gnome-terminal has no CLI font/background flags; using its default profile"
		}},
		{name: "xterm", build: func(c string, s int) (*exec.Cmd, []string, string) {
			fa := fontName
			if bold {
				fa = fontName + ":bold" // Xft style selector
			}
			return exec.Command("xterm",
				"-fa", fa, "-fs", fmt.Sprintf("%d", s),
				"-bg", bgHex, "-fg", fgHex,
				"-e", c), nil, ""
		}},
	}
}

func launchLinux(opts options, cmdPath string, fontEnv []string) error {
	terms := linuxTerminals()

	// If user forced one, narrow the list to just that.
	if opts.terminal != "" {
		var picked *linuxTerminal
		var names []string
		for _, t := range terms {
			names = append(names, t.name)
			if t.name == opts.terminal {
				picked = t
			}
		}
		if picked == nil {
			return fmt.Errorf("unknown -terminal %q; supported: %s",
				opts.terminal, strings.Join(names, ", "))
		}
		terms = []*linuxTerminal{picked}
	}

	var tried []string
	for _, t := range terms {
		if _, err := exec.LookPath(t.name); err != nil {
			tried = append(tried, t.name+" (not installed)")
			continue
		}
		cmd, extraEnv, note := t.build(cmdPath, opts.size)
		if cmd == nil {
			tried = append(tried, t.name+" (setup failed)")
			continue
		}
		mergedEnv := append([]string{}, extraEnv...)
		mergedEnv = append(mergedEnv, fontEnv...)
		if len(mergedEnv) > 0 {
			cmd.Env = append(os.Environ(), mergedEnv...)
		}
		infof("using terminal: %s", t.name)
		if note != "" {
			warnf("%s: %s", t.name, note)
		}
		if opts.dryRun {
			if len(mergedEnv) > 0 {
				fmt.Println("# env:", strings.Join(mergedEnv, " "))
			}
			fmt.Println(shellQuote(cmd.Args))
			return nil
		}
		return spawnDetached(cmd)
	}
	return fmt.Errorf("no supported terminal emulator found on PATH. Tried: %s",
		strings.Join(tried, ", "))
}

// prepareKonsoleProfile writes an ephemeral profile and returns env vars that
// make konsole discover it, without touching ~/.local/share/konsole.
func prepareKonsoleProfile(size int) ([]string, error) {
	tmp, err := os.MkdirTemp("", "claune-konsole-*")
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(tmp, "konsole")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	// Qt font weight: 50 = Normal, 75 = Bold.
	weight := 50
	if bold {
		weight = 75
	}
	br, bg_, bb, _ := parseHexColor(bgHex)
	fr, fg_, fb, _ := parseHexColor(fgHex)
	// Set Background / Foreground and their Intense/Faint variants so text
	// stays readable in every rendering mode (konsole's default foreground is
	// black, which is invisible on a dark pink background).
	scheme := fmt.Sprintf(`[Background]
Color=%d,%d,%d

[BackgroundIntense]
Color=%d,%d,%d

[BackgroundFaint]
Color=%d,%d,%d

[Foreground]
Color=%d,%d,%d

[ForegroundIntense]
Color=%d,%d,%d

[ForegroundFaint]
Color=%d,%d,%d

[General]
Description=Claune
Opacity=1
`, br, bg_, bb, br, bg_, bb, br, bg_, bb, fr, fg_, fb, fr, fg_, fb, fr, fg_, fb)
	schemePath := filepath.Join(dir, "Claune.colorscheme")
	if err := os.WriteFile(schemePath, []byte(scheme), 0o644); err != nil {
		return nil, err
	}
	profile := fmt.Sprintf(`[Appearance]
ColorScheme=Claune
Font=%s,%d,-1,5,%d,0,0,0,0,0

[General]
Name=Claune
Parent=FALLBACK/
`, fontName, size, weight)
	path := filepath.Join(dir, "Claune.profile")
	if err := os.WriteFile(path, []byte(profile), 0o644); err != nil {
		return nil, err
	}
	// Prepend our tmp to XDG_DATA_DIRS so konsole finds Claune.profile
	// while still loading system color schemes from /usr/share.
	existing := os.Getenv("XDG_DATA_DIRS")
	if existing == "" {
		existing = "/usr/local/share:/usr/share"
	}
	return []string{"XDG_DATA_DIRS=" + tmp + ":" + existing}, nil
}

// ---------- macOS ----------

func launchDarwin(opts options, cmdPath string) error {
	// Escape for AppleScript string literals.
	esc := func(s string) string {
		s = strings.ReplaceAll(s, `\`, `\\`)
		s = strings.ReplaceAll(s, `"`, `\"`)
		return s
	}
	// `do script` returns the new tab; set font properties on that reference
	// rather than "selected tab of front window" (which races with the new tab
	// becoming selected).
	// Terminal.app's "font name" property takes the family face name. To get
	// bold rendering for *all* text we ask for the Bold face by name; if the
	// Bold face isn't installed the AppleScript catches the error and falls
	// back to regular.
	faceName := fontName
	if opts.bold {
		faceName = fontName + " Bold"
	}
	// Terminal.app wants RGB channels in 0..65535.
	r, g, b, _ := parseHexColor(bgHex)
	fr, fg, fb, _ := parseHexColor(fgHex)
	r16, g16, b16 := uint32(r)*257, uint32(g)*257, uint32(b)*257
	fr16, fg16, fb16 := uint32(fr)*257, uint32(fg)*257, uint32(fb)*257
	script := fmt.Sprintf(`
tell application "Terminal"
	activate
	set newTab to do script "clear; exec %s"
	try
		set background color of newTab to {%d, %d, %d}
		set normal text color of newTab to {%d, %d, %d}
	end try
	try
		set font name of newTab to "%s"
		set font size of newTab to %d
	on error errMsg
		try
			set font name of newTab to "%s"
			display notification "Bold face not found; using regular." with title "claune"
		on error errMsg2
			display notification "Could not apply %s: " & errMsg2 with title "claune"
		end try
	end try
end tell`, esc(cmdPath), r16, g16, b16, fr16, fg16, fb16, esc(faceName), opts.size, esc(fontName), esc(fontName))

	cmd := exec.Command("osascript", "-e", script)
	if opts.dryRun {
		fmt.Println("osascript -e <<APPLESCRIPT")
		fmt.Println(script)
		fmt.Println("APPLESCRIPT")
		return nil
	}
	return spawnDetached(cmd)
}

// ---------- Windows ----------

// Windows is the awkward case: wt.exe has no CLI flag for font/bg/fg and no
// way to point at an alternate settings.json, so we can't fully style a
// single instance without mutating the user's config. We try, in order:
//
//  1. alacritty / wezterm / mintty — these honor per-instance overrides
//     exactly like on Linux and don't touch any user settings file.
//  2. wt.exe — partial: we set --tabColor and --title (the only per-instance
//     knobs wt offers), and note the limitation.
//  3. cmd.exe — last-resort, unstyleable from the outside.
type windowsTerminal struct {
	name  string
	build func(cmdPath string, size int) (*exec.Cmd, string) // (cmd, advisory note)
}

func windowsTerminals() []*windowsTerminal {
	return []*windowsTerminal{
		{name: "alacritty", build: func(c string, s int) (*exec.Cmd, string) {
			args := []string{
				"-o", fmt.Sprintf(`font.normal.family=%q`, fontName),
				"-o", fmt.Sprintf("font.size=%d", s),
				"-o", fmt.Sprintf(`colors.primary.background="%s"`, bgHex),
				"-o", fmt.Sprintf(`colors.primary.foreground="%s"`, fgHex),
			}
			if bold {
				args = append(args,
					"-o", `font.normal.style="Bold"`,
					"-o", `font.italic.style="Bold Italic"`)
			}
			args = append(args, "-e", c)
			return exec.Command(resolveWindowsExe("alacritty"), args...), ""
		}},
		{name: "wezterm", build: func(c string, s int) (*exec.Cmd, string) {
			fontExpr := fmt.Sprintf(`wezterm.font("%s")`, fontName)
			if bold {
				fontExpr = fmt.Sprintf(`wezterm.font("%s", {weight="Bold"})`, fontName)
			}
			return exec.Command(resolveWindowsExe("wezterm"),
				"--config", "font="+fontExpr,
				"--config", fmt.Sprintf("font_size=%d", s),
				"--config", fmt.Sprintf(`colors={background="%s",foreground="%s"}`, bgHex, fgHex),
				"start", "--always-new-process", "--", c), ""
		}},
		{name: "mintty", build: func(c string, s int) (*exec.Cmd, string) {
			// mintty ships with Git for Windows / Cygwin / MSYS2. All its
			// appearance options are per-instance and take effect without
			// touching the user's ~/.minttyrc.
			args := []string{
				"-o", fmt.Sprintf("Font=%s", fontName),
				"-o", fmt.Sprintf("FontHeight=%d", s),
				"-o", fmt.Sprintf("BackgroundColour=%s", bgHex),
				"-o", fmt.Sprintf("ForegroundColour=%s", fgHex),
				"-t", "Claune",
			}
			if bold {
				args = append(args, "-o", "BoldAsFont=yes")
			}
			args = append(args, "-e", c)
			return exec.Command(resolveWindowsExe("mintty"), args...), ""
		}},
		{name: "wt", build: func(c string, s int) (*exec.Cmd, string) {
			// wt.exe only exposes --tabColor (tab header stripe) and --title
			// as per-instance appearance flags. Font and terminal background
			// cannot be set without editing the user's settings.json, which
			// we refuse to do.
			note := fmt.Sprintf("wt.exe cannot set font/background per-instance; tab stripe tinted %s only. "+
				"To fully customise, add a \"Claune\" color scheme + profile to Settings once.", bgHex)
			return exec.Command(resolveWindowsExe("wt"), "new-tab",
				"--title", "Claune",
				"--tabColor", bgHex,
				"cmd.exe", "/k", c), note
		}},
		{name: "cmd", build: func(c string, _ int) (*exec.Cmd, string) {
			// `start`'s first quoted argument is interpreted as the window
			// title. Go auto-quotes argv entries containing spaces, so if
			// cmdPath has a space the quoted path would be mistaken for a
			// title and start would have no command left. Pass an explicit
			// empty title ("") so the command slot is unambiguous.
			return exec.Command("cmd.exe", "/c", "start", "", "cmd.exe", "/k", c),
				"cmd.exe cannot be restyled from outside — Properties → Font sets it manually."
		}},
	}
}

func launchWindows(opts options, cmdPath string) error {
	terms := windowsTerminals()
	if opts.terminal != "" {
		var picked *windowsTerminal
		var names []string
		for _, t := range terms {
			names = append(names, t.name)
			if t.name == opts.terminal {
				picked = t
			}
		}
		if picked == nil {
			return fmt.Errorf("unknown -terminal %q; supported: %s",
				opts.terminal, strings.Join(names, ", "))
		}
		terms = []*windowsTerminal{picked}
	}

	var tried []string
	for _, t := range terms {
		// PATHEXT handling: LookPath(t.name) will find .exe / .cmd / .bat /
		// Scoop shim wrappers automatically. Trying name+".exe" explicitly
		// was redundant and skipped non-.exe installs.
		if _, err := exec.LookPath(t.name); err != nil {
			tried = append(tried, t.name+" (not installed)")
			continue
		}
		cmd, note := t.build(cmdPath, opts.size)
		infof("using terminal: %s", t.name)
		if note != "" {
			warnf("%s: %s", t.name, note)
		}
		if opts.dryRun {
			fmt.Println(shellQuote(cmd.Args))
			return nil
		}
		return spawnDetached(cmd)
	}
	return fmt.Errorf("no supported terminal emulator found on PATH. Tried: %s",
		strings.Join(tried, ", "))
}

// ---------- Process spawn ----------

// spawnDetached starts the launcher and gives it a brief grace window to
// surface errors (e.g. "DISPLAY not set", "profile not found") before
// returning. The child is then released so it outlives this process.
func spawnDetached(cmd *exec.Cmd) error {
	cmd.Stdin = nil
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to spawn %s: %w", cmd.Path, err)
	}

	// Watch for an immediate exit (launcher crashed) without blocking forever.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("%s exited immediately: %w", filepath.Base(cmd.Path), err)
		}
		// Clean exit this fast is also suspicious but not necessarily fatal —
		// some terminals fork a daemon and return 0. Carry on.
		return nil
	case <-time.After(300 * time.Millisecond):
		// Still running — assume the terminal window is up. Detach.
		if cmd.Process != nil {
			_ = cmd.Process.Release()
		}
		return nil
	}
}

// ---------- Font file loading ----------

// queryFontFamily asks fc-query for the family name inside a TTF/OTF. Returns
// "" if fc-query isn't available or output is empty.
func queryFontFamily(path string) (string, error) {
	fcq, err := exec.LookPath("fc-query")
	if err != nil {
		return "", nil // not fatal
	}
	out, err := exec.Command(fcq, "--format=%{family[0]}", path).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// registerFontFile makes the font discoverable to a child process. Strategy
// differs per OS:
//   - Linux: write an ephemeral fontconfig config that adds the file's dir,
//     return FONTCONFIG_FILE=... for the child terminal's env.
//   - macOS/Windows: copy the file into the user-scope fonts directory
//     (reversible by deleting). No env changes required.
func registerFontFile(path string) ([]string, error) {
	switch runtime.GOOS {
	case "linux":
		return linuxFontconfigEnv(path)
	case "darwin":
		return nil, installFontMac(path)
	case "windows":
		return nil, installFontWindows(path)
	}
	return nil, fmt.Errorf("unsupported OS: %s", runtime.GOOS)
}

func linuxFontconfigEnv(fontFile string) ([]string, error) {
	dir := filepath.Dir(fontFile)
	tmp, err := os.MkdirTemp("", "claune-fc-*")
	if err != nil {
		return nil, err
	}
	cacheDir := filepath.Join(tmp, "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, err
	}
	conf := filepath.Join(tmp, "fonts.conf")
	xml := fmt.Sprintf(`<?xml version="1.0"?>
<!DOCTYPE fontconfig SYSTEM "urn:fontconfig:fonts.dtd">
<fontconfig>
  <include ignore_missing="yes">/etc/fonts/fonts.conf</include>
  <dir>%s</dir>
  <cachedir>%s</cachedir>
</fontconfig>
`, dir, cacheDir)
	if err := os.WriteFile(conf, []byte(xml), 0o644); err != nil {
		return nil, err
	}
	// Warm the cache so the terminal doesn't stall on first resolve.
	if fcc, err := exec.LookPath("fc-cache"); err == nil {
		c := exec.Command(fcc, "-f", dir)
		c.Env = append(os.Environ(), "FONTCONFIG_FILE="+conf)
		_ = c.Run() // best-effort
	}
	return []string{"FONTCONFIG_FILE=" + conf}, nil
}

func installFontMac(src string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dstDir := filepath.Join(home, "Library", "Fonts")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return err
	}
	return copyIfMissing(src, filepath.Join(dstDir, filepath.Base(src)))
}

func installFontWindows(src string) error {
	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		return fmt.Errorf("LOCALAPPDATA not set")
	}
	dstDir := filepath.Join(local, "Microsoft", "Windows", "Fonts")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return err
	}
	dst := filepath.Join(dstDir, filepath.Base(src))
	if err := copyIfMissing(src, dst); err != nil {
		return err
	}
	// On Windows 10+, copying a TTF into the per-user Fonts dir is not
	// enough — GDI/DirectWrite only discovers fonts that are registered
	// under HKCU\Software\Microsoft\Windows NT\CurrentVersion\Fonts. Shell
	// out to reg.exe (ships with every Windows install) instead of pulling
	// in golang.org/x/sys/windows/registry.
	family, err := queryFontFamily(src)
	if err != nil || family == "" {
		// fc-query is optional; fall back to the base filename without
		// extension, which is usually close enough for the registry key.
		family = strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
	}
	valueName := family + " (TrueType)"
	key := `HKCU\Software\Microsoft\Windows NT\CurrentVersion\Fonts`
	reg := exec.Command("reg.exe", "add", key, "/v", valueName, "/t", "REG_SZ", "/d", dst, "/f")
	if out, err := reg.CombinedOutput(); err != nil {
		return fmt.Errorf("reg add failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// resolveWindowsExe returns the PATH-resolved absolute path for a terminal
// binary (honouring PATHEXT, so .cmd/.bat shims work). Falls back to
// name+".exe" when LookPath fails so the error surfaces in exec.Start
// instead of at argv-build time.
func resolveWindowsExe(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return name + ".exe"
}

func copyIfMissing(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		infof("font already present at %s", dst)
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	infof("installed font to %s", dst)
	return nil
}

// shellQuote renders argv as a copy-pasteable shell command for -dry-run.
func shellQuote(argv []string) string {
	var b strings.Builder
	for i, a := range argv {
		if i > 0 {
			b.WriteByte(' ')
		}
		if strings.ContainsAny(a, " \t\"'$`\\") || a == "" {
			b.WriteByte('\'')
			b.WriteString(strings.ReplaceAll(a, `'`, `'\''`))
			b.WriteByte('\'')
		} else {
			b.WriteString(a)
		}
	}
	return b.String()
}
