package discord

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log/slog"
	"strings"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/gofont/gomonobold"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

var (
	tableFontRegular font.Face
	tableFontBold    font.Face
	tableFontOnce    sync.Once
)

func initTableFonts() {
	tableFontOnce.Do(func() {
		const size = 22
		const dpi = 72
		if tt, err := opentype.Parse(gomono.TTF); err == nil {
			tableFontRegular, _ = opentype.NewFace(tt, &opentype.FaceOptions{
				Size: size, DPI: dpi, Hinting: font.HintingFull,
			})
		}
		if tt, err := opentype.Parse(gomonobold.TTF); err == nil {
			tableFontBold, _ = opentype.NewFace(tt, &opentype.FaceOptions{
				Size: size, DPI: dpi, Hinting: font.HintingFull,
			})
		}
	})
}

const (
	tblCellPadX = 18
	tblCellPadY = 12
	tblMargin   = 16
	tblMaxRows  = 30
)

var (
	colBg      = color.RGBA{47, 49, 54, 255}
	colHdrBg   = color.RGBA{32, 34, 37, 255}
	colAltBg   = color.RGBA{54, 57, 63, 255}
	colText    = color.RGBA{220, 221, 222, 255}
	colHdrText = color.RGBA{255, 255, 255, 255}
	colLine    = color.RGBA{64, 68, 75, 255}
)

func extractMarkdownTables(s string) []tableData {
	lines := strings.Split(s, "\n")
	var tables []tableData
	inCodeBlock := false
	var buf []string

	flush := func() {
		if len(buf) >= 2 {
			if t, ok := parseTable(buf); ok {
				tables = append(tables, t)
			}
		}
		buf = nil
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			flush()
			inCodeBlock = !inCodeBlock
			continue
		}
		if inCodeBlock {
			continue
		}
		isTableLine := len(trimmed) >= 3 &&
			strings.HasPrefix(trimmed, "|") &&
			strings.HasSuffix(trimmed, "|")
		if isTableLine {
			buf = append(buf, trimmed)
		} else {
			flush()
		}
	}
	flush()
	return tables
}

func renderTablePNG(t tableData) ([]byte, error) {
	initTableFonts()
	if tableFontRegular == nil {
		return nil, fmt.Errorf("table font not loaded")
	}

	face := tableFontRegular
	boldFace := tableFontBold
	if boldFace == nil {
		boldFace = face
	}

	rows := t.rows
	if len(rows) > tblMaxRows {
		rows = rows[:tblMaxRows]
	}

	numCols := len(t.headers)
	if numCols == 0 && len(rows) > 0 {
		numCols = len(rows[0])
	}

	colWidths := make([]int, numCols)
	for i, h := range t.headers {
		if w := font.MeasureString(boldFace, h).Ceil(); w > colWidths[i] {
			colWidths[i] = w
		}
	}
	for _, row := range rows {
		for i := range min(len(row), numCols) {
			if w := font.MeasureString(face, row[i]).Ceil(); w > colWidths[i] {
				colWidths[i] = w
			}
		}
	}
	for i := range colWidths {
		colWidths[i] += tblCellPadX * 2
	}

	metrics := face.Metrics()
	rowH := metrics.Height.Ceil() + tblCellPadY*2
	ascent := metrics.Ascent.Ceil()

	totalW := tblMargin * 2
	for _, w := range colWidths {
		totalW += w
	}

	hasHdr := len(t.headers) > 0
	numRows := len(rows)
	if hasHdr {
		numRows++
	}
	totalH := tblMargin*2 + numRows*rowH + 2

	img := image.NewRGBA(image.Rect(0, 0, totalW, totalH))
	draw.Draw(img, img.Bounds(), image.NewUniform(colBg), image.Point{}, draw.Src)

	y := tblMargin

	if hasHdr {
		imgFillRect(img, tblMargin, y, totalW-tblMargin, y+rowH, colHdrBg)
		x := tblMargin
		for i, h := range t.headers {
			if i >= numCols {
				break
			}
			imgDrawString(img, boldFace, x+tblCellPadX, y+tblCellPadY+ascent, h, colHdrText)
			x += colWidths[i]
		}
		y += rowH
		imgDrawHLine(img, tblMargin, totalW-tblMargin, y, colLine)
		imgDrawHLine(img, tblMargin, totalW-tblMargin, y+1, colLine)
		y += 2
	}

	for ri, row := range rows {
		if ri%2 == 1 {
			imgFillRect(img, tblMargin, y, totalW-tblMargin, y+rowH, colAltBg)
		}
		x := tblMargin
		for i := range min(len(row), numCols) {
			imgDrawString(img, face, x+tblCellPadX, y+tblCellPadY+ascent, row[i], colText)
			x += colWidths[i]
		}
		y += rowH
	}

	imgDrawBorder(img, tblMargin-1, tblMargin-1, totalW-tblMargin+1, y, colLine)

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("encode table png: %w", err)
	}
	return buf.Bytes(), nil
}

func renderTableImages(content string) [][]byte {
	tables := extractMarkdownTables(content)
	if len(tables) == 0 {
		return nil
	}
	var images [][]byte
	for _, t := range tables {
		data, err := renderTablePNG(t)
		if err != nil {
			slog.Warn("discord: render table image", "error", err)
			continue
		}
		images = append(images, data)
	}
	return images
}

func imgDrawString(img *image.RGBA, face font.Face, x, y int, text string, col color.RGBA) {
	d := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(col),
		Face: face,
		Dot:  fixed.Point26_6{X: fixed.I(x), Y: fixed.I(y)},
	}
	d.DrawString(text)
}

func imgFillRect(img *image.RGBA, x1, y1, x2, y2 int, col color.RGBA) {
	draw.Draw(img, image.Rect(x1, y1, x2, y2), image.NewUniform(col), image.Point{}, draw.Src)
}

func imgDrawHLine(img *image.RGBA, x1, x2, y int, col color.RGBA) {
	draw.Draw(img, image.Rect(x1, y, x2, y+1), image.NewUniform(col), image.Point{}, draw.Src)
}

func imgDrawBorder(img *image.RGBA, x1, y1, x2, y2 int, col color.RGBA) {
	u := image.NewUniform(col)
	draw.Draw(img, image.Rect(x1, y1, x2, y1+1), u, image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(x1, y2, x2, y2+1), u, image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(x1, y1, x1+1, y2+1), u, image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(x2, y1, x2+1, y2+1), u, image.Point{}, draw.Src)
}
