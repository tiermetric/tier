const fs = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');

function element(tagName) {
  return {
    tagName, children: [], style: {}, attrs: {},
    set textContent(value) { this.text = String(value); this.children = []; },
    get textContent() { return this.text || ''; },
    set innerHTML(value) { throw new Error('Markup insertion: ' + value); },
    appendChild(child) { this.children.push(child); return child; },
    addEventListener() {},
    setAttribute(key, value) { this.attrs[key] = value; },
    querySelector(selector) {
      return descendants(this).find(node => (node.className || '').split(' ').includes(selector.slice(1)));
    }
  };
}
function descendants(node) { return node.children.flatMap(child => [child, ...descendants(child)]); }
function visibleText(node) { return [node.textContent, ...node.children.map(visibleText)].filter(Boolean).join('\n'); }
const nodes = {};
const ctx = {document: {createElement: element, getElementById(id) { return nodes[id] ||= element(); }}, console};
vm.createContext(ctx);
vm.runInContext(fs.readFileSync(process.argv[2] || 'assets/dashboard.js', 'utf8').split('// --- Wiring')[0], ctx);

const total = {
  outcome_linked_cost_usd: 0.300001, outcome_linked_cost_micro: 300001,
  no_outcome_cost_usd: 0.250002, no_outcome_cost_micro: 250002,
  unattributed_cost_usd: 0.050003, unattributed_cost_micro: 50003,
  window_cost_usd: 0.600006, window_cost_micro: 600006
};
const row = {developer: 'alice', ranked: false, total_cost_usd: 999, sample_n: 0};
const data = {
  work_types: ['bug', 'feature'].map(work_type => ({work_type, developers: [row]})),
  segment_reconciliation: {total, developers: [{developer: 'alice', window_cost_micro: 999000000}]}
};
const caveat = 'Segments use cost associated with accepted outcomes. They omit no-outcome and unattributed cost, and their denominators can overlap. Use the pooled figure beside them; do not add segment costs.';
const golden = [
  'Pooled window cost reconciliation (all work types)',
  'Outcome-linked cost: $0.30 (300,001 micro-USD)',
  'No-outcome cost: $0.25 (250,002 micro-USD)',
  'Unattributed cost: $0.05 (50,003 micro-USD)',
  'Total window cost: $0.60 (600,006 micro-USD)',
  caveat
].join('\n');

// A filtered response keeps the same pooled partition; the renderer must not
// derive costs from the selected segment or the per-developer sidecar.
for (const url of ['/api/v1/scores', '/api/v1/scores?work_type=feature']) {
  const workType = new URL(url, 'https://dashboard.test').searchParams.get('work_type');
  const response = {...data, work_types: data.work_types.filter(seg => !workType || seg.work_type === workType)};
  ctx.renderSegments(response, false, '2026-10-01', '2026-11-01');
  const panels = nodes.segments.children;
  assert.equal(panels.length, workType ? 1 : 2, url);
  panels.forEach((panel, index) => {
    const reconciliation = panel.querySelector('.segment-reconciliation');
    assert.ok(reconciliation, url + ': every panel must reconcile');
    assert.equal(visibleText(reconciliation), golden, url);
    const heading = panel.querySelector('.panel-title');
    assert.equal(heading.tagName, 'h2');
    assert.equal(heading.id, undefined);
    const reconciliationHeading = reconciliation.children[0];
    assert.equal(reconciliationHeading.tagName, 'h3');
    assert.equal(reconciliationHeading.id, 'segment-heading-' + index + '-reconciliation');
    assert.equal(reconciliation.attrs['aria-labelledby'], reconciliationHeading.id);
  });
}

// Deliberately inconsistent wire values detect client-side summing of the parts.
ctx.renderSegments({...data, segment_reconciliation: {total: {...total, window_cost_micro: 700007, window_cost_usd: 0.700007}}}, false);
for (const panel of nodes.segments.children) {
  const text = visibleText(panel.querySelector('.segment-reconciliation'));
  assert.equal(text, golden.replace('Total window cost: $0.60 (600,006 micro-USD)', 'Total window cost: $0.70 (700,007 micro-USD)'));
}

// Zero and absent are different; neither can inherit the previous window's cost.
ctx.renderSegments({...data, segment_reconciliation: {total: Object.fromEntries(Object.keys(total).map(key => [key, 0]))}}, false);
assert.match(visibleText(nodes.segments), /No-outcome cost: \$0\.00 \(0 micro-USD\)/);
assert.doesNotMatch(visibleText(nodes.segments), /250,002/);
for (const segment_reconciliation of [undefined, {}, {total: null}]) {
  ctx.renderSegments({...data, segment_reconciliation}, false);
  for (const panel of nodes.segments.children) {
    const reconciliation = panel.querySelector('.segment-reconciliation');
    assert.ok(reconciliation, 'missing totals must retain the caveat in every panel');
    assert.equal(visibleText(reconciliation), [
      'Pooled window cost reconciliation (all work types)',
      'Reconciliation unavailable.',
      caveat
    ].join('\n'));
    assert.equal(descendants(reconciliation).filter(node => node.tagName === 'li').length, 0);
  }
}
// Captured spend without accepted outcomes still needs the pooled reconciliation.
const noOutcomes = {...data, work_types: [], segment_reconciliation: {total: {
  ...total, outcome_linked_cost_usd: 0, outcome_linked_cost_micro: 0,
  window_cost_usd: 0.300005, window_cost_micro: 300005
}}};
ctx.renderSegments(noOutcomes, false);
assert.equal(nodes.segments.children.length, 1, 'no-outcome spend must reconcile once');
const standalone = nodes.segments.children[0];
assert.equal(standalone.className, 'segment-reconciliation');
assert.equal(descendants(nodes.segments).filter(node => node.className === 'segment-reconciliation').length, 1);
assert.equal(visibleText(standalone), golden
  .replace('Outcome-linked cost: $0.30 (300,001 micro-USD)', 'Outcome-linked cost: $0.00 (0 micro-USD)')
  .replace('Total window cost: $0.60 (600,006 micro-USD)', 'Total window cost: $0.30 (300,005 micro-USD)'));
assert.equal(descendants(standalone).filter(node => node.tagName === 'li').length, 4);
assert.equal(standalone.children[0].tagName, 'h3');
assert.equal(standalone.children[0].id, 'segment-heading-standalone-reconciliation');
assert.equal(standalone.attrs['aria-labelledby'], standalone.children[0].id);
assert.equal(nodes['segments-note'].style.display, 'none');
assert.equal(nodes['segments-retro'].style.display, 'none');

for (const segment_reconciliation of [undefined, {}, {total: null}]) {
  ctx.renderSegments(noOutcomes, false);
  ctx.renderSegments({...noOutcomes, segment_reconciliation}, false);
  assert.equal(nodes.segments.children.length, 0, 'no panels or pooled total means no reconciliation');
  assert.equal(visibleText(nodes.segments), '');
}

// Switching to either anonymous mode clears even a stale developer payload.
for (const mode of ['team', 'division']) {
  for (const payload of [data, noOutcomes]) {
    ctx.renderSegments(payload, false);
    ctx.renderSegments({...payload, mode, teams: []}, true);
    assert.equal(nodes.segments.children.length, 0, mode);
    assert.equal(visibleText(nodes.segments), '', mode);
    assert.equal(nodes['segments-note'].style.display, 'none', mode);
    assert.equal(nodes['segments-retro'].style.display, 'none', mode);
  }
}

// API labels are text, even if a future work type contains markup.
ctx.renderSegments({...data, work_types: [{work_type: '<img src=x onerror=alert(1)>', developers: []}]}, false);
assert.equal(nodes.segments.querySelector('.panel-title').textContent, '<img src=x onerror=alert(1)>');
assert.equal(visibleText(nodes.segments.querySelector('.segment-reconciliation')), golden);
console.log('PASS segment reconciliation golden strings, filtered panels, no-outcome spend, privacy, lifetime, and text safety');
