package discord

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

type tableData struct {
	headers []string
	rows    [][]string
}

var (
	reTableSepLine = regexp.MustCompile(`^\|[\s:|\-]+\|$`)
	reMarkdownBold = regexp.MustCompile(`\*\*(.+?)\*\*`)
	reMarkdownItal = regexp.MustCompile(`(?:^|[^*])\*([^*]+?)\*(?:[^*]|$)`)
	reMarkdownUndB = regexp.MustCompile(`__(.+?)__`)
	reMarkdownUndI = regexp.MustCompile(`(?:^|[^_])_([^_]+?)_(?:[^_]|$)`)
	reMarkdownCode = regexp.MustCompile("`([^`]+)`")
)

type contentSegment struct {
	text  string
	table *tableData
}

// splitContentAroundTables splits content into segments: plain text and tables.
// Each table becomes a separate segment with its parsed data so it can be sent
// as its own message with an attached image. Tables inside code blocks are
// left as plain text.
func splitContentAroundTables(s string) []contentSegment {
	lines := strings.Split(s, "\n")
	var segments []contentSegment
	inCodeBlock := false
	var textBuf []string
	var tableBuf []string

	flushText := func() {
		text := strings.TrimSpace(strings.Join(textBuf, "\n"))
		if text != "" {
			segments = append(segments, contentSegment{text: text})
		}
		textBuf = nil
	}

	flushTable := func() {
		if len(tableBuf) >= 2 {
			if t, ok := parseTable(tableBuf); ok {
				flushText()
				segments = append(segments, contentSegment{
					text:  alignedTableCodeBlock(t),
					table: &t,
				})
				tableBuf = nil
				return
			}
		}
		textBuf = append(textBuf, tableBuf...)
		tableBuf = nil
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "```") {
			flushTable()
			inCodeBlock = !inCodeBlock
			textBuf = append(textBuf, line)
			continue
		}

		if inCodeBlock {
			textBuf = append(textBuf, line)
			continue
		}

		isTableLine := len(trimmed) >= 3 &&
			strings.HasPrefix(trimmed, "|") &&
			strings.HasSuffix(trimmed, "|")

		if isTableLine {
			tableBuf = append(tableBuf, trimmed)
		} else {
			flushTable()
			textBuf = append(textBuf, line)
		}
	}
	flushTable()
	flushText()

	return segments
}

func alignedTableCodeBlock(t tableData) string {
	numCols := len(t.headers)
	if numCols == 0 && len(t.rows) > 0 {
		numCols = len(t.rows[0])
	}

	colWidths := make([]int, numCols)
	for i, h := range t.headers {
		if w := utf8.RuneCountInString(h); w > colWidths[i] {
			colWidths[i] = w
		}
	}
	for _, row := range t.rows {
		for i := range min(len(row), numCols) {
			if w := utf8.RuneCountInString(row[i]); w > colWidths[i] {
				colWidths[i] = w
			}
		}
	}

	padCell := func(s string, width int) string {
		n := utf8.RuneCountInString(s)
		if n >= width {
			return s
		}
		return s + strings.Repeat(" ", width-n)
	}

	formatRow := func(cells []string) string {
		var b strings.Builder
		b.WriteString("|")
		for i := range numCols {
			b.WriteString(" ")
			cell := ""
			if i < len(cells) {
				cell = cells[i]
			}
			b.WriteString(padCell(cell, colWidths[i]))
			b.WriteString(" |")
		}
		return b.String()
	}

	separatorRow := func() string {
		var b strings.Builder
		b.WriteString("|")
		for i := range numCols {
			b.WriteString(strings.Repeat("-", colWidths[i]+2))
			b.WriteString("|")
		}
		return b.String()
	}

	var lines []string
	lines = append(lines, "```")
	if len(t.headers) > 0 {
		lines = append(lines, formatRow(t.headers))
		lines = append(lines, separatorRow())
	}
	for _, row := range t.rows {
		lines = append(lines, formatRow(row))
	}
	lines = append(lines, "```")
	return strings.Join(lines, "\n")
}

func parseTable(lines []string) (tableData, bool) {
	if len(lines) < 2 {
		return tableData{}, false
	}

	sepIdx := -1
	for i, line := range lines {
		if reTableSepLine.MatchString(line) {
			sepIdx = i
			break
		}
	}

	var t tableData
	var dataStart int

	if sepIdx == 1 {
		t.headers = parseCells(lines[0])
		dataStart = 2
	} else if sepIdx == 0 {
		dataStart = 1
	} else {
		t.headers = parseCells(lines[0])
		dataStart = 1
	}

	numCols := len(t.headers)
	if numCols == 0 && dataStart < len(lines) {
		numCols = len(parseCells(lines[dataStart]))
	}
	if numCols == 0 {
		return tableData{}, false
	}

	for _, line := range lines[dataStart:] {
		if reTableSepLine.MatchString(line) {
			continue
		}
		cells := parseCells(line)
		for len(cells) < numCols {
			cells = append(cells, "")
		}
		if len(cells) > numCols {
			cells = cells[:numCols]
		}
		t.rows = append(t.rows, cells)
	}

	return t, true
}

func parseCells(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	parts := strings.Split(line, "|")
	cells := make([]string, len(parts))
	for i, p := range parts {
		cells[i] = stripInlineMarkdown(strings.TrimSpace(p))
	}
	return cells
}

func stripInlineMarkdown(s string) string {
	s = reMarkdownBold.ReplaceAllString(s, "$1")
	s = reMarkdownUndB.ReplaceAllString(s, "$1")
	s = reMarkdownItal.ReplaceAllString(s, "$1")
	s = reMarkdownUndI.ReplaceAllString(s, "$1")
	s = reMarkdownCode.ReplaceAllString(s, "$1")
	return s
}
