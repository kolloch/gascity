package tmux

import (
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Compile-time check: the tmux Provider implements the optional
// ParkedInputProvider capability.
var _ runtime.ParkedInputProvider = (*Provider)(nil)

// promptInputSuffix returns the text following the runtime prompt prefix on a
// captured pane line, plus whether line is a prompt line at all. It mirrors
// matchesPromptPrefix's NBSP (U+00A0) normalization so detection works whether
// the agent renders a regular space or a non-breaking space after its prompt
// glyph. The returned suffix is whitespace-trimmed: an empty idle prompt
// ("❯ ") yields ("", true), while a prompt holding input ("❯ do it") yields
// ("do it", true).
func promptInputSuffix(line, readyPromptPrefix string) (string, bool) {
	if readyPromptPrefix == "" {
		return "", false
	}
	normalized := strings.TrimSpace(strings.ReplaceAll(line, "\u00a0", " "))
	normalizedPrefix := strings.ReplaceAll(readyPromptPrefix, "\u00a0", " ")
	if strings.HasPrefix(normalized, normalizedPrefix) {
		return strings.TrimSpace(strings.TrimPrefix(normalized, normalizedPrefix)), true
	}
	// The prompt glyph may appear without its trailing space (the space was
	// trimmed, or the agent emitted a bare glyph). Treat the glyph alone as a
	// prompt line so an empty prompt is recognized rather than misread.
	bare := strings.TrimSpace(normalizedPrefix)
	if bare != "" && strings.HasPrefix(normalized, bare) {
		return strings.TrimSpace(strings.TrimPrefix(normalized, bare)), true
	}
	return "", false
}

// paneShowsParkedInput reports whether captured pane lines indicate the session
// is idle (not actively processing) while holding unsubmitted text in its input
// box. A "parked" session looks frozen: the agent will not act until the
// buffered input is submitted with Enter.
//
// Detection is best-effort and provider-specific:
//   - A processing indicator anywhere in the pane (see paneContainsBusyIndicator)
//     means the agent is busy, so the session is never reported as parked.
//   - Otherwise the LAST prompt line is the live input box; earlier
//     prompt-shaped lines are conversation scrollback. The session is parked
//     when that last prompt line carries non-whitespace content after the
//     prompt prefix.
//
// Limitation: this trusts that only the live input prompt renders with the
// configured prefix (the same assumption WaitForIdle relies on). If an agent
// renders dimmed placeholder text on the prompt line when the box is empty,
// that would read as parked input.
func paneShowsParkedInput(lines []string, readyPromptPrefix string) bool {
	if paneContainsBusyIndicator(lines) {
		return false
	}
	suffix := ""
	foundPrompt := false
	for _, line := range lines {
		if s, ok := promptInputSuffix(line, readyPromptPrefix); ok {
			suffix = s
			foundPrompt = true
		}
	}
	return foundPrompt && suffix != ""
}

// ParkedInput reports whether the named session appears idle while holding
// unsubmitted text in its input box. It satisfies
// [runtime.ParkedInputProvider]. The result is best-effort: it captures the
// pane, honors the session's configured ready-prompt prefix (falling back to
// the Claude default), and applies paneShowsParkedInput. An error is returned
// only when the pane cannot be captured.
func (p *Provider) ParkedInput(name string) (bool, error) {
	lines, err := p.tm.CapturePaneLines(name, promptObservationLines)
	if err != nil {
		return false, err
	}
	promptPrefix := DefaultReadyPromptPrefix
	if configured, err := p.tm.GetEnvironment(name, sessionReadyPromptEnvKey); err == nil {
		promptPrefix = idlePromptPrefix(configured)
	}
	return paneShowsParkedInput(lines, promptPrefix), nil
}
