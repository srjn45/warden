// BackendLogo renders the brand mark for the AI agent driving a session
// (claude, aider, …). The `backend` field is json `omitempty`, so pre-#52 Claude
// agents send nothing — we default to 'claude' so a card never shows a blank.
//
// Marks are inlined (not <img>) so `fill="currentColor"` inherits the tile text
// color and themes cleanly in both light and dark. Sources, used for nominative
// identification only: Claude = simple-icons (CC0); Aider = aider's own
// safari-pinned-tab icon (Apache-2.0), with its full-bleed square dropped so the
// glyph itself becomes the positive shape. The same files live in
// public/brand/<id>.svg. Any backend without a registered mark falls back to a
// tasteful monochrome lettermark chip, so adding antigravity/codex/etc. is just
// a new entry below.

export const DEFAULT_BACKEND = 'claude';

// title-cased label for the accessible name (e.g. "claude" → "Claude").
function titleCase(id: string): string {
  return id.charAt(0).toUpperCase() + id.slice(1);
}

// Registered brand marks, keyed by backend id. Each value is inline SVG markup
// using currentColor so it themes with the surrounding text.
const MARKS: Record<string, string> = {
  claude: "<svg role=\"img\" aria-label=\"Claude\" viewBox=\"0 0 24 24\" xmlns=\"http://www.w3.org/2000/svg\"><title>Claude</title><path fill=\"currentColor\" d=\"m4.7144 15.9555 4.7174-2.6471.079-.2307-.079-.1275h-.2307l-.7893-.0486-2.6956-.0729-2.3375-.0971-2.2646-.1214-.5707-.1215-.5343-.7042.0546-.3522.4797-.3218.686.0608 1.5179.1032 2.2767.1578 1.6514.0972 2.4468.255h.3886l.0546-.1579-.1336-.0971-.1032-.0972L6.973 9.8356l-2.55-1.6879-1.3356-.9714-.7225-.4918-.3643-.4614-.1578-1.0078.6557-.7225.8803.0607.2246.0607.8925.686 1.9064 1.4754 2.4893 1.8336.3643.3035.1457-.1032.0182-.0728-.164-.2733-1.3539-2.4467-1.445-2.4893-.6435-1.032-.17-.6194c-.0607-.255-.1032-.4674-.1032-.7285L6.287.1335 6.6997 0l.9957.1336.419.3642.6192 1.4147 1.0018 2.2282 1.5543 3.0296.4553.8985.2429.8318.091.255h.1579v-.1457l.1275-1.706.2368-2.0947.2307-2.6957.0789-.7589.3764-.9107.7468-.4918.5828.2793.4797.686-.0668.4433-.2853 1.8517-.5586 2.9021-.3643 1.9429h.2125l.2429-.2429.9835-1.3053 1.6514-2.0643.7286-.8196.85-.9046.5464-.4311h1.0321l.759 1.1293-.34 1.1657-1.0625 1.3478-.8804 1.1414-1.2628 1.7-.7893 1.36.0729.1093.1882-.0183 2.8535-.607 1.5421-.2794 1.8396-.3157.8318.3886.091.3946-.3278.8075-1.967.4857-2.3072.4614-3.4364.8136-.0425.0304.0486.0607 1.5482.1457.6618.0364h1.621l3.0175.2247.7892.522.4736.6376-.079.4857-1.2142.6193-1.6393-.3886-3.825-.9107-1.3113-.3279h-.1822v.1093l1.0929 1.0686 2.0035 1.8092 2.5075 2.3314.1275.5768-.3218.4554-.34-.0486-2.2039-1.6575-.85-.7468-1.9246-1.621h-.1275v.17l.4432.6496 2.3436 3.5214.1214 1.0807-.17.3521-.6071.2125-.6679-.1214-1.3721-1.9246L14.38 17.959l-1.1414-1.9428-.1397.079-.674 7.2552-.3156.3703-.7286.2793-.6071-.4614-.3218-.7468.3218-1.4753.3886-1.9246.3157-1.53.2853-1.9004.17-.6314-.0121-.0425-.1397.0182-1.4328 1.9672-2.1796 2.9446-1.7243 1.8456-.4128.164-.7164-.3704.0667-.6618.4008-.5889 2.386-3.0357 1.4389-1.882.929-1.0868-.0062-.1579h-.0546l-6.3385 4.1164-1.1293.1457-.4857-.4554.0608-.7467.2307-.2429 1.9064-1.3114Z\"/></svg>",
  aider: "<svg xmlns=\"http://www.w3.org/2000/svg\" viewBox=\"0 0 436 436\" role=\"img\" aria-label=\"Aider\">\n  <title>Aider</title>\n  <g transform=\"translate(0,436) scale(0.1,-0.1)\" fill=\"currentColor\" stroke=\"none\">\n    <path d=\"M2705 3998 c20 -20 28 -121 30 -398 l2 -305 216 -5 c118 -3 218 -8 222\n-12 3 -3 10 -46 15 -95 5 -48 16 -126 25 -172 17 -86 17 -81 -17 -233 -14 -67\n-13 -365 2 -438 21 -100 22 -159 5 -247 -24 -122 -24 -363 1 -458 23 -88 23\n-213 1 -330 -9 -49 -17 -109 -17 -132 l0 -43 203 0 c111 0 208 -4 216 -9 10\n-6 18 -51 27 -148 8 -76 16 -152 20 -168 7 -39 -23 -361 -37 -387 -10 -18 -21\n-19 -214 -16 -135 2 -208 7 -215 14 -22 22 -33 301 -21 501 6 102 8 189 5 194\n-8 13 -417 12 -431 -2 -12 -12 -8 -146 8 -261 8 -55 8 -95 1 -140 -6 -35 -14\n-99 -17 -143 -9 -123 -14 -141 -41 -154 -18 -8 -217 -11 -679 -11 l-653 0 -11\n33 c-31 97 -43 336 -27 533 5 56 6 113 2 128 l-6 26 -194 0 c-211 0 -252 4\n-261 28 -12 33 -17 392 -6 522 15 186 -2 174 260 180 115 3 213 8 217 12 4 4\n1 52 -5 105 -7 54 -17 130 -22 168 -7 56 -5 91 11 171 10 55 22 130 26 166 4\n36 10 72 15 79 7 12 128 15 665 19 l658 5 8 30 c5 18 4 72 -3 130 -12 115 -7\n346 11 454 10 61 10 75 -1 82 -8 5 -300 9 -650 9 l-636 0 -27 25 c-18 16 -26\n34 -26 57 0 18 -5 87 -10 153 -10 128 5 449 22 472 5 7 26 13 46 15 78 6 1281\n3 1287 -4z\"/>\n  </g>\n</svg>",
};

export default function BackendLogo({ backend }: { backend?: string }) {
  const id = (backend && backend.trim()) || DEFAULT_BACKEND;
  const label = titleCase(id);
  const mark = MARKS[id];
  if (mark) {
    return (
      <span
        className="backend-logo"
        title={label}
        aria-label={label}
        role="img"
        dangerouslySetInnerHTML={{ __html: mark }}
      />
    );
  }
  // Unknown backend: monochrome lettermark chip with the initial.
  return (
    <span className="backend-logo backend-logo--letter" title={label} aria-label={label} role="img">
      {id.charAt(0).toUpperCase()}
    </span>
  );
}
