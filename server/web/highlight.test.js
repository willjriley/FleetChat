// Guards for the code-block highlighter in index.html.
//
//   node server/web/highlight.test.js
//
// WHY THIS EXISTS: highlight() used to chain four .replace() calls over text that
// already contained its own generated HTML. The number pass emitted
//     <span class="s-num">4257</span>
// and the keyword pass then matched the literal word `class` INSIDE that tag --
// `class` being one of its keywords -- producing `<span <span class="s-kw">class</span>=`,
// which is malformed, so browsers rendered the remainder as visible text. Every
// number, string and comment was affected; a UUID just made it unmissable.
//
// The two assertions below are the ones that catch it, and neither is "does it look
// right": no output may contain a nested `<span <`, and stripping all tags must return
// the input exactly -- so nothing is swallowed or duplicated either.
//
// It reads the live functions out of index.html rather than copying them, because a
// test against a copy stops guarding the moment the copy drifts.
const fs = require("fs");
const path = require("path");

const html = fs.readFileSync(path.join(__dirname, "index.html"), "utf8");
const slice = html.slice(html.indexOf("function esc(s){"), html.indexOf("function renderCode("));
const mod = {};
new Function("module", slice + "\nmodule.esc = esc; module.highlight = highlight;")(mod);
const { esc, highlight } = mod;

let failures = 0;
function check(input, mustBeUntouched) {
  const out = highlight(esc(input));
  const malformed = /<span <|<span [^>]*<span[^>]*=/.test(out);
  const stripped = out.replace(/<[^>]*>/g, "");
  const lost = stripped !== esc(input);
  const coloured = out !== esc(input);
  const bad = malformed || lost || (mustBeUntouched && coloured);
  if (bad) {
    failures++;
    console.log("  FAIL  " + JSON.stringify(input));
    if (malformed) console.log("        nested/malformed span: " + out);
    if (lost) console.log("        text changed: " + JSON.stringify(stripped));
    if (mustBeUntouched && coloured) console.log("        should not be highlighted: " + out);
  } else {
    console.log("  ok    " + JSON.stringify(input));
  }
}

console.log("identifiers must survive untouched (digits inside them are not numbers):");
check("929f2a9e-2ac4-4257-9e53-949206e2cb41", true);   // the UUID Will reported
check("2026-08-14 15:30:00", true);
check("192.0.2.10:8188", true);
check("sha256:487f87faaf547ea30e0aba4d5b53346292571256b25333a978db1692bcee9dd2", true);
check("v1.2.3-rc4", true);

console.log("\nreal tokens must still colour, losing no characters:");
check("run id 4257 took 12.5s", false);
check("const x = 5; // note", false);
check('say "hello 42" now', false);
check("# a comment with 7 in it", false);

console.log("\nregex must parse without lookbehind (Chromium-only; inside the main");
console.log("IIFE a parse failure blanks the whole board rather than dropping colour):");
if (/\(\?<[=!]/.test(html.slice(html.indexOf("var HL_RX"), html.indexOf("function highlight")))) {
  failures++;
  console.log("  FAIL  HL_RX contains a lookbehind");
} else {
  console.log("  ok    HL_RX has no lookbehind");
}

console.log(failures === 0 ? "\nALL PASS" : "\n" + failures + " FAILURES");
process.exit(failures === 0 ? 0 : 1);
