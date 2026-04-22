package discord

import (
	"bytes"
	"image/png"
	"testing"
)

func TestExtractMarkdownTables(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantLen int
	}{
		{"no table", "hello world", 0},
		{"simple table", "| a | b |\n| - | - |\n| 1 | 2 |", 1},
		{"table inside code block skipped", "```\n| a | b |\n| - | - |\n| 1 | 2 |\n```", 0},
		{"multiple tables", "| a | b |\n| - | - |\n| 1 | 2 |\n\n| c | d |\n| - | - |\n| 3 | 4 |", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractMarkdownTables(tt.input)
			if len(got) != tt.wantLen {
				t.Errorf("extractMarkdownTables() = %d tables, want %d", len(got), tt.wantLen)
			}
		})
	}
}

func TestRenderTablePNG(t *testing.T) {
	td := tableData{
		headers: []string{"Name", "Language", "Stars"},
		rows: [][]string{
			{"cc-connect", "Go", "1.2k"},
			{"claude-code", "TypeScript", "5.8k"},
		},
	}
	data, err := renderTablePNG(td)
	if err != nil {
		t.Fatalf("renderTablePNG() error: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("renderTablePNG() returned empty data")
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("png.Decode() error: %v", err)
	}
	bounds := img.Bounds()
	if bounds.Dx() < 100 || bounds.Dy() < 50 {
		t.Errorf("image too small: %dx%d", bounds.Dx(), bounds.Dy())
	}
}

func TestRenderTableImages(t *testing.T) {
	images := renderTableImages("| a | b |\n| - | - |\n| 1 | 2 |")
	if len(images) != 1 {
		t.Errorf("renderTableImages() = %d images, want 1", len(images))
	}
}

func TestRenderTableImagesNoTable(t *testing.T) {
	images := renderTableImages("just text")
	if len(images) != 0 {
		t.Errorf("renderTableImages() = %d images, want 0", len(images))
	}
}
