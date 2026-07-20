package main

import "net/http"

// The daemon binds loopback only, but "loopback" is NOT a trust boundary: the
// user's own browser is a confused deputy -- any web page they visit can fire
// cross-site requests at http://127.0.0.1:8137, and DNS-rebinding can make a
// remote page reach it with same-origin privileges. This middleware is the single
// global CSRF / rebinding gate (from the security review -- the §1/§7 P1 cluster):
//
//  1. Host allowlist on EVERY request (incl. the WS upgrade) -- a DNS-rebound
//     request carries a foreign Host, so this alone defeats rebinding (and stops a
//     rebinding page from even READING /messages, /roster, ...).
//  2. State-changing requests must be POST -- so a bare <img>/<script> GET can't
//     trigger a mutation (/shutdown, /control/clear, ...).
//  3. State-changing requests must carry the X-Fleet-Client header -- a cross-site
//     page cannot set a custom request header without a CORS preflight this server
//     never grants, so both simple-request and preflighted CSRF fail.
//  4. When the browser sends an Origin, it must be ours (defense in depth).
//
// The real UI already sends X-Fleet-Client on every request and POSTs all its
// mutations, so it is unaffected. Reads (GET /messages, /roster, ...) stay open
// but are still Host-checked. This is NOT authentication against a local
// non-browser process (which forges any header and already holds the user's
// privileges) -- it defends the browser-confused-deputy vectors, which are the
// exploitable ones. A per-session token for the local-process residual is a
// separate, optional follow-up.

// hostAllowed: the Host header must be our loopback authority. Anything else is a
// DNS-rebound or otherwise-forged request.
func hostAllowed(host string) bool {
	switch host {
	case "127.0.0.1:" + daemonPort, "localhost:" + daemonPort, "[::1]:" + daemonPort:
		return true
	}
	return false
}

// originAllowed: an Origin, when present, must be our own.
func originAllowed(origin string) bool {
	switch origin {
	case "http://127.0.0.1:" + daemonPort, "http://localhost:" + daemonPort, "http://[::1]:" + daemonPort:
		return true
	}
	return false
}

// mustPOST: routes that perform a state change and must NEVER act on a GET (so a
// bare <img>/<script> GET can't fire them). Dual read/write routes that serve a
// safe GET (status) are deliberately NOT listed -- their POST branch is still
// gated because the middleware treats every POST as a mutation. (/control/board
// self-guards its stop/start branch to POST for the same reason.)
var mustPOST = map[string]bool{
	"/shutdown":                true,
	"/kill":                    true,
	"/spawn":                   true,
	"/control/add":             true,
	"/control/restart":         true,
	"/control/clear":           true,
	"/control/respawn":         true,
	"/control/kick":            true,
	"/control/pick":            true,
	"/control/voices/download": true, // spawns the Kokoro downloader
	"/control/speaker":         true, // spawns/stops the voice speaker
}

// securityMiddleware wraps the whole mux -- see the file header for the model.
func securityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hostAllowed(r.Host) {
			http.Error(w, "forbidden: bad Host (loopback authority only)", http.StatusForbidden)
			return
		}
		// A request is state-changing if it is NOT a plain GET read, OR it targets a
		// route that mutates state (mustPOST) -- so a GET aimed at a pure-mutation
		// route is caught too. Gating EVERY non-GET (not just POST) is deliberate: a
		// CORS-"simple" HEAD carries no custom header and triggers no preflight, and
		// a dual GET+mutate handler doing `if GET {read} else {mutate}` would otherwise
		// mutate on it -- the /control/debug HEAD-toggle bypass the security review
		// found. This closes that whole class at one point (WS upgrades are GETs, so
		// they still pass and are Host-checked).
		if r.Method != http.MethodGet || mustPOST[r.URL.Path] {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed: this route mutates state -- use POST", http.StatusMethodNotAllowed)
				return
			}
			if r.Header.Get("X-Fleet-Client") == "" {
				http.Error(w, "forbidden: missing X-Fleet-Client (CSRF guard)", http.StatusForbidden)
				return
			}
			if o := r.Header.Get("Origin"); o != "" && !originAllowed(o) {
				http.Error(w, "forbidden: cross-origin request refused", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
