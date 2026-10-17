const fs=require('node:fs'),vm=require('node:vm'),assert=require('node:assert/strict');
function element(tagName){return {tagName,children:[],style:{},attributes:{},setAttribute(k,v){this.attributes[k]=v;},set textContent(v){this.text=v;this.children=[];},get textContent(){return this.text||'';},appendChild(x){this.children.push(x);return x;}};}
const nodes={},ctx={document:{createElement:element,getElementById(id){return nodes[id] ||= element();}},console};
vm.createContext(ctx);vm.runInContext(fs.readFileSync('assets/dashboard.js','utf8').split('// --- Wiring')[0],ctx);
function text(x){return [x.textContent,...x.children.map(text)].join(' ');}
const report={since:'2026-10-01',rubric_version:2,label:'attributed (lower bound)',attributed_cost_usd:5,total_cost_usd:10,unattributed_cost_usd:5,unattributed_share:.5,reconciliation:[
 {repo:'acme/app',issue_id:'issue-1',label:'attributed (lower bound)',cost_usd:5,weighted_points:3,tier:600,score_status:'scored',outcome_n:3,sample_n:3,unattributed_share:.5},
 {repo:'acme/app',issue_id:'issue-2',label:'attributed (lower bound)',cost_usd:0,weighted_points:0,tier:null,score_status:'no score',outcome_n:0,sample_n:0,unattributed_share:.5}]};
(async()=>{
 assert.equal(ctx.issueUSDText(.004), '$0.004000');assert.equal(ctx.issueUSDText(4.999999), '$4.999999');
 const host=ctx.document.getElementById('issue-report');ctx.renderIssueReport(host,report);
 assert.match(text(host),/attributed \(lower bound\)/);assert.match(text(host),/50.00%/);assert.match(text(host),/no score/);assert.match(text(host),/rubric 2/);assert.match(text(host),/600\.0\b/);
 assert.ok(text(host).indexOf('issue-1')<text(host).indexOf('issue-2'));
 const [title,summary,scroll]=host.children;assert.equal(title.id,'issue-report-title');assert.equal(summary.tagName,'p');
 assert.equal(host.children.length,4);const note=host.children[3];assert.equal(note.tagName,'p');assert.equal(note.className,'tr-caption');assert.match(note.textContent,/can be misfiled/);
 assert.equal(scroll.className,'tr-scroll');assert.equal(scroll.tabIndex,0);assert.equal(scroll.attributes['aria-labelledby'],title.id);
 const table=scroll.children[0];assert.equal(table.tagName,'table');assert.equal(scroll.children.length,1);
 const headers=table.children[0].children[0].children;assert.equal(headers.length,7);assert.doesNotMatch(text(table),/Unattributed share/);
 const cells=table.children[1].children[0].children;assert.equal(cells[1].tagName,'th');assert.equal(cells[1].scope,'row');assert.equal(cells[4].textContent,'3.0');assert.equal(cells[6].textContent,'600.0');
 for(let i=0;i<3;i++){assert.equal(headers[i].className,'tr-team');assert.equal(cells[i].className,'tr-team');}
 // Missing numeric fields are handled by num(); an absent TIER keeps its status.
 ctx.renderIssueReport(host,{...report,reconciliation:[{...report.reconciliation[1],weighted_points:undefined,tier:undefined}]});assert.match(text(host),/0\.0/);assert.match(text(host),/no score/);
 ctx.renderIssueReport(host,{...report,reconciliation:[]});assert.match(text(host),/No per-issue reconciliation data/);assert.doesNotMatch(text(host),/misfiled/);assert.ok(!host.children.some(x=>x.tagName==='table'||x.className==='tr-scroll'));
 let calls=[];let resolve;ctx.fetchJSON=q=>{calls.push(q);return new Promise(r=>{resolve=r;});};
 ctx.loadIssueReport('2026-10-01','',0,true);assert.equal(calls.length,0);assert.equal(host.style.display,'none');
 ctx.sealedMode=true;ctx.loadIssueReport('2026-10-01','',0,false);assert.equal(calls.length,0);assert.equal(host.style.display,'none');ctx.sealedMode=false;
 ctx.loadIssueReport('2026-10-01','',0,false);assert.equal(calls.length,1);assert.equal(calls[0],'/api/v1/issues?since=2026-10-01');
 ctx.loadGen=1;resolve(report);await new Promise(setImmediate);assert.equal(host.style.display,'none');
 ctx.loadIssueReport('2026-10-01','2026-11-01',1,false);assert.match(calls[1],/until=2026-11-01/);resolve(report);await new Promise(setImmediate);assert.equal(host.style.display,'block');
 ctx.resetViews();assert.equal(host.style.display,'none');
 let reject;ctx.fetchJSON=()=>new Promise((_,r)=>{reject=r;});
 ctx.loadIssueReport('2026-10-01','',ctx.loadGen,false);reject(new Error('offline'));await new Promise(setImmediate);
 assert.match(text(host),/unavailable/);assert.equal(host.style.display,'block');
 ctx.loadIssueReport('2026-10-01','',ctx.loadGen,false);ctx.loadGen++;reject(new Error('old window'));await new Promise(setImmediate);
 assert.equal(host.style.display,'none');assert.doesNotMatch(text(host),/unavailable/);
 console.log('PASS issue report labels, privacy, window and stale-response guards');
})().catch(e=>{console.error(e);process.exit(1);});
