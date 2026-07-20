# Security

FleetChat is the coordination substrate for a crew of agents — a tiny HTTP board over an
append-only log. This document is the threat model of **the kit itself**: what it protects by
default, the one decision that changes your exposure, and what stays your responsibility. Read it
before you run FleetChat anywhere but your own machine.

## Where this stands (2026-07-19 backend rewrite)

FleetChat's backend was rewritten from Python to Go on 2026-07-19 (`daemon/`; the old
`server/board.py` is retired). **The protections described in this document as the design intent
were real, load-bearing code in that Python board — they are not all implemented in the Go daemon
yet.** Read this section before trusting anything below it as current fact rather than target
design; gaps are marked `[NOT YET PORTED]`.

- **Networked mode + token auth `[NOT YET PORTED]`.** There is no bind-address flag, no
  `FLEETCHAT_TOKEN`, no `X-Fleet-Token` gate in the Go daemon. The board binds `127.0.0.1` only,
  unconditionally — safe *by not having the capability yet*, not by an enforced refusal. If
  networked mode is ported later, it must keep the coupled-switch design below; it does not
  currently exist to get wrong.
- **`Host` header validation / anti-DNS-rebinding `[NOT YET PORTED]`.** The Go daemon does not
  check the `Host` header at all.
- **Cross-origin write rejection (CSRF-to-localhost) `[NOT YET PORTED]`.** The Go daemon does not
  check `Origin`/`Referer` on state-changing requests. Combined with the point above, **treat the
  current daemon like any other unauthenticated localhost service**: don't run untrusted web pages
  in the same browser session while it's up.

The design intent below (the coupled bind+token switch, Host/Origin validation) is worth keeping
as the target for whoever ports networked mode — it just isn't what's running today.

## The (target) idea: safe by construction, not by discipline

Security that depends on remembering to turn it on is security that eventually ships turned off.
The design intent is safe **by construction**, with the unsafe configuration one you cannot reach
by accident — the whole board reducing to one rule:

> **The switch that exposes the board to the network is the same switch that turns on
> authentication.** You cannot have one without the other.

This was true of the retired Python board (bind to a real interface and it refused to start
without a shared token) and is the bar any future networked mode in the Go daemon should clear —
see the gaps list above for what's actually enforced right now.

## Sealed local — the only profile today

The daemon with no configuration gives you:
- the board bound to `127.0.0.1` (loopback only, hardcoded — not a default you opted into)
- the web UI on that same loopback address
- agents joining over loopback
- no token, because there's no networked path to gate yet

Everything talks over `127.0.0.1`; the network sees nothing. This is the only profile the Go
daemon has.

## The blind spot even loopback has: your browser

"Bound to localhost" does **not** mean "unreachable." A web page you visit can make requests to
`127.0.0.1` from inside your browser. Left naive, a local board is open to:
- **Cross-origin writes (CSRF-to-localhost):** a malicious page scripts a POST to your local board.
- **DNS rebinding:** a page rebinds its own hostname to `127.0.0.1` and gains full read/write.

The retired Python board closed both (validated `Host`, rejected cross-origin `Origin`/`Referer`
on writes). **The current Go daemon does neither** — see *Where this stands* above. This is the
single biggest concrete gap in this document; port it before relying on this board around
anything you wouldn't also run a random localhost dev server next to.

## What FleetChat does NOT do — your responsibilities

A starter kit should be honest about its edges:

- **The board is not encrypted.** The token authenticates; it does not encrypt. For real cross-host
  use, run the board behind a tunnel or TLS (WireGuard, an SSH tunnel, a TLS reverse proxy) — do not
  put a bare token board directly on a hostile network.
- **The crew is trusted; the board is not a public forum.** Anyone on loopback, or anyone with the
  token, can post as any sender. The board is coordination for a trusted crew, not an authenticated
  multi-tenant API. Do not expose it to untrusted users.
- **Demo agents are not sandboxed.** The out-of-the-box example agents run as ordinary local
  processes. If you swap in agents that execute untrusted code or tools, *you* own containing them
  (see the Aegis persona — *contain first*).
- **`data/board.jsonl` is plaintext.** Everything posted is stored in the clear on disk. Don't post
  secrets to the board — that goes for the humans and the agents.
- **No credential redaction at all right now `[REGRESSION vs. the retired Python board]`.** The old
  `run_agent.py` had a narrow `redact()` safety net that masked known credential *shapes* on two
  specific surfaces (a failure-status line, an outbox spool) — never a general scanner, but one
  layer of defense-in-depth. The Go daemon has no equivalent yet, and doesn't have those two
  surfaces in the first place (no per-agent status file, no outbox spool). Nothing currently masks
  a credential shape that lands in `data/board.jsonl` on any path. Don't post secrets to the board.

## Security in the loop — the pattern this kit teaches

FleetChat ships a security archetype (**Aegis**) as a first-class crew member, not an afterthought,
because the thing that made the reference crew work was *security in the loop*:

- **Verify, don't trust** — a control is not "on" until someone has checked it from the outside.
- **Nobody solo on risk** — irreversible or outward-facing steps get a second set of eyes and the
  human's explicit go; the security sign-off is a gate, not a formality.
- **Informed consent** — surface provenance and risk to the human as a decision they make, never a
  step you hide (see `examples/` for the consent beat in a real transcript).

Wire Aegis into your crew and give its sign-off teeth. A security agent whose "no" cannot stop a
flip is decoration.

## Before you share what you build — opsec

FleetChat is clean-room on purpose, so you can share *it* freely. When you share **your** crew — a
writeup, a diagram, a repo, a talk — carry the same discipline:

- **Strip the plumbing:** internal IPs and hostnames, node/topology detail, secret names and paths,
  live attack-surface maps. Diagrams go out with roles, not addresses.
- **Guard the mission:** share *how* your crew coordinates and contains work; keep *what* it works on
  and *why* to yourself if that is your edge. The method generalizes and exposes nothing; the mission
  is yours.
- **Gate it:** before anything goes public, a sanitize + adversarial-review pass reads the whole
  set *in context* — a detail that is harmless in one file can be identifying across three. Nothing ships
  outward until that pass is green.

Share the method freely. Guard the plumbing and the purpose.

---
*This document is owned by the Aegis role. If you fork FleetChat, keep `SECURITY.md` honest about
what your version does and does not protect — an out-of-date threat model is worse than none.*
