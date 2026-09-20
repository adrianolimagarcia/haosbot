(() => {
  'use strict';
  const SESSION_KEY = 'haosbot_session_id';
  const maxFiles = 8;
  let pending = [];
  let aborter = null;

  const headers = (extra={}) => typeof window.webuiAuthHeaders === 'function' ? window.webuiAuthHeaders(extra) : extra;
  const sessionID = () => sessionStorage.getItem(SESSION_KEY) || '';
  const escapeText = value => String(value ?? '');

  function composerBox() {
    return document.getElementById('user-input')?.closest('.border.border-gray-200.rounded-xl') || document.getElementById('user-input')?.parentElement;
  }
  function randomTempID() {
    if (crypto?.randomUUID) return 'tmp_' + crypto.randomUUID().replaceAll('-', '');
    const b=new Uint8Array(16);crypto.getRandomValues(b);return 'tmp_'+Array.from(b,x=>x.toString(16).padStart(2,'0')).join('');
  }
  async function discardCurrentTemporary() {
    const id=sessionID(); if(!id.startsWith('tmp_')) return;
    try { await fetch('/api/webui/temporary?session_id='+encodeURIComponent(id),{method:'DELETE',headers:headers()}); } catch (_) {}
  }
  async function newTemporaryChat() {
    await discardCurrentTemporary();
    const res=await fetch('/api/webui/temporary',{method:'POST',headers:headers()});
    if(!res.ok) throw new Error(await res.text());
    const data=await res.json();sessionStorage.setItem(SESSION_KEY,data.session_id);
    pending=[];renderChips();
    document.getElementById('chat-stream')?.replaceChildren();
    const title=document.getElementById('header-chat-title');if(title)title.textContent='Temporary Chat';
    const welcome=document.querySelector('.haosbot-welcome');if(welcome)welcome.classList.remove('hidden');
  }
  function ensureAttachmentUI() {
    const box = composerBox();
    if (!box || document.getElementById('haos-attachment-input')) return;
    const row = document.createElement('div'); row.className='haos-attachment-row';
    const button = document.createElement('button'); button.type='button'; button.id='haos-attachment-button'; button.className='haos-attach-button'; button.textContent='+'; button.title='Anexar arquivos ou imagens';
    const input = document.createElement('input'); input.type='file'; input.multiple=true; input.id='haos-attachment-input'; input.className='hidden'; input.accept='image/*,text/*,.json,.xml,.pdf,.zip';
    const chips = document.createElement('div'); chips.id='haos-attachment-chips'; chips.className='haos-attachment-chips';
    button.addEventListener('click',()=>input.click());
    input.addEventListener('change',()=>uploadFiles([...input.files]));
    const temp=document.createElement('button');temp.type='button';temp.className='haos-temp-button';temp.textContent='Temp';temp.title='Temporary Chat: não persiste sessão nem memória derivada';temp.addEventListener('click',()=>newTemporaryChat().catch(err=>window.alert(err.message)));
    row.append(button,input,chips,temp);
    box.prepend(row);
  }
  function renderChips() {
    prunePending();
    const root=document.getElementById('haos-attachment-chips'); if(!root)return; root.replaceChildren();
    pending.forEach((item,index)=>{
      const chip=document.createElement('span'); chip.className='haos-attachment-chip';
      chip.textContent=item.name || 'arquivo';
      const x=document.createElement('button');x.type='button';x.textContent='×';x.title='Remover';
      x.addEventListener('click',()=>{pending.splice(index,1);renderChips();});
      chip.appendChild(x);root.appendChild(chip);
    });
  }
  // The server stores an upload under the media directory of the session that
  // uploaded it and rejects a turn whose media belongs to another session, so a
  // chip left over from a previous conversation would fail the next turn with a
  // 400 after the composer had already been cleared.
  function prunePending() {
    const id=sessionID();
    pending=pending.filter(item=>item.__session===id);
  }
  async function uploadFiles(files) {
    prunePending();
    for (const file of files.slice(0,Math.max(0,maxFiles-pending.length))) {
      const form=new FormData(); form.append('sessionId',sessionID()); form.append('file',file,file.name);
      const res=await fetch('/api/webui/attachment',{method:'POST',headers:headers(),body:form});
      if(!res.ok){window.alert('Falha ao anexar '+file.name+': '+await res.text());continue;}
      const item=await res.json(); item.__session=sessionID(); pending.push(item); renderChips();
    }
    const input=document.getElementById('haos-attachment-input'); if(input)input.value='';
  }

  function appendUser(stream,text,attachments){
    const wrap=document.createElement('div');wrap.className='flex justify-end';
    const bubble=document.createElement('div');bubble.className='haosbot-user-bubble';
    if(text){const t=document.createElement('div');t.textContent=text;bubble.appendChild(t);}
    if(attachments.length){const row=document.createElement('div');row.className='haos-sent-attachments';for(const a of attachments){const chip=document.createElement('span');chip.textContent=a.name||'arquivo';row.appendChild(chip);}bubble.appendChild(row);}
    wrap.appendChild(bubble);stream.appendChild(wrap);
  }
  function appendAssistant(stream){
    const wrap=document.createElement('div');wrap.className='haosbot-assistant-message';
    const top=document.createElement('div');top.className='haosbot-message-meta';top.textContent='HAOSBOT';
    const body=document.createElement('div');body.className='prose max-w-none text-sm whitespace-pre-wrap';
    const details=document.createElement('details');details.className='haos-activity';const summary=document.createElement('summary');summary.textContent='Atividade';const list=document.createElement('div');list.className='haos-activity-list';details.append(summary,list);
    const footer=document.createElement('div');footer.className='text-gray-400 text-[10px] pt-2';footer.textContent='Gerando...';
    wrap.append(top,body,details,footer);stream.appendChild(wrap);return{body,list,details,footer};
  }
  function activity(list,text,kind='info'){
    const row=document.createElement('div');row.className='haos-activity-item '+kind;row.textContent=text;list.appendChild(row);return row;
  }
  async function renderMarkdown(text,target){
    const res=await fetch('/api/webui/render',{method:'POST',headers:headers({'Content-Type':'application/json'}),body:JSON.stringify({text})});
    if(!res.ok){target.textContent=text;return;}
    const data=await res.json();target.replaceChildren();const tpl=document.createElement('template');tpl.innerHTML=data.html||'';target.append(tpl.content.cloneNode(true));
  }
  async function consume(res,onEvent){
    const reader=res.body.getReader(),decoder=new TextDecoder();let buffer='';
    for(;;){const {value,done}=await reader.read();buffer+=decoder.decode(value||new Uint8Array(),{stream:!done});const lines=buffer.split('\n');buffer=lines.pop()||'';for(const line of lines){if(line.trim()){try{onEvent(JSON.parse(line));}catch(_){}}}if(done)break;}
    if(buffer.trim()){try{onEvent(JSON.parse(buffer));}catch(_){}}
  }

  const originalNewSession=window.newSession;
  window.newSession=async function(){await discardCurrentTemporary();pending=[];renderChips();return originalNewSession?.();};
  window.newTemporaryChat=newTemporaryChat;
  window.stopTurn=()=>{if(aborter)aborter.abort();};
  window.handleSend=async function(){
    if(aborter)return;
    const input=document.getElementById('user-input'); if(!input)return;
    prunePending();
    const text=input.value.trim(); if(!text&&!pending.length)return;
    const attachments=pending.slice(); pending=[];renderChips();input.value='';input.disabled=true;
    const stream=document.getElementById('chat-stream');appendUser(stream,text,attachments);
    const assistant=appendAssistant(stream);let content='',failed='';const active=new Map();
    aborter=new AbortController();
    if(typeof window.setTurnRunning==='function')window.setTurnRunning(true);
    try{
      const res=await fetch('/api/agent/turn/stream',{method:'POST',headers:headers({'Content-Type':'application/json','X-HAOS-Session-ID':sessionID()}),body:JSON.stringify({sessionId:sessionID(),message:text||'Please inspect the attached file(s).',media:attachments.map(a=>a.path)}),signal:aborter.signal});
      if(!res.ok)throw new Error('HTTP '+res.status+': '+await res.text());
      await consume(res,event=>{
        if(event.type==='text_delta'){content+=event.delta||'';assistant.body.textContent=content;}
        else if(event.type==='reasoning_delta'){assistant.footer.textContent='Raciocinando...';}
        else if(event.type==='context_snapshot'){const approx=Math.ceil(Number(event.contextChars||0)/4);const indicator=document.getElementById('token-indicator');if(indicator)indicator.textContent=approx.toLocaleString()+' tokens ~';activity(assistant.list,'Contexto · iteração '+event.iteration+' · '+event.messageCount+' mensagens · ~'+approx.toLocaleString()+' tokens','context');}
        else if(event.type==='tool_start'){const row=activity(assistant.list,'Executando '+(event.toolName||'ferramenta')+'…','running');active.set(event.toolCallId,row);assistant.details.open=true;}
        else if(event.type==='tool_end'){const row=active.get(event.toolCallId)||activity(assistant.list,event.toolName||'ferramenta');row.textContent=(event.toolError?'Falhou ':'Concluído ')+(event.toolName||'ferramenta');row.className='haos-activity-item '+(event.toolError?'error':'ok');if(event.fileDiffs&&Object.keys(event.fileDiffs).length){const pre=document.createElement('pre');pre.className='haos-diff';pre.textContent=JSON.stringify(event.fileDiffs,null,2);assistant.list.appendChild(pre);}}
        else if(event.type==='done'){content=event.content||content||'(sem resposta)';assistant.footer.textContent='Agora';}
        else if(event.type==='error'){failed=event.error||'erro no turno';}
      });
      if(failed)throw new Error(failed);
      await renderMarkdown(content||'(sem resposta)',assistant.body);
    }catch(err){assistant.body.textContent=err?.name==='AbortError'?'Turno interrompido pelo usuário.':'Erro: '+escapeText(err.message);assistant.body.className='text-red-700 text-sm whitespace-pre-wrap';}
    finally{aborter=null;input.disabled=false;input.focus();if(typeof window.setTurnRunning==='function')window.setTurnRunning(false);window.dispatchEvent(new CustomEvent('haosbot:turn-complete'));const c=document.getElementById('messages-container');if(c)c.scrollTop=c.scrollHeight;}
  };

  window.haosbotLoadContext=async function(key){
    const res=await fetch('/api/webui/session/context?key='+encodeURIComponent(key),{headers:headers()});if(!res.ok)throw new Error(await res.text());return res.json();
  };
  ensureAttachmentUI();
  window.addEventListener('DOMContentLoaded',ensureAttachmentUI);
})();
;(() => {
'use strict';
const auth=(extra={})=>typeof window.webuiAuthHeaders==='function'?window.webuiAuthHeaders(extra):extra;
function currentSessionKey(){const id=sessionStorage.getItem('haosbot_session_id')||'';return id?'webui:'+id:''}
function ensureQuickAutomation(){
 if(document.getElementById('quick-automation-button'))return;
 const host=document.querySelector('.thread-header-right')||document.querySelector('.thread-header');if(!host)return;
 const b=document.createElement('button');b.id='quick-automation-button';b.className='thread-icon-button';b.type='button';b.textContent='Auto';b.title='Criar automação desta conversa';b.addEventListener('click',open);host.appendChild(b);
}
function field(label,type='text'){const wrap=document.createElement('label');wrap.className='qa-field';const span=document.createElement('span');span.textContent=label;const input=document.createElement(type==='textarea'?'textarea':'input');if(type!=='textarea')input.type=type;input.className='control-input';wrap.append(span,input);return{wrap,input}}
function open(){
 let modal=document.getElementById('quick-automation-modal');if(modal){modal.classList.remove('hidden');return}
 modal=document.createElement('div');modal.id='quick-automation-modal';modal.className='qa-modal';const card=document.createElement('div');card.className='qa-card';const title=document.createElement('h3');title.textContent='Automatizar esta conversa';
 const n=field('Nome'),m=field('Instrução','textarea'),kind=field('Agenda');const select=document.createElement('select');select.className='control-input';[['every','A cada N minutos'],['cron','Cron'],['at','Data/hora única']].forEach(([v,l])=>{const o=document.createElement('option');o.value=v;o.textContent=l;select.append(o)});kind.wrap.replaceChild(select,kind.input);kind.input=select;
 const value=field('Valor');value.input.placeholder='60';const status=document.createElement('div');status.className='control-muted';const actions=document.createElement('div');actions.className='control-toolbar';const save=document.createElement('button');save.className='control-button primary';save.textContent='Criar';const cancel=document.createElement('button');cancel.className='control-button';cancel.textContent='Cancelar';cancel.onclick=()=>modal.classList.add('hidden');actions.append(save,cancel);
 save.onclick=async()=>{let schedule={kind:kind.input.value};if(kind.input.value==='every'){const mins=Number(value.input.value);if(!Number.isFinite(mins)||mins<=0){status.textContent='Minutos inválidos';return}schedule.everyMs=Math.round(mins*60000)}else if(kind.input.value==='cron'){schedule.expr=value.input.value.trim();schedule.tz=Intl.DateTimeFormat().resolvedOptions().timeZone||'UTC'}else{const ms=Date.parse(value.input.value);if(!Number.isFinite(ms)){status.textContent='Data/hora inválida';return}schedule.atMs=ms}
 const payload={name:n.input.value.trim(),message:m.input.value.trim(),session_key:currentSessionKey(),schedule,delete_after_run:kind.input.value==='at',misfire_policy:'fire_once'};const r=await fetch('/api/webui/automation/from-session',{method:'POST',headers:auth({'Content-Type':'application/json'}),body:JSON.stringify(payload)});if(!r.ok){status.textContent=await r.text();return}status.textContent='Automação criada';setTimeout(()=>modal.classList.add('hidden'),500)};
 card.append(title,n.wrap,m.wrap,kind.wrap,value.wrap,actions,status);modal.append(card);document.body.append(modal);
}
window.addEventListener('DOMContentLoaded',ensureQuickAutomation);ensureQuickAutomation();
})();