package ui

import "testing"

// VisibleLen decides every boxed layout in the CLI. It used to count every
// rune as one column, so an emoji beside padded text shifted the right border
// by one per emoji and the status box looked ragged.
func TestVisibleLen(t *testing.T) {
	SetEnabled(false)
	defer SetEnabled(false)

	cases := []struct {
		name string
		in   string
		want int
	}{
		{"ascii", "abc", 3},
		{"empty", "", 0},
		{"emoji is two columns", "🟢", 2},
		{"emoji plus text", "🟢 x", 4},
		{"cjk is two columns", "日本語", 6},
		{"box drawing is one column", "│─┌", 3},
		{"check mark is one column", "✔", 1},
		{"combining mark adds nothing", "e\u0301", 1},
		{"variation selector adds nothing", "✔\ufe0f", 1},
	}
	for _, c := range cases {
		if got := VisibleLen(c.in); got != c.want {
			t.Errorf("%s: VisibleLen(%q) = %d, want %d", c.name, c.in, got, c.want)
		}
	}
}

// Padding must produce a stable column count regardless of styling, which is
// what keeps coloured boxes aligned.
func TestPadRespectsEmojiWidth(t *testing.T) {
	SetEnabled(false)
	plain := Pad("🟢 mode", 10)
	if got := VisibleLen(plain); got != 10 {
		t.Errorf("VisibleLen(Pad(...,10)) = %d, want 10", got)
	}

	SetEnabled(true)
	styled := Pad(Success("🟢 mode"), 10)
	if got := VisibleLen(styled); got != 10 {
		t.Errorf("styled VisibleLen(Pad(...,10)) = %d, want 10", got)
	}
}
