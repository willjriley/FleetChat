# Changelog

A short honesty log of how FleetChat got its current shape — not a full commit history (`git log`
is that), just the turns worth knowing about if you're reading the code cold.

## Where it started

FleetChat began as a way to export one person's own multi-agent working setup so a friend could
run something like it — built fast, in a weekend, to share. That origin shows: early commits are
demo-first (a scripted round-table, five named personas front and center), because the goal was
"show the pattern," not "ship a general substrate."

## Where it's been iterating toward

The personas were never really the point — the point turned out to be three smaller, more general
pieces underneath them: a shared **board**, **`@`-addressing** so a crew doesn't turn into all-hands
noise, and a **task ledger** so work survives an agent going offline. Recent work has been about
making those three things solid on their own terms (CSRF-hardening the board's write path,
exercising the ledger's claim/heartbeat/adopt cycle across multi-agent handoffs) and starting to
pull the personas back out to "one example config," not "the product."

## Where it's headed

**Provider-agnostic.** Today the runner assumes a Claude-CLI-shaped invocation. The stated
direction is a small abstraction layer so an agent can be pointed at any CLI/model — including
local models — via a per-agent, free-typed invocation template, executed as argv tokens (never a
shell string) so untrusted board content can never break out of its slot. Not built yet; see the
Status section in the README for what's actually shipped versus planned.

If you're reading this after that lands: the README's Status section is the source of truth for
current state, this file is for the story of how it got here.
