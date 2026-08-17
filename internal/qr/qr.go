// Package qr renders the codes that go on the tables.
package qr

import (
	"bytes"
	"fmt"
	"html"

	qrcode "github.com/skip2/go-qrcode"
)

// Recovery level M tolerates about 15% damage. Restaurant QR codes live on
// tables and get spilled on, scratched, and partly covered — L would be smaller
// but stops working far sooner in practice.
const recoveryLevel = qrcode.Medium

const (
	MinSize     = 128
	MaxSize     = 2048
	DefaultSize = 512
)

// ClampSize keeps a caller-supplied pixel size sane. Rendering is CPU work, so
// an unbounded size is a cheap way to load the server.
func ClampSize(size int) int {
	switch {
	case size <= 0:
		return DefaultSize
	case size < MinSize:
		return MinSize
	case size > MaxSize:
		return MaxSize
	default:
		return size
	}
}

// PNG renders content as a PNG of the given pixel width.
func PNG(content string, size int) ([]byte, error) {
	if content == "" {
		return nil, fmt.Errorf("qr: nothing to encode")
	}
	png, err := qrcode.Encode(content, recoveryLevel, ClampSize(size))
	if err != nil {
		return nil, fmt.Errorf("qr: encode png: %w", err)
	}
	return png, nil
}

// SVG renders content as a vector QR code.
//
// Print shops want vectors: a PNG blown up to A4 for a table tent goes soft,
// while an SVG stays crisp at any size. The output is one <path> of black
// squares over a white background, which keeps the file small enough to inline.
func SVG(content string, size int) ([]byte, error) {
	if content == "" {
		return nil, fmt.Errorf("qr: nothing to encode")
	}

	code, err := qrcode.New(content, recoveryLevel)
	if err != nil {
		return nil, fmt.Errorf("qr: encode svg: %w", err)
	}

	grid := code.Bitmap()
	modules := len(grid)
	if modules == 0 {
		return nil, fmt.Errorf("qr: empty bitmap")
	}

	var buf bytes.Buffer
	fmt.Fprintf(&buf,
		`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" `+
			`viewBox="0 0 %d %d" shape-rendering="crispEdges" role="img" aria-label="QR code">`,
		ClampSize(size), ClampSize(size), modules, modules)
	buf.WriteString(`<rect width="100%" height="100%" fill="#ffffff"/><path fill="#000000" d="`)

	// One path with many subpaths beats one <rect> per module: a typical code is
	// ~1000 dark modules, and separate elements would balloon the file.
	for y, row := range grid {
		for x, dark := range row {
			if dark {
				fmt.Fprintf(&buf, "M%d %dh1v1h-1z", x, y)
			}
		}
	}
	buf.WriteString(`"/></svg>`)

	return buf.Bytes(), nil
}

// Card renders a printable table tent: the code plus the restaurant and table
// names, as a standalone SVG a shop can print directly.
func Card(content, restaurantName, tableLabel string, size int) ([]byte, error) {
	code, err := SVG(content, size)
	if err != nil {
		return nil, err
	}

	// Strip the outer <svg> wrapper so the code can be nested inside the card.
	inner := bytes.TrimSuffix(code, []byte("</svg>"))
	if idx := bytes.IndexByte(inner, '>'); idx >= 0 {
		inner = inner[idx+1:]
	}

	codeModules := 0
	if c, err := qrcode.New(content, recoveryLevel); err == nil {
		codeModules = len(c.Bitmap())
	}
	if codeModules == 0 {
		return nil, fmt.Errorf("qr: empty bitmap")
	}

	var buf bytes.Buffer
	fmt.Fprintf(&buf,
		`<svg xmlns="http://www.w3.org/2000/svg" width="420" height="560" viewBox="0 0 420 560">`+
			`<rect width="420" height="560" fill="#ffffff"/>`+
			`<text x="210" y="60" text-anchor="middle" font-family="system-ui,sans-serif" `+
			`font-size="26" font-weight="600" fill="#111111">%s</text>`+
			`<text x="210" y="96" text-anchor="middle" font-family="system-ui,sans-serif" `+
			`font-size="18" fill="#666666">%s</text>`,
		html.EscapeString(truncate(restaurantName, 40)),
		html.EscapeString(truncate(tableLabel, 40)))

	// Scale the module grid into a 300pt square centred below the heading.
	scale := 300.0 / float64(codeModules)
	fmt.Fprintf(&buf, `<g transform="translate(60,130) scale(%f)" shape-rendering="crispEdges">`, scale)
	buf.Write(inner)
	buf.WriteString(`</g>`)

	fmt.Fprintf(&buf,
		`<text x="210" y="482" text-anchor="middle" font-family="system-ui,sans-serif" `+
			`font-size="16" fill="#111111">Scan for the menu</text>`+
			`<text x="210" y="512" text-anchor="middle" font-family="system-ui,sans-serif" `+
			`font-size="12" fill="#999999">%s</text></svg>`,
		html.EscapeString(truncate(content, 60)))

	return buf.Bytes(), nil
}

func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
}
