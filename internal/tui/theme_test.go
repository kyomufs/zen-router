package tui

// Theme contract (approved redesign, step 1): all colors and styles live in
// one place — theme.go — built exactly once per Model in New() from the
// terminal background (lipgloss.HasDarkBackground, defaults to dark on
// error). The view only reads theme styles; this file pins:
//
//   - newTheme(isDark) exists and records its background choice;
//   - the Catppuccin dark and light palettes are genuinely different
//     (a theme that ignores the background is a bug, see charter);
//   - styles are derived from that palette (not hard-coded per style);
//   - New() wires a theme into the model.

import (
	"fmt"
	"image/color"
	"strings"
	"testing"
)

// TestNewThemeAdaptsToBackground: dark and light palettes must differ for
// every accent semantic the dashboard renders. Compared as raw palette
// values (not rendered strings) so the assertion holds regardless of the
// color profile of the test process.
func TestNewThemeAdaptsToBackground(t *testing.T) {
	dark := newTheme(true)
	light := newTheme(false)

	if !dark.isDark {
		t.Error("newTheme(true).isDark = false, want true")
	}
	if light.isDark {
		t.Error("newTheme(false).isDark = true, want false")
	}

	for _, tc := range []struct {
		name string
		d, l color.Color
	}{
		{"accent", dark.accent, light.accent},
		{"direct", dark.direct, light.direct},
		{"warp", dark.warp, light.warp},
		{"ok", dark.ok, light.ok},
		{"warn", dark.warn, light.warn},
		{"err", dark.err, light.err},
		{"dim", dark.dim, light.dim},
	} {
		if tc.d == nil || tc.l == nil {
			t.Errorf("%s: missing palette color (dark=%v light=%v)", tc.name, tc.d, tc.l)
		}
		if fmt.Sprint(tc.d) == fmt.Sprint(tc.l) {
			t.Errorf("%s: dark and light palette identical (%s) — background ignored", tc.name, fmt.Sprint(tc.d))
		}
	}
}

// TestThemeStylesDeriveFromPalette: the styles the view renders must be
// built from the theme palette — bold header in the accent color, a
// distinct focused-border style (focus ring), status semantic styles in
// their palette colors.
func TestThemeStylesDeriveFromPalette(t *testing.T) {
	for _, isDark := range []bool{true, false} {
		th := newTheme(isDark)
		prefix := fmt.Sprintf("isDark=%v", isDark)

		if !th.header.GetBold() {
			t.Errorf("%s: header style must be bold", prefix)
		}
		if got, want := fmt.Sprint(th.header.GetForeground()), fmt.Sprint(th.accent); got != want {
			t.Errorf("%s: header foreground = %s, want accent %s", prefix, got, want)
		}
		if got, want := fmt.Sprint(th.statusOK.GetForeground()), fmt.Sprint(th.ok); got != want {
			t.Errorf("%s: statusOK foreground = %s, want ok %s", prefix, got, want)
		}
		if got, want := fmt.Sprint(th.statusWarn.GetForeground()), fmt.Sprint(th.warn); got != want {
			t.Errorf("%s: statusWarn foreground = %s, want warn %s", prefix, got, want)
		}
		if got, want := fmt.Sprint(th.statusErr.GetForeground()), fmt.Sprint(th.err); got != want {
			t.Errorf("%s: statusErr foreground = %s, want err %s", prefix, got, want)
		}
		if !th.panelTitle.GetBold() {
			t.Errorf("%s: panel title style must be bold", prefix)
		}
	}
}

// TestNewWiresTheme: every constructed model carries a built theme — the
// view renders through it, a zero theme would drop all styling.
func TestNewWiresTheme(t *testing.T) {
	m := New(newFake(fakeResult{status: upStatus()}))
	if m.th.accent == nil {
		t.Fatal("New must build a theme (Model.th.accent is nil)")
	}
	if got := m.th.header.Render("zen-router"); !strings.Contains(got, "zen-router") {
		t.Errorf("header style renders %q, want the text preserved", got)
	}
}
