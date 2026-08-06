// Dashboard client script (served as an embedded asset by dashboard.go, #145).
// SECURITY INVARIANTS governing this file are documented above the //go:embed
// block in dashboard.go -- read them before editing: the API token lives in
// sessionStorage; render every user-supplied value via textContent (never as
// parsed HTML markup); the token travels ONLY in the Authorization header.
//
// This is the "Engineering Yield Dashboard" renderer (#274). The visual system
// is an instrument panel: KPI tiles for the org reading, and per-work-type
// small multiples whose leaderboards are ranked HORIZONTAL YIELD BARS with
// 95%-CI whiskers. Every DOM value that originates from the API is set via
// textContent; the only styles this script writes are numeric bar widths and
// offsets computed from finite numbers -- never a user-supplied string.
"use strict";

// --- Small DOM helpers ------------------------------------------------------

function $(id) { return document.getElementById(id); }

function setText(id, value) { $(id).textContent = value; }

// el creates an element with a class and (optional) textContent. text is always
// assigned via .textContent, so callers can pass user-controlled strings safely.
function el(tag, cls, text) {
  var node = document.createElement(tag);
  if (cls) { node.className = cls; }
  if (text !== undefined && text !== null) { node.textContent = text; }
  return node;
}

// num coerces a possibly-null/undefined/NaN/Inf field to a finite number.
// Server schemas should not produce these for our scalar fields, but a future
// encoder change (e.g. *float64 with json.Marshal of nil) would otherwise crash
// row rendering on .toFixed(). Defensive in depth.
function num(v) {
  return (typeof v === 'number' && isFinite(v)) ? v : 0;
}

// pct clamps a 0..1 fraction to a CSS percentage string. Bar widths and whisker
// offsets are the ONLY inline styles this script writes; both come from finite
// numbers computed below, never from any user-supplied value.
function pct(fraction) {
  var f = isFinite(fraction) ? fraction : 0;
  if (f < 0) { f = 0; }
  if (f > 1) { f = 1; }
  return (f * 100).toFixed(3) + '%';
}

// usdText renders a USD amount that is displayed ALONGSIDE a floor it is being
// compared against. Two rules, both load-bearing, both learned from the #502 cause
// line:
//
//   - TRUNCATE to the cent, never round. Rounding lets a figure print as having
//     reached a threshold it is below: at $4.995 the cause line rendered "$5.00 of
//     measured AI spend — below the $5.00 evidence floor", a sentence that
//     contradicts itself. Truncating down cannot cross the floor.
//   - Below a cent, say "<$0.01", never "$0.00". The canonical #502 window is
//     $0.0001; as "$0.00" the evidence the reader is meant to weigh is not on
//     screen at all, and it is indistinguishable from a genuine zero-cost row,
//     which is the FREE reading — unbounded yield, the opposite meaning.
//
// EVERY USD *amount* on this page goes through here — the SPEND tile, the
// cost-composition total, the per-model and per-bucket costs, the unattributed
// figure, and the below-floor cause line. That is not tidiness: these figures sit
// beside each other and are read against each other, and two formatters for one
// quantity is how "$5.00 of measured AI spend — below the $5.00 evidence floor"
// happened. Two renderings are deliberately NOT routed here, both for stated
// reasons: cost_per_point is a RATE in $/point, not an amount (#239), and the
// compare view renders signed DELTAS whose semantics are still under review
// (#605). TestDashboard_MoneyHasOneFormatter pins the list.
//
// A true zero renders "$0.00", which is the honest figure there, and that branch
// IS exercised: the SPEND tile is in renderKPIs' common tail, so a FREE or
// no-spend window reaches it, as does a fully-attributed window's unattributed
// figure. num() maps NaN/Infinity/absent to 0; the domain is non-negative
// (total_cost_usd is a sum of non-negative micro-USD values) and negatives are not
// formatted meaningfully.
//
// The toFixed(6) inside the truncation is NOT decoration. `Math.floor(n * 100)`
// alone is wrong for ordinary amounts, because n * 100 is not exact in binary:
// 0.29 * 100 is 28.999999999999996, so a genuine $0.29 truncated to "$0.28" and
// $8.20 to "$8.19" — losing a cent on values that have nothing to do with the
// floor. Rounding the cent count to 6 decimal places before flooring absorbs that
// representation error while leaving a real fraction of a cent intact.
//
// ⚠️ That tolerance means truncation ALONE does not guarantee the "cannot cross
// the floor" property: any n in [4.9999999995, 5.00) still renders "$5.00". It is
// tempting to argue this is unreachable because cost is stored in micro-USD — it
// is NOT. total_cost_usd is a float64 SUM of micro-USD-exact values (RollupTeam),
// and a sum is not on that grid: 2.918582 + 0.956618 + 0.420133 + 0.704667 is
// 4.999999999999999, which is genuinely below the floor and renders "$5.00".
// Callers that print this figure next to a floor MUST route through usdUnder,
// which closes the gap structurally instead of by tolerance.
function usdText(v) {
  var n = num(v);
  if (n > 0 && n < 0.01) { return '<$0.01'; }
  // n is finite (num guarantees it) but n * 100 can still overflow to Infinity.
  if (!isFinite(n * 100)) { return '$' + n.toFixed(2); }
  return '$' + (Math.floor(Number((n * 100).toFixed(6))) / 100).toFixed(2);
}

// usdUnder renders an amount that the CALLER has already established is strictly
// below `floor`, and guarantees the rendering does not claim otherwise. When the
// truncated rendering lands exactly on the floor's own rendering, it says "<".
//
// Without this, one sentence can contradict itself: "$5.00 of measured AI spend —
// below the $5.00 evidence floor". usdText's truncation removes that for every
// value a human would type, but not for a float64 sum that lands a few ulps below
// the floor (see the warning on usdText). This is a structural fix rather than a
// wider tolerance: there is no value of v for which the output can equal the
// floor's rendering.
function usdUnder(v, floor) {
  // Enforce the precondition rather than documenting it. Every caller today gates
  // on a strict `<`, so this cannot fire -- it is here so that a caller which one
  // day does not gate fails SAFE rather than silently lying.
  //
  // The fallback must not be usdText(v). Out of domain that returns "$5.00", and
  // the caller concatenates it into '... of measured spend, below the $5.00
  // evidence floor' -- rebuilding the exact #502 sentence this function exists to
  // delete. '>=' cannot be read as agreeing with the floor.
  //
  // NaN never arrives (num maps it to 0) but 0 would read as a MEASURED zero, i.e.
  // the FREE meaning, which is the opposite reading. It is unreachable only
  // because every caller sits behind an earlier cost<=0 FREE gate, not because 0
  // is a safe answer here.
  if (num(v) >= num(floor)) { return '\u2265' + usdText(floor); }
  var s = usdText(v);
  return s === usdText(floor) ? '<' + s : s;
}

// spendTextFor renders the org's measured window spend. It exists so that the two
// places that print that ONE number -- the SPEND tile and the below-floor cause
// line, both on screen at once, sixty lines apart in renderKPIs -- cannot drift.
//
// They already did, twice. First the tile rounded while the caption truncated
// ($4.997 -> tile "$5.00" beside a caption asserting the spend was below $5.00).
// Then both truncated but only the caption used usdUnder, so at a float64 sum a
// few ulps under the floor the tile said "$5.00" and the caption said "<$5.00" --
// the same contradiction, narrowed to a window the code itself argues is
// reachable. Two call sites of one function cannot disagree.
function spendTextFor(totalCostUSD) {
  return totalCostUSD < MIN_RANKED_COST_USD
    ? usdUnder(totalCostUSD, MIN_RANKED_COST_USD)
    : usdText(totalCostUSD);
}

// unrankedReasons names the #133/#136 conditions a row actually FAILS, read from
// the fields the row itself carries.
//
// It never asserts a single cause, because `ranked` is a three-way conjunction
// (flagged == 0 AND sample_n >= MIN_RANKED_OUTCOMES AND cost >= MIN_RANKED_COST_USD).
// The old wording, "insufficient sample to rank: 20 outcomes / $500.00 cost",
// blamed the sample on a row whose 20 outcomes and $500 clear both floors and
// which is unranked because ONE outcome had no measured tokens — a number on
// screen contradicting the sentence next to it.
//
// Developer rows carry all three inputs, so nothing is guessed. If none of the
// three is failing, the client cannot explain the server's verdict and says so
// rather than inventing a cause.
function unrankedReasons(d) {
  var why = [];
  var flagged = num(d.flagged_outcomes);
  if (flagged > 0) {
    why.push(flagged + ' outcome' + (flagged !== 1 ? 's' : '') + ' with no measured tokens');
  }
  if (num(d.sample_n) < MIN_RANKED_OUTCOMES) {
    why.push('fewer than ' + MIN_RANKED_OUTCOMES + ' accepted outcomes (' + num(d.sample_n) + ')');
  }
  if (num(d.total_cost_usd) < MIN_RANKED_COST_USD) {
    why.push(usdUnder(d.total_cost_usd, MIN_RANKED_COST_USD) + ' of measured spend, below the ' +
      usdText(MIN_RANKED_COST_USD) + ' evidence floor');
  }
  return why.length ? why.join('; ') : 'the server did not vouch for this row';
}

function showStatus(msg, isError) {
  var elm = $('status-msg');
  elm.textContent = msg;
  elm.className = isError ? 'error-msg' : 'loading-msg';
  elm.style.display = msg ? '' : 'none';
}

// --- Theme toggle -----------------------------------------------------------
// The theme preference is a UI choice, not a secret, so it persists in
// localStorage (survives reload). It is applied by stamping data-theme on the
// document root; the CSS variables do the rest. No user data is involved.
//
// First-paint contract (#274 review): we persist and stamp a theme ONLY on an
// explicit user toggle. A first-time visitor with no stored preference is left
// with NO data-theme attribute, so the CSS @media(prefers-color-scheme) block
// drives first paint -- we never silently pin 'dark' for them on load.

// storedTheme returns the persisted 'light'/'dark' preference, or null when the
// visitor has never toggled (or localStorage is unavailable).
function storedTheme() {
  try {
    var s = localStorage.getItem('tier_theme');
    return (s === 'light' || s === 'dark') ? s : null;
  } catch (e) { return null; }
}

// effectiveTheme reports the theme currently in force: an explicit data-theme
// stamp if one exists, otherwise the OS preference (which the CSS uses for first
// paint when no stamp is present). It is the correct base for a toggle so we
// always flip away from what the user is actually seeing.
function effectiveTheme() {
  var stamped = document.documentElement.getAttribute('data-theme');
  if (stamped === 'light' || stamped === 'dark') { return stamped; }
  try {
    if (window.matchMedia && matchMedia('(prefers-color-scheme: light)').matches) {
      return 'light';
    }
  } catch (e) { /* fall through to the dark default */ }
  return 'dark';
}

// applyTheme stamps data-theme on the root WITHOUT persisting. Used on load for
// an already-stored preference, so replaying a stored choice never re-writes it.
function applyTheme(theme) {
  document.documentElement.setAttribute('data-theme', theme);
}

// setTheme is the explicit-user-toggle action: stamp AND persist so the choice
// survives reload. This is the ONLY path that writes localStorage.
function setTheme(theme) {
  applyTheme(theme);
  try { localStorage.setItem('tier_theme', theme); } catch (e) { /* non-fatal */ }
}

// --- Auth (#59) -------------------------------------------------------------
// The score GETs require the API token when one is configured. The token lives
// in sessionStorage (cleared when the tab closes; never in the URL -- see the
// invariants comment above). Entry is a masked password field rather than
// window.prompt: it doesn't render the secret in plaintext, password managers
// can fill it, and a cancel doesn't dead-end the page.
function authHeaders() {
  var t = sessionStorage.getItem('tier_token');
  return t ? { 'Authorization': 'Bearer ' + t } : {};
}

// On 401 the token field is revealed, any stale stored token is dropped (so a
// wrong value can't loop), and the thrown message tells the user the recovery
// path. Refresh picks the field's value up via loadScores.
function fetchJSON(url) {
  return fetch(url, { headers: authHeaders() }).then(function(r) {
    if (r.status === 401) {
      sessionStorage.removeItem('tier_token');
      $('token-input').style.display = '';
      throw new Error('this server requires an API token — enter it above and click Refresh');
    }
    if (!r.ok) { throw new Error('HTTP ' + r.status); }
    return r.json();
  });
}

// --- Load / orchestration ---------------------------------------------------

// The most recent successfully-rendered /scores payload and its window lower
// bound, retained SOLELY so the levers CSV export (#419) can be assembled from
// data already in the browser -- it makes NO additional network request. Reset
// to null on every load so a stale window can never be exported.
var lastScores = null;
var lastSince = '';

// --- Date helpers (#278 range presets) --------------------------------------
// All preset math is UTC-anchored to match the server's UTC window boundaries
// (#276): a preset date must mean the same day the API would resolve it to.
function isoDate(d) { return d.toISOString().slice(0, 10); }
function todayUTC() { var d = new Date(); d.setUTCHours(0, 0, 0, 0); return d; }
function daysAgoISO(n) { var d = todayUTC(); d.setUTCDate(d.getUTCDate() - n); return isoDate(d); }
function quarterStartISO() {
  var d = todayUTC();
  var qm = d.getUTCMonth() - (d.getUTCMonth() % 3);
  return isoDate(new Date(Date.UTC(d.getUTCFullYear(), qm, 1)));
}

// --- REVEAL INVARIANT (#516) -------------------------------------------------
// An element whose CSS rule declares `display: none` as its resting state MUST be
// revealed with an explicit display value. `el.style.display = ''` does NOT
// restore the element default -- it removes the inline override and hands control
// straight back to the stylesheet, which says none. The element then renders its
// full content, correctly, to nobody.
//
// This shipped. Eight elements were revealed with '' against an id rule carrying
// display:none: the attribution-coverage banner, the unattributed-spend
// breakdown, the unjoined-developer strip, the trust strip, the provenance stamp,
// the per-developer detail card, and BOTH halves of the compare view. The compare
// feature built 682 characters of content and displayed nothing at all.
//
// Nothing caught it because every guard asserted on state: the render function
// ran, the fields were read, the text was set, the class was applied. All true,
// all invisible. Only computed style or a bounding box can tell the difference,
// which is why TestDashboard_NoEmptyStringReveal derives the id-to-alias map from
// the assets themselves rather than trusting a hand-maintained list.
//
// Elements that no `display: none` RULE applies to may keep using '' -- clearing
// the inline style falls through to the stylesheet (#kpi-row's `grid`, .controls'
// `flex`) or, where no rule declares display at all, to the UA default
// (#cost-comp, #token-input, #kpi-tier-provisional). Either is the intended box.
// State it that way round: the test is "does a display:none rule apply?", NOT
// "does the stylesheet carry a value?" -- three of those four have no stylesheet
// display at all, and asking the wrong question is how the next element gets it
// wrong.
//
// resetViews hides EVERY result panel -- the single-window set AND the compare
// view -- so neither a stale single-window panel nor a stale comparison can
// linger across a mode switch or a "No data" early-return (both render paths can
// return before their render fns run). Each render path re-shows only what it
// fills. Also drops the retained levers-export payload so a stale window can
// never be exported.
function resetViews() {
  var hide = ['kpi-row', 'provenance', 'segments-note', 'segments-retro',
              'detail-card', 'trust-strip', 'unattr-breakdown', 'attr-coverage',
              'unjoined-strip', 'cost-comp', 'compare-view', 'cmp-total',
              // cost-horizon (#512) MUST be in this list. It is the only banner
              // that renders in a calm "checked and covered" state, so a stale
              // one does not merely linger -- it actively asserts that a window
              // now on screen was verified, when it was the PREVIOUS window that
              // was. Three paths reach here without re-rendering it: the no-data
              // early return, a fetch error, and the switch into compare mode.
              'cost-horizon',
              // dq-band (#520) MUST be here for the same reason cost-horizon is,
              // one level up: it carries a FRAME LINE asserting how many checks
              // ran on "this window". A stale band does not just linger, it makes
              // a specific claim about a window that is no longer on screen.
              'dq-band'];
  for (var i = 0; i < hide.length; i++) { $(hide[i]).style.display = 'none'; }
  $('segments').textContent = '';
  lastScores = null;
  lastSince = '';
}

// load dispatches to the compare or single-window path based on the toggle. The
// refresh button, the range presets, and first paint all go through here.
function load() {
  if ($('compare-toggle').checked) { loadCompare(); } else { loadScores(); }
}

function loadScores() {
  var tokenField = $('token-input');
  if (tokenField.value) {
    sessionStorage.setItem('tier_token', tokenField.value);
    tokenField.value = '';
  }
  var since = $('since-input').value;
  var until = $('until-input').value;
  resetViews();
  showStatus('Loading...');

  // until is optional (#276): empty = open-ended (through now), matching the
  // single "Since" behaviour this view had before the From/To picker (#278).
  var q = '/api/v1/scores?since=' + encodeURIComponent(since)
        + (until ? '&until=' + encodeURIComponent(until) : '');
  fetchJSON(q)
    .then(function(data) { renderScores(data, since); })
    .catch(function(e) { showStatus('Error: ' + e.message, true); });
}

// showDetail fetches a developer's issue-level breakdown with the auth header (a
// plain <a> navigation can't carry it) and renders the JSON via textContent --
// same XSS posture as the bars.
function showDetail(url, developer) {
  fetchJSON(url)
    .then(function(data) {
      setText('detail-title', 'Developer detail — ' + developer);
      setText('detail-json', JSON.stringify(data, null, 2));
      $('detail-card').style.display = 'block';
      // Honour prefers-reduced-motion: fall back to an instant jump for users
      // who have asked the OS to minimise motion (#274 a11y review).
      var scrollBehavior = matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth';
      $('detail-card').scrollIntoView({ behavior: scrollBehavior, block: 'nearest' });
    })
    .catch(function(e) { showStatus('Error: ' + e.message, true); });
}

function renderScores(data, since) {
  // Team-aggregation mode (#185): the server ships a k-anonymized "teams" array
  // (already ordered, no individual names) and an empty "developers". The same
  // signal governs the per-type panels -- in team mode each panel carries "teams"
  // rows and NEVER a named individual.
  var teamMode = Array.isArray(data.teams);

  var pooled = (teamMode ? data.teams : data.developers) || [];

  // #593: a k-anonymity suppression must NEVER render as "no data". The server
  // withholds the cohort rows, the grand total and the composition sidecar when a
  // residual cohort is too small to hide behind, and DECLARES it here. Without this
  // branch the checks below read a suppressed window as an empty one -- the exact
  // indistinguishability the wire field exists to prevent, moved from the JSON to the
  // screen. "No data" tells an operator to go fix their capture; the truth is that
  // capture is fine and the answer cannot be shown at this k.
  var kanon = (data.data_quality && data.data_quality.kanon_suppressed) || null;
  if (kanon) {
    var who = kanon.developers === 1 ? '1 developer' : kanon.developers + ' developers';
    showStatus(
      'Withheld for anonymity: a cohort of ' + who + ' is below the k-anonymity floor of ' +
      kanon.k_anonymity + '. Cohort rows, the org total and the cost breakdown are ' +
      'suppressed for this window -- widen the date range, or query a level with more ' +
      'people in it. This is not missing data.', true);
    return;
  }

  if (pooled.length === 0 && !data.total) {
    showStatus('No data for this period.');
    return;
  }

  // Retain the rendered payload for the client-side levers CSV export (#419).
  // Stored only past the no-data early-return, so the export always reflects a
  // window that actually rendered.
  lastScores = data;
  lastSince = since;

  renderKPIs(data, pooled, teamMode);
  renderProvenance(data);
  renderAttributionCoverage(data.data_quality);
  renderCostHorizon(data.data_quality, data.since || since,
                    (data.total && typeof data.total.total_cost_usd === 'number') ? data.total.total_cost_usd : 0);
  renderUnjoinedDevelopers(data.data_quality);
  renderTrustStrip(data.data_quality);
  // Immediately after the four checks and NOT at the end of the function. The
  // band must run once every check has decided whether it is showing -- but the
  // four caveats are now gated on the band's own reveal, so every unrelated
  // renderer left between them is a way for all four to render at height 0 while
  // the KPI row still shows the number they qualify. That is #516 one level up,
  // in the one part of the page where silent invisibility IS the defect.
  renderDataQualityBand();
  renderUnattributedBreakdown(data.data_quality);
  renderCostComposition(data.cost_composition);
  renderSegments(data, teamMode, since);
  showStatus('');
}

// --- DATA-QUALITY BAND (#520) ------------------------------------------------
// Four sibling cards said "four separate things are wrong with this dashboard".
// One framed band says "this instrument checks itself" -- same content, same
// caveats, nothing hidden, but reclassified from symptom to output.
//
// DQ_CHECKS is the registry of what lives in the band, in DOM order, together
// with how each one reports severity. It is an explicit table rather than a
// className scan because the four checks do NOT share one severity convention:
// #unjoined-strip is red by its id rule and carries no class at all, while
// #attr-coverage and #cost-horizon opt into red with .warn. A generic "does it
// have .warn" scan would silently under-count the one check that is ALWAYS an
// active defect.
//
// STATE IS THREE-VALUED, not a red/amber boolean. The first cut asked each check
// "does this need attention?" and hardcoded #trust-strip to false on the reasoning
// that amber is this dashboard's "look here" colour rather than a failure. That is
// right about the COLOUR and wrong about that row: renderTrustStrip renders ONLY
// when the server flagged outcomes, so it is never a clearance. The reachable
// result was a frame line reading "None need attention." directly above a row
// itemising 245 flagged outcomes -- consolidation reading as minimisation, which
// is the one thing this band must never do.
//
// The real distinction is FINDING vs CLEARANCE, and it cuts across colour:
//
//   row              hidden means   amber means   red means
//   unjoined-strip   clean          --            attention
//   attr-coverage    not applicable clearance     attention
//   cost-horizon     not applicable clearance     attention
//   trust-strip      clean          NOTE          --
//
// Amber is a clearance in two rows and a finding in one, so no colour-derived
// predicate can be correct.
var DQ_CHECKS = [
  // Always red when shown: an unjoined developer reads a silent TIER=0 until an
  // operator maps the identity. The only check that names an action.
  { id: 'unjoined-strip', attnId: 'unjoined-attn', state: function () { return 'attention'; } },
  // Red below ATTR_WARN_THRESHOLD; amber above it is a genuine clearance.
  { id: 'attr-coverage', attnId: 'attr-coverage-attn',
    state: function (row) { return row.classList.contains('warn') ? 'attention' : 'clear'; } },
  // Red when the window predates cost capture or the check could not run; 'calm'
  // is the explicit "checked and covered" clearance the server emits a literal
  // false to preserve.
  { id: 'cost-horizon', attnId: 'cost-horizon-attn',
    state: function (row) { return row.classList.contains('warn') ? 'attention' : 'clear'; } },
  // Amber, and always a FINDING: this row exists only when outcomes were flagged.
  { id: 'trust-strip', attnId: 'trust-attn', state: function () { return 'note'; } }
];

// stateRank orders rows worst-first. Used to sort at render time, because
// severity is decided per window and a static DOM rank puts a calm amber row
// above a red one -- main's own defect restated.
var DQ_STATE_RANK = { attention: 0, note: 1, clear: 2 };

// countWord renders "1 check" / "N checks".
function dqPlural(n, one, many) { return n === 1 ? one : many; }

// renderDataQualityBand frames whatever the four checks decided, orders them by
// observed severity, and reveals the band.
//
// THE DENOMINATOR IS FIXED AT DQ_CHECKS.length, deliberately. Counting only the
// rows that RENDERED made the sentence report a numerator as though it were the
// whole: on the public demo it read "2 data-quality checks ran" when four ran and
// two found nothing -- which is the single most valuable thing this band can say,
// denied. A check that ran and found nothing still ran; hiding its row is a
// display decision, not an epistemic one. This project emits an explicit `false`
// for a covered cost horizon (#512) for exactly this reason.
function renderDataQualityBand() {
  var band = $('dq-band');
  var total = DQ_CHECKS.length;
  var attention = 0;
  var notes = 0;
  var visible = [];

  for (var i = 0; i < DQ_CHECKS.length; i++) {
    // `row`, not `el` -- `el` is this file's node-factory helper, and shadowing it
    // here would make the next appendChild in this function a TypeError.
    var row = $(DQ_CHECKS[i].id);
    row.classList.remove('dq-first');
    if (row.style.display === 'none') {
      setText(DQ_CHECKS[i].attnId, '');
      continue;
    }
    var st = DQ_CHECKS[i].state(row);
    if (st === 'attention') { attention++; } else if (st === 'note') { notes++; }
    // Non-visual severity. The frame line asserts a COUNT; without this the only
    // channel saying WHICH rows it refers to is the red left rule (WCAG 1.4.1).
    // First in each row's aria-labelledby, so the region announces the severity
    // before the title.
    setText(DQ_CHECKS[i].attnId,
      st === 'attention' ? 'Needs attention.' : (st === 'note' ? 'Flagged for review.' : 'Checked, no issue.'));
    visible.push({ row: row, rank: DQ_STATE_RANK[st], seq: i });
  }

  // Reorder by OBSERVED state. Moves the real nodes rather than using flex
  // `order`, so visual, keyboard and screen-reader order stay identical.
  visible.sort(function (a, b) { return a.rank - b.rank || a.seq - b.seq; });
  var current = [];
  for (var c = band.firstElementChild; c; c = c.nextElementSibling) {
    if (c.id && c.id !== 'dq-frame') { current.push(c); }
  }
  var differs = current.length !== visible.length;
  for (var k = 0; !differs && k < visible.length; k++) {
    if (current[k] !== visible[k].row) { differs = true; }
  }
  if (differs) {
    // Preserve focus: every row contains a focusable <details><summary>, and
    // re-appending an ancestor blurs it mid-interaction.
    var focused = document.activeElement;
    for (var m = 0; m < visible.length; m++) { band.appendChild(visible[m].row); }
    if (focused && typeof focused.focus === 'function' && band.contains(focused)) { focused.focus(); }
  }

  if (visible.length > 0) {
    visible[0].row.classList.add('dq-first');
  }

  // The frame line. "affect the numbers below" states the CONSEQUENCE, which is
  // what a reader needs in order to decide whether to trust the digit; "needs
  // attention" reads like a chore. Naming TIER as the actor is the "checks
  // itself" reframe in two words.
  var lead = 'TIER ran ' + total + ' data-quality checks on this window. ';
  var tail;
  if (attention > 0 && notes > 0) {
    tail = attention + dqPlural(attention, ' affects', ' affect') + ' the numbers below; ' +
           notes + dqPlural(notes, ' is', ' are') + ' flagged for review.';
  } else if (attention > 0) {
    tail = attention + dqPlural(attention, ' affects', ' affect') + ' the numbers below.';
  } else if (notes > 0) {
    tail = 'None affect the numbers below; ' + notes + dqPlural(notes, ' is', ' are') + ' flagged for review.';
  } else {
    tail = 'All clear.';
  }
  setText('dq-frame', lead + tail);
  // ALWAYS visible, including when no row renders. Hiding the band on a clean
  // window makes "checked and clean" indistinguishable from "the band failed to
  // render" -- the precise distinction the calm cost-horizon state exists to
  // preserve, re-introduced one level up.
  //
  // Explicit 'block', never '' -- #dq-band's rule declares display:none, so the
  // empty string would hand control back to the stylesheet and render the whole
  // band, correctly, to nobody. See the REVEAL INVARIANT block at the top.
  band.style.display = 'block';
}

// --- Cost-composition panel (#234) ------------------------------------------
// Hidden unless the server shipped a cost_composition block (a window with no
// token spend omits it, exactly like data_quality). Shows the two optimization
// levers (cache-read share, premium-model share), the attributed/unattributed
// split, and a by-model spend breakdown. Every value is a finite number or a
// server-controlled model/host string written via textContent -- the same XSS
// posture as the rest of this file.
function renderCostComposition(cc) {
  var panel = $('cost-comp');
  if (!cc) {
    panel.style.display = 'none';
    return;
  }
  var total = num(cc.total_cost_usd);
  setText('cc-total', usdText(total) + ' total');

  // Levers + attribution readouts.
  var levers = $('cc-levers');
  levers.textContent = '';
  var unattrUSD = num(cc.unattributed_cost_usd);
  levers.appendChild(buildLever(
    'Cache-read share', (num(cc.cache_read_share) * 100).toFixed(1) + '%',
    'of input tokens served from cache'));
  levers.appendChild(buildLever(
    'Premium-model share', (num(cc.premium_model_share) * 100).toFixed(1) + '%',
    'of spend on top-price-tier models'));
  levers.appendChild(buildLever(
    'Unattributed spend', usdText(unattrUSD),
    (num(cc.unattributed_share) * 100).toFixed(1) + '% with no issue attached'));

  // By-model bars, scaled to the largest model's share so the leader fills the
  // track. by_model arrives server-sorted by descending cost.
  var models = $('cc-models');
  models.textContent = '';
  var rows = cc.by_model || [];
  var scale = 0;
  rows.forEach(function(m) { scale = Math.max(scale, num(m.share)); });
  if (scale <= 0) { scale = 1; }
  rows.forEach(function(m) { models.appendChild(buildModelBar(m, scale)); });

  panel.style.display = '';
}

function buildLever(label, value, sub) {
  var box = el('div', 'cc-lever');
  box.appendChild(el('div', 'cc-lever-label', label));
  box.appendChild(el('div', 'cc-lever-value num', value));
  box.appendChild(el('div', 'cc-lever-sub', sub));
  return box;
}

// buildModelBar renders one by-model row: a label (model name, host, and a
// premium badge), a share-scaled bar, and the dollar value. model and host are
// server-controlled strings, always set via textContent.
function buildModelBar(m, scale) {
  var row = el('div', 'cc-row');

  var label = el('div', 'cc-row-label');
  // Open-weights models are host-qualified (#300); first-party Anthropic rows
  // carry the 'unknown' host sentinel, which adds no information, so suppress it.
  var name = m.model + (m.host && m.host !== 'unknown' ? ' @ ' + m.host : '');
  label.appendChild(el('span', null, name));
  if (m.premium) { label.appendChild(el('span', 'cc-badge', 'premium')); }
  row.appendChild(label);

  var track = el('div', 'cc-track');
  var fill = el('div', 'cc-fill' + (m.premium ? ' premium' : ''));
  fill.style.width = pct(num(m.share) / scale);
  track.appendChild(fill);
  row.appendChild(track);

  row.appendChild(el('div', 'cc-value num', usdText(m.cost_usd)));
  return row;
}

// --- KPI tiles --------------------------------------------------------------
// The #133 ranking floor, mirrored from scoring.MinRankedCostUSD and
// scoring.MinRankedOutcomes so the tile can say WHICH floor a below-floor org
// missed -- the server sends the verdict (`ranked`), not the thresholds behind
// it. That mirroring is a drift risk by construction, so it is pinned against the
// Go constants by TestDashboard_KPIFloorsMatchEngine: move the constant and that
// test fails rather than the caption quietly citing a floor that no longer
// exists. Declared ABOVE its reader, not hoisted -- an undefined threshold here
// would silently make every comparison false.
var MIN_RANKED_COST_USD = 5.00;
var MIN_RANKED_OUTCOMES = 3;

// BELOW_FLOOR_TAG is the single phrase this dashboard uses for "the server did not
// rank this". Declared once so the compare view's row tags and its withheld org
// cells name the verdict identically instead of each minting its own wording
// (#605 criterion 6); the yield-bar titles and the KPI cause line open with the
// same words and then add the reason their surface can actually verify. Declared
// ABOVE its readers, not hoisted, for the same reason as the floors above: an
// undefined tag would silently render "undefined" as the verdict.
var BELOW_FLOOR_TAG = 'below ranking floor';

// The org total is an aggregate over ALL work -- a legitimate whole-org number,
// not a cross-type comparison. Values come straight from scoring.RollupTeam (no
// client re-summing).
function renderKPIs(data, pooled, teamMode) {
  var total = data.total || {};
  var totalCost = num(total.total_cost_usd);
  var totalPaid = num(total.actual_paid_usd);
  var orgTIER = num(total.tier);
  var fidelity = num(total.coverage_pct);
  var leverage = num(total.spend_leverage);

  var tierValueEl = $('kpi-tier');
  var provisionalEl = $('kpi-tier-provisional');
  var noun = teamMode ? 'team' : 'developer';
  var noScore = num(total.weighted_points) <= 0;

  var orgFree = !noScore && num(total.total_cost_usd) <= 0;
  // #502: the org rollup carries the #133/#136 evidence floor on its summed
  // inputs. `!total.ranked` fails CLOSED -- a response without the field is not
  // treated as ranked, so no headline is ever published on an unstated verdict.
  var orgUnranked = !noScore && !orgFree && !total.ranked;

  if (noScore) {
    // The window held spend but no accepted outcome anywhere, so the org TIER
    // computes a literal 0.0 that is the ABSENCE of a score, not a score of zero
    // (the rule the per-row bars apply via buildTierReading, and the rule locked
    // for the Model Report). "0.0" would headline the whole org as the
    // worst-possible yield when the honest reading is "nothing merged to score
    // yet". Gate on weighted_points, exactly like the bars. The spend, leverage
    // and fidelity tiles below still render — only the yield headline is absent.
    setText('kpi-tier', 'NO SCORE');
    tierValueEl.classList.add('kpi-noscore');
    tierValueEl.classList.remove('provisional');
    setText('kpi-tier-ci', 'no accepted outcomes across ' + pooled.length + ' ' + noun + (pooled.length !== 1 ? 's' : ''));
    provisionalEl.textContent = '';
    provisionalEl.style.display = 'none';
  } else if (orgFree) {
    // Accepted work at $0 recorded cost: yield is unbounded, but the engine
    // leaves org TIER at a literal 0.0 (it divides only when cost>0). Headlining
    // "0.0" would flip the most efficient possible org into the worst-looking one
    // — the mirror of the NO SCORE case (#499). Same faint-label treatment; the
    // spend/leverage/fidelity tiles still render.
    setText('kpi-tier', 'FREE');
    tierValueEl.classList.add('kpi-noscore');
    tierValueEl.classList.remove('provisional');
    setText('kpi-tier-ci', 'accepted work at $0 recorded AI cost — yield unbounded');
    provisionalEl.textContent = '';
    provisionalEl.style.display = 'none';
  } else if (orgUnranked) {
    // Below the evidence floor (#502): the ratio is a TRUE quotient of real
    // measurements, and it is still on the wire untouched -- but 28 points over
    // $0.0001 of measured spend is 280,000,000, a number produced by a denominator
    // too small to mean anything. So the RATIO IS WITHHELD, not printed faintly.
    //
    // This is the one place the per-row treatment must NOT be copied. A below-floor
    // BAR prints its number muted with "insufficient sample to rank", which works
    // in a ranked list where the muting and the floor line carry the meaning. A KPI
    // tile has no list around it: it is the org headline, read alone and quoted
    // onward, and a muted 280,000,000.0 is still a published 280,000,000.0. Muting
    // a bar is not withholding a headline.
    //
    // The measured INPUTS stay on screen -- points and spend in the caption here,
    // spend again in its own tile, every row still listed below. Suppressing those
    // would be its own dishonesty: the reader must be able to see exactly how thin
    // the evidence is. That is why the spend is formatted below rather than passed
    // straight to toFixed(2): rounded to the cent, the canonical #502 window
    // ($0.0001) prints "$0.00" and the evidence criterion 4 exists to expose is not
    // on screen at all.
    //
    // Same kpi-noscore treatment as the two cases above, deliberately: one badge
    // taxonomy for "no headline number", never a second. The CAUSE LINE is what
    // differs, because the reader's next action does -- "no accepted outcomes"
    // means nothing shipped, this means the meter is not reading.
    //
    // Ranked is a conjunction of three conditions and the wire carries only the
    // verdict, so the cause line claims a specific cause ONLY for the one this
    // client can verify itself (spend below the floor, from the spend it was
    // handed). Otherwise it names the remaining possibilities without picking one:
    // asserting "not enough spend" over a $120 window would be a fabricated cause.
    //
    // "Check token capture." is on the SPEND arm only. On the other arm the org has
    // cleared $5.00 of measured, captured spend and is unranked because fewer than
    // MIN_RANKED_OUTCOMES outcomes merged -- sending that reader to debug capture
    // points them at the one part of the system demonstrably working. That arm
    // names its own two causes and stops.
    var belowSpendFloor = totalCost < MIN_RANKED_COST_USD;
    // usdUnder on the below-floor arm, not usdText and never toFixed(2). This
    // figure is read AGAINST the floor named in the same sentence, so it must
    // neither vanish into "$0.00" on the canonical $0.0001 window nor print as
    // having reached the floor it is below. Truncation alone does not give the
    // second property for a float64 SUM landing a few ulps under $5.00 — usdUnder
    // closes that structurally. The other arm names no floor, so it needs neither.
    var spendText = spendTextFor(totalCost);
    setText('kpi-tier', belowSpendFloor ? 'NOT ENOUGH SPEND TO SCORE' : 'NOT ENOUGH EVIDENCE TO SCORE');
    tierValueEl.classList.add('kpi-noscore');
    tierValueEl.classList.remove('provisional');
    setText('kpi-tier-ci',
      num(total.weighted_points).toFixed(1) + ' points from ' + spendText +
      ' of measured AI spend — ' +
      (belowSpendFloor
        ? 'below the ' + usdText(MIN_RANKED_COST_USD) + ' evidence floor. A yield computed on this ' +
          'little spend would be noise. Check token capture.'
        : 'clear of the spend floor but short of the rest: fewer than ' + MIN_RANKED_OUTCOMES +
          ' accepted outcomes, or an outcome with no measured tokens.'));
    provisionalEl.textContent = '';
    provisionalEl.style.display = 'none';
  } else {
    tierValueEl.classList.remove('kpi-noscore');
    // Org TIER headline (green = yield).
    setText('kpi-tier', orgTIER.toFixed(1));
    // The org "total" is a teamScoreJSON point estimate and carries no CI today
    // (only per-developer rows do, #133). Show a CI line ONLY if the server ever
    // populates one -- we never fabricate an interval. The caption otherwise
    // states the population the number is drawn from.
    var ciHigh = num(total.ci_high);
    if (ciHigh > 0) {
      setText('kpi-tier-ci', '95% CI [' + num(total.ci_low).toFixed(0) + ', ' + ciHigh.toFixed(0) + ']');
    } else {
      setText('kpi-tier-ci', 'across ' + pooled.length + ' ' + noun + (pooled.length !== 1 ? 's' : ''));
    }

    // Low-coverage headline treatment (#354 re-validation item a): when TIER is
    // computed over a thin attributed base, DE-EMPHASIZE the headline (dim + a
    // "provisional" qualifier) so an EM does not over-trust it -- do NOT hide it,
    // qualify it. The threshold is ATTR_WARN_THRESHOLD, the SAME one that flips
    // the #354 coverage banner to its red warning, so the two treatments agree by
    // construction. attributed_cost_share is a *float64 server-side (#351):
    // present (incl. a genuine 0.0) when the window has spend, absent for a
    // no-spend window, so we test for a finite number, not truthiness. Absent or
    // healthy => normal.
    var dq = data.data_quality;
    var attrShare = (dq && typeof dq.attributed_cost_share === 'number' && isFinite(dq.attributed_cost_share))
      ? dq.attributed_cost_share
      : null;
    if (attrShare !== null && attrShare < ATTR_WARN_THRESHOLD) {
      // classList.add/remove (not className=) so the tile's 'kpi-value yield num'
      // classes survive; the element is reused across loads, so the healthy
      // branch MUST clear the class and blank the qualifier or a stale dim would
      // persist.
      tierValueEl.classList.add('provisional');
      provisionalEl.textContent = 'provisional — ' + (attrShare * 100).toFixed(0) + '% attribution coverage';
      provisionalEl.style.display = '';
    } else {
      tierValueEl.classList.remove('provisional');
      provisionalEl.textContent = '';
      provisionalEl.style.display = 'none';
    }
  }

  // AI spend (neutral -- it's a cost, not a yield, so no green).
  //
  // usdText, for the same reason the cause line uses it, and it must STAY in step
  // with it: this tile and the below-floor cause line render THE SAME totalCost,
  // in the same function, both on screen at once. With toFixed(2) here they
  // disagreed -- at $4.997 the tile read "$5.00" beside a sentence asserting the
  // spend was below the $5.00 floor, and on the canonical $0.0001 window the tile
  // read "$0.00" in the larger typeface while the caption read "<$0.01". Two
  // renderings of one quantity is the contradiction, wherever it is split.
  setText('kpi-spend', spendTextFor(totalCost));
  setText('kpi-spend-sub', 'metered token cost');

  // Spend leverage -- the CFO metric. Green when positive; a net credit balance
  // (#24) is shown in the danger colour, not green.
  var levEl = $('kpi-leverage');
  var levSub = $('kpi-leverage-sub');
  levSub.className = 'kpi-sub num';
  if (leverage > 0) {
    levEl.textContent = leverage.toFixed(1) + 'x';
    levEl.className = 'kpi-value yield num';
    levSub.textContent = 'metered cost / paid spend';
  } else if (totalPaid < 0) {
    levEl.textContent = '(credit)';
    levEl.className = 'kpi-value num';
    levSub.textContent = 'net credit balance ' + usdText(Math.abs(totalPaid));
    levSub.className = 'kpi-sub num credit';
  } else {
    levEl.textContent = '\u2014';
    levEl.className = 'kpi-value num';
    levSub.textContent = 'no billed spend recorded';
  }

  // Capture fidelity meter (amber = trust). toFixed(1) so 99.6% doesn't round up
  // to "100%" and hide the not-quite-fully-captured case -- the case this number
  // exists to surface. Wire field stays coverage_pct (#136); only the label
  // changed to "Capture fidelity".
  setText('kpi-fidelity', fidelity.toFixed(1) + '%');
  $('kpi-fidelity-meter').style.width = pct(fidelity / 100);

  $('kpi-row').style.display = '';
}

// --- Provenance stamp (#239) ------------------------------------------------
// Which rubric (weights) and price table (dollars) produced every figure on the
// page. A matched rubric.version + price_table.version is the NECESSARY condition
// for comparing a TIER / cost_per_point across responses (see rubricJSON in
// handler.go), so the stamp is the caveat that makes such a comparison sound; it
// is deliberately NOT a verdict or band (#239=C keeps absolute good/ok/poor out
// of the UI). Both fields are always-present integers server-side, but we guard
// each independently and render only what is a finite number, so a future schema
// change can never crash the row or print "vundefined". version and
// effective_date reach the DOM only via textContent (setText/el), never markup.
function renderProvenance(data) {
  var stamp = $('provenance');
  var parts = [];
  var rubric = data.rubric;
  if (rubric && typeof rubric.version === 'number' && isFinite(rubric.version)) {
    parts.push('rubric v' + rubric.version);
  }
  var price = data.price_table;
  if (price && typeof price.version === 'number' && isFinite(price.version)) {
    var p = 'price table v' + price.version;
    // effective_date is a server config string; append it as data (textContent)
    // for extra provenance when present, omit it silently otherwise.
    if (typeof price.effective_date === 'string' && price.effective_date) {
      p += ' (effective ' + price.effective_date + ')';
    }
    parts.push(p);
  }
  if (parts.length === 0) {
    stamp.style.display = 'none';
    return;
  }
  // setText assigns textContent, so the assembled provenance string (server
  // integers + a config date) is never parsed as markup -- same posture as the
  // rest of this file.
  setText('provenance',
    'Scored under ' + parts.join(', ') +
    '. Compare TIER / cost-per-point only across matching versions.');
  stamp.style.display = 'block';
}

// --- Per-work-type small multiples (#187) -----------------------------------
function renderSegments(data, teamMode, since) {
  var container = $('segments');
  container.textContent = '';
  var segments = data.work_types || [];
  segments.forEach(function(seg) {
    container.appendChild(buildPanel(seg, teamMode, since));
  });
  // The retro prompt (#419) and the cross-type caveat share the panels' presence:
  // show both when there are work-type panels, hide both when there are none.
  var segShow = segments.length ? '' : 'none';
  $('segments-note').style.display = segShow;
  $('segments-retro').style.display = segShow;
}

// buildPanel renders one work-type's leaderboard as a small-multiple panel. Each
// panel computes its OWN horizontal scale, so bar lengths are never comparable
// across panels -- the UI enforces "never compare TIER across types".
function buildPanel(seg, teamMode, since) {
  var panel = el('div', 'panel');

  var rows = orderedRows(seg, teamMode);

  var head = el('div', 'panel-head');
  // textContent: work_type is a fixed enum, but treated as data on principle.
  head.appendChild(el('div', 'panel-title', seg.work_type));
  head.appendChild(el('div', 'panel-count num',
    rows.length + ' ' + (teamMode ? 'team' : 'developer') + (rows.length !== 1 ? 's' : '')));
  panel.appendChild(head);

  if (rows.length === 0) {
    panel.appendChild(el('div', 'panel-empty', 'No scored outcomes in this type.'));
    return panel;
  }

  // Panel scale: the largest RANKED row's TIER and CI upper bound, so both bars
  // and whiskers fit within the track.
  //
  // Unranked rows are EXCLUDED from the scale (#502). A below-floor row's TIER is
  // a true quotient over a denominator too small to mean anything -- the canonical
  // case is 2.8e8 -- and a scale computed over it hands that row the entire track
  // while flattening every honest reading on the panel to a zero-width bar.
  // Measured on the #502 numbers: a genuine ranked 4.2 rendered at 0% beside a
  // below-floor row at 100%. The KPI tile withholds that number precisely so it is
  // not published; the panel must not re-publish it as bar LENGTH. Unranked rows
  // still draw -- pct() clamps to 100% -- muted, and their number is read from the
  // value cell rather than inferred from the track.
  //
  // Pre-existing: the scale never consulted `ranked`, so this was already wrong
  // for below-floor DEVELOPER rows before team rows could be unranked at all.
  //
  // There is deliberately NO all-rows fallback. One was tried and re-imported the
  // same distortion one scope down: an all-below-floor panel (which per-work_type
  // splitting makes the COMMON case in team mode) scaled to its own 2.8e8 outlier
  // and flattened two perfectly readable rows to zero width. Since an unranked row
  // draws no proportional bar at all (see buildYieldBar), there is nothing left for
  // a fallback to scale — the guard below only protects the division.
  var scale = 0;
  rows.forEach(function(d) {
    if (d.ranked) { scale = Math.max(scale, num(d.tier), num(d.ci_high)); }
  });
  if (scale <= 0) { scale = 1; }

  // Ranked treatment is read from the row, in BOTH modes (#603). It used to be
  // hardcoded true for team rows -- `teamMode ? true : !!d.ranked` -- on the
  // premise that team rows carried no ranking. They now do (#502), but the
  // hardcode was already a live defect before that field existed: EVERY team row
  // rendered as ranked-green evidence at any cost level, so a 2-outcome, $0.30
  // team headlined as a measured yield. Fail closed on a missing field: `!!`
  // treats absent as unranked, so a server that does not say never gets the
  // green.
  //
  // The floor DIVIDER stays developer-only. It marks the boundary in a
  // ranked-first ordering, which is a property of orderedRows' developer sort;
  // team rows arrive server-ordered, so a divider inserted at the first unranked
  // team would draw a line through an arbitrary point in the list and claim
  // everything below it is below-floor. Per-row muting is honest without it.
  var floorInserted = false;
  rows.forEach(function(d) {
    var isRanked = !!d.ranked;
    if (!teamMode && !isRanked && !floorInserted) {
      panel.appendChild(buildFloorLine());
      floorInserted = true;
    }
    panel.appendChild(buildYieldBar(d, teamMode, isRanked, scale, since));
  });

  return panel;
}

// orderedRows applies the two-tier order (#133) in developer mode: ranked first
// (TIER desc), then below-floor (weighted points desc). Team rows arrive
// server-ordered and are returned as-is (a defensive copy either way).
function orderedRows(seg, teamMode) {
  var rows = (teamMode ? seg.teams : seg.developers) || [];
  if (teamMode) { return rows.slice(); }
  return rows.slice().sort(function(a, b) {
    var ar = a.ranked ? 1 : 0, br = b.ranked ? 1 : 0;
    if (br !== ar) { return br - ar; }
    if (ar === 1) { return num(b.tier) - num(a.tier); }
    return num(b.weighted_points) - num(a.weighted_points);
  });
}

function buildFloorLine() {
  var line = el('div', 'floor-line');
  line.appendChild(el('span', 'floor-label', 'ranking floor'));
  return line;
}

// buildYieldBar renders one leaderboard row as a horizontal yield bar. Ranked
// developer rows are green with a 95%-CI whisker; below-floor rows are muted
// with no whisker. Team rows (#185) are plain-text labels -- never a drill-down
// link and never an individual name. Every user-controlled value (developer,
// team) is written via textContent, preserving the token-exfiltration XSS
// posture; the only styles set are numeric widths/offsets.
function buildYieldBar(d, teamMode, isRanked, scale, since) {
  var row = el('div', 'ybar-row');
  // Text a screen reader must hear that the sighted reader gets from colour alone.
  // Collected here and appended after the value cell (bottom of this function) so
  // it lands last in reading order.
  var srNote = '';

  // Label cell.
  var label = el('div', 'ybar-label' + (isRanked ? '' : ' below'));
  if (teamMode) {
    label.textContent = d.team; // no drill-down; endpoint is disabled in team mode.
  } else {
    var a = el('a', 'dev-link');
    a.href = '/api/v1/scores/' + encodeURIComponent(d.developer) + '?since=' + encodeURIComponent(since);
    a.textContent = d.developer;
    a.addEventListener('click', function(e) {
      e.preventDefault();
      showDetail(e.currentTarget.href, e.currentTarget.textContent);
    });
    label.appendChild(a);
  }
  row.appendChild(label);

  // Track + fill (bar length proportional to TIER on the panel scale).
  var track = el('div', 'ybar-track');
  var tierVal = num(d.tier);
  var fill = el('div', 'ybar-fill' + (isRanked ? '' : ' below'));
  // An unranked row draws NO proportional bar (#502). Excluding it from the panel
  // scale is only half the fix: its own fill is still tierVal/scale, and pct()
  // clamps UP, so the withheld 2.8e8 came back as a full-width bar — the longest on
  // the board, next to the one honest reading. A length is a comparison, and this
  // number is not comparable; the value cell states it, the track does not imply it.
  fill.style.width = pct(isRanked ? tierVal / scale : 0);
  track.appendChild(fill);

  // 95%-CI whisker: only ranked developer rows carry a CI (team rows and
  // below-floor rows do not, #133). Positioned on the same panel scale.
  if (!teamMode && isRanked) {
    var lo = num(d.ci_low), hi = num(d.ci_high);
    // Expose the 95% CI to keyboard/screen-reader users on the row itself: the
    // whisker is a visual glyph and the value's title= is hover-only, so without
    // this the interval is inaccessible on exactly the ranked rows that carry it
    // (#274 a11y review). Team rows and below-floor rows carry no CI, so they get
    // no aria-label and read as their plain textContent.
    row.setAttribute('aria-label',
      'TIER ' + tierVal.toFixed(1) + ', 95% CI [' + lo.toFixed(0) + ', ' + hi.toFixed(0) + ']' +
      ', cost per point $' + num(d.cost_per_point).toFixed(2));
    if (hi > lo) {
      var whisker = el('div', 'ybar-whisker');
      whisker.style.left = pct(lo / scale);
      whisker.style.width = pct((hi - lo) / scale);
      whisker.title = '95% CI [' + lo.toFixed(0) + ', ' + hi.toFixed(0) + '] over ' +
        num(d.sample_n) + ' outcomes (bootstrap)';
      track.appendChild(whisker);
    }
  } else if (num(d.weighted_points) <= 0) {
    // A no-score row carries no number to read, so without this a screen reader
    // would announce only the row label and an empty bar. Say what the visual
    // NO SCORE label says, so the reason is not a sighted-only affordance.
    row.setAttribute('aria-label', 'no score: no accepted outcomes in this window');
  } else if (num(d.total_cost_usd) <= 0) {
    // Same, for a FREE row: voice the label the sighted reader sees.
    row.setAttribute('aria-label', 'free: accepted work at zero recorded AI cost, yield unbounded');
  } else if (!isRanked) {
    // WCAG 2.1 SC 1.4.1 (Use of Color). A below-floor row is distinguished from a
    // ranked one by MUTED COLOUR and nothing else unless something says so in
    // text. Developer rows at least sit under the floor divider; TEAM rows have
    // neither that divider (it is developer-only, see buildPanel) nor -- before
    // #603 -- the hover title, so colour was the only channel. #603 made
    // below-floor team rows reachable for the first time, so the row states its
    // own verdict, opening with the compare view's exact tag ('below ranking
    // floor', dashboard.js's buildTeamDumbbell) so the two views name the concept
    // identically before this one adds its reason.
    //
    // NOT an aria-label, unlike the three arms above, and the difference is
    // deliberate. A .ybar-row is a plain <div> -- role="generic" -- and the
    // accessible-name computation refuses to name a generic element, so an
    // aria-label here would be computed and then discarded. Giving the row a role
    // that accepts a name would fix that, but it would also switch ON the three
    // labels above, which were written for a row whose contents were assumed
    // unreadable and which now duplicate the number and the $/pt already in the
    // value cell. Real text, positioned off-screen, needs no role and duplicates
    // nothing. (Those three labels being inert is a separate, pre-existing finding:
    // they need rewriting before any role is added to this row.)
    srNote = 'below ranking floor: insufficient evidence to rank';
  }
  row.appendChild(track);

  // Value cell: stacks the TIER reading (green yield) over the interpretable
  // cost-per-point unit ($/pt, #239). Ranked = green yield; below-floor mutes the
  // TIER line so a low-evidence number never reads as a ranked score. The TIER CI
  // title lives on the TIER number itself (set inside buildTierReading), so hover
  // over the number surfaces the interval.
  var value = el('div', 'ybar-value' + (isRanked ? '' : ' below'));
  value.appendChild(buildTierReading(d, teamMode, isRanked));
  value.appendChild(buildCostPerPoint(d, teamMode, isRanked));
  row.appendChild(value);

  // Appended LAST so it reads after the row's own label and number rather than
  // interrupting them. .ybar-sr is clipped, not display:none -- display:none would
  // remove it from the accessibility tree, which is the whole point of it.
  if (srNote) { row.appendChild(el('span', 'ybar-sr', srNote)); }

  return row;
}

// buildTierReading renders a row's TIER number, or "NO SCORE" when the window
// held spend but no accepted outcome.
//
// TIER = points / (cost/1000), so a row with zero weighted points computes a
// literal 0.0 — but that is NOT a score of zero, it is the ABSENCE of a score:
// no work was accepted, so cost-per-accepted-outcome is undefined, not bad. The
// product's own rule (locked for the Model Report) is that zero accepted
// outcomes is NO SCORE, never a low score, and the dashboard must not contradict
// it — a repo that spent real money and merged nothing would otherwise render as
// "TIER 0.0" and read like the worst performer on the board, when the honest
// statement is that it cannot be scored yet.
//
// Two misleading zeros, both gated ahead of the number:
//   - weighted_points == 0 -> NO SCORE. No work was accepted, so
//     cost-per-accepted-outcome is undefined, not bad; "TIER 0.0" would read as
//     the worst performer on the board when nothing was merged to score.
//   - total_cost_usd == 0 (with points) -> FREE. Yield is points-per-dollar, and
//     at zero cost that is UNBOUNDED, not zero — the best possible efficiency,
//     yet the engine leaves TIER at a literal 0.0 (it only divides when cost>0).
//     Rendering that 0.0 flips the most efficient row into the worst-looking one.
//     Such a row is already below the ranking floor (MinRankedCostUSD), so it is
//     never on the board; FREE is a faint label, not a rankable number (#499).
// The NO SCORE gate comes first: a row with neither points nor cost is NO SCORE,
// not FREE. Gating on weighted_points mirrors buildCostPerPoint's "--", so the
// two sub-readings always agree.
function buildTierReading(d, teamMode, isRanked) {
  if (num(d.weighted_points) <= 0) {
    var none = el('div', 'ybar-tier ybar-noscore', 'NO SCORE');
    none.title = 'no accepted outcomes in this window — TIER is undefined, not zero (spend with nothing merged cannot be scored)';
    return none;
  }
  if (num(d.total_cost_usd) <= 0) {
    var free = el('div', 'ybar-tier ybar-noscore', 'FREE');
    free.title = 'accepted work at $0 recorded AI cost — yield is unbounded, not zero (TIER is undefined when cost is 0)';
    return free;
  }
  var tier = el('div', 'ybar-tier num', num(d.tier).toFixed(1));
  if (!teamMode) {
    tier.title = isRanked
      ? '95% CI [' + num(d.ci_low).toFixed(0) + ', ' + num(d.ci_high).toFixed(0) + '] over ' +
        num(d.sample_n) + ' outcomes (bootstrap)'
      // Names the conditions this row actually fails, never one asserted cause:
      // "insufficient sample to rank" was false for a 20-outcome, $500 row held
      // back by a single zero-token outcome, with both cleared numbers printed
      // beside it. See unrankedReasons.
      : 'below ranking floor: ' + unrankedReasons(d);
  } else if (!isRanked) {
    // WCAG 2.1 SC 1.4.1, team half. The whole title used to be gated on !teamMode
    // because team rows were hardcoded ranked and could never take the else arm;
    // #603 made them reachable, and left muted colour as the row's only signal.
    //
    // unrankedReasons cannot be reused here: a team row carries NO sample_n and no
    // flagged count by construction (that count is the k-anonymity denominator,
    // withheld on purpose -- see teamScoreJSON). Spend is the ONE condition this
    // row can check for itself, so it claims that cause only where it holds and
    // otherwise names the remaining possibilities -- the same discipline the KPI
    // cause line follows, for the same reason: over a $120 window "not enough
    // spend" would be a fabricated cause.
    tier.title = num(d.total_cost_usd) < MIN_RANKED_COST_USD
      ? 'below ranking floor: ' + usdUnder(d.total_cost_usd, MIN_RANKED_COST_USD) +
        ' of measured spend, below the ' + usdText(MIN_RANKED_COST_USD) + ' evidence floor'
      : 'below ranking floor: clear of the spend floor but short of the rest — fewer than ' +
        MIN_RANKED_OUTCOMES + ' accepted outcomes, or an outcome with no measured tokens';
  }
  return tier;
}

// buildCostPerPoint renders the $/point sub-reading under a row's TIER number.
// cost_per_point is USD per weighted point (#239) -- TIER's inverse-unit dual and
// the interpretable figure ("your cost per point is X, comparable across label
// cultures on a matched rubric/price version"). It rides every row: developer,
// team, and per-work-type segment. A zero-POINT row has no denominator, so the
// API emits cost_per_point 0/absent there and we render the no-value placeholder
// ("--", the glyph the KPI tiles already use) rather than a misleading "$0.00/pt"
// -- gating on weighted_points, not on cost_per_point, so a genuine free (zero-
// cost) but non-zero-point row still reads an honest "$0.00/pt". A ranked
// developer row also carries a self-relative 95% CI (cost_per_point_ci_low/high,
// #239), surfaced on hover/title so it does not clutter the row. All text reaches
// the DOM via el()'s textContent.
function buildCostPerPoint(d, teamMode, isRanked) {
  var points = num(d.weighted_points);
  if (points <= 0) {
    var none = el('div', 'ybar-cpp num', '\u2014');
    none.title = 'no weighted points in this window — cost-per-point undefined';
    return none;
  }
  var cpp = el('div', 'ybar-cpp num', '$' + num(d.cost_per_point).toFixed(2) + '/pt');
  // Self-relative 95% CI only on ranked developer rows (team rows and below-floor
  // rows carry none, mirroring the TIER CI). scoring.CostPerPointCI already orders
  // low < high (reciprocal transform), so [low, high] reads naturally.
  if (!teamMode && isRanked) {
    var lo = num(d.cost_per_point_ci_low), hi = num(d.cost_per_point_ci_high);
    if (hi > 0 || lo > 0) {
      cpp.title = 'cost-per-point 95% CI [$' + lo.toFixed(2) + ', $' + hi.toFixed(2) +
        '/pt] (self-relative)';
    }
  }
  return cpp;
}

// --- Attribution-coverage banner (#354) -------------------------------------
// The HONEST coverage headline: attributed_cost_share is the fraction of the
// window's spend TIER can actually tie to a real issue (1 - unattributed_share).
// It is NOT the amber Capture-Fidelity meter, which measures recorded-vs-
// estimated of the spend we DID record and reads ~100% even when almost nothing
// attributes -- confusing the two is the exact failure #351/#354 close.
//
// The field is a *float64 server-side (#351), always present when the window has
// spend INCLUDING a genuine 0.0, and absent only for a no-spend window. So we
// test for a finite number (0.0 is real and MUST render, loudly), not truthiness.
// Below ATTR_WARN_THRESHOLD the banner renders as a red WARNING so the coverage
// caveat is never a quiet stat beside a clean-looking headline. The only strings
// written are our own literals plus a formatted percentage -- all via textContent.
var ATTR_WARN_THRESHOLD = 0.5; // below this, attribution coverage is a WARNING, not a stat.

function renderAttributionCoverage(dq) {
  var banner = $('attr-coverage');
  var share = (dq && typeof dq.attributed_cost_share === 'number' && isFinite(dq.attributed_cost_share))
    ? dq.attributed_cost_share
    : null;
  if (share === null) {
    banner.style.display = 'none';
    return;
  }
  var low = share < ATTR_WARN_THRESHOLD;
  // The base look is the id-scoped calm style; the 'warn' class swaps it to the
  // danger palette. No user string ever reaches className -- it is one of two
  // literals chosen by a numeric comparison.
  banner.className = low ? 'warn' : '';
  var pctText = (share * 100).toFixed(1) + '%';
  setText('attr-coverage-title',
    'Attribution coverage — TIER can attribute ' + pctText + ' of your AI spend to issues');
  setText('attr-coverage-sub', low
    ? 'The headline TIER reflects only this attributed ' + pctText + ' of spend — the rest maps to no ' +
      'issue and is not scored, so treat the score as a floor. Most of that gap is exploratory work ' +
      'with no linked issue, which TIER keeps in the denominator on purpose — see the breakdown below. ' +
      'This is attribution coverage, not capture fidelity.'
    : 'How much of your spend maps to an issue — distinct from Capture Fidelity below, which only measures ' +
      'recorded-vs-estimated spend and can read high even when attribution is low.');
  banner.style.display = 'flex';
}

// --- Cost-horizon banner (#512) ---------------------------------------------
// The COST HORIZON is the date this installation began capturing spend. Outcomes
// arrive by webhook and backfill freely; cost only exists from the horizon
// forward. So a window reaching back past the horizon divides a FULL window of
// outcomes by a PARTIAL window of cost and reports a silently INFLATED TIER --
// measured at about twice on a real multi-repo installation.
//
// This is NOT a log-retention artifact and must not be worded as one here or
// anywhere else: extracted events are append-only and outlive the provider
// session logs they came from. The horizon is simply the date capture began, it
// is permanent per install, and no retention setting moves it.
//
// TWO server fields drive it, and BOTH must be read as types, not truthiness:
//   - cost_coverage_start: RFC3339 string, omitted only on an empty store.
//   - window_predates_cost_capture: a real boolean whose FALSE is meaningful.
//     Testing truthiness here would collapse "we checked, the window is covered"
//     into "no signal" -- the identical trap that makes 0.0 vanish in the
//     attribution banner above, and the exact failure the server emits an
//     explicit false to prevent. We therefore render in BOTH states.
//
// source_coverage_start (optional, emitted only with 2+ paying sources) refines
// it: the global horizon is the LOOSEST bound, so a window can clear it and
// still predate one capture path entirely, counting that path's outcomes against
// none of its cost. That case is a warning even though the global flag is false.
var HORIZON_MAX_SOURCES = 3; // cap named sources so a wide fleet cannot flood the line.

function renderCostHorizon(dq, since, totalCostUSD) {
  var banner = $('cost-horizon');
  var start = (dq && typeof dq.cost_coverage_start === 'string') ? dq.cost_coverage_start : null;
  // Strict boolean test: an explicit false is a real answer and MUST render.
  var predates = (dq && typeof dq.window_predates_cost_capture === 'boolean')
    ? dq.window_predates_cost_capture
    : null;
  // Absent signal. On the DASHBOARD this cannot mean version skew -- the same
  // binary serves this page and the API -- so with cost in the window the only
  // remaining cause is a horizon query that FAILED server-side (it logs and
  // omits rather than 500-ing). Hiding the banner there would render "could not
  // check" identically to "no problem here", which is the version of this bug
  // the server went to the trouble of emitting an explicit false to prevent.
  // Only a genuinely costless window earns silence.
  if (start === null || predates === null) {
    if (totalCostUSD > 0) {
      banner.className = 'warn';
      setText('cost-horizon-title', 'Cost horizon — could not be checked');
      setText('cost-horizon-sub',
        'This window has spend, but the server reported no cost-horizon signal, so whether it ' +
        'starts before cost capture began is unknown. Treat the TIER above as an upper bound and ' +
        'check the server log.');
      banner.style.display = 'flex';
      return;
    }
    banner.style.display = 'none';
    return;
  }

  // Sources whose OWN horizon starts after the window does. sourcesOK=false means
  // the comparison could not be MADE (an unusable window start, or a source whose
  // date will not parse) -- which must never render as the calm state. "Nobody
  // checked" and "checked, nothing found" are different facts.
  var late = [];
  var sourcesOK = true;
  var sinceMs = Date.parse(since);
  var perSource = (dq && dq.source_coverage_start && typeof dq.source_coverage_start === 'object')
    ? dq.source_coverage_start
    : null;
  if (perSource) {
    var names = Object.keys(perSource).sort(); // sorted so the line is deterministic
    if (isNaN(sinceMs)) {
      sourcesOK = false;
    } else {
      for (var i = 0; i < names.length; i++) {
        var ms = Date.parse(perSource[names[i]]);
        if (isNaN(ms)) { sourcesOK = false; break; }
        if (ms > sinceMs) { late.push(names[i] + ' (' + dayOf(perSource[names[i]]) + ')'); }
      }
    }
  }

  var startDay = dayOf(start);
  // The server's precomputed remedy. Falling back to the horizon's own day is
  // knowingly approximate -- since is a date and the horizon is an instant, so
  // that day is usually still hours too early and following it would not clear
  // this banner. The field exists precisely to stop us shipping that advice.
  var safeSince = (dq && typeof dq.cost_coverage_safe_since === 'string' && dq.cost_coverage_safe_since)
    ? dq.cost_coverage_safe_since
    : startDay;
  // Loud whenever ANY cost is missing from the head of the window -- globally,
  // for a single source while the global bound looks clean, or unverifiably.
  banner.className = (predates || late.length > 0 || !sourcesOK) ? 'warn' : 'calm';
  // Deliberately does NOT swap role to "alert" for the loud state (#516 review).
  // Two reasons: assistive tech commonly caches live-region properties at first
  // render, so a role flipped later is unreliable in exactly the case that
  // matters; and these banners are static-at-load content, not announcements —
  // as live regions they re-read ~1,600 characters in full on every refresh,
  // drowning the one genuinely asynchronous message on the page. They are
  // role="region" + aria-labelledby now, reachable by rotor, and #status-msg
  // owns the live region. The loud state is carried by colour, by the title text
  // itself, and by the region landing above the number it qualifies.

  if (predates) {
    setText('cost-horizon-title',
      'Cost horizon — this window starts before TIER captured any cost (' + startDay + ')');
    setText('cost-horizon-sub',
      'The TIER above is inflated: outcomes from the uncovered head of this window are counted ' +
      'against none of their cost. Set the window start to ' + safeSince + ' or later to compare ' +
      'like with like. This is not a retention setting — capture began ' + startDay + ' on this ' +
      'installation and nothing moves it backwards.');
  } else if (!sourcesOK) {
    setText('cost-horizon-title',
      'Cost horizon — per-source coverage could not be checked');
    setText('cost-horizon-sub',
      'Overall cost capture starts ' + startDay + ' and this window clears it, but the per-source ' +
      'dates could not be read, so a source that started later than this window cannot be ruled ' +
      'out. The global horizon alone cannot express that case.');
  } else if (late.length > 0) {
    var shown = late.slice(0, HORIZON_MAX_SOURCES).join(', ');
    if (late.length > HORIZON_MAX_SOURCES) { shown += ' (+' + (late.length - HORIZON_MAX_SOURCES) + ' more)'; }
    setText('cost-horizon-title',
      'Cost horizon — this window predates one or more capture sources');
    setText('cost-horizon-sub',
      'Overall cost capture starts ' + startDay + ' and this window clears it, but these sources ' +
      'started later and contribute no cost to the earlier part of the window: ' + shown + '. ' +
      'Outcomes they produced before those dates are counted against none of their spend.');
  } else {
    setText('cost-horizon-title',
      '\u2713 Cost horizon checked: captured since ' + startDay + ', this window is fully covered');
    // No sub-paragraph in the calm state. A card with a warning glyph, a bold
    // coloured title and an explanatory paragraph is this page's vocabulary for
    // "something needs your attention", and the calm state's entire message is the
    // opposite. Rendered as one muted line instead (the 'calm' class strips the
    // card), which keeps the load-bearing distinction intact -- a visible line
    // still separates "checked and covered" from "no signal at all" -- while
    // spending ~20px on good news rather than ~110px.
    //
    // Also dropped the old closing sentence, which explained to the user WHY the
    // banner appears when clean. That rationale belongs in the source comment
    // where it already lives; the reader needs the fact, not its justification.
    setText('cost-horizon-sub', '');
  }
  // Explicit 'flex', never the empty string -- see the REVEAL INVARIANT block at
  // the top of this file. (#516 fixed the eight siblings that had this defect;
  // that comment is gone because the defect is.)
  banner.style.display = 'flex';
}

// dayOf renders an RFC3339 instant as a plain UTC date. Falls back to the raw
// server string rather than printing "Invalid Date" or inventing a value.
function dayOf(ts) {
  var ms = Date.parse(ts);
  if (isNaN(ms)) { return ts; }
  return new Date(ms).toISOString().slice(0, 10);
}

// --- Unattributed-spend breakdown (#360) ------------------------------------
// Completes the #354/#360 honesty UI. The attribution-coverage banner says WHAT
// FRACTION of spend TIER can tie to an issue; this panel says WHERE the rest
// goes. Two server fields on data_quality drive it, BOTH emitted only when the
// window has unattributed spend (omit-when-clean, like the other exception
// signals -- a fully-attributed or empty window ships neither):
//   - exploratory_cost_share: the honest headline for the largest, legitimate
//     slice -- work on main with no issue. It is overhead TIER deliberately
//     KEEPS in the denominator, so it is SHOWN as context, never alarmed. It is
//     a *float64 server-side, so a genuine 0.0 is real and MUST render; we test
//     for a finite number, not truthiness (which would drop 0.0).
//   - unattributed_buckets: the labeled split ({bucket, cost_usd, share}[]),
//     server-sorted by descending cost. Each share is of TOTAL window cost, so
//     the buckets compose with attributed_cost_share.
//
// SECURITY (XSS): the bucket LABEL is a pass-through server string flagged by
// the #360 k-anon review. It is written ONLY through el()'s textContent, so a
// label containing markup renders as inert text and is never parsed as HTML --
// preserving this file's token-exfiltration XSS posture. The only inline style
// written is a numeric bar width computed from a finite share, never a string.
function renderUnattributedBreakdown(dq) {
  var panel = $('unattr-breakdown');
  var buckets = (dq && Array.isArray(dq.unattributed_buckets)) ? dq.unattributed_buckets : [];
  var expShare = (dq && typeof dq.exploratory_cost_share === 'number' && isFinite(dq.exploratory_cost_share))
    ? dq.exploratory_cost_share
    : null;
  // Omit-when-clean: a fully-attributed (or no-spend) window ships neither field,
  // so hide the whole panel rather than render an empty list or a spurious 0%.
  if (buckets.length === 0 && expShare === null) {
    panel.style.display = 'none';
    return;
  }

  // Prominent exploratory headline. The two fields are emitted together today;
  // if the scalar is ever absent while buckets are present, blank the headline
  // rather than fabricate a percentage -- the bucket bars still carry the split.
  var expLine = $('unattr-exploratory');
  var expSub = $('unattr-exploratory-sub');
  if (expShare === null) {
    expLine.textContent = '';
    expSub.textContent = '';
  } else {
    expLine.textContent = (expShare * 100).toFixed(1) + '%';
    expSub.textContent = 'of spend is exploratory / overhead — work on main with no issue. ' +
      'Legitimate overhead, kept in the denominator and shown for honesty, not flagged.';
  }

  // Bucket bars, scaled to the largest bucket's share so the leader fills the
  // track. Rows arrive server-sorted by descending cost; we do not re-sort.
  var list = $('unattr-buckets');
  list.textContent = '';
  var scale = 0;
  buckets.forEach(function(b) { scale = Math.max(scale, num(b.share)); });
  if (scale <= 0) { scale = 1; }
  buckets.forEach(function(b) { list.appendChild(buildBucketBar(b, scale)); });

  panel.style.display = 'block';
}

// buildBucketBar renders one unattributed bucket: its label, a share-scaled bar,
// and the dollar + share readout. b.bucket is a pass-through server string (#360
// k-anon review) and is written via el()'s textContent -- NEVER assembled into
// parsed markup -- so a label containing HTML renders inert. The only style set
// is the numeric bar width from a finite share.
function buildBucketBar(b, scale) {
  var row = el('div', 'ub-row');
  row.appendChild(el('div', 'ub-label', b.bucket));

  var track = el('div', 'ub-track');
  var fill = el('div', 'ub-fill');
  fill.style.width = pct(num(b.share) / scale);
  track.appendChild(fill);
  row.appendChild(track);

  row.appendChild(el('div', 'ub-value num',
    usdText(b.cost_usd) + ' (' + (num(b.share) * 100).toFixed(1) + '%)'));
  return row;
}

// --- Identity-mismatch callout (#354/#351) ----------------------------------
// unjoined_developers flags developers present on only ONE side of the
// cost/outcome join -- cost keyed to an OS username, outcomes to a GitHub login,
// un-aliased -- so they read a silent TIER=0 while a misleading org total still
// prints. The two counts are ALWAYS present when the block is; the name lists are
// populated ONLY in developer mode and suppressed under team-aggregation k-anon
// (#185). We therefore show names WHEN THE SERVER SENT THEM, else the counts
// alone -- honouring the suppression instead of second-guessing it. Every
// developer id is user-controlled and reaches the DOM via textContent (el()).
// Hidden when the block is absent (omit-when-clean) or both sides are zero.
function renderUnjoinedDevelopers(dq) {
  var strip = $('unjoined-strip');
  var list = $('unjoined-list');
  list.textContent = '';
  var uj = dq && dq.unjoined_developers;
  if (!uj) {
    strip.style.display = 'none';
    return;
  }
  var costCount = num(uj.cost_only_count);
  var outcomeCount = num(uj.outcome_only_count);
  var total = costCount + outcomeCount;
  if (total <= 0) {
    strip.style.display = 'none';
    return;
  }
  // Subject is "Cost and outcomes" (fixed plural), so only the count noun varies.
  // The previous wording pluralised the noun but not the verb and rendered
  // "1 developer have cost but no outcomes" -- a subject-verb disagreement in the
  // loudest element on the public demo's first screen, on a product whose pitch
  // is correctness. Agreement-proof by construction now, not by a second ternary.
  setText('unjoined-title',
    'Cost and outcomes are unlinked for ' + total + ' developer' + (total !== 1 ? 's' : ''));
  setText('unjoined-sub',
    'Their scores read 0 until you map their identity — cost keys to the OS username, outcomes to the ' +
    'GitHub login. ' + costCount + ' cost-only, ' + outcomeCount + ' outcome-only.');
  // Names present only in developer mode; team mode ships counts alone, so the
  // list simply stays empty and the counts above carry the signal.
  appendUnjoinedNames(list, uj.cost_only, 'cost, no outcomes');
  appendUnjoinedNames(list, uj.outcome_only, 'outcomes, no cost');
  // Names are absent in every anonymized mode, so the list is legitimately empty
  // and the disclosure must not offer to reveal what k-anonymity withheld.
  var named = (uj.cost_only || []).length + (uj.outcome_only || []).length;
  setEvidence(list, Math.min(named, TRUST_MAX_ROWS * 2), named, 'unjoined developers');
  strip.style.display = 'flex';
}

// appendUnjoinedNames appends one <li> per developer name. name is a
// user-controlled id, so it is written via el()'s textContent -- never parsed
// markup -- preserving the token-exfiltration XSS posture of this file.

// setEvidence shows or hides a strip's <details> disclosure and labels it with
// the real counts (#519 review).
//
// The disclosure is an affordance for EVIDENCE. Under team (#185) and division
// (#270) aggregation the server withholds the rows BY DESIGN, so the strip still
// renders its count while the list stays empty -- and a static <details> then
// offers to "show the flagged outcomes" directly beneath a title that just said
// identities are suppressed. That is a dead control that misdescribes the privacy
// policy, and it inverts the honesty UI the same way an invisible banner does.
//
// Explicit 'block', never '' -- see the REVEAL INVARIANT at the top of this file.
function setEvidence(list, shown, total, noun) {
  var d = list.parentNode;
  if (!d || d.tagName !== 'DETAILS') { return; }
  if (!shown) {
    d.style.display = 'none';
    d.open = false; // so a reopened window does not restore an empty disclosure
    return;
  }
  // Label carries the counts: a 1-row window and a 245-row window must not read
  // identically on the control that reveals them.
  var label = 'Show the ' + noun;
  if (total > shown) { label += ' (' + shown + ' of ' + total + ')'; }
  var sum = d.querySelector('summary');
  if (sum) { sum.textContent = label; }
  d.style.display = 'block';
}

// --- Row cap shared by the two identity-listing strips (#516) ---------------
// Declared ABOVE its first use deliberately. It previously sat below, working
// only by `var` hoisting: had it ever been undefined at call time,
// slice(0, undefined) returns the WHOLE array and `length > undefined` is false,
// so the cap and the overflow line would both vanish silently and a 245-row list
// would render with no error and no failing test. A data-quality control that
// fails open is worse than none.
//
// Both strips lead with a COUNT taken from the server, never from the sliced
// array, so truncation can never understate the problem being reported.
var TRUST_MAX_ROWS = 12;

// Capped like the trust strip (#516): a large fleet with a broken identity map
// produces one row per developer, and the counts in the title already carry the
// signal. Each side is capped independently so one long side cannot crowd the
// other out of the sample entirely.
function appendUnjoinedNames(list, names, side) {
  var all = names || [];
  all.slice(0, TRUST_MAX_ROWS).forEach(function(name) {
    list.appendChild(el('li', 'num', name + ' — ' + side));
  });
  if (all.length > TRUST_MAX_ROWS) {
    // Says WHICH rows these are. The server sorts by name, so the visible ones
    // are the alphabetically first -- not the worst, not the most recent. A bare
    // "and N more" invites the reader to assume the shown rows are the notable
    // ones. Count first so it reads sensibly aloud.
    list.appendChild(el('li', 'cmp-caveat',
      'Showing the first ' + TRUST_MAX_ROWS + ' of ' + all.length + ' (' + side + '), by developer name'));
  }
}

// --- Data-quality trust strip (#136) ----------------------------------------

// Hidden unless the server shipped a non-empty zero_token_outcomes list (or, in
// team-aggregation mode, a name-free count). Every value is written via
// textContent -- developer/issue ids are user-controlled.
function renderTrustStrip(dq) {
  var strip = $('trust-strip');
  var list = $('trust-list');
  list.textContent = '';
  var rows = (dq && dq.zero_token_outcomes) || [];
  // Team-aggregation mode (#185) suppresses the named list and ships a name-free
  // aggregate count instead, so the signal survives without identifying anyone.
  var countOnly = (dq && dq.zero_token_outcome_count) || 0;
  if (rows.length === 0 && countOnly === 0) {
    strip.style.display = 'none';
    return;
  }
  if (rows.length === 0) {
    setText('trust-title',
      'Data quality — ' + countOnly + ' outcome' + (countOnly !== 1 ? 's' : '') +
      ' merged with <1K recorded tokens (identities suppressed in team-aggregation mode)');
    setEvidence(list, 0, 0, 'flagged outcomes');
    strip.style.display = 'flex';
    return;
  }
  setText('trust-title',
    'Data quality — ' + rows.length + ' outcome' + (rows.length !== 1 ? 's' : '') +
    ' merged with <1K recorded tokens.');
  // Capped (#516). The full list is unbounded -- 245 rows on this project's own
  // database over the dashboard's default 30-day window, and ~3.5x that at 90
  // days -- and rendering all of them pushes the KPIs, the cost composition and
  // every other panel below the fold, so the page opens on a wall of bullets with
  // the number the dashboard exists to show off screen. This list was never seen
  // before (the strip itself never rendered), which is exactly why nobody hit the
  // limit. The COUNT in the title is the signal; the rows are a sample.
  rows.slice(0, TRUST_MAX_ROWS).forEach(function(o) {
    // textContent (never parsed markup): developer and issue_id are user-controlled.
    list.appendChild(el('li', 'num', o.developer + ' — issue ' + o.issue_id +
      ' (' + num(o.tokens) + ' tokens)'));
  });
  setEvidence(list, Math.min(rows.length, TRUST_MAX_ROWS), rows.length, 'flagged outcomes');
  if (rows.length > TRUST_MAX_ROWS) {
    // As in appendUnjoinedNames: name what the sample IS. Server-sorted by
    // (developer, issue), so these are alphabetically first, not worst.
    list.appendChild(el('li', 'cmp-caveat',
      'Showing the first ' + TRUST_MAX_ROWS + ' of ' + rows.length + ', by developer name'));
  }
  strip.style.display = 'flex';
}

// --- Levers CSV export (#419) -----------------------------------------------
// A team-retro takeaway: a name-free CSV of the cost levers, assembled ENTIRELY
// from the /scores JSON already in the browser (lastScores). It issues NO new
// network request and needs NO new server endpoint -- bulk exports deliberately
// 403 in anonymized modes, and this stays well clear of that policy line. Every
// exported value is a whole-window aggregate from `cost_composition` +
// `data_quality.exploratory_cost_share`; NO per-developer field is read, so the
// output names no individual in ANY aggregation mode (team/division/developer).

// csvCell quotes a field per RFC 4180 when it contains a comma, quote, CR or LF.
// Our cells are server-controlled labels and finite numbers (never a user id),
// but we escape defensively so a future field can't break the row shape.
function csvCell(v) {
  var s = String(v);
  if (/[",\r\n]/.test(s)) { return '"' + s.replace(/"/g, '""') + '"'; }
  return s;
}

// leverMode names the aggregation grain the payload was scored at, for the CSV's
// window-context column. Mirrors renderScores' teamMode test: a `teams` array
// means an anonymized grouping (the `aggregation` discriminator names it, #270);
// its absence means developer mode.
function leverMode(data) {
  if (data && Array.isArray(data.teams)) { return data.aggregation || 'team'; }
  return 'developer';
}

// buildLeversCSV assembles the levers summary from an ALREADY-loaded /scores
// payload. Returns a CSV string, or null when the window shipped no
// cost_composition (a no-spend window has no levers to export). Each lever is
// guarded for a finite value, so an absent field is SKIPPED -- never fabricated
// as 0 (exploratory_cost_share is *float64 server-side: a genuine 0.0 is real
// and MUST export, which is why we test finiteness, not truthiness) and never
// thrown on. NO per-developer field is read: the output is name-free by
// construction in every aggregation mode.
function buildLeversCSV(data, since) {
  if (!data || !data.cost_composition) { return null; }
  var cc = data.cost_composition;
  var dq = data.data_quality || {};
  var mode = leverMode(data);
  var win = data.since || since || '';
  var rubricV = (data.rubric && typeof data.rubric.version === 'number' && isFinite(data.rubric.version))
    ? data.rubric.version : '';
  var priceV = (data.price_table && typeof data.price_table.version === 'number' && isFinite(data.price_table.version))
    ? data.price_table.version : '';

  var rows = [];
  function push(lever, value, detail) {
    rows.push([lever, value, detail, mode, win, rubricV, priceV].map(csvCell).join(','));
  }
  function finite(v) { return typeof v === 'number' && isFinite(v); }

  if (finite(cc.cache_read_share)) {
    push('Cache-read share', (cc.cache_read_share * 100).toFixed(1) + '%',
      'of input tokens served from cache');
  }
  if (finite(cc.premium_model_share)) {
    push('Premium-model share', (cc.premium_model_share * 100).toFixed(1) + '%',
      'of spend on top-price-tier models');
  }
  if (finite(cc.unattributed_cost_usd)) {
    var uDetail = finite(cc.unattributed_share)
      ? (cc.unattributed_share * 100).toFixed(1) + '% with no issue attached' : '';
    push('Unattributed spend', usdText(cc.unattributed_cost_usd), uDetail);
  }
  if (finite(cc.total_cost_usd)) {
    push('Total spend', usdText(cc.total_cost_usd), 'metered token cost for the window');
  }
  if (finite(dq.exploratory_cost_share)) {
    push('Exploratory cost share', (dq.exploratory_cost_share * 100).toFixed(1) + '%',
      'work on main with no issue — legitimate overhead, shown for honesty');
  }

  if (rows.length === 0) { return null; }
  var header = ['lever', 'value', 'detail', 'mode', 'window_since', 'rubric_version', 'price_table_version'].join(',');
  return header + '\n' + rows.join('\n') + '\n';
}

// downloadLeversCSV builds the CSV from the last-rendered payload and triggers a
// client-side download via a Blob + object URL -- NO network request. Belt-and-
// braces guard on a missing/leverless payload: the button lives inside the
// cost-comp panel, which is hidden without cost_composition, so it is normally
// only reachable when there is something to export.
function downloadLeversCSV() {
  var csv = buildLeversCSV(lastScores, lastSince);
  if (!csv) {
    showStatus('No levers to export for this window.', true);
    return;
  }
  var mode = leverMode(lastScores);
  var win = (lastScores && lastScores.since) || lastSince || 'window';
  var name = ('tier-levers-' + mode + '-' + win + '.csv').replace(/[^A-Za-z0-9._-]/g, '_');

  var blob = new Blob([csv], { type: 'text/csv;charset=utf-8' });
  var url = URL.createObjectURL(blob);
  var a = document.createElement('a');
  a.href = url;
  a.download = name;
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
  // Revoke on the next tick so the click has claimed the URL first.
  setTimeout(function() { URL.revokeObjectURL(url); }, 0);
}

// --- Compare view (#278) ----------------------------------------------------
// Two-window before/after comparison over GET /api/v1/scores/compare. Window B =
// the selected period ("after"), window A = the comparison period ("before");
// every Δ is B − A, matching the endpoint contract. In an anonymized mode
// (team/division, #185/#270) the rows are k-anonymized groups and significance
// is NEVER asserted (a group aggregate carries no bootstrap CI); in developer
// mode a row is "significant" only when the SERVER flags it (present + ranked in
// BOTH windows and disjoint 95% CIs). Every value here is a finite number or a
// server-controlled developer/team string written via textContent -- the same
// XSS posture as the rest of this file; the only inline styles are dot/connector
// positions routed through pct() from finite numbers.

// setPreviousPeriod fills the baseline (window A) as the adjacent-earlier window
// of the SAME length as the selected period (window B), so "vs previous period"
// answers "did this period improve on the one before it?". Open-ended selected
// windows (no To) are treated as ending today for the length computation.
function setPreviousPeriod() {
  var fromS = $('since-input').value;
  if (!fromS) { fromS = daysAgoISO(30); $('since-input').value = fromS; }
  var toS = $('until-input').value;
  var from = new Date(fromS + 'T00:00:00Z');
  var to = toS ? new Date(toS + 'T00:00:00Z') : todayUTC();
  var lenMs = to.getTime() - from.getTime();
  if (lenMs <= 0) { lenMs = 90 * 86400000; }
  $('baseline-until-input').value = isoDate(from);                       // baseline ends where selected begins
  $('baseline-since-input').value = isoDate(new Date(from.getTime() - lenMs));
}

function loadCompare() {
  var tokenField = $('token-input');
  if (tokenField.value) {
    sessionStorage.setItem('tier_token', tokenField.value);
    tokenField.value = '';
  }
  resetViews();
  showStatus('Loading comparison...');

  var sinceB = $('since-input').value, untilB = $('until-input').value;
  var sinceA = $('baseline-since-input').value, untilA = $('baseline-until-input').value;
  var q = '/api/v1/scores/compare'
        + '?since_a=' + encodeURIComponent(sinceA)
        + (untilA ? '&until_a=' + encodeURIComponent(untilA) : '')
        + '&since_b=' + encodeURIComponent(sinceB)
        + (untilB ? '&until_b=' + encodeURIComponent(untilB) : '');
  fetchJSON(q)
    .then(renderCompare)
    .catch(function(e) { showStatus('Error: ' + e.message, true); });
}

function renderCompare(data) {
  var devMode = (data.mode === 'developer');
  var rows = (devMode ? data.developers : data.teams) || [];
  if (rows.length === 0 && !data.total) {
    showStatus('No data for the selected periods.');
    return;
  }

  $('compare-view').style.display = 'block';
  renderCompareWindows(data);
  renderCompareTotal(data.total);

  // A single shared scale (max TIER across every row-side and the org total) so
  // dot positions are comparable WITHIN the compare view -- the dumbbell analogue
  // of the per-panel yield-bar scale.
  var scale = compareScale(rows, data.total);
  var host = $('cmp-rows');
  host.textContent = '';
  if (rows.length === 0) {
    host.appendChild(el('div', 'cmp-empty',
      'No per-' + (devMode ? 'developer' : 'team') + ' rows in these periods; see the org-level change above.'));
  } else {
    for (var i = 0; i < rows.length; i++) {
      host.appendChild(devMode ? buildDevDumbbell(rows[i], scale) : buildTeamDumbbell(rows[i], scale));
    }
  }
  setText('cmp-note', compareNote(devMode));
  showStatus('');
}

// --- Compare header (windows + per-window data-quality caveats) --------------
function fmtWindow(meta) {
  if (!meta) { return '?'; }
  return meta.since + (meta.until ? ' → ' + meta.until : ' → now');
}

function renderCompareWindows(data) {
  var host = $('cmp-windows');
  host.textContent = '';
  host.appendChild(el('span', 'cmp-win-a', 'Baseline ' + fmtWindow(data.window_a)));
  host.appendChild(el('span', 'cmp-arrow', '→'));
  host.appendChild(el('span', 'cmp-win-b', 'Selected ' + fmtWindow(data.window_b)));
  host.appendChild(el('span', 'cmp-mode', data.mode || 'developer'));
  appendWindowCaveat(host, 'baseline', data.window_a && data.window_a.data_quality);
  appendWindowCaveat(host, 'selected', data.window_b && data.window_b.data_quality);
}

// appendWindowCaveat surfaces the decision-relevant data-quality flags a window
// carries (#277: each window has its OWN data_quality) as a compact amber tag, so
// a comparison drawn over a window with mixed price versions / low attribution /
// zero-token outcomes never reads as clean. Silent when the window is clean.
function appendWindowCaveat(host, which, dq) {
  if (!dq) { return; }
  var notes = [];
  if (Array.isArray(dq.mixed_price_versions) && dq.mixed_price_versions.length > 1) {
    notes.push('mixed price versions');
  }
  if (typeof dq.attributed_cost_share === 'number' && dq.attributed_cost_share < ATTR_WARN_THRESHOLD) {
    notes.push('low coverage ' + Math.round(dq.attributed_cost_share * 100) + '%');
  }
  if (num(dq.zero_token_outcome_count) > 0) {
    var n = num(dq.zero_token_outcome_count);
    notes.push(n + ' zero-token outcome' + (n === 1 ? '' : 's'));
  }
  // Cost horizon (#512). Listed FIRST in the tag because it is the only flag here
  // that means the window's TIER is structurally inflated rather than merely
  // caveated -- and compare is where it does the most damage: window A is by
  // construction the older one, so it is the likeliest to predate the horizon,
  // and the resulting delta reads as a real regression carrying a significance
  // flag. The single-window path gives this a full red banner; without this line
  // the compare view gave it nothing at all, which is the one outcome #512 exists
  // to prevent. Strict boolean test so an explicit false stays silent but a
  // missing field never masquerades as covered.
  if (dq.window_predates_cost_capture === true) {
    notes.unshift('predates cost capture — TIER inflated');
  }
  if (notes.length === 0) { return; }
  host.appendChild(el('span', 'cmp-caveat', which + ': ' + notes.join(', ')));
}

// --- Org-level delta card ----------------------------------------------------
// renderCompareTotal applies #605's one-sentence rule: ANYTHING DERIVED FROM AN
// UNRANKED INPUT IS ITSELF UNRANKED.
//
// This card is the SECOND of the org TIER headline's three consumers and, until
// #605, the only unguarded one on a live surface. It re-derived the reading from
// the sides instead of reading the verdict, so #502's floor was asserted on the
// main view and violated one tab over.
//
// Three cells are gated and one is not, and the split is the point:
//   - each Org TIER headline under ITS OWN side's `ranked`, exactly as the KPI
//     tile does. A ranked side keeps its honestly-earned headline.
//   - Δ Org TIER and its % change under `total.ranked`, the server-derived
//     A.Ranked && B.Ranked (see teamDeltaJSON.Ranked). EITHER side being unranked
//     withholds both, because with a ranked baseline beside an unranked selected
//     window `selected = baseline + Δ` reconstructs the withheld number exactly —
//     and `% change = Δ/baseline` is a pure function of the withheld ratio, so it
//     leaks directly rather than additively.
//   - Δ AI spend always renders. It is a MEASURED INPUT, not a derived ratio, and
//     the same criterion that withholds the quotient requires the evidence behind
//     the verdict to stay on screen — the reader must see how thin it is.
//
// The withheld verdict is READ, never recomputed here. A client-side
// reimplementation of the AND is exactly what the ruling rejected: it forfeits the
// propagation that made one field able to fix three consumers. `!!` fails CLOSED,
// so a server that does not say never gets a published delta.
//
// SHAPE, not taste: every TIER-bearing cell goes through cmpRankedCell, which
// takes its gate as an ARGUMENT and discards the value when the gate is false, and
// the only ungated cmpTotalCell call on this card is the measured spend. That is
// what lets TestDashboard_CompareTotalSuppressesUnrankedRatio decide the question
// by reading four call sites instead of trying to prove dominance over an
// arbitrary expression — and it is why a value can never be routed onto the card
// past a gate.
function renderCompareTotal(total) {
  var card = $('cmp-total');
  var grid = $('cmp-total-grid');
  grid.textContent = '';
  if (!total) { card.style.display = 'none'; return; }
  card.style.display = 'block';

  var aRanked = !!(total.a && total.a.ranked);
  var bRanked = !!(total.b && total.b.ranked);
  var deltaRanked = !!total.ranked;

  var aT = num(total.a && total.a.tier), bT = num(total.b && total.b.tier);
  var dT = num(total.delta_tier);
  var dir = deltaDir(dT);
  var pctChange = (aT !== 0) ? (dT / aT) * 100 : null;
  var sub = (pctChange === null) ? 'no baseline yield' : (signStr(pctChange) + Math.abs(pctChange).toFixed(1) + '%');

  grid.appendChild(cmpRankedCell('Org TIER — baseline', aRanked, aT.toFixed(1), null, null));
  grid.appendChild(cmpRankedCell('Org TIER — selected', bRanked, bT.toFixed(1), null, null));
  grid.appendChild(cmpRankedCell('Δ Org TIER', deltaRanked, signStr(dT) + Math.abs(dT).toFixed(1), sub, dir));

  // Ungated, and the only such cell: Δ AI spend is a MEASURED INPUT, not a derived
  // ratio. #502's criterion 4 requires the evidence behind a withheld verdict to
  // stay on screen — suppressing it too would be its own dishonesty, because the
  // reader could no longer see how thin the evidence is.
  var dCost = num(total.delta_total_cost_usd);
  grid.appendChild(cmpTotalCell('Δ AI spend', signStr(dCost) + '$' + Math.abs(dCost).toFixed(2), null, null));
}

// cmpRankedCell is the gate. It renders `value`/`sub`/`dir` only when `ranked` is
// true and otherwise DISCARDS all three, returning the withheld cell — so the
// decision cannot be separated from the rendering, and no call site can carry a
// number past its own gate. Four cells go onto the card either way, in the same
// order, so the grid tracks never move between the two states (the card's shape is
// what got option D rejected).
function cmpRankedCell(label, ranked, value, sub, dir) {
  if (!ranked) { return cmpWithheldCell(label); }
  return cmpTotalCell(label, value, sub, dir);
}

// cmpWithheldCell renders a withheld cell of the org delta card. It takes a LABEL
// and nothing else: the withheld state has no value to pass, so no caller can
// smuggle one through, and every withheld cell on the card is identical by
// construction.
//
// Four rules, all from the ruling:
//   - An explicit em dash plus a stated reason, NEVER a blank. A blank cell trains
//     the reader to treat a withheld number as a rendering bug, and a floor whose
//     output looks broken stops being credible.
//   - BELOW_FLOOR_TAG verbatim, so compare has one vocabulary rather than a second
//     phrase for the same verdict.
//   - dir is null, so the cell takes no `cmp-delta up/down` class. A withheld delta
//     that renders in confident green or red is a directional claim about a number
//     we just declined to publish.
//   - The state reaches assistive tech as TEXT, not as colour and not as an
//     aria-label. A .cmp-total-cell is a plain <div> — role="generic" — and the
//     accessible-name computation refuses to name a generic element, so an
//     aria-label here would be computed and discarded (measured on the #603 team
//     rows). Real off-screen text needs no role. Appended LAST so it reads after
//     the cell's own label and dash.
function cmpWithheldCell(label) {
  var cell = cmpTotalCell(label, '—', BELOW_FLOOR_TAG, null);
  cell.appendChild(el('span', 'cmp-total-sr',
    'withheld: ' + BELOW_FLOOR_TAG + ' — insufficient evidence to rank this window'));
  return cell;
}

function cmpTotalCell(label, value, sub, dir) {
  var cell = el('div', 'cmp-total-cell');
  cell.appendChild(el('span', 'cmp-total-label', label));
  var v = el('span', 'cmp-total-value num' + (dir ? ' cmp-delta ' + dir : ''), value);
  cell.appendChild(v);
  if (sub) { cell.appendChild(el('span', 'cmp-total-sub num' + (dir ? ' cmp-delta ' + dir : ''), sub)); }
  return cell;
}

// --- Dumbbell rows -----------------------------------------------------------
// deltaDir/signStr classify a delta for colour (up = green yield, down = danger,
// flat = muted) and its printed sign. A small epsilon avoids a −0.0/"+0.0" flip.
function deltaDir(v) { return v > 1e-9 ? 'up' : (v < -1e-9 ? 'down' : 'flat'); }
function signStr(v) { return v > 1e-9 ? '+' : (v < -1e-9 ? '−' : '±'); }

// compareScale is the shared denominator for dot positions: the largest RANKED
// TIER seen on any row-side or the org total, floored at 1 so a zero-yield
// comparison does not divide by zero.
//
// Gating on `ranked` (#605) is required for the chart to RENDER, not merely for it
// to be honest. Feeding a below-floor total into this denominator was measured
// collapsing every row's dot — ranked and unranked alike — to zero width: the
// canonical #502 window is 28 points over $0.0001, i.e. 2.8e8, so every real
// reading on the chart divides to ~0 and the dumbbells vanish. Same defect, same
// value, same page as buildPanel's scale.
//
// Each SIDE is tested on its own `ranked`, not the row's conjunction: a row with a
// ranked baseline and a below-floor selected window still contributes the reading
// that IS ranked. `&&` fails closed on a missing field.
function compareScale(rows, total) {
  var mx = 0;
  for (var i = 0; i < rows.length; i++) {
    mx = Math.max(mx, rankedTier(rows[i].a), rankedTier(rows[i].b));
  }
  if (total) { mx = Math.max(mx, rankedTier(total.a), rankedTier(total.b)); }
  return mx > 0 ? mx : 1;
}

// rankedTier returns a side's TIER for SCALING purposes: its number when the
// server ranked it, and 0 otherwise.
//
// ⚠️ It is the gate for the shared DENOMINATOR only. This comment used to claim it
// was "one function for the scale and the dot positions, so the two can never
// disagree" — that is not what the code does. Dot positions go through plotA/plotB,
// which apply a STRICTLY STRONGER gate (`has && ranked`, where `has` additionally
// requires sample_n > 0 on a developer row). The denominator gate being weaker is
// safe only because the server guarantees ranked ⟹ sample_n >= MinRankedOutcomes,
// which is pinned in internal/api/compare_ranked_test.go rather than assumed here.
// Were that to stop holding, a side could enter the denominator while being neither
// plotted nor printed, and every published dot inverts the denominator.
function rankedTier(side) {
  return (side && side.ranked) ? num(side.tier) : 0;
}

// sideData reports whether a side of a developer row has real data to plot: it is
// present in that window AND has at least one outcome (sample_n > 0). A present-
// but-zero-sample side is treated as "no data" for that dot (#278 acceptance),
// never a misleading 0.
function sideData(present, side) {
  return !!(present && side && num(side.sample_n) > 0);
}

// buildDumbbellTrack renders the shared track: an optional connecting rule (only
// when both sides plot) and the A (before, hollow) / B (after, filled) dots.
// posFracA/posFracB are 0..1 fractions; every inline position goes through pct().
function buildDumbbellTrack(hasA, fracA, hasB, fracB, bClass, connectClass) {
  var track = el('div', 'cmp-db-track');
  if (hasA && hasB) {
    var lo = Math.min(fracA, fracB), hi = Math.max(fracA, fracB);
    var conn = el('div', 'cmp-db-connect' + (connectClass ? ' ' + connectClass : ''));
    conn.style.left = pct(lo);
    conn.style.width = pct(hi - lo);
    track.appendChild(conn);
  }
  if (hasA) {
    var a = el('div', 'cmp-db-dot a');
    a.style.left = pct(fracA);
    track.appendChild(a);
  }
  if (hasB) {
    var b = el('div', 'cmp-db-dot b' + (bClass ? ' ' + bClass : ''));
    b.style.left = pct(fracB);
    track.appendChild(b);
  }
  return track;
}

// cmpRankedSide is the PER-SIDE publication gate (#613 ruling of 2026-08-05).
//
// The rule it applies, and the whole rule:
//
//     A TIER DIGIT IS PRINTED IFF ITS OWN SIDE IS RANKED.
//
// No row count, no grain, no builder name, no mode — which is why one function
// serves both dumbbell builders. It is #605's own card rule (cmpRankedCell gates
// each Org TIER cell on that side's flag and only the Δ on the conjunction) pushed
// down two grains, so PUBLICATION granularity is now identical to PLOTTING
// granularity (plotA/plotB). The two channels can no longer disagree, and that
// disagreement is exactly what produced this defect: the row-level conjunction
// withheld a side the system had ranked, while the dot for that same side stayed on
// the track — so the withheld digits were recoverable from the position beside them
// to three decimals (17.5000 against an actual 17.5).
//
// SHAPE, not taste, for the same reason as cmpRankedCell: the gate is an ARGUMENT
// and the unranked path DISCARDS `tier`, so no call site can carry a below-floor
// number past its own gate and a guard decides the question by reading one call
// site instead of proving dominance over an expression.
//
// THREE outcomes, and the distinction between the last two is load-bearing:
//   'n/a'  — no data on this side (absent, or present with zero outcomes)
//   '—'    — data exists and is WITHHELD because the side is below the floor
//   digits — the side is ranked, so its magnitude is publishable
// 'n/a' is the pre-existing vocabulary and keeps its meaning; the em dash is the
// same one every withheld cell on this page already uses. A blank is never
// returned: it trains the reader to read a withheld number as a rendering bug.
function cmpRankedSide(has, ranked, tier) {
  if (!has) { return 'n/a'; }
  if (!ranked) { return '—'; }
  return num(tier).toFixed(1);
}

// abReadout composes the A→B readout from two independently gated sides. It holds
// no gate of its own: both halves go through cmpRankedSide, so there is no path
// through this function that prints a digit its side did not earn.
function abReadout(hasA, aRanked, aTier, hasB, bRanked, bTier) {
  return cmpRankedSide(hasA, aRanked, aTier) + ' → ' + cmpRankedSide(hasB, bRanked, bTier);
}

// withheldSidesNote states the withholding as TEXT for assistive tech, and NAMES
// WHICH SIDE. Per-side withholding is asymmetric, so the row-level sentence this
// replaced ("…to rank this window") was singular on a row that has two, and wrong in
// the exact case the ruling is about.
//
// It takes, per side, whether that side's number is WITHHELD — the side has data and
// is below the floor. A side with NO data is NOT withheld, it is absent, and the tag
// cell already says which window is missing (missingSideTag). Calling it withheld
// would claim we are holding back a number that does not exist, which is the
// 'n/a' vs '—' distinction cmpRankedSide draws, restated for a reader who cannot
// see the glyphs.
//
// Empty string when nothing is withheld, so the caller appends NO span rather than an
// empty one: a span in the DOM saying nothing is indistinguishable to a screen reader
// from no span at all — the empty-string reveal defect this file guards elsewhere.
//
// The window words are the page's existing vocabulary ('baseline' / 'selected', as on
// the Org TIER cards and in missingSideTag), not a second phrasing minted here.
function withheldSidesNote(withheldA, withheldB) {
  if (!withheldA && !withheldB) { return ''; }
  var which = !withheldA ? 'the selected period' : (!withheldB ? 'the baseline period' : 'both periods');
  return 'withheld: ' + BELOW_FLOOR_TAG + ' — insufficient evidence to rank ' + which;
}

// missingSideTag labels a one-sided developer row precisely: "no outcomes in
// <window>" when the developer was PRESENT in that window but shipped nothing
// (sample_n == 0), versus "only in <window>" when they were truly absent -- so a
// present-but-unproductive window is never mislabeled as absent (honesty, #278).
function missingSideTag(hasA, presentA, hasB, presentB) {
  if (!hasA && !hasB) { return 'no outcomes in either period'; }
  if (!hasA) { return presentA ? 'no outcomes in baseline' : 'only in selected period'; }
  return presentB ? 'no outcomes in selected' : 'only in baseline period';
}

function buildDevDumbbell(row, scale) {
  var hasA = sideData(row.present_a, row.a);
  var hasB = sideData(row.present_b, row.b);
  var both = hasA && hasB;
  // HOISTED to the same two identifiers buildTeamDumbbell uses (#613, 2026-08-05).
  // They were previously read inline inside plotA/plotB only, which is precisely how
  // the two channels drifted: the digits gated on the conjunction while the dots
  // gated per side. Naming them once means the publication gate and the plot flag are
  // THE SAME FLAG, which is the cross-channel identity a named guard now enforces.
  // `!!(row.x && …)` fails closed: a server that does not say never gets published.
  var aRanked = !!(row.a && row.a.ranked);
  var bRanked = !!(row.b && row.b.ranked);
  var bothRanked = both && aRanked && bRanked;
  var sig = both && !!row.significant; // server-authoritative significance (#277)
  var d = num(row.delta_tier);
  // Only a RANKED side is PLOTTED (#605), which is the second half of the scale
  // fix: excluding an unranked reading from the denominator leaves its own dot at
  // tier/scale, and pct() clamps UP — so the 2.8e8 the scale just refused came back
  // pinned to the far right of the track, reading as the best yield in the view.
  // Exactly the defect buildYieldBar's zero-width fill closes one panel over.
  //
  // hasA/hasB are NOT redefined: they mean "this side has data", and they still
  // drive the missing-side tag and the 'n/a' arm of the readout.
  //
  // ⚠️ The comment that stood here said a below-floor side "is present and its number
  // is stated in the readout; what it does not get is a POSITION". That is no longer
  // true and was the defect: since 2026-08-05 a below-floor side gets NEITHER. The
  // digit and the dot are gated on the same flag, so the position can never encode
  // what the text declined to state.
  var plotA = hasA && aRanked;
  var plotB = hasB && bRanked;
  var fracA = plotA ? num(row.a.tier) / scale : 0;
  var fracB = plotB ? num(row.b.tier) / scale : 0;

  var rowEl = el('div', 'cmp-db-row');
  var label = el('div', 'cmp-db-label' + (bothRanked ? '' : ' below'));
  label.textContent = row.developer;
  rowEl.appendChild(label);

  var bClass, connectClass, tag, deltaCls, deltaText;
  if (!both) {
    // No before/after claim -- show the one plottable dot and say why the other
    // side is missing. Distinguish truly-absent (no data at all) from present-
    // but-zero-outcomes (spend, but nothing merged), so a dev who was active-yet-
    // shipped-nothing is not mislabeled as absent. Server leaves the delta 0 here.
    bClass = 'below'; connectClass = '';
    tag = missingSideTag(hasA, !!row.present_a, hasB, !!row.present_b);
    // …and if the ONE side that has data is itself below the floor, this row's track
    // is now completely empty (#605 stopped plotting unranked sides), while the tag
    // talks only about the window that is missing. An empty track with no stated
    // cause is the blank cell cmpWithheldCell exists to avoid, one component over:
    // the reader sees nothing and cannot tell a withheld reading from a broken
    // render. Name the floor first, then the missing window.
    if ((hasA && !plotA) || (hasB && !plotB)) {
      tag = BELOW_FLOOR_TAG + ', ' + tag;
    }
    deltaCls = 'flat'; deltaText = '—';
  } else if (sig) {
    var dir = deltaDir(d);
    connectClass = '';
    // ALL THREE CHANNELS GATED (#613). `sig` is server-authoritative and the server
    // pins that a below-floor row is never significant (compare.go computes
    // Significant = a.Ranked && b.Ranked && ciDisjoint), so this arm should be
    // unreachable without both sides ranked — but "should be unreachable" is not an
    // enforcement, and a server contradiction here would assert a tested,
    // beyond-noise move about numbers this row just withheld.
    //
    // 🔴 The dot's COLOUR is the third channel, and the first draft of this hardening
    // gated only the other two. `.cmp-db-dot.b.up`/`.down` paint yield-green and
    // danger-red, so an ungated bClass publishes SIGN(Δ) for a Δ printed as '—' —
    // the same cross-channel leak this ruling exists to close, in the arm written to
    // close it. When you harden against a class, the next question is which SITES
    // got the hardening.
    bClass = bothRanked ? dir : 'below';
    tag = bothRanked ? 'significant' : BELOW_FLOOR_TAG;
    deltaCls = dir; deltaText = signStr(d) + Math.abs(d).toFixed(1);
  } else {
    // Both present but not significant: within the CI (noise) or below the
    // ranking floor. Dashed connector + muted delta so it never reads as a
    // confident move; the tag says which.
    //
    // ⚠️ THIS is the arm every two-sided below-floor developer row actually reaches —
    // the `sig` arm above cannot fire for one. Its tag is gated for that reason: left
    // ungated it would label a withheld row 'not significant', i.e. "we tested and
    // found nothing", when the truth is "we cannot rank this at all".
    bClass = ''; connectClass = 'insignificant';
    tag = bothRanked ? 'not significant' : BELOW_FLOOR_TAG;
    deltaCls = 'flat'; deltaText = signStr(d) + Math.abs(d).toFixed(1);
  }
  rowEl.appendChild(buildDumbbellTrack(plotA, fracA, plotB, fracB, bClass, connectClass));

  // RULED 2026-08-05 (#613 half b): developer rows go through the SAME gate as team
  // rows, at side grain. This was the open question the previous comment here
  // recorded — deliberately deferred rather than closed by inference, because #613's
  // first criteria named buildTeamDumbbell and its exclusions named the main panel's
  // bars and the KPI tile (#502 territory), leaving a compare-view DEVELOPER row
  // named by neither: an unruled gap inside a ruled scope.
  //
  // What decided it: on a SOLO-DEVELOPER install — the common self-hosted OSS case —
  // this one row IS the org total, so the card above withholding that headline as '—'
  // while this line printed it in full is the same nullification #613 was ruled to
  // close, one grain down. The harm ordering was inverted: the single-cohort
  // k-anonymity fold is the corner case, the solo install is the common one.
  //
  // ⚠️ It also overturns a TESTED commitment. #605 asserted, and pinned in
  // TestDashboard_CompareScaleIgnoresUnrankedSides, that a below-floor developer side
  // "states its number, but is not placed" on the scale. That is now false in both
  // halves, and compareNote's developer clause changed in this same commit — a legend
  // describing behaviour the code does not have is the defect #613 exists to close.
  //
  // 🔴 `bothRanked` is NOT the per-side gate here and must never become one: it folds
  // `both` (PRESENCE) in, so it is false for every ONE-SIDED row. Passing it per side
  // would withhold a legitimately RANKED one-sided reading — over-withholding, which
  // teaches the reader that '—' sometimes means "we have this and won't say it" and
  // dilutes the signal #605 spent a whole card on. It stays the Δ gate, which is what
  // it correctly describes: a derived figure needs both sides.
  rowEl.appendChild(cmpRankedRowValue(tag, deltaCls, bothRanked, deltaText,
    abReadout(hasA, aRanked, row.a && row.a.tier, hasB, bRanked, row.b && row.b.tier),
    withheldSidesNote(hasA && !aRanked, hasB && !bRanked)));
  return rowEl;
}

function buildTeamDumbbell(row, scale) {
  // A k-anonymized team is present in BOTH windows by construction (the compare
  // endpoint intersects the k-floor across windows), so both sides have data.
  // Direction is shown by the Δ sign, but the dot stays NEUTRAL and the connector
  // dashed: an aggregate has no CI, so we never assert the move is beyond noise
  // (#277).
  var hasA = !!row.a, hasB = !!row.b;
  var d = num(row.delta_tier);
  // #603's own defect, in a second location. buildDevDumbbell has computed
  // bothRanked and muted on it since #278; team rows read `ranked` on neither side,
  // so a 3-developer, $0.30 team rendered as full-authority evidence — the same
  // hardcoded-ranked lie the yield bars carried until #603, one view over. `!!`
  // fails closed: a server that does not say never gets the unmuted treatment.
  var aRanked = !!(row.a && row.a.ranked);
  var bRanked = !!(row.b && row.b.ranked);
  var bothRanked = aRanked && bRanked;
  // Ranked sides only are PLOTTED, for the reason spelled out in buildDevDumbbell:
  // pct() clamps up, so an unranked side excluded from compareScale would otherwise
  // pin its dot to the end of the track.
  //
  // ⚠️ The comment here used to end "these flags stay the plotting question, not the
  // publication one, so the two rules cannot drift onto one flag." That was exactly
  // backwards, and the drift it permitted was the defect: publication gated on the
  // conjunction, plotting per side, and the gap between them WAS the leak. Since
  // 2026-08-05 these ARE the publication flags too — one flag per side, both
  // channels — which is the identity dumbbellPlotViolations now derives and enforces.
  var plotA = hasA && aRanked;
  var plotB = hasB && bRanked;
  var fracA = plotA ? num(row.a.tier) / scale : 0;
  var fracB = plotB ? num(row.b.tier) / scale : 0;

  var rowEl = el('div', 'cmp-db-row');
  rowEl.appendChild(el('div', 'cmp-db-label' + (bothRanked ? '' : ' below'), row.team || 'other'));
  rowEl.appendChild(buildDumbbellTrack(plotA, fracA, plotB, fracB, '', 'insignificant'));

  // "aggregate — not tested" says significance was not TESTED, which is true of
  // every team row and is a statement about method. It is not true that a
  // below-floor row merely went untested: it lacks the evidence to rank at all, so
  // it names the verdict the rest of the dashboard names.
  var tag = bothRanked ? 'aggregate — not tested' : BELOW_FLOOR_TAG;
  // Every digit goes through a gate, and NOTHING new is threaded in: `aRanked`,
  // `bRanked` and `bothRanked` are all computed above, and aRanked/bRanked are the
  // SAME flags plotA/plotB read. That identity is the point — it is what makes the
  // published set and the plotted set equal, and #613's ruling made it a named guard
  // so drift in either direction is caught without anyone remembering the case.
  //
  // The delta class is the literal 'flat' for a team row: an aggregate has no CI, so
  // its magnitude is shown but never asserted as beyond noise (#277).
  rowEl.appendChild(cmpRankedRowValue(tag, 'flat', bothRanked,
    signStr(d) + Math.abs(d).toFixed(1),
    abReadout(hasA, aRanked, row.a && row.a.tier, hasB, bRanked, row.b && row.b.tier),
    withheldSidesNote(hasA && !aRanked, hasB && !bRanked)));
  return rowEl;
}

// --- A dumbbell row's value column, gated (#613) ------------------------------
// #613 AMENDS #603, and the 2026-08-05 ruling then AMENDED #613's own first answer.
// The history matters, because each step was a correction of the step before:
//
//   #603  ruled a below-floor team row "muted but printed" — a row sits in a LIST,
//         and the muting plus the tag beside it carry the verdict.
//   #613  retired that: when the k-anonymity fold yields a SINGLE cohort, the one
//         row IS the org total, so it republished the headline #605 had withheld one
//         line above — including, via an unconditionally printed Δ, the withheld
//         `Δ Org TIER`. #605 was not merely contradicted there, it was NULLIFIED.
//         Its answer withheld the WHOLE value column on `bothRanked`.
//   2026-08-05  that answer was too strict, and the excess strictness became a LEAK.
//         Publication gated on the CONJUNCTION while plotting gated PER SIDE, so a
//         row with a ranked A side and an unranked B side printed '—'/'—' and still
//         placed its A dot. pct() writes three decimals, so the withheld number was
//         recoverable EXACTLY (17.5000 against an actual 17.5) — and, through the
//         shared denominator, recoverable from OTHER rows' dots as well.
//
// The rule now, and it has no row count, no grain and no builder name in it:
//
//     A TIER DIGIT IS PRINTED IFF ITS OWN SIDE IS RANKED.
//     A DERIVED FIGURE (Δ) IS PRINTED IFF BOTH SIDES ARE RANKED.
//
// That is exactly what renderCompareTotal's card has always done (cmpRankedCell per
// side; Δ on `deltaRanked`), so the rows and the card above them are now one rule at
// three grains rather than three rules. Publication granularity EQUALS plotting
// granularity, which is the invariant that makes the dot innocent again: every dot
// on the track has its own number printed in text on the same row, so inverting a
// position recovers only something already on screen.
//
// Suppressing only "when there is one row" was refused at every stage and stays
// refused: a cohort count is not visible to either builder (both take `row` and
// `scale`; only the loop in renderCompare knows `rows.length`), and a rule that
// needs new plumbing to express is a rule that drifts away from its enforcement.
//
// #603's SUBSTANCE survives — a below-floor reading must not render as
// full-authority evidence — and so does #136: the stored number is never altered
// and is still on the wire untouched. What is revoked is its display AUTHORITY.
//
// SHAPE, not taste, for the same reason as cmpRankedCell. TWO gates live here and
// both are ARGUMENTS whose withheld path DISCARDS its value: `bothRanked` for the Δ
// (below), and cmpRankedSide's `ranked` for each digit (above). A guard therefore
// decides the question by reading one call site rather than proving dominance over
// an expression. The withheld Δ also drops back to the LITERAL 'flat' class, never
// the caller's: a number we declined to publish must not render as a confident green
// or red claim — the same rule cmpWithheldCell enforces with a null direction.
//
// The srNote span is appended LAST so it reads after the row's own label and tag,
// and only when there IS something withheld — a span in the DOM saying nothing is
// indistinguishable to a screen reader from no span at all. NOT an aria-label: a
// .cmp-db-value is a bare <div>, i.e. role="generic", and the accessible-name
// computation refuses to name a generic element. Measured inert on the #603 rows.
function cmpRankedRowValue(tag, deltaCls, bothRanked, deltaText, ab, srNote) {
  var val = el('div', 'cmp-db-value');
  val.appendChild(el('div', 'cmp-db-delta num ' + (bothRanked ? deltaCls : 'flat'),
    bothRanked ? deltaText : '—'));
  val.appendChild(el('div', 'cmp-db-tag', tag));
  val.appendChild(el('div', 'cmp-db-ab num', ab));
  if (srNote) { val.appendChild(el('span', 'cmp-db-sr', srNote)); }
  return val;
}

// compareNote is the on-page legend, and it has to describe what the chart
// ACTUALLY does. Before #605 it promised that "each row plots its TIER before and
// after on a shared scale" — true of every row until an unranked side stopped
// being placed on that scale. A legend describing behaviour the code no longer has
// is the same defect this branch exists to close, one element over: an output
// contradicting itself.
function compareNote(devMode) {
  // ONE sentence now describes BOTH views, because as of 2026-08-05 there is one rule
  // (#613): a side below the floor is neither placed nor stated, at either grain. The
  // previous split — teams "state no number at all", developers "still state their
  // number" — was two rules on one screen, switchable by a mode toggle, and the
  // developer clause is the one #605 had asserted and pinned in a test.
  var base = 'Δ = selected period − baseline period. Each row plots its TIER before (hollow) and ' +
    'after (filled) on a shared scale of ranked readings; a side below the ranking floor is ' +
    'neither placed on that scale nor stated as a number — its denominator is too small for ' +
    'either to mean anything. A Δ is shown only when both periods are ranked, since a difference ' +
    'of two readings is no more publishable than the readings themselves.';
  if (devMode) {
    return base + ' A move is "significant" only when the developer is ranked in both periods and their 95% confidence intervals do not overlap; otherwise it is within noise (dashed) or below the ranking floor. A below-floor side is withheld on the same terms as the headline above it, always — not only when a single developer makes this row the whole organisation.';
  }
  return base + ' Rows are k-anonymized teams — no individual names; significance is not tested for a group aggregate, so the magnitude is shown but never asserted as beyond noise. The withholding does not depend on how many rows are present: when the k-anonymity fold yields a single cohort that row IS the org total, and publishing it would republish the headline withheld above it.';
}

// --- Wiring -----------------------------------------------------------------

// Default the selected-window "From" to 30 days ago, UTC-anchored via the same
// helper the presets use -- so first paint matches the "30d" chip exactly and
// both align to the server's UTC window boundaries (not local date math, #278).
//
// 30d, not 90d (#497): Claude Code retains only ~30 days of JSONL (the cost
// side), while backfill reconstructs outcomes across the full window. A 90d
// default divides ~90d of outcomes by ~30d of cost, inflating the headline TIER
// ~2x on first paint (measured on a real multi-repo installation). 30d
// window-matches the common retention so the first number a user sees is honest;
// the 90d / quarter chips still let them widen deliberately. (Auto-deriving the
// default from the actual earliest token_events.ts -- self-adjusting to a proxy
// install with longer retention -- is the follow-up refinement.)
$('since-input').value = daysAgoISO(30);

// Replay a STORED preference on load; if none exists leave data-theme unset so
// the CSS @media(prefers-color-scheme) drives first paint. Never persist here.
(function() {
  var stored = storedTheme();
  if (stored) { applyTheme(stored); }
})();

$('refresh-btn').addEventListener('click', load);
$('theme-toggle').addEventListener('click', function() {
  setTheme(effectiveTheme() === 'dark' ? 'light' : 'dark');
});
$('detail-close').addEventListener('click', function() {
  $('detail-card').style.display = 'none';
});
$('cc-export').addEventListener('click', downloadLeversCSV);

// Range presets (#278): set the SELECTED period (window B) to From-only (open-
// ended = through now) and reload. When comparing, RE-DERIVE the baseline from
// the new selected window so it stays adjacent + equal-length -- otherwise a
// preset would silently compare, e.g., a 30-day selected window against a stale
// 90-day baseline with a gap. The "vs previous period" invariant holds for
// presets too, never a hidden unequal/non-adjacent comparison.
function applyPreset(sinceVal) {
  $('since-input').value = sinceVal;
  $('until-input').value = '';
  if ($('compare-toggle').checked) { setPreviousPeriod(); }
  load();
}
$('preset-30').addEventListener('click', function() { applyPreset(daysAgoISO(30)); });
$('preset-90').addEventListener('click', function() { applyPreset(daysAgoISO(90)); });
$('preset-quarter').addEventListener('click', function() { applyPreset(quarterStartISO()); });
// "vs previous period": fill the baseline window (A) from the selected window (B).
$('preset-prev').addEventListener('click', function() { setPreviousPeriod(); load(); });

// Compare toggle: reveal/hide the baseline controls row, seed a sensible baseline
// on first enable (previous adjacent period) so the view is never empty, and
// reload through the dispatcher.
$('compare-toggle').addEventListener('change', function() {
  var on = $('compare-toggle').checked;
  $('compare-controls').style.display = on ? '' : 'none';
  if (on && !$('baseline-since-input').value) { setPreviousPeriod(); }
  load();
});

load();
