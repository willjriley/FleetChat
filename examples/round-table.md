# A worked round-table

A short, illustrative transcript of a FleetChat crew doing one real thing: adding a feature
that **fetches and runs an external tool** on `<YOUR DOMAIN>`. It shows the pattern end to end —
decomposition, a creative angle, a *security review with an informed-consent beat*, a
rollback-safe cutover, and the human owning the irreversible switch.

This is what `python run.py` gestures at, written out in full. Read it to see the principles in
`docs/PRINCIPLES.md` actually load-bearing.

---

**#1 · lodestar** — New ask: let users point the tool at an arbitrary public repo and run it against their data. That's untrusted code executing on our box, so this is a security-first design, not a feature-first one. @aegis, @muse, @keystone — I want your lanes on this before any code. @aegis, you have veto.

**#2 · muse** — Before we build a fetch-and-run pipeline: the obvious design runs the repo directly and bolts safety on after. Flip it. What if "fetched" and "runnable" are two separate states, and nothing crosses from one to the other without passing through Aegis? The safety boundary becomes the architecture, not a wrapper.

**#3 · aegis** — That framing is right, and it's the only one I'll sign off on. My requirements: the fetch is inspected before it's cached (provenance + a scan), and the run happens **contained** — no network, no host access, in *both* directions. Curation is defense-in-depth, never a substitute for the sandbox. And the human sees what we found before anything runs.

**#4 · keystone** — Infra side: I'll stand the sandbox up beside the existing runner, not replace it. Old path keeps serving while we prove the new one. The fetch cache is append-only and content-addressed, so a re-run is deterministic and I always have a way back. Nothing about the current flow changes until we cut over and validate.

**#5 · muse** — And make the scan result *legible*, not a raw dump. A one-line verdict — clean / flagged, with the why — is what lets a human actually decide. If we make them read a report, they'll click through it; if we make it a glance, they'll read it.

**#6 · aegis** — Agreed, and that's the consent beat. Here is the surface for the repo you asked about:

> **fetch → github.com/example/tool @ a1b2c3d** · 1.2k★ · genuine, not archived
> **scan → CLEAN** · no secrets, no known-vuln deps, no install-time code execution
> This will run **sandboxed**: no network, no host mount. Proceed?

Clean, so it's a normal proceed — but *you* make the call, not me. If it had come back flagged, this is exactly where you'd decide with the risk in front of you.

**#7 · will (human)** — Surface looks good. Go.

**#8 · lodestar** — Human's go recorded. @keystone, cut over to the sandboxed path; keep the old runner warm until Aegis validates from the outside.

**#9 · keystone** — Cut over. New path live, old runner still up and untouched. Rollback is one flag.

**#10 · aegis** — Validated from *inside* a live run: the sandbox has no route off the box and no view of the host; the declared fetch is the only thing it touched. Containment holds. This one's green. I'll re-check it on the next change — a fix isn't done until I've confirmed it didn't quietly regress.

**#11 · lumen** — Worth naming: the user got a scary-sounding capability ("run arbitrary code from the internet") delivered as a calm, one-glance decision they stayed in control of. That's the difference between powerful and trustworthy. Nicely done, crew.

**#12 · lodestar** — That's the pattern: nobody soloed, security gated the irreversible step, the human owned the switch, and the old path stayed up until the new one was proven. Ship it.

---

*Notice what did the work: not the tooling, but the sequence — assess → surface → **human decides** →
contained execution → outside-in validation → a way back at every step. Swap in your own domain and
your own tools; keep the sequence.*
