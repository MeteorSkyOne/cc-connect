package discord

import "testing"

func TestSplitContentAroundTables(t *testing.T) {
	tests := []struct {
		name       string
		in         string
		wantCount  int
		wantTables int
		wantTexts  []string
	}{
		{
			name:       "no table",
			in:         "hello world\nno tables here",
			wantCount:  1,
			wantTables: 0,
			wantTexts:  []string{"hello world\nno tables here"},
		},
		{
			name:       "table only",
			in:         "| a | b |\n| - | - |\n| 1 | 2 |",
			wantCount:  1,
			wantTables: 1,
		},
		{
			name:       "text then table then text",
			in:         "before\n| a | b |\n| - | - |\n| 1 | 2 |\nafter",
			wantCount:  3,
			wantTables: 1,
			wantTexts:  []string{"before", "", "after"},
		},
		{
			name:       "table in code block stays as text",
			in:         "```\n| a | b |\n| 1 | 2 |\n```",
			wantCount:  1,
			wantTables: 0,
		},
		{
			name:       "multiple tables with text between",
			in:         "| a | b |\n| - | - |\n| 1 | 2 |\n\nmiddle\n| c | d |\n| - | - |\n| 3 | 4 |",
			wantCount:  3,
			wantTables: 2,
			wantTexts:  []string{"", "middle", ""},
		},
		{
			name:       "table segment has aligned code block",
			in:         "| Name | Val |\n| --- | --- |\n| short | 1 |\n| longername | 2 |",
			wantCount:  1,
			wantTables: 1,
			wantTexts:  []string{"```\n| Name       | Val |\n|------------|-----|\n| short      | 1   |\n| longername | 2   |\n```"},
		},
		{
			name:       "bold stripped in table segments",
			in:         "| Config | TPS |\n|---|---|\n| **Patched** | **~178,200** |",
			wantCount:  1,
			wantTables: 1,
			wantTexts:  []string{"```\n| Config  | TPS      |\n|---------|----------|\n| Patched | ~178,200 |\n```"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitContentAroundTables(tt.in)
			if len(got) != tt.wantCount {
				t.Errorf("got %d segments, want %d", len(got), tt.wantCount)
				for i, seg := range got {
					t.Logf("  segment[%d]: table=%v text=%q", i, seg.table != nil, seg.text)
				}
				return
			}

			tableCount := 0
			for _, seg := range got {
				if seg.table != nil {
					tableCount++
				}
			}
			if tableCount != tt.wantTables {
				t.Errorf("got %d table segments, want %d", tableCount, tt.wantTables)
			}

			if tt.wantTexts != nil {
				for i, want := range tt.wantTexts {
					if want != "" && got[i].text != want {
						t.Errorf("segment[%d].text:\n got: %q\nwant: %q", i, got[i].text, want)
					}
				}
			}
		})
	}
}

func TestStripInlineMarkdown(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"**bold**", "bold"},
		{"*italic*", "italic"},
		{"__underline bold__", "underline bold"},
		{"`code`", "code"},
		{"**~178,200**", "~178,200"},
		{"no markup", "no markup"},
		{"**mixed** and plain", "mixed and plain"},
	}
	for _, tt := range tests {
		got := stripInlineMarkdown(tt.in)
		if got != tt.want {
			t.Errorf("stripInlineMarkdown(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParseCells(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"| a | b |", []string{"a", "b"}},
		{"| hello world | foo |", []string{"hello world", "foo"}},
		{"|a|b|c|", []string{"a", "b", "c"}},
		{"| **bold** | normal |", []string{"bold", "normal"}},
	}
	for _, tt := range tests {
		got := parseCells(tt.input)
		if len(got) != len(tt.want) {
			t.Errorf("parseCells(%q) = %v, want %v", tt.input, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("parseCells(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
			}
		}
	}
}
