package session

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// Resolution errors returned by ResolveSessionID.
var (
	ErrSessionNotFound = errors.New("session not found")
	ErrAmbiguous       = errors.New("ambiguous session identifier")
)

// ResolveSessionID resolves a user-provided identifier to a bead ID.
// It first attempts a direct store lookup; if the identifier exists as
// a session bead, it is returned immediately. Otherwise, it resolves against
// live identifiers: open exact session_name matches first, then open exact
// current alias matches. Normal session targeting does not fall through to
// template, agent_name, or historical alias compatibility identifiers.
// When a bead has both alias and session_name equal to the identifier, a
// separate session_name-only bead owns the identifier; the dual bead remains
// the session_name match only when no other session_name match exists.
//
// Returns ErrSessionNotFound if no live match is found, or ErrAmbiguous
// (wrapped with details) if multiple sessions match the identifier.
func ResolveSessionID(store beads.Store, identifier string) (string, error) {
	return resolveSessionID(store, identifier, false)
}

// ResolveSessionIDAllowClosed is the read-only variant of ResolveSessionID.
// When no live identifier claims the requested identifier, it falls back to
// closed exact alias and session_name matches so closed sessions remain
// inspectable by their stable current handles.
func ResolveSessionIDAllowClosed(store beads.Store, identifier string) (string, error) {
	return resolveSessionID(store, identifier, true)
}

// ResolveSessionBead resolves a user-provided identifier exactly like
// ResolveSessionID but also returns the session bead loaded during
// resolution. Callers that need the bead's fields immediately (for example to
// derive mailbox addresses) use this to avoid a redundant store.Get for the
// same ID. The returned bead has its empty type field repaired, matching what
// a fresh Get of the resolved ID would yield.
func ResolveSessionBead(store beads.Store, identifier string) (beads.Bead, string, error) {
	return resolveSessionBead(store, identifier, false)
}

// ResolveSessionIDByExactID resolves only direct bead ID matches.
func ResolveSessionIDByExactID(store beads.Store, identifier string) (string, error) {
	_, id, err := ResolveSessionBeadByExactID(store, identifier)
	return id, err
}

// ResolveSessionBeadByExactID is like ResolveSessionIDByExactID but also
// returns the loaded session bead, so callers that immediately need it can
// avoid a second store.Get.
func ResolveSessionBeadByExactID(store beads.Store, identifier string) (beads.Bead, string, error) {
	if store == nil {
		return beads.Bead{}, "", fmt.Errorf("session store unavailable")
	}
	b, err := store.Get(identifier)
	if err == nil && IsSessionBeadOrRepairable(b) {
		RepairEmptyType(store, &b)
		return b, b.ID, nil
	}
	if err != nil && !errors.Is(err, beads.ErrNotFound) {
		return beads.Bead{}, "", fmt.Errorf("looking up session %q: %w", identifier, err)
	}
	return beads.Bead{}, "", fmt.Errorf("%w: %q", ErrSessionNotFound, identifier)
}

func resolveSessionID(store beads.Store, identifier string, allowClosed bool) (string, error) {
	_, id, err := resolveSessionBead(store, identifier, allowClosed)
	return id, err
}

// resolveSessionBead is the bead-returning core of session resolution. It
// performs the same lookup sequence as resolveSessionID but also returns the
// session bead it loaded along the way, so callers needing the bead avoid a
// redundant store.Get for the same ID.
func resolveSessionBead(store beads.Store, identifier string, allowClosed bool) (beads.Bead, string, error) {
	if b, id, err := ResolveSessionBeadByExactID(store, identifier); err == nil {
		return b, id, nil
	} else if !errors.Is(err, ErrSessionNotFound) {
		return beads.Bead{}, "", err
	}

	lookupIdentifier := strings.TrimSpace(identifier)
	if lookupIdentifier == "" {
		return beads.Bead{}, "", fmt.Errorf("%w: %q", ErrSessionNotFound, identifier)
	}

	bySessionName, err := listSessionBeadsByMetadata(store, "session_name", lookupIdentifier, false)
	if err != nil {
		return beads.Bead{}, "", fmt.Errorf("listing sessions by session_name: %w", err)
	}
	bySessionName = filterOutAliasMatches(bySessionName, lookupIdentifier)
	if len(bySessionName) > 0 {
		return chooseSessionMatch(identifier, bySessionName)
	}

	byAlias, err := listSessionBeadsByMetadata(store, "alias", lookupIdentifier, false)
	if err != nil {
		return beads.Bead{}, "", fmt.Errorf("listing sessions by alias: %w", err)
	}
	if len(byAlias) > 0 {
		return chooseSessionMatch(identifier, byAlias)
	}
	if !allowClosed {
		return beads.Bead{}, "", fmt.Errorf("%w: %q", ErrSessionNotFound, identifier)
	}

	bySessionName, err = listSessionBeadsByMetadata(store, "session_name", lookupIdentifier, true)
	if err != nil {
		return beads.Bead{}, "", fmt.Errorf("listing closed sessions by session_name: %w", err)
	}
	bySessionName = filterOutAliasMatches(bySessionName, lookupIdentifier)
	openSessionName, closedSessionName := splitOpen(bySessionName)
	if len(openSessionName) > 0 {
		return chooseSessionMatch(identifier, openSessionName)
	}
	if len(closedSessionName) > 0 {
		return chooseSessionMatch(identifier, closedSessionName)
	}

	byAlias, err = listSessionBeadsByMetadata(store, "alias", lookupIdentifier, true)
	if err != nil {
		return beads.Bead{}, "", fmt.Errorf("listing closed sessions by alias: %w", err)
	}
	openAlias, closedAlias := splitOpen(byAlias)
	if len(openAlias) > 0 {
		return chooseSessionMatch(identifier, openAlias)
	}
	if len(closedAlias) > 0 {
		return chooseSessionMatch(identifier, closedAlias)
	}
	return beads.Bead{}, "", fmt.Errorf("%w: %q", ErrSessionNotFound, identifier)
}

func listSessionBeadsByMetadata(store beads.Store, key, value string, allowClosed bool) ([]beads.Bead, error) {
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	if key == "" || value == "" {
		return nil, nil
	}
	raw, err := store.List(beads.ListQuery{
		Metadata:      map[string]string{key: value},
		IncludeClosed: allowClosed,
	})
	if err != nil {
		return nil, err
	}
	out := make([]beads.Bead, 0, len(raw))
	for _, b := range raw {
		if !IsSessionBeadOrRepairable(b) {
			continue
		}
		RepairEmptyType(store, &b)
		out = append(out, b)
	}
	return out, nil
}

func filterOutAliasMatches(in []beads.Bead, identifier string) []beads.Bead {
	hasSessionNameOnlyMatch := false
	for _, b := range in {
		if strings.TrimSpace(b.Metadata["alias"]) != identifier {
			hasSessionNameOnlyMatch = true
			break
		}
	}
	if !hasSessionNameOnlyMatch {
		return in
	}
	// Demote dual alias/session_name beads only when another session_name
	// match can own the identifier; otherwise session_name still wins.
	out := in[:0]
	for _, b := range in {
		if strings.TrimSpace(b.Metadata["alias"]) == identifier {
			continue
		}
		out = append(out, b)
	}
	return out
}

func splitOpen(in []beads.Bead) (open, closed []beads.Bead) {
	for _, b := range in {
		if b.Status == "closed" {
			closed = append(closed, b)
			continue
		}
		open = append(open, b)
	}
	return open, closed
}

func chooseSessionMatch(identifier string, matches []beads.Bead) (beads.Bead, string, error) {
	switch len(matches) {
	case 0:
		return beads.Bead{}, "", fmt.Errorf("%w: %q", ErrSessionNotFound, identifier)
	case 1:
		return matches[0], matches[0].ID, nil
	default:
		var ids []string
		for _, m := range matches {
			ids = append(ids, fmt.Sprintf("%s (%s)", m.ID, sessionIdentifierLabel(m)))
		}
		return beads.Bead{}, "", fmt.Errorf("%w: %q matches %d sessions: %s", ErrAmbiguous, identifier, len(matches), strings.Join(ids, ", "))
	}
}

// hasSessionLabel returns true if the bead carries the gc:session label.
func hasSessionLabel(b beads.Bead) bool {
	for _, l := range b.Labels {
		if l == LabelSession {
			return true
		}
	}
	return false
}

// IsSessionBeadOrRepairable returns true if the bead is either a proper
// session bead (Type == "session") or a broken session bead (empty type
// but carries the gc:session label). The latter can occur after crashes
// or schema migrations that leave partially-written records.
func IsSessionBeadOrRepairable(b beads.Bead) bool {
	if b.Type == BeadType {
		return true
	}
	return b.Type == "" && hasSessionLabel(b)
}

// RepairEmptyType fixes a session bead with an empty type field by
// setting it to "session". This is a best-effort repair — if the store
// update fails, the in-memory bead is still patched so the current
// operation can proceed.
func RepairEmptyType(store beads.Store, b *beads.Bead) {
	if b.Type != "" {
		return
	}
	t := BeadType
	_ = store.Update(b.ID, beads.UpdateOpts{Type: &t})
	b.Type = BeadType
}

func sessionIdentifierLabel(b beads.Bead) string {
	for _, field := range []string{
		b.Metadata["alias"],
		b.Metadata["session_name"],
	} {
		if field != "" {
			return field
		}
	}
	if b.Metadata["template"] != "" {
		return b.Metadata["template"]
	}
	return b.Title
}
