package tmux

import "testing"

func TestPromptInputSuffix(t *testing.T) {
	const (
		nbsp          = "\u00a0"
		regularPrefix = "❯ "
	)
	tests := []struct {
		name       string
		line       string
		prefix     string
		wantSuffix string
		wantOK     bool
	}{
		{"empty prompt regular space", "❯ ", regularPrefix, "", true},
		{"empty prompt nbsp", "❯" + nbsp, regularPrefix, "", true},
		{"bare glyph no space", "❯", regularPrefix, "", true},
		{"prompt with input", "❯ drain the backlog", regularPrefix, "drain the backlog", true},
		{"prompt with nbsp input", "❯" + nbsp + "claude --help", regularPrefix, "claude --help", true},
		{"leading whitespace then prompt", "   ❯ status update", regularPrefix, "status update", true},
		{"trailing whitespace trimmed", "❯ hi   ", regularPrefix, "hi", true},
		{"not a prompt line", "some output", regularPrefix, "", false},
		{"empty line", "", regularPrefix, "", false},
		{"whitespace only", "   ", regularPrefix, "", false},
		{"empty prefix never matches", "❯ text", "", "", false},
		{"nbsp configured prefix matches regular line", "❯ hello", "❯" + nbsp, "hello", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			suffix, ok := promptInputSuffix(tt.line, tt.prefix)
			if ok != tt.wantOK {
				t.Fatalf("promptInputSuffix(%q, %q) ok = %v, want %v", tt.line, tt.prefix, ok, tt.wantOK)
			}
			if suffix != tt.wantSuffix {
				t.Errorf("promptInputSuffix(%q, %q) suffix = %q, want %q", tt.line, tt.prefix, suffix, tt.wantSuffix)
			}
		})
	}
}

func TestPaneShowsParkedInput(t *testing.T) {
	const prefix = "❯ "
	tests := []struct {
		name  string
		lines []string
		want  bool
	}{
		{"empty pane", nil, false},
		{"idle empty prompt", []string{"some history", "❯ ", ""}, false},
		{
			name:  "parked single line prompt",
			lines: []string{"assistant output", "❯ drain the backlog", "  ? for shortcuts"},
			want:  true,
		},
		{
			name:  "busy with text in box is not parked",
			lines: []string{"❯ drain the backlog", "  esc to interrupt  "},
			want:  false,
		},
		{
			name:  "busy gemini spinner is not parked",
			lines: []string{"❯ keep going", "Waiting for authentication... (Press Esc or Ctrl+C to cancel)"},
			want:  false,
		},
		{
			// A previously submitted turn may sit in scrollback as a prompt-shaped
			// line; the LIVE input box is the LAST prompt line. When it is empty,
			// the session is not parked even though scrollback holds text.
			name:  "scrollback prompt above empty live prompt",
			lines: []string{"❯ earlier submitted command", "assistant reply", "❯ "},
			want:  false,
		},
		{
			// Live prompt (last) holds text even though an earlier prompt is empty.
			name:  "scrollback empty prompt above parked live prompt",
			lines: []string{"❯ ", "assistant reply", "❯ new unsent text"},
			want:  true,
		},
		{
			name:  "no prompt visible (dialog/startup) is not parked",
			lines: []string{"Do you trust the files in this folder?", "1. Yes  2. No"},
			want:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := paneShowsParkedInput(tt.lines, prefix); got != tt.want {
				t.Errorf("paneShowsParkedInput(%v) = %v, want %v", tt.lines, got, tt.want)
			}
		})
	}
}
