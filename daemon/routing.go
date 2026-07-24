package main

import (
	"regexp"
	"strings"
)

// Routing is now STRUCTURED, not derived from prose. This is the fix for the
// anti-pattern that caused the wake-cycle: previously routing scanned a
// message's body text for "@name", so an agent mentioning another agent in
// passing ("not waking @dave") actually woke them, whose reply mentioned a
// third, and so on. Now who a message NOTIFIES comes from an explicit
// recipient list on the message -- humans set it from the composer's chips,
// agents set it with a >>to: directive (see splitDirective). An @name written
// in the body is DISPLAY ONLY and never wakes anyone.

// resolveRecipients decides who a message wakes, from the structured `to`
// list -- never by scanning prose:
//   - to contains "all" (any case) -> every agent
//   - to non-empty          -> exactly the named agents that are real crew
//   - to empty + human sender (not a known agent) -> every agent (the
//     operator's plain message reaches the crew by default)
//   - to empty + agent sender -> NOBODY (an agent's reply notifies no one
//     unless it explicitly addresses someone; this is what makes the board
//     cycle-proof)
//
// The sender is never woken by their own message.
func resolveRecipients(sender string, to []string, agentIDs []string) map[string]bool {
	out := map[string]bool{}
	// "board" is a reserved system sender for announcements (restart notices,
	// etc.) -- never a participant, so it never wakes anyone regardless of `to`.
	if sender == "board" {
		return out
	}
	idset := make(map[string]bool, len(agentIDs))
	senderIsAgent := false
	for _, id := range agentIDs {
		idset[id] = true
		if id == sender {
			senderIsAgent = true
		}
	}

	wakeAll := func() {
		for _, id := range agentIDs {
			if id != sender {
				out[id] = true
			}
		}
	}

	if len(to) == 0 {
		if !senderIsAgent {
			wakeAll() // human/operator default: broadcast to the crew
		}
		return out // agent default: nobody
	}
	for _, name := range to {
		n := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(name), "@")))
		if n == "all" {
			wakeAll()
			continue
		}
		if n != "" && n != sender && idset[n] {
			out[n] = true
		}
	}
	return out
}

// toDirectiveRe matches a leading routing directive on an agent's reply:
// ">>to: carol, dave" / ">>to: all" / ">>to:" (empty = explicitly nobody).
// Deliberately a distinctive token at line start, NOT a prose @-scan -- that
// is the whole point.
var toDirectiveRe = regexp.MustCompile(`(?i)^\s*>>\s*to\s*:\s*(.*)$`)

// splitDirective pulls a leading >>to: directive off an agent's reply and
// returns (recipients, body-with-the-directive-line-removed). If there is no
// directive, recipients is nil (the caller's agent-sender default -> nobody)
// and the body is unchanged. Only the FIRST line is inspected: routing is one
// explicit field, never something scattered through the message.
func splitDirective(text string) (to []string, body string) {
	first, rest := text, ""
	if nl := strings.IndexByte(text, '\n'); nl >= 0 {
		first, rest = text[:nl], text[nl+1:]
	}
	m := toDirectiveRe.FindStringSubmatch(first)
	if m == nil {
		return nil, text // no directive -> no recipients, whole text is body
	}
	to = []string{} // directive present but possibly empty -> explicit "nobody"
	for _, part := range strings.FieldsFunc(m[1], func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		if p := strings.TrimSpace(strings.TrimPrefix(part, "@")); p != "" {
			to = append(to, p)
		}
	}
	return to, strings.TrimLeft(rest, "\n")
}
