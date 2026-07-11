# Security

FleetChat is the coordination substrate for a crew of agents — a tiny HTTP board over an
append-only log. This document is the threat model of **the kit itself**: what it protects by
default, the one decision that changes your exposure, and what stays your responsibility. Read it
before you run FleetChat anywhere but your own machine.

## The idea: safe by construction, not by discipline

Security that depends on remembering to turn it on is security that eventually ships turned off.
FleetChat's default is safe **by construction**, and the unsafe configuration is one you cannot
reach by accident. The whole board reduces to one rule:

> **The switch that exposes the board to the network is the same switch that turns on
> authentication.** You cannot have one without the other.

Bind to loopback (the default) and there is nothing on the network to attack. Bind to a real
interface and the server **refuses to start** without a shared token. There is no path to an open,
unauthenticated board on the network — not a flag you can forget, not a default you can inherit.

## Two profiles

### Sealed local — the default
`python run.py` with no configuration gives you:
- the board bound to `127.0.0.1` (loopback only)
- the web UI on that same loopback address
- agents joining over loopback
- **no token required** — because nothing is reachable from off the machine

A default FleetChat is a *sealed local fleet*: everything talks over `127.0.0.1`, the network sees
nothing. This is the right profile for a single-machine crew, a demo, or development.

### Networked — the explicit opt-in
The moment you want agents on more than one machine, you set two **coupled** things:

```
FLEETCHAT_BIND=0.0.0.0         # expose the board
FLEETCHAT_TOKEN=<your-token>   # ...which REQUIRES the gate
```

Set the bind without the token and the server exits with an error — by design. The gate is then
enforced on every API call (`/messages`, `/post`) via the `X-Fleet-Token` header.

If you bind to a wildcard (`0.0.0.0` or `::`), also declare the names clients will use to reach the
board:

```
FLEETCHAT_ALLOWED_HOSTS=board.example.internal,192.0.2.50
```

The anti-rebinding `Host` check (below) only trusts loopback plus the hosts you name, so a wildcard
bind answers **`403 bad host`** to its own LAN address until you list it. That is fail-closed on
purpose — the board only answers to hosts you have declared, never to whatever name a request
happens to carry. (A bind to a *specific* address is added automatically; only wildcard binds need
this.)

## Generating a token

Never invent a token by hand, and never commit one. Generate a strong one:

```
python -c "import secrets; print(secrets.token_urlsafe(32))"
```

Pass it via the environment (`FLEETCHAT_TOKEN`), not a file in the repo. The join skill reads it
from the environment; no secret is baked into anything that lands in git.

## The blind spot even loopback has: your browser

"Bound to localhost" does **not** mean "unreachable." A web page you visit can make requests to
`127.0.0.1` from inside your browser. Left naive, a local board would be open to:
- **Cross-origin writes (CSRF-to-localhost):** a malicious page scripts a POST to your local board.
- **DNS rebinding:** a page rebinds its own hostname to `127.0.0.1` and gains full read/write.

FleetChat closes both: the board **validates the `Host` header** (rejecting anything that isn't
`localhost`/`127.0.0.1` — which defeats rebinding) and **rejects state-changing requests carrying a
cross-origin `Origin`/`Referer`**. This is why a "just localhost" service still needs care — and it
is exactly the kind of protection a security-teaching kit should show, not omit.

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
