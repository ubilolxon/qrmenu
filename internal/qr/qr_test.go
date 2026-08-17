package qr

import (
	"bytes"
	"encoding/xml"
	"image/png"
	"strings"
	"testing"
)

// These tests do not decode the QR codes back into text — that would mean
// pulling in a reader library for the sake of re-testing skip2's encoder. What
// is worth pinning is everything around it: the size contract the HTTP handler
// depends on, the well-formedness of the SVG a print shop will open, and the
// fact that different tables really do get different images.

func TestClampSize(t *testing.T) {
	tests := []struct {
		name string
		in   int
		want int
	}{
		{"zero means unspecified", 0, DefaultSize},
		{"negative is nonsense", -1, DefaultSize},
		{"far below the floor", 1, MinSize},
		{"just below the floor", MinSize - 1, MinSize},
		{"the floor itself", MinSize, MinSize},
		{"in range", 300, 300},
		{"the ceiling itself", MaxSize, MaxSize},
		{"just above the ceiling", MaxSize + 1, MaxSize},
		{"absurd", 99999, MaxSize},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClampSize(tt.in); got != tt.want {
				t.Errorf("ClampSize(%d) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestPNGDimensions(t *testing.T) {
	const content = "https://qrmenu.uz/t/099XQRSR2FH6DB"

	tests := []struct {
		name string
		size int
		want int
	}{
		{"default", 0, DefaultSize},
		{"explicit", 256, 256},
		{"clamped up", 10, MinSize},
		{"clamped down", 99999, MaxSize},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := PNG(content, tt.size)
			if err != nil {
				t.Fatalf("PNG(%d): %v", tt.size, err)
			}

			cfg, err := png.DecodeConfig(bytes.NewReader(raw))
			if err != nil {
				t.Fatalf("decode png: %v", err)
			}
			// The handler advertises the size it was asked for, so the bytes
			// have to actually be that size.
			if cfg.Width != tt.want || cfg.Height != tt.want {
				t.Errorf("PNG(%d) is %dx%d, want %dx%d",
					tt.size, cfg.Width, cfg.Height, tt.want, tt.want)
			}
		})
	}
}

func TestPNGRejectsEmptyContent(t *testing.T) {
	if _, err := PNG("", DefaultSize); err == nil {
		t.Error("PNG(\"\") returned no error; an empty code would scan to nothing")
	}
	if _, err := SVG("", DefaultSize); err == nil {
		t.Error("SVG(\"\") returned no error")
	}
	if _, err := Card("", "Choyxona Navruz", "Terrace 1", DefaultSize); err == nil {
		t.Error("Card(\"\") returned no error")
	}
}

// A print shop opens these in Illustrator or a browser, both of which reject
// malformed XML outright. Parsing is the cheapest way to guarantee the string
// concatenation in SVG() and Card() never produces something unopenable.
func TestSVGIsWellFormedXML(t *testing.T) {
	const content = "https://qrmenu.uz/m/choyxona-navruz"

	svg, err := SVG(content, 512)
	if err != nil {
		t.Fatalf("SVG: %v", err)
	}
	assertParses(t, svg)

	if !bytes.Contains(svg, []byte(`viewBox="0 0 `)) {
		t.Error("SVG has no viewBox, so it will not scale")
	}
	if !bytes.Contains(svg, []byte(`width="512" height="512"`)) {
		t.Errorf("SVG does not carry the requested pixel size:\n%s", head(svg))
	}
	if !bytes.Contains(svg, []byte(`shape-rendering="crispEdges"`)) {
		t.Error("SVG lacks crispEdges; modules will render blurred at small sizes")
	}
	// One path of subpaths, not one element per module.
	if n := bytes.Count(svg, []byte("<path")); n != 1 {
		t.Errorf("SVG has %d <path> elements, want exactly 1", n)
	}
	if n := bytes.Count(svg, []byte("<rect")); n != 1 {
		t.Errorf("SVG has %d <rect> elements, want exactly 1 (the white ground)", n)
	}
}

func TestSVGSizeIsClamped(t *testing.T) {
	svg, err := SVG("https://qrmenu.uz/m/x", 99999)
	if err != nil {
		t.Fatalf("SVG: %v", err)
	}
	if !bytes.Contains(svg, []byte(`width="2048" height="2048"`)) {
		t.Errorf("oversized SVG was not clamped:\n%s", head(svg))
	}
}

func TestCard(t *testing.T) {
	const content = "https://qrmenu.uz/t/099XQRSR2FH6DB"

	card, err := Card(content, "Choyxona Navruz", "Terrace 1", DefaultSize)
	if err != nil {
		t.Fatalf("Card: %v", err)
	}
	assertParses(t, card)

	for what, want := range map[string]string{
		"restaurant name": "Choyxona Navruz",
		"table label":     "Terrace 1",
		"instruction":     "Scan for the menu",
		"the URL in text": content,
		"fixed card size": `width="420" height="560"`,
	} {
		if !bytes.Contains(card, []byte(want)) {
			t.Errorf("card is missing the %s (%q)", what, want)
		}
	}

	// The nested code must lose its own <svg> wrapper, or the card contains a
	// second root element scaled independently of the transform.
	if n := bytes.Count(card, []byte("<svg")); n != 1 {
		t.Errorf("card has %d <svg> elements, want exactly 1", n)
	}
	if !bytes.Contains(card, []byte("<g transform=\"translate(60,130) scale(")) {
		t.Error("card does not scale the code into place")
	}
}

// A restaurant name is user input and lands directly in the SVG text.
func TestCardEscapesNames(t *testing.T) {
	card, err := Card("https://qrmenu.uz/m/x", `Bob & "Sons" <script>`, "Table 1", DefaultSize)
	if err != nil {
		t.Fatalf("Card: %v", err)
	}
	assertParses(t, card)

	if bytes.Contains(card, []byte("<script>")) {
		t.Errorf("card did not escape the restaurant name:\n%s", head(card))
	}
	if !bytes.Contains(card, []byte("&amp;")) {
		t.Error("card did not escape the ampersand")
	}
}

func TestCardTruncatesLongNames(t *testing.T) {
	long := strings.Repeat("ў", 200) // multi-byte, to catch rune-vs-byte slicing
	card, err := Card("https://qrmenu.uz/m/x", long, long, DefaultSize)
	if err != nil {
		t.Fatalf("Card: %v", err)
	}
	assertParses(t, card)

	if !bytes.Contains(card, []byte("…")) {
		t.Error("an over-long name was not truncated; it will overflow the card")
	}
	if bytes.Contains(card, []byte(strings.Repeat("ў", 41))) {
		t.Error("name was truncated to more than 40 runes")
	}
}

// If two tables ever produced the same image, every code printed after the
// first would point a guest at the wrong table.
func TestDifferentContentProducesDifferentCodes(t *testing.T) {
	a, err := PNG("https://qrmenu.uz/t/099XQRSR2FH6DB", 256)
	if err != nil {
		t.Fatalf("PNG: %v", err)
	}
	b, err := PNG("https://qrmenu.uz/t/099XQRSR2FH6DC", 256)
	if err != nil {
		t.Fatalf("PNG: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Error("two different tokens encoded to identical PNGs")
	}
}

// Rendering is deterministic, which is what lets the QR endpoints be cached
// and ETagged on the token alone.
func TestRenderingIsDeterministic(t *testing.T) {
	const content = "https://qrmenu.uz/t/099XQRSR2FH6DB"

	first, err := PNG(content, 256)
	if err != nil {
		t.Fatalf("PNG: %v", err)
	}
	second, err := PNG(content, 256)
	if err != nil {
		t.Fatalf("PNG: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Error("PNG output varies between calls; the ETag would change on every request")
	}
}

func assertParses(t *testing.T, doc []byte) {
	t.Helper()
	dec := xml.NewDecoder(bytes.NewReader(doc))
	for {
		_, err := dec.Token()
		if err != nil {
			if err.Error() == "EOF" {
				return
			}
			t.Fatalf("malformed XML: %v\n%s", err, head(doc))
		}
	}
}

func head(doc []byte) []byte {
	if len(doc) > 400 {
		return doc[:400]
	}
	return doc
}
