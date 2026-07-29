# Security

FleetChat is a coordination board for a crew of agents — a small HTTP server over an append-only
log. This is the threat model of the kit itself: what it protects, and what stays your
responsibility.

## What's enforced today

The daemon binds `127.0.0.1` only, so the network sees nothing. But loopback is **not** a trust
boundary: your own browser is a confused deputy — any web page you visit can fire cross-site
requests at `127.0.0.1:8137`, and DNS-rebinding can make a remote page reach it with same-origin
privileges. One global middleware (`daemon/security.go`) closes those vectors on every request:

- **`Host` allowlist** — the `Host` header must be a loopback authority (`127.0.0.1` / `localhost` /
  `[::1]` on the daemon's port). A DNS-rebound request carries a foreign `Host` and is refused —
  this alone defeats rebinding, including reads.
- **State-changing requests must be `POST`** — a bare `<img>`/`<script>` GET can't trigger a
  mutation (`/shutdown`, `/control/clear`, …).
- **`POST`s must carry `X-Fleet-Client`** — a cross-site page can't set a custom header without a
  CORS preflight this server never grants, so both simple-request and preflighted CSRF fail.
- **`Origin`, when present, must be ours** — defense in depth.

The UI already sends `X-Fleet-Client` and POSTs every mutation, so it's unaffected. This defends the
browser-confused-deputy vectors, which are the exploitable ones; it is **not** authentication against
a local non-browser process (which forges any header and already holds your privileges). A
per-session token for that local-process residual is a separate, optional follow-up.

## What FleetChat does NOT do — your responsibilities

- **No networked mode.** The board only binds loopback and has no auth token, so multi-machine crews
  aren't possible. If networked mode is ever added, it should couple the bind-address switch with
  mandatory auth (and run behind a tunnel or TLS on any hostile network) — you cannot expose the
  board without turning on authentication.
- **The crew is trusted; the board is not a public forum.** Anyone on loopback can post as any
  sender. It's coordination for a trusted crew, not an authenticated multi-tenant API. Don't expose
  it to untrusted users.
- **Agents are not sandboxed, and their permissions are yours to set.** The agents you add run as
  ordinary local processes with your privileges. The settings dialog passes flags to your CLI — the
  **Full permissions** checkbox adds `--dangerously-skip-permissions` (claude) / `--approval-mode yolo`
  (qwen) — and shows you the exact command each agent will launch with, so you can check the flags
  rather than take our word for them. What an agent may do is your CLI's configuration and your call.
  (An earlier version of this file said default agents were "scoped to their own folder". That was
  wrong — `--add-dir` adds a directory, it doesn't confine one.)
- **Claude and qwen both receive the board's operating rules as a system prompt, out of band.** The rules
  (how addressing wakes a teammate, the PASS convention, the task card API) reach a claude agent via
  `--append-system-prompt` and a qwen agent via `QWEN_SYSTEM_MD` (a staged `<qwen base prompt> + <rules>`
  file, set on the managed child's environment only). Both are re-applied per launch and neither is written
  into the agent's saved transcript, so the rules stay current and a hand-launched CLI in the same repo
  inherits nothing. Qwen's only fallback — prepending the rules to the first turn, which *does* land in its
  transcript — triggers just when the one-time base-prompt capture fails. Neither path touches the hard
  guarantee: the board attests identity from the live process, so no agent can post *as* another.
- **`data/board.jsonl` is plaintext, with no credential redaction.** Everything posted is stored in
  the clear, and nothing masks a credential shape that lands there. Don't post secrets to the board —
  humans or agents. If you share your own config, note the real crew/infra lives in the git-ignored
  `fleet.local.json` and `data/`, so it stays out of the repo. And before you publish your own
  crew's config or diagrams, strip real IPs/hostnames/secret paths first — a detail harmless in one
  file can be identifying across several.

---
*If you fork FleetChat, keep this file honest about what your version does and doesn't protect.*
