package main

import (
	"regexp"
	"strings"
)

// Routing wakes only who a message EXPLICITLY addresses. Two ways to address:
// the structured `to` list (humans set it from the composer; agents may set it
// with a >>to: directive -- see splitDirective), AND a deliberate @name in the
// body (mergeAtMentions) that names a real crew member (or @all). A BARE name --
// no @ -- is display only and wakes no one, so an agent mentioning "dave" in
// passing never summons him; only a deliberate "@dave" does.
//
// That @-sigil rule stops INCIDENTAL mentions from waking (the original bug: any
// "@dave" in prose, even "not waking @dave", woke him). It is NOT a full loop
// breaker: a deliberate @-acknowledgment chain (A "@B thanks" -> B "@A np" -> ...)
// can still ping-pong, held in check only by the PASS convention (agents are told
// not to @ anyone for a bare acknowledgment). If a real loop ever shows up, the
// [route] debug log now records every @-wake source (see board.go Post), so add a
// mechanical breaker then rather than pre-emptively.

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

// atMentionRe finds a DELIBERATE @name in a message body. It mirrors the
// front-end's target parser (server/web/index.html parseTargets) exactly, so the
// chips a human sees == who the daemon wakes: a leading boundary (start / space /
// "(" / ">"), the '@' sigil, a 2-32 char id, and a trailing word boundary. The
// leading boundary is what makes it deliberate -- it does NOT fire inside an
// address like "bob@carol.com", nor on a bare name, so incidental or quoted
// mentions never wake anyone. Group 1 is the name ("all" for the broadcast).
var atMentionRe = regexp.MustCompile(`(?i)(?:^|[\s(>])@([a-z0-9_-]{2,32})\b`)

// Contexts an @name is quoting, not addressing: a fenced code block (``` or ~~~),
// an inline `code` span, and a blockquote line (leading ">"). A pasted diff/log
// or a quoted teammate must not wake anyone -- and since the UI deliberately does
// NOT chip mentions inside code, a wake from there would show NO chip to explain
// it. addressableText blanks these out before the scan; the UI's parseTargets
// mirrors the same exclusion so composer chips still match who the daemon wakes.
var (
	fencedCodeRe = regexp.MustCompile("(?s)```.*?```|~~~.*?~~~")
	inlineCodeRe = regexp.MustCompile("`[^`]*`")
)

func addressableText(s string) string {
	s = fencedCodeRe.ReplaceAllString(s, " ")
	s = inlineCodeRe.ReplaceAllString(s, " ")
	// Blank whole blockquote lines (first non-space char is ">") -- quoting a
	// message that named someone must not re-summon them.
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), ">") {
			lines[i] = ""
		}
	}
	return strings.Join(lines, "\n")
}

// mergeAtMentions adds any @name in the body that names a real crew member (or
// @all) to the recipient list, so "@dave …" wakes dave alongside whatever the
// structured `to` already carries. Case-insensitive; de-duplicated against `to`.
// Code spans and quoted lines are excluded first (see addressableText), so a
// pasted diff or a quoted message never wakes a teammate.
// unknownMentions returns @names in the body that match NO crew id and are not
// "all". They wake nobody, which is exactly the failure this exists to surface:
// in practice this happens when a second identity exists for the same person
// (say "alice" and "alice-cli"): replies go to the name the sender remembers
// rather than the one on this board, and every one of them reaches no one. The
// board delivers correctly each time; the ADDRESS is wrong, and nothing says so.
//
// Deliberately reuses addressableText and atMentionRe so it sees exactly what
// mergeAtMentions sees: a mention inside a code or quote span is display-only
// and must not be reported as a failed address.
// resolveAlias maps a second name for the same agent onto its board id. Two
// identities for one person is the condition that makes misaddressing possible
// at all: replies go to the name the sender remembers, not the one the board
// knows, and the board correctly delivers them to nobody.
//
// Aliases are read from the crew file (see FleetConfig.Aliases) so they live
// with the crew, in one place, outside the repo. An alias pointing at a name
// that is not on the crew is ignored rather than honoured -- an alias must
// resolve to a REAL recipient or it just moves the failure one step along.
func resolveAlias(name string, aliases map[string]string, agentIDs []string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	target, ok := aliases[n]
	if !ok {
		return n
	}
	t := strings.ToLower(strings.TrimSpace(target))
	for _, id := range agentIDs {
		if strings.ToLower(id) == t {
			return t
		}
	}
	return n // alias points at nobody: leave it unresolved so it is REPORTED
}

func unknownMentions(text string, agentIDs []string) []string {
	if text == "" {
		return nil
	}
	idset := make(map[string]bool, len(agentIDs))
	for _, id := range agentIDs {
		idset[strings.ToLower(id)] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, mm := range atMentionRe.FindAllStringSubmatch(addressableText(text), -1) {
		n := strings.ToLower(mm[1])
		if n == "all" || idset[n] || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

func mergeAtMentions(to []string, text string, agentIDs []string) []string {
	if text == "" {
		return to
	}
	text = addressableText(text)
	idset := make(map[string]bool, len(agentIDs))
	for _, id := range agentIDs {
		idset[strings.ToLower(id)] = true
	}
	have := make(map[string]bool, len(to))
	for _, t := range to {
		have[strings.ToLower(strings.TrimPrefix(strings.TrimSpace(t), "@"))] = true
	}
	for _, mm := range atMentionRe.FindAllStringSubmatch(text, -1) {
		n := strings.ToLower(mm[1])
		if (n == "all" || idset[n]) && !have[n] {
			to = append(to, n)
			have[n] = true
		}
	}
	return to
}
