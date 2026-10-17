const fs = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');
function element() {
 return {children: [], style: {}, className: '', textContent: '', attrs: {}, classList: {add(){},remove(){}}, appendChild(x){this.children.push(x);return x;}, removeAttribute(k){delete this.attrs[k];},setAttribute(k,v){this.attrs[k]=v;}, addEventListener(){}};
}
const nodes = {};
const ctx = {document: {createElement:element, getElementById(id){return nodes[id] ||= element();}}, console};
vm.createContext(ctx);
vm.runInContext(fs.readFileSync(process.argv[2] || 'assets/dashboard.js','utf8').split('// --- Wiring')[0],ctx);
function all(x) { return [x.textContent,x.title,...Object.values(x.attrs||{}),...(x.children||[]).map(all)].filter(Boolean).join(' '); }
const side = {ranked:true,tier:100,weighted_points:10,total_cost_usd:100,sample_n:5,ci_low:80,ci_high:120,cost_per_point:10,cost_per_point_ci_low:8,cost_per_point_ci_high:12};
let failed = 0;
function check(name, fn) { try { fn(); console.log('PASS '+name); } catch(e) { failed++; console.error('FAIL '+name+': '+e.message); } }
check('aggregate pricing anonymous notice with zero counts',()=>{
 const notice='Conditional anonymous policy notice.';
 ctx.renderAggregatePricing({aggregate_pricing:{cutover:'aggregate-policy',approximation_notice:notice,ceiling_status:'not_shown'}});
 assert.match(nodes['aggregate-pricing'].textContent,/Conditional anonymous policy notice\./);
 assert.equal(nodes['aggregate-pricing'].style.display,'block');
 assert.doesNotMatch(nodes['aggregate-pricing'].textContent,/ceiling|unavailable/i);
});
check('aggregate pricing absent metadata policy fallback',()=>{
 const policy='New provider daily remainders use base-rate approximations and may understate long-context cost, raising TIER. Historical provider aggregates may differ from request-level billing in either direction. Request lengths are unavailable in daily aggregates.';
 for (const dq of [undefined,{}]) {
  ctx.renderAggregatePricing(dq);
  assert.equal(nodes['aggregate-pricing'].textContent,policy);
  assert.equal(nodes['aggregate-pricing'].style.display,'block');
 }
});
for (const sig of [true,false]) {
 check('comparison verdict/color '+sig, () => {
 const row = ctx.buildDevDumbbell({developer:'alice',present_a:true,present_b:true,a:side,b:{...side,tier:200},delta_tier:100,significant:sig},200);
 assert.match(all(row),/inference not established/i);
 assert.doesNotMatch(all(row),/\b(?:not significant|significant|within noise)\b/i);
 function colors(x){assert.doesNotMatch(x.className||'',/\b(?:up|down|insignificant)\b/);(x.children||[]).forEach(colors);}
 colors(row);
 });
}
for (const [name,build] of [
 ['TIER hover',()=>ctx.buildTierReading(side,false,true)],
 ['cost-per-point hover',()=>ctx.buildCostPerPoint(side,false,true)],
 ['whisker and accessible label',()=>ctx.buildYieldBar({...side,developer:'alice'},false,true,200,'','')]
]) check(name,()=>{
 const text = all(build());
 assert.match(text,/unvalidated/i);
 assert.doesNotMatch(text,/95% CI|confidence interval/i);
});
check('KPI bounds',()=>{
 ctx.renderKPIs({total:{...side},data_quality:{}},[side],false);
 assert.match(nodes['kpi-tier-ci'].textContent,/unvalidated/i);
});
check('comparison legend',()=>assert.match(ctx.compareNote(true),/inference not established/i));
for (const status of ['open','closed','not_recorded','withheld']) check('seal disclosure '+status,()=>{
 const note=(status==='open'?"Some outcomes' 60-day revert windows were still open at sealing. ":'')+'The default 14-day grace can leave 60-day revert windows open. Longer grace does not establish delivery completeness.';
 const d={sealed_at:'2026-07-01T12:00:00Z',configured_grace_seconds:1209600,days_after_period_end:30.5,revert_windows_at_seal:status,note};
 const text=ctx.sealDisclosureText(d);
 assert.match(text,/2026-07-01T12:00:00Z/);
 assert.match(text,/14 days/);
 assert.equal(/were still open/.test(text),status==='open');
 assert.ok(text.endsWith(note));
});
check('comparison uses both historical seals even when rows are withheld',()=>{
 const d={sealed_at:'2026-07-01T12:00:00Z',configured_grace_seconds:1209600,days_after_period_end:30.5,revert_windows_at_seal:'closed'};
 ctx.showKAnonWithheld=()=>{};
 ctx.renderCompare({kanon_suppressed:{},window_a:{seal_disclosure:d},window_b:{seal_disclosure:{...d,sealed_at:'2026-08-01T09:00:00Z'}}});
 assert.match(nodes['seal-disclosure'].textContent,/2026-07-01T12:00:00Z/);
 assert.match(nodes['seal-disclosure'].textContent,/2026-08-01T09:00:00Z/);
});
for (const compare of [false,true]) check('stall warning survives '+(compare?'comparison':'scores'),()=>{
 const d={sealed_at:'2026-07-01T12:00:00Z',configured_grace_seconds:1209600,days_after_period_end:30.5,revert_windows_at_seal:'withheld'};
 ctx.enterSealedMode({period:'2026-05',earliest_period:'2026-05',latest_period:'2026-05',sealed_at:d.sealed_at,stalled_period:'2026-06',owed_since:'2026-07-15T00:00:00Z',stall_reason:'source_watermark'});
 const warning=nodes['seal-note'].textContent;
 assert.match(warning,/stalled at 2026-06, owed since 2026-07-15.*source_watermark/);
 if (compare) ctx.renderCompare({kanon_suppressed:{},window_a:{seal_disclosure:d},window_b:{seal_disclosure:d}});
 else ctx.renderScores({_sealDisclosure:d,teams:[],data_quality:{kanon_suppressed:{}}});
 assert.equal(nodes['seal-note'].textContent,warning);
 assert.match(nodes['seal-disclosure'].textContent,/14 days/);
});
check('below-floor and one-sided comparison still withhold correctly',()=>{
 const row=ctx.buildDevDumbbell({developer:'alice',present_a:true,present_b:true,a:side,b:{...side,ranked:false,tier:987654321},delta_tier:987654221,significant:true},200);
 assert.doesNotMatch(all(row),/987654321|987654221|inference not established|significant/);
 assert.match(all(row),/below evidence floor/i);
 const one=ctx.buildDevDumbbell({developer:'alice',present_a:true,present_b:false,a:side,delta_tier:0,significant:true},200);
 assert.match(all(one),/100\.0/);
 assert.match(all(one),/only in baseline period/);
});
check('group coverage disclosure remains constant',()=>{
 ctx.renderAttributionCoverage({attribution_coverage:'not_shown'},true);
 const before=nodes['attr-coverage-sub'].textContent;
 ctx.renderAttributionCoverage({},true);
 assert.equal(nodes['attr-coverage-sub'].textContent,before);
 assert.equal(nodes['attr-coverage'].className,'not-shown');
 assert.doesNotMatch(before,/\d+%/);
});
(async()=>{
 ctx.sessionStorage={getItem(){return null;}};
 for (const [grace,delay] of [['1209600','30.500000'],['not_recorded',null],['invalid','Infinity'],['','']]) {
  ctx.fetch=async()=>({ok:true,headers:{get(name){return {'Tier-Sealed-At':'2026-07-01T12:00:00Z','Tier-Report-Grace-Seconds':grace,'Tier-Sealed-Days-After-End':delay,'Tier-Revert-Windows-At-Seal':'withheld','Tier-Seal-Note':'Server caveat.'}[name]??null;}},json:async()=>({teams:[],data_quality:{kanon_suppressed:{}}})});
  const data=await ctx.fetchJSON('/api/v1/scores');
  check('string disclosure headers '+grace+'/'+delay,()=>{
   const d=data._sealDisclosure;
   if (grace==='1209600') {
    assert.equal(d.configured_grace_seconds,1209600);
    assert.equal(d.days_after_period_end,30.5);
   } else {
    assert.equal(d.configured_grace_seconds,null);
    assert.equal(d.days_after_period_end,null);
   }
   ctx.renderScores(data);
   assert.match(nodes['seal-disclosure'].textContent,grace==='1209600'?/14 days; sealed 30\.50 days/:/grace not recorded; seal delay not recorded/);
   assert.ok(nodes['seal-disclosure'].textContent.endsWith('Server caveat.'));
  });
 }
 if (failed) process.exit(1);
})().catch(e=>{console.error(e);process.exit(1);});
