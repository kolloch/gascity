package config

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// ErrInPlaceTableNotFound is returned by the in-place TOML editors when the
// target table is absent from the document. Callers branch on it to fall back
// to a full re-marshal (which can create the table) or to surface a
// not-found error to the user.
var ErrInPlaceTableNotFound = errors.New("config: in-place edit target table not found")

// SetRigSuspendedInPlace returns city.toml content with the `suspended` key of
// the [[rigs]] table named `name` set to `suspended`, editing only that key's
// line and leaving every other byte — comments, key order, blank lines,
// formatting — untouched.
//
// When suspended is true the key is set to `true` (inserted at the end of the
// table's direct body if absent). When suspended is false the key line is
// removed entirely, matching the default-omitted form that the struct
// marshaler (suspended has `omitempty`) would otherwise emit; this makes a
// suspend→resume round trip byte-identical for the common case of a rig with
// no hand-written suspended line.
//
// It returns the input slice unchanged when the rig is already in the desired
// state (so a no-op write can be skipped), or ErrInPlaceTableNotFound wrapped
// with the rig name when no [[rigs]] table declares that name. The result is
// self-verified by re-parsing: if the edit would change anything other than
// the targeted rig's suspended flag, or produce invalid TOML, an error is
// returned and the original bytes are left untouched.
func SetRigSuspendedInPlace(raw []byte, name string, suspended bool) ([]byte, error) {
	out, changed, found := editSuspendedInPlace(raw, suspended, func(lines []string, tables []tomlTable) (tomlTable, bool) {
		return locateRigTable(lines, tables, name)
	})
	if !found {
		return nil, fmt.Errorf("%w: rig %q", ErrInPlaceTableNotFound, name)
	}
	if !changed {
		return raw, nil
	}
	if err := verifyRigSuspended(raw, out, name, suspended); err != nil {
		return nil, err
	}
	return out, nil
}

// SetWorkspaceSuspendedInPlace returns city.toml content with the `suspended`
// key of the [workspace] table set to `suspended`, with the same
// comment- and order-preserving guarantees as [SetRigSuspendedInPlace].
//
// It returns ErrInPlaceTableNotFound when the document has no [workspace]
// table, so the caller can fall back to a full re-marshal that creates one.
func SetWorkspaceSuspendedInPlace(raw []byte, suspended bool) ([]byte, error) {
	out, changed, found := editSuspendedInPlace(raw, suspended, locateWorkspaceTable)
	if !found {
		return nil, ErrInPlaceTableNotFound
	}
	if !changed {
		return raw, nil
	}
	if err := verifyWorkspaceSuspended(raw, out, suspended); err != nil {
		return nil, err
	}
	return out, nil
}

// SetAgentSuspendedInPlace returns city.toml content with the `suspended` key
// of the inline [[agent]] table identified by `identity` set to `suspended`,
// editing only that key's line with the same comment- and order-preserving
// guarantees as [SetRigSuspendedInPlace] — the agent counterpart.
//
// `identity` is matched against each [[agent]] table's (name, dir) keys with
// the canonical resolver [AgentMatchesIdentity], so a bare name and a
// fully-qualified "dir/name" both resolve exactly as the struct-level
// suspend path does. The `binding_name` key is intentionally ignored: it
// carries `toml:"-"`, so it is never present in city.toml and the TOML
// parser never reads it — reading it here would diverge from the parser.
// The first matching table wins, mirroring SetAgentSuspended.
//
// As with the rig editor, suspend=true sets (or inserts) `suspended = true`
// and suspend=false removes the line entirely, so a suspend→resume round
// trip on an agent with no hand-written suspended line is byte-identical.
//
// It returns the input unchanged when the agent is already in the desired
// state, or ErrInPlaceTableNotFound wrapped with the identity when no
// [[agent]] table matches — letting the caller fall through to the
// derived-agent path (agents/<name>/agent.toml or [[patches.agent]]) or a
// full re-marshal. The result is self-verified by re-parsing.
func SetAgentSuspendedInPlace(raw []byte, identity string, suspended bool) ([]byte, error) {
	out, changed, found := editSuspendedInPlace(raw, suspended, func(lines []string, tables []tomlTable) (tomlTable, bool) {
		return locateAgentTable(lines, tables, identity)
	})
	if !found {
		return nil, fmt.Errorf("%w: agent %q", ErrInPlaceTableNotFound, identity)
	}
	if !changed {
		return raw, nil
	}
	if err := verifyAgentSuspended(raw, out, identity, suspended); err != nil {
		return nil, err
	}
	return out, nil
}

// editSuspendedInPlace locates a table via `locate`, toggles its `suspended`
// key, and returns the rewritten bytes. changed reports whether any byte
// changed; found reports whether the target table was located. When found but
// unchanged it returns the original raw slice so callers preserve it exactly.
func editSuspendedInPlace(raw []byte, suspended bool, locate func([]string, []tomlTable) (tomlTable, bool)) (out []byte, changed, found bool) {
	lines := splitLinesKeepEnds(raw)
	tables := scanTables(lines)
	tbl, ok := locate(lines, tables)
	if !ok {
		return nil, false, false
	}
	newLines, changed := setSuspendedInTable(lines, tbl, suspended)
	if !changed {
		return raw, false, true
	}
	return []byte(strings.Join(newLines, "")), true, true
}

// --- table targeting ---

// tomlTable is the line span of a single TOML table header and its direct
// body — the lines between this header and the next header. Sub-tables (e.g.
// [rigs.imports] following [[rigs]]) are separate tomlTables, not part of this
// table's body, mirroring TOML's flat table model.
type tomlTable struct {
	headerIdx int    // index of the header line
	bodyStart int    // first body line index (headerIdx+1)
	bodyEnd   int    // exclusive end of the direct body (next header index, or len)
	path      string // dotted header path, e.g. "workspace", "rigs", "rigs.imports"
	isArray   bool   // true for [[array.of.tables]] headers
}

// locateRigTable returns the [[rigs]] table whose direct body declares
// name = "<name>".
func locateRigTable(lines []string, tables []tomlTable, name string) (tomlTable, bool) {
	for _, t := range tables {
		if t.isArray && t.path == "rigs" && rigTableName(lines, t) == name {
			return t, true
		}
	}
	return tomlTable{}, false
}

// locateWorkspaceTable returns the [workspace] table. The lines argument is
// unused but kept to satisfy the shared locate signature.
func locateWorkspaceTable(_ []string, tables []tomlTable) (tomlTable, bool) {
	for _, t := range tables {
		if !t.isArray && t.path == "workspace" {
			return t, true
		}
	}
	return tomlTable{}, false
}

// rigTableName returns the value of the `name` key in a rig table's direct
// body, or "" if absent.
func rigTableName(lines []string, t tomlTable) string {
	for i := t.bodyStart; i < t.bodyEnd; i++ {
		if k, v, ok := parseScalarStringKV(stripLineEnding(lines[i])); ok && k == "name" {
			return v
		}
	}
	return ""
}

// locateAgentTable returns the first [[agent]] table whose (name, dir) keys
// identify the agent matching `identity`, reusing the canonical
// [AgentMatchesIdentity] resolver so in-place matching is identical to the
// struct-level suspend path.
func locateAgentTable(lines []string, tables []tomlTable, identity string) (tomlTable, bool) {
	for _, t := range tables {
		if !t.isArray || t.path != "agent" {
			continue
		}
		a := agentTableIdentity(lines, t)
		if AgentMatchesIdentity(&a, identity) {
			return t, true
		}
	}
	return tomlTable{}, false
}

// agentTableIdentity reads the identity-bearing keys (name, dir) from an
// [[agent]] table's direct body into an Agent so the shared
// [AgentMatchesIdentity] resolver can be reused. BindingName is left zero:
// it is `toml:"-"`, so it never appears in city.toml and the parser never
// populates it — reading it here would diverge from the parser's view.
func agentTableIdentity(lines []string, t tomlTable) Agent {
	var a Agent
	for i := t.bodyStart; i < t.bodyEnd; i++ {
		k, v, ok := parseScalarStringKV(stripLineEnding(lines[i]))
		if !ok {
			continue
		}
		switch k {
		case "name":
			a.Name = v
		case "dir":
			a.Dir = v
		}
	}
	return a
}

// --- line editing ---

// setSuspendedInTable toggles the `suspended` key within tbl's direct body and
// returns the new lines plus whether anything changed.
func setSuspendedInTable(lines []string, tbl tomlTable, suspended bool) ([]string, bool) {
	suspIdx := -1
	for i := tbl.bodyStart; i < tbl.bodyEnd; i++ {
		if isSuspendedKeyLine(stripLineEnding(lines[i])) {
			suspIdx = i
			break
		}
	}
	if suspended {
		return setSuspendedTrue(lines, tbl, suspIdx)
	}
	return removeSuspendedLine(lines, suspIdx)
}

// setSuspendedTrue sets an existing suspended line to true (preserving its
// indentation and line terminator) or inserts a new `suspended = true` line at
// the end of the table's direct body.
func setSuspendedTrue(lines []string, tbl tomlTable, suspIdx int) ([]string, bool) {
	if suspIdx >= 0 {
		cur := lines[suspIdx]
		newLine := leadingWhitespace(stripLineEnding(cur)) + "suspended = true" + lineTerminator(cur)
		if newLine == cur {
			return lines, false
		}
		out := append([]string(nil), lines...)
		out[suspIdx] = newLine
		return out, true
	}
	insAfter := lastContentLineIdx(lines, tbl)
	indent := leadingWhitespace(stripLineEnding(lines[insAfter]))
	nl := detectNewline(lines)
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:insAfter+1]...)
	// The line we insert after must end with a newline so the inserted key
	// lands on its own line. The only case where it doesn't is an
	// unterminated final line at EOF — append a newline there.
	if !strings.HasSuffix(out[insAfter], "\n") {
		out[insAfter] += nl
	}
	out = append(out, indent+"suspended = true"+nl)
	out = append(out, lines[insAfter+1:]...)
	return out, true
}

// removeSuspendedLine drops the suspended line at suspIdx, if present.
func removeSuspendedLine(lines []string, suspIdx int) ([]string, bool) {
	if suspIdx < 0 {
		return lines, false
	}
	out := make([]string, 0, len(lines)-1)
	out = append(out, lines[:suspIdx]...)
	out = append(out, lines[suspIdx+1:]...)
	return out, true
}

// lastContentLineIdx returns the index of the last non-blank, non-comment line
// in tbl's direct body, falling back to the header line when the body has no
// content. New keys are inserted after this line.
func lastContentLineIdx(lines []string, tbl tomlTable) int {
	last := tbl.headerIdx
	for i := tbl.bodyStart; i < tbl.bodyEnd; i++ {
		s := strings.TrimSpace(stripLineEnding(lines[i]))
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		last = i
	}
	return last
}

// --- verification ---

// verifyRigSuspended re-parses the original and edited bytes and confirms that
// the only semantic change is the named rig's suspended flag reaching the
// desired value. A mismatch or parse failure means the surgical edit went
// wrong, so the caller must discard the result.
func verifyRigSuspended(raw, out []byte, name string, suspended bool) error {
	before, after, err := parseEditPair(raw, out)
	if err != nil {
		return err
	}
	bi, bok := rigIndex(before, name)
	ai, aok := rigIndex(after, name)
	if !bok || !aok {
		return fmt.Errorf("in-place edit lost rig %q", name)
	}
	if after.Rigs[ai].Suspended != suspended {
		return fmt.Errorf("in-place edit did not set rig %q suspended=%v", name, suspended)
	}
	before.Rigs[bi].Suspended = false
	after.Rigs[ai].Suspended = false
	if !reflect.DeepEqual(before, after) {
		return fmt.Errorf("in-place edit changed config beyond rig %q suspended flag", name)
	}
	return nil
}

// verifyWorkspaceSuspended is the [workspace] counterpart of
// verifyRigSuspended.
func verifyWorkspaceSuspended(raw, out []byte, suspended bool) error {
	before, after, err := parseEditPair(raw, out)
	if err != nil {
		return err
	}
	if after.Workspace.Suspended != suspended {
		return fmt.Errorf("in-place edit did not set workspace suspended=%v", suspended)
	}
	before.Workspace.Suspended = false
	after.Workspace.Suspended = false
	if !reflect.DeepEqual(before, after) {
		return fmt.Errorf("in-place edit changed config beyond the workspace suspended flag")
	}
	return nil
}

// verifyAgentSuspended is the [[agent]] counterpart of verifyRigSuspended:
// it re-parses both byte slices and confirms the only semantic change is the
// targeted agent's suspended flag reaching the desired value.
func verifyAgentSuspended(raw, out []byte, identity string, suspended bool) error {
	before, after, err := parseEditPair(raw, out)
	if err != nil {
		return err
	}
	bi, bok := agentIndex(before, identity)
	ai, aok := agentIndex(after, identity)
	if !bok || !aok {
		return fmt.Errorf("in-place edit lost agent %q", identity)
	}
	if after.Agents[ai].Suspended != suspended {
		return fmt.Errorf("in-place edit did not set agent %q suspended=%v", identity, suspended)
	}
	before.Agents[bi].Suspended = false
	after.Agents[ai].Suspended = false
	if !reflect.DeepEqual(before, after) {
		return fmt.Errorf("in-place edit changed config beyond agent %q suspended flag", identity)
	}
	return nil
}

// parseEditPair parses the pre- and post-edit bytes. A failure to parse the
// original is reported distinctly from corruption introduced by the edit.
func parseEditPair(raw, out []byte) (before, after *City, err error) {
	before, err = Parse(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("verifying in-place edit: parsing original config: %w", err)
	}
	after, err = Parse(out)
	if err != nil {
		return nil, nil, fmt.Errorf("in-place edit produced invalid TOML: %w", err)
	}
	return before, after, nil
}

func rigIndex(c *City, name string) (int, bool) {
	for i := range c.Rigs {
		if c.Rigs[i].Name == name {
			return i, true
		}
	}
	return 0, false
}

// agentIndex returns the index of the first agent matching `identity`, using
// the same resolver as locateAgentTable so verification targets the agent
// the edit actually changed.
func agentIndex(c *City, identity string) (int, bool) {
	for i := range c.Agents {
		if AgentMatchesIdentity(&c.Agents[i], identity) {
			return i, true
		}
	}
	return 0, false
}

// --- TOML-aware line scanning ---

// lexState tracks cross-line value context so that brackets and headers inside
// multi-line arrays or multi-line strings are not mistaken for table headers.
type lexState struct {
	mlString     string // "", `"""`, or `'''` while inside a multi-line string
	bracketDepth int    // unclosed `[` in array values
}

// scanTables returns every table header in the document with its direct-body
// line span. Lines inside multi-line array or string values are skipped for
// header detection via lexState.
func scanTables(lines []string) []tomlTable {
	type hdr struct {
		idx     int
		path    string
		isArray bool
	}
	var hdrs []hdr
	var st lexState
	for i, ln := range lines {
		body := stripLineEnding(ln)
		if st.mlString == "" && st.bracketDepth == 0 {
			if path, isArr, ok := parseTableHeader(strings.TrimLeft(body, " \t")); ok {
				hdrs = append(hdrs, hdr{idx: i, path: path, isArray: isArr})
				continue // a header line carries no value tokens to lex
			}
		}
		advanceLexLine(body, &st)
	}
	tables := make([]tomlTable, len(hdrs))
	for j, h := range hdrs {
		end := len(lines)
		if j+1 < len(hdrs) {
			end = hdrs[j+1].idx
		}
		tables[j] = tomlTable{
			headerIdx: h.idx,
			bodyStart: h.idx + 1,
			bodyEnd:   end,
			path:      h.path,
			isArray:   h.isArray,
		}
	}
	return tables
}

// parseTableHeader recognizes a `[table]` or `[[array.of.tables]]` header at
// the start of trimmed, returning the dotted path and whether it is an
// array-of-tables header. A trailing comment after the closing bracket is
// ignored. Quoted keys containing `]` are not supported (none appear in
// city.toml).
func parseTableHeader(trimmed string) (path string, isArray, ok bool) {
	if strings.HasPrefix(trimmed, "[[") {
		end := strings.Index(trimmed, "]]")
		if end < 0 {
			return "", false, false
		}
		return strings.TrimSpace(trimmed[2:end]), true, true
	}
	if strings.HasPrefix(trimmed, "[") {
		end := strings.IndexByte(trimmed, ']')
		if end < 0 {
			return "", false, false
		}
		return strings.TrimSpace(trimmed[1:end]), false, true
	}
	return "", false, false
}

// advanceLexLine updates st across one line's value tokens, tracking
// single-line and multi-line strings, comments, and array brackets so that
// scanTables can tell statement-level lines from value continuations.
func advanceLexLine(s string, st *lexState) {
	i := 0
	for i < len(s) {
		if st.mlString != "" {
			if strings.HasPrefix(s[i:], st.mlString) {
				i += 3
				st.mlString = ""
				continue
			}
			i++
			continue
		}
		switch {
		case s[i] == '#':
			return // comment runs to end of line
		case strings.HasPrefix(s[i:], `"""`):
			st.mlString = `"""`
			i += 3
		case strings.HasPrefix(s[i:], `'''`):
			st.mlString = `'''`
			i += 3
		case s[i] == '"':
			i = skipBasicString(s, i)
		case s[i] == '\'':
			i = skipLiteralString(s, i)
		case s[i] == '[':
			st.bracketDepth++
			i++
		case s[i] == ']':
			if st.bracketDepth > 0 {
				st.bracketDepth--
			}
			i++
		default:
			i++
		}
	}
}

// skipBasicString returns the index just past a single-line basic string that
// starts at s[i] == '"', honoring backslash escapes.
func skipBasicString(s string, i int) int {
	i++ // opening quote
	for i < len(s) {
		switch s[i] {
		case '\\':
			i += 2
		case '"':
			return i + 1
		default:
			i++
		}
	}
	return i
}

// skipLiteralString returns the index just past a single-line literal string
// that starts at s[i] == '\” (literal strings have no escapes).
func skipLiteralString(s string, i int) int {
	i++ // opening quote
	for i < len(s) && s[i] != '\'' {
		i++
	}
	if i < len(s) {
		i++ // closing quote
	}
	return i
}

// --- key/value parsing ---

// isSuspendedKeyLine reports whether body is a `suspended = ...` assignment
// (ignoring leading whitespace). Commented-out lines and keys merely prefixed
// with "suspended" (e.g. suspended_until) do not match.
func isSuspendedKeyLine(body string) bool {
	line := strings.TrimLeft(body, " \t")
	rest := strings.TrimPrefix(line, "suspended")
	if rest == line {
		return false
	}
	rest = strings.TrimLeft(rest, " \t")
	return strings.HasPrefix(rest, "=")
}

// parseScalarStringKV parses a `key = "value"` or `key = 'value'` assignment,
// returning the bare key and the unquoted string value. Non-string values and
// comment lines return ok=false.
func parseScalarStringKV(body string) (key, val string, ok bool) {
	line := strings.TrimLeft(body, " \t")
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	eq := strings.IndexByte(line, '=')
	if eq < 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:eq])
	val, ok = parseTOMLStringValue(strings.TrimLeft(line[eq+1:], " \t"))
	if !ok {
		return "", "", false
	}
	return key, val, true
}

// parseTOMLStringValue extracts the contents of a leading basic ("...") or
// literal ('...') TOML string from rest. It is intentionally minimal — enough
// to read identifier-like values such as rig names — and treats a backslash in
// a basic string as escaping the next byte literally.
func parseTOMLStringValue(rest string) (string, bool) {
	if rest == "" {
		return "", false
	}
	switch rest[0] {
	case '\'':
		if end := strings.IndexByte(rest[1:], '\''); end >= 0 {
			return rest[1 : 1+end], true
		}
		return "", false
	case '"':
		var sb strings.Builder
		for i := 1; i < len(rest); i++ {
			if rest[i] == '\\' && i+1 < len(rest) {
				sb.WriteByte(rest[i+1])
				i++
				continue
			}
			if rest[i] == '"' {
				return sb.String(), true
			}
			sb.WriteByte(rest[i])
		}
		return "", false
	default:
		return "", false
	}
}

// --- byte/line helpers ---

// splitLinesKeepEnds splits raw into lines that retain their trailing newline
// (and any carriage return), so strings.Join(lines, "") reconstructs the input
// byte-for-byte. The final line is included without a newline when the input
// does not end in one.
func splitLinesKeepEnds(raw []byte) []string {
	s := string(raw)
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i+1])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// stripLineEnding removes a trailing \n and \r from a line retrieved from
// splitLinesKeepEnds.
func stripLineEnding(s string) string {
	s = strings.TrimSuffix(s, "\n")
	return strings.TrimSuffix(s, "\r")
}

// lineTerminator returns the trailing line terminator of s ("\r\n", "\n", or "").
func lineTerminator(s string) string {
	if strings.HasSuffix(s, "\r\n") {
		return "\r\n"
	}
	if strings.HasSuffix(s, "\n") {
		return "\n"
	}
	return ""
}

// detectNewline returns the newline style used by the document ("\r\n" or
// "\n"), defaulting to "\n" when there is no terminated line.
func detectNewline(lines []string) string {
	for _, ln := range lines {
		if strings.HasSuffix(ln, "\r\n") {
			return "\r\n"
		}
		if strings.HasSuffix(ln, "\n") {
			return "\n"
		}
	}
	return "\n"
}

// leadingWhitespace returns the run of spaces and tabs at the start of s.
func leadingWhitespace(s string) string {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return s[:i]
}
