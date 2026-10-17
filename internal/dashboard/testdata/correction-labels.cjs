const fs=require('node:fs'), vm=require('node:vm'), assert=require('node:assert/strict');
function element(tagName){return {tagName,children:[],style:{},attrs:{},classList:{add(){},remove(){}},set textContent(v){this.text=v;this.children=[];},get textContent(){return this.text||'';},appendChild(x){this.children.push(x);return x;},removeAttribute(k){delete this.attrs[k];},setAttribute(k,v){this.attrs[k]=v;},addEventListener(){}};}
const nodes={},ctx={document:{createElement:element,getElementById(id){return nodes[id] ||= element();}},console};
vm.createContext(ctx);vm.runInContext(fs.readFileSync('assets/dashboard.js','utf8').split('// --- Wiring')[0],ctx);
function text(x){return [x.textContent,...x.children.map(text)].join(' ');}
let failures=0;function check(name,fn){try{fn();console.log('PASS '+name);}catch(e){failures++;console.error('FAIL '+name+': '+e.message);}}
const diagnostics={status:'shown',declaration_coverage:'incomplete',history_coverage:'not_established',captured_spend_fidelity:'fixture fidelity',ci_or_revert_evidence:true,claim_limit:'Do not sum relation rows.',relations:[{child_repo:'acme/app',child_pr:2,parent_repo:'acme/app',parent_pr:1,status:'pending',pending_reason:'frozen-chain',disputed:true,escalation:'unadjudicated_30_days',raw_weight:'',credited_weight:null}]};
const spend={cost_micro:1230000,supported_pr_cost_micro:1000000,pooled_issue_cost_micro:230000,attribution:'partial',claim_limit:'Supporting spend only.'};
check('human dispute escalation and pending labels',()=>{
 const host=element();ctx.renderCorrectionContext(host,diagnostics,spend);const all=text(host);
 for(const label of [/disputed/i,/unadjudicated for 30 days/i,/pending/i,/frozen chain/i,/raw weight unknown/i,/credited weight unknown/i,/fixture fidelity/,/Do not sum relation rows\./,/CI or revert evidence: yes/,/\$1\.23 USD/])assert.match(all,label);
 assert.doesNotMatch(all,/unadjudicated_30_days|not_established|undefined|null/);
 assert.equal(host.children[0].tagName,'h2');
});
check('not shown has no heading or list',()=>{
 const host=element();ctx.renderCorrectionContext(host,{status:'not_shown'},{});
 assert.equal(host.children.length,0);assert.doesNotMatch(text(host),/not_shown/);
});
check('panel hidden while loading and anonymous load skips evidence',()=>{
 const host=ctx.document.getElementById('correction-context');host.style.display='block';host.textContent='old evidence';
 let fetched=0;ctx.fetchJSON=()=>{fetched++;return new Promise(()=>{});};
 ctx.loadCorrectionContext('2026-10-01','',0,false);
 assert.equal(host.style.display,'none');assert.equal(host.textContent,'');assert.equal(fetched,2);
 ctx.loadCorrectionContext('2026-10-01','',0,true);
 assert.equal(fetched,2);assert.equal(host.children.length,0);assert.match(text(host),/not shown/i);
});
check('renderScores forwards teamMode',()=>{
 const calls=[];ctx.loadCorrectionContext=(...args)=>calls.push(args);
 ctx.renderScores({developers:[],data_quality:{}},'2026-10-01','');
 ctx.renderScores({teams:[],data_quality:{}},'2026-10-01','');
 assert.equal(calls[0][3],false);assert.equal(calls[1][3],true);
});
if(failures)process.exit(1);
