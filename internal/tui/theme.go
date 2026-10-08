package tui

// Theme: every color and style the dashboard renders lives here (approved
// redesign step 1). The palette is Catppuccin — Mocha for dark terminals,
// Latte for light — chosen once per Model in New() from the terminal
// background (lipgloss.HasDarkBackground, defaults to dark on error; the
// OSC query runs at most once per process via sync.Once, so test binaries
// constructing hundreds of models never re-query a terminal).
//
// View stays pure: it only reads the theme styles stamped into the model —
// no detection, no clock, no I/O.

import (
	"image/color"
	"os"
	"sync"

	"charm.land/lipgloss/v2"
)

// theme is the immutable style set built by newTheme. Palette fields are
// exposed for tests and for helpers that need a raw color (badges,
// borders); rendering code goes through the styles.
type theme struct {
	isDark bool

	// Palette (Catppuccin Mocha / Latte).
	accent color.Color // mauve — focus ring, header
	direct color.Color // green — direct-egress badge
	warp   color.Color // pink — warp-egress badge
	ok     color.Color // green — quota within limits
	warn   color.Color // yellow — 429 seen today
	err    color.Color // red — key/egress exhausted
	dim    color.Color // subtext — secondary labels

	// Styles composed from the palette.
	header     lipgloss.Style // bold accent header line
	panelTitle lipgloss.Style // bold panel titles
	statusOK   lipgloss.Style
	statusWarn lipgloss.Style
	statusErr  lipgloss.Style
	dimText    lipgloss.Style
}

// newTheme builds the style set for one background. Dark and light palettes
// are complete and different per semantic — a background the theme ignores
// is a bug (theme_test.go).
func newTheme(isDark bool) theme {
	pick := func(light, dark string) color.Color {
		return lipgloss.LightDark(isDark)(lipgloss.Color(light), lipgloss.Color(dark))
	}
	th := theme{
		isDark: isDark,
		accent: pick("#8839ef", "#cba6f7"), // mauve
		direct: pick("#40a02b", "#a6e3a1"), // green
		warp:   pick("#ea76cb", "#f5c2e7"), // pink
		ok:     pick("#40a02b", "#a6e3a1"), // green
		warn:   pick("#df8e1d", "#f9e2af"), // yellow
		err:    pick("#d20f39", "#f38ba8"), // red
		dim:    pick("#6c6f85", "#a6adc8"), // subtext
	}
	th.header = lipgloss.NewStyle().Bold(true).Foreground(th.accent)
	th.panelTitle = lipgloss.NewStyle().Bold(true).Foreground(th.accent)
	th.statusOK = lipgloss.NewStyle().Foreground(th.ok)
	th.statusWarn = lipgloss.NewStyle().Foreground(th.warn)
	th.statusErr = lipgloss.NewStyle().Foreground(th.err)
	th.dimText = lipgloss.NewStyle().Foreground(th.dim)
	return th
}

var (
	backgroundOnce sync.Once
	backgroundDark bool
)

// detectBackground queries the terminal background at most once per
// process (non-TTY stdin/stdout answer immediately with the dark default).
// It runs in New(), never in View.
func detectBackground() bool {
	backgroundOnce.Do(func() {
		backgroundDark = lipgloss.HasDarkBackground(os.Stdin, os.Stdout)
	})
	return backgroundDark
}
