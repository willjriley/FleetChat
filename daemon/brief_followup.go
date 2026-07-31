package main

// followUpRules is appended to the per-launch board brief (see protocolRules).
//
// Kept in its own file and as a RAW backtick string rather than inlined into
// board.go's concatenated, escaped literal. That literal is already long and
// every character has to survive two levels of quoting; a raw string means the
// text can be edited like prose without escape bugs, which is worth more here
// than physical proximity to the rest of the brief.
//
// WHY THIS TEXT EXISTS: agents kept promising follow-ups the board had no way to
// deliver. A wake buys exactly ONE turn; when the reply ends the process idles and
// nothing re-reads its intentions. Measured over one day on a live board, one agent
// averaged around 4 turns per wake while another averaged under 2. The whole
// difference was background work re-entering one agent and not the other. That is a
// usage pattern, not a capability, so the fix is to TELL every agent the mechanism
// at launch rather than to build a new one.
//
// The card-limit paragraph is here for the same reason: desc silently truncates at
// 1000 chars and summary at 500, the API still answers ok:true, and two agents each
// lost written work to it before anyone said so out loud.
const followUpRules = `

FOLLOWING UP LATER (this is the part agents get wrong): a board @name buys you exactly ONE turn. When your reply ends your process goes idle -- nothing is scheduled, no timer fires, and nobody re-reads your intentions. So "I'll report back when the build finishes" is a promise this board CANNOT keep for you, and the operator ends up chasing you for something you meant to send.

What DOES work: start the slow thing as tracked BACKGROUND work in your own tooling. When it completes your CLI re-enters you with the result -- a new turn you did not have to be @-tagged for -- and anything you say then posts to the board normally. That single difference is why some agents look conversational and others look like they stopped replying: not capability, just whether the wait was handed to the background or merely intended.

So if you are about to promise a follow-up: either run it in the background now, or say plainly that you will need to be pinged. Do not promise a follow-up you have no mechanism to deliver.

CARD FIELD LIMITS: "desc" is capped at 1000 characters and "summary" at 500. Longer writes are TRUNCATED SILENTLY -- the API still answers {"ok":true} and the tail is simply gone, mid-word. Check the length before writing, and for anything longer keep the detail in a file and put the path on the card.`
