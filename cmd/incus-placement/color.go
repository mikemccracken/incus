package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/fatih/color"
)

// cell is a table cell with an optional style.
type cell struct {
	text  string
	style *color.Color
}

// printTable writes aligned rows, padding the cells before they get styled.
func printTable(w io.Writer, rows [][]cell) {
	var widths []int
	for _, row := range rows {
		for i, c := range row {
			if i >= len(widths) {
				widths = append(widths, 0)
			}

			widths[i] = max(widths[i], len(c.text))
		}
	}

	for _, row := range rows {
		cells := make([]string, 0, len(row))
		for i, c := range row {
			// The last column isn't padded to avoid trailing spaces.
			text := c.text
			if i < len(row)-1 {
				text = fmt.Sprintf("%-*s", widths[i], c.text)
			}

			if c.style != nil {
				text = c.style.Sprint(text)
			}

			cells = append(cells, text)
		}

		_, _ = fmt.Fprintln(w, strings.Join(cells, "  "))
	}
}

// palette holds the styles used by the text report.
type palette struct {
	title   *color.Color
	tag     *color.Color
	dim     *color.Color
	good    *color.Color
	caution *color.Color
	bad     *color.Color
}

// newPalette returns the report styles for a color mode (auto, always or never).
func newPalette(mode string) (*palette, error) {
	p := &palette{
		title:   color.New(color.Bold),
		tag:     color.New(color.FgCyan, color.Bold),
		dim:     color.New(color.Faint),
		good:    color.New(color.FgGreen, color.Bold),
		caution: color.New(color.FgYellow),
		bad:     color.New(color.FgRed, color.Bold),
	}

	styles := []*color.Color{p.title, p.tag, p.dim, p.good, p.caution, p.bad}

	switch mode {
	case "auto":
		// Colors are used when writing to a terminal and NO_COLOR isn't set.
	case "always":
		for _, style := range styles {
			style.EnableColor()
		}

	case "never":
		for _, style := range styles {
			style.DisableColor()
		}

	default:
		return nil, fmt.Errorf("Invalid color mode %q", mode)
	}

	return p, nil
}

// trace returns the style of a scriptlet trace entry.
func (p *palette) trace(kind string) *color.Color {
	switch kind {
	case "call":
		return p.dim
	case "warn":
		return p.caution
	case "error":
		return p.bad
	}

	return color.New()
}
