(() => {
'use strict';
const auth=(extra={})=>typeof window.webuiAuthHeaders==='function'?window.webuiAuthHeaders(extra):extra;
const sid=()=>sessionStorage.getItem('haosbot_session_id')||'';
const key=()=> 'webui:'+sid();
function e(tag,cls,text){const n=document.createElement(tag);if(cls)n.className=cls;if(text!==undefined)n.textContent=text;return n}
let pane,body,current='context';
function ensure(){
 if(document.getElementById('haos-workbench-button'))return;
 const host=document.querySelector('.thread-header-right')||document.querySelector('.thread-header');if(!host)return;
 const b=e('button','thread-icon-button','WB');b.id='haos-workbench-button';b.title='Workbench';b.type='button';b.addEventListener('click',toggle);host.appendChild(b);
 pane=e('aside','haos-workbench hidden');pane.id='haos-workbench';const drag=e('div','haos-workbench-drag');
 const head=e('div','haos-workbench-head');head.append(e('strong','','Workbench'));const close=e('button','control-button','×');close.addEventListener('click',toggle);head.append(close);
 const tabs=e('div','haos-workbench-tabs');[['context','Context'],['activity','Activity'],['automations','Automations'],['triggers','Triggers'],['files','Files']].forEach(([id,label])=>{const t=e('button','',label);t.dataset.tab=id;t.addEventListener('click',()=>render(id));tabs.append(t)});
 body=e('div','haos-workbench-body');pane.append(drag,head,tabs,body);document.querySelector('main')?.appendChild(pane);
 let startX,startW;drag.addEventListener('pointerdown',ev=>{startX=ev.clientX;startW=pane.getBoundingClientRect().width;drag.setPointerCapture(ev.pointerId)});
 drag.addEventListener('pointermove',ev=>{if(startX===undefined)return;pane.style.width=Math.max(300,Math.min(720,startW+(startX-ev.clientX)))+'px'});
 drag.addEventListener('pointerup',()=>{startX=undefined});
 drag.addEventListener('pointercancel',()=>{startX=undefined});
 const stream=document.getElementById('chat-stream');if(stream)new MutationObserver(()=>{if(current==='activity'&&!pane.classList.contains('hidden'))renderActivity()}).observe(stream,{subtree:true,childList:true,characterData:true});
}
function toggle(){ensure();if(!pane)return;pane.classList.toggle('hidden');document.documentElement.classList.toggle('workbench-open',!pane.classList.contains('hidden'));if(!pane.classList.contains('hidden'))render(current)}
async function get(path){const r=await fetch(path,{headers:auth()});if(!r.ok)throw new Error((await r.text())||('HTTP '+r.status));const text=await r.text();if(!text)return {};try{return JSON.parse(text)}catch(_){throw new Error('Resposta inválida do servidor')}}
async function render(tab){if(!body)return;current=tab;body.replaceChildren(e('div','session-loading','Carregando…'));pane.querySelectorAll('.haos-workbench-tabs button').forEach(b=>b.classList.toggle('active',b.dataset.tab===tab));try{if(tab==='context')await renderContext();else if(tab==='activity')renderActivity();else if(tab==='automations')await renderJobs();else if(tab==='triggers')await renderTriggers();else renderFiles()}catch(err){body.replaceChildren(e('div','session-loading',err.message))}}
async function renderContext(){const d=await get('/api/webui/session/context?key='+encodeURIComponent(key()));body.replaceChildren();const grid=e('div','wb-grid');[['Mensagens',d.messages],['Tokens ~',Number(d.approx_tokens||0).toLocaleString()],['Janela',Number(d.context_window_tokens||0).toLocaleString()],['Arquivadas',d.last_archived||0]].forEach(([a,b])=>{const c=e('div','wb-card');c.append(e('span','control-muted',a),e('strong','',String(b)));grid.append(c)});body.append(grid);if(d.summary?.text)body.append(e('h4','','Checkpoint'),e('pre','control-pre',d.summary.text))}
function renderActivity(){body.replaceChildren();const nodes=[...document.querySelectorAll('#chat-stream .haos-activity-item,#chat-stream .haos-diff')];if(!nodes.length){body.append(e('div','session-loading','Nenhuma atividade neste turno.'));return}nodes.forEach(n=>body.append(e(n.matches('.haos-diff')?'pre':'div',n.matches('.haos-diff')?'haos-diff':'wb-activity',n.textContent)))}
async function renderJobs(){const d=await get('/api/webui/automations');body.replaceChildren();const rows=(d.jobs||[]).filter(j=>j.payload?.sessionKey===key());rows.forEach(j=>{const r=e('div','wb-row');r.append(e('strong','',j.name||j.id),e('span','control-muted',j.state?.pending?'executando':j.enabled?'ativa':'pausada'),e('small','control-muted','Próxima: '+(j.state?.nextRunAtMs?new Date(j.state.nextRunAtMs).toLocaleString():'—')));body.append(r)});if(!rows.length)body.append(e('div','session-loading','Nenhuma automação vinculada.'))}
async function renderTriggers(){const d=await get('/api/webui/triggers?session_key='+encodeURIComponent(key()));body.replaceChildren();(d.triggers||[]).forEach(t=>{const r=e('div','wb-row');r.append(e('strong','',t.name||t.id),e('span','control-muted',t.enabled?'ativo':'pausado'));body.append(r)});if(!(d.triggers||[]).length)body.append(e('div','session-loading','Nenhum trigger local vinculado.'))}
function renderFiles(){body.replaceChildren();const row=e('div','control-toolbar'),input=e('input','control-input');input.placeholder='Caminho relativo';const open=e('button','control-button primary','Abrir'),pre=e('pre','control-pre','');open.addEventListener('click',async()=>{pre.textContent='Carregando…';try{const r=await fetch('/api/webui/file-preview?path='+encodeURIComponent(input.value.trim()),{headers:auth()});const text=await r.text();if(!r.ok){pre.textContent=text||('HTTP '+r.status);return}let data={};try{data=text?JSON.parse(text):{}}catch(_){pre.textContent=text;return}pre.textContent=typeof data.content==='string'?data.content:(data.content===undefined?'(sem conteúdo)':JSON.stringify(data.content))}catch(err){pre.textContent=err.message}});row.append(input,open);body.append(row,pre)}
document.addEventListener('keydown',ev=>{if((ev.ctrlKey||ev.metaKey)&&ev.shiftKey&&ev.key.toLowerCase()==='b'){ev.preventDefault();toggle()}});
window.addEventListener('haosbot:session-changed',()=>{if(pane&&!pane.classList.contains('hidden'))render(current)});
window.addEventListener('haosbot:turn-complete',()=>{if(pane&&!pane.classList.contains('hidden'))render(current)});
window.addEventListener('DOMContentLoaded',ensure);ensure();
})();