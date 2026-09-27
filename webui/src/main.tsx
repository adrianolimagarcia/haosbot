import React, { useEffect, useRef, useState } from 'react';
import { createRoot } from 'react-dom/client';
import { Activity, Archive, Bot, Cable, ChevronLeft, Clock3, KeyRound, Layers3, MessageSquare, Plus, Search, Settings2, Sparkles, Wrench, type LucideIcon } from 'lucide-react';
import { request, sessionAction, setToken, streamTurn, token, uploadAttachment, type Session, type State } from './api';
import './style.css';
import type { View } from './types';
import { Workspace } from './components/pages/Workspace';

document.documentElement.dataset.theme = localStorage.getItem('haosbot-next-theme') || 'light';

type Message = { role: string; content: string };
const nav: Array<{ view: View; label: string; icon: LucideIcon }> = [
  { view: 'apps', label: 'Apps & MCP', icon: Cable }, { view: 'skills', label: 'Skills', icon: Sparkles },
  { view: 'automations', label: 'Automações', icon: Clock3 }, { view: 'channels', label: 'Canais', icon: MessageSquare },
  { view: 'models', label: 'Models & Providers', icon: Bot }, { view: 'memory', label: 'Memory & GraphRAG', icon: Layers3 },
  { view: 'files', label: 'Arquivos', icon: Wrench },
  { view: 'runtime', label: 'Runtime', icon: Activity }, { view: 'settings', label: 'Configurações', icon: Settings2 },
];
function App() {
  const [state, setState] = useState<State | null>(null);
  const [view, setView] = useState<View>('chat');
  const [selected, setSelected] = useState<string>('');
  const [messages, setMessages] = useState<Message[]>([]);
  const [draft, setDraft] = useState('');
  const [query, setQuery] = useState('');
  const [showArchived, setShowArchived] = useState(false);
  const [activity, setActivity] = useState<string[]>([]);
  const [attachments, setAttachments] = useState<Array<{ name: string; path: string }>>([]);
  const [working, setWorking] = useState(false);
  const [error, setError] = useState('');
  const [showAuth, setShowAuth] = useState(false);
  const controller = useRef<AbortController | null>(null);
  const sessionId = selected.replace(/^webui:/, '') || sessionStorage.getItem('haosbot_session_id') || (() => {
    const id = crypto.randomUUID(); sessionStorage.setItem('haosbot_session_id', id); return id;
  })();
  const refresh = async () => {
    try { const next = await request<State>('/api/webui/state'); setState(next); setError(''); }
    catch (cause) { setError(String(cause)); }
  };
  useEffect(() => { void refresh(); return () => controller.current?.abort(); }, []);
  async function openSession(session: Session) {
    if (!session.selectable) return;
    try {
      const data = await request<{ messages: Array<{ role: string; content: string | { text?: string } }> }>('/api/webui/session?key=' + encodeURIComponent(session.key));
      setMessages(data.messages.map(m => ({ role: m.role, content: typeof m.content === 'string' ? m.content : m.content?.text || '' })));
      setSelected(session.key); sessionStorage.setItem('haosbot_session_id', session.session_id);
      setView('chat'); setActivity([]); setAttachments([]); setError('');
    } catch (cause) { setError(String(cause)); }
  }
  function newChat() {
    controller.current?.abort();
    const id = crypto.randomUUID();
    sessionStorage.setItem('haosbot_session_id', id);
    setSelected(''); setMessages([]); setActivity([]); setAttachments([]); setView('chat'); setError('');
  }
  async function send() {
    const text = draft.trim() || (attachments.length ? 'Analise os arquivos anexados.' : ''); if (!text || working) return;
    setDraft(''); setWorking(true); setError(''); setActivity([]);
    setMessages(previous => [...previous, { role: 'user', content: text }, { role: 'assistant', content: '' }]);
    const abort = new AbortController(); controller.current = abort;
    let partial = '';
    try {
      await streamTurn(sessionId, text, attachments.map(item => item.path), event => {
        if (event.type === 'text_delta') {
          partial += event.delta || '';
          setMessages(previous => previous.map((message, index) => index === previous.length - 1 ? { ...message, content: partial } : message));
        } else if (event.type === 'tool_start') setActivity(previous => [...previous, `Ferramenta: ${event.toolName || 'tool'}`]);
        else if (event.type === 'context_snapshot') setActivity(previous => [...previous, `Contexto: ${event.contextChars || 0} caracteres`]);
        else if (event.type === 'done') {
          setMessages(previous => previous.map((message, index) => index === previous.length - 1 ? { ...message, content: event.content || partial } : message));
        } else if (event.type === 'error') throw new Error(event.error || 'Falha no turno');
      }, abort.signal);
      setAttachments([]); await refresh();
    } catch (cause) { if (!abort.signal.aborted) setError(String(cause)); }
    finally { setWorking(false); controller.current = null; }
  }
  async function addFiles(files: FileList | null) {
    if (!files?.length) return;
    for (const file of Array.from(files)) {
      try { const item = await uploadAttachment(sessionId, file); setAttachments(previous => [...previous, item]); }
      catch (cause) { setError(String(cause)); }
    }
  }
  async function act(session: Session, action: string, value?: unknown) {
    try { await sessionAction(session.key, action, value); if (action === 'delete' && session.key === selected) newChat(); await refresh(); }
    catch (cause) { setError(String(cause)); }
  }
  const currentTitle = state?.sessions?.find(s => s.key === selected)?.title || 'Novo chat';
  const sessions = (state?.sessions || []).filter(s => (showArchived || !s.archived) && s.title.toLowerCase().includes(query.toLowerCase()));
  return <div className="shell">
    <aside className="sidebar">
      <div className="brand"><div className="brand-icon"><Bot size={21}/></div><div><strong>HAOSBOT</strong><span>Agent workspace</span></div></div>
      <button className="new-chat" onClick={newChat}><Plus size={17}/> Novo chat</button>
      <nav aria-label="Navegação principal">{nav.slice(0, 4).map(({ view: item, icon: Icon, label }) => <button key={item} className={view === item ? 'nav-link active' : 'nav-link'} onClick={() => setView(item)}><Icon size={17}/>{label}</button>)}</nav>
      <div className="section-caption">CONVERSAS</div>
      <label className="search"><Search size={15}/><input value={query} onChange={event => setQuery(event.target.value)} placeholder="Buscar conversas" aria-label="Buscar conversas"/></label>
      <button className="archive-toggle" onClick={() => setShowArchived(!showArchived)}>{showArchived ? 'Ocultar arquivadas' : 'Mostrar arquivadas'}</button>
      <div className="sessions">{sessions.map(session => <div key={session.key} className={selected === session.key && view === 'chat' ? 'session active' : 'session'}>
        <button title={session.title} onClick={() => void openSession(session)}><MessageSquare size={15}/><span>{session.title}</span></button>
        <button className="session-more" title={session.pinned ? 'Desafixar' : 'Fixar'} aria-label={'Fixar ' + session.title} onClick={() => void act(session, 'pin', !session.pinned)}>☆</button>
        <button className="session-more" title={session.archived ? 'Restaurar' : 'Arquivar'} aria-label={'Arquivar ' + session.title} onClick={() => void act(session, 'archive', !session.archived)}><Archive size={14}/></button>
      </div>)}{!sessions.length && <div className="empty-small">Nenhuma conversa encontrada.</div>}</div>
      <div className="sidebar-bottom">{nav.slice(4).map(({ view: item, icon: Icon, label }) => <button key={item} className={view === item ? 'nav-link active' : 'nav-link'} onClick={() => setView(item)}><Icon size={17}/>{label}</button>)}<a className="old-ui" href="/">WebUI anterior <ChevronLeft size={15}/></a></div>
    </aside>
    <main className="main">
      <header className="topbar"><div className="breadcrumbs">Workspace <span>/</span> <strong>{view === 'chat' ? currentTitle : nav.find(item => item.view === view)?.label}</strong></div>
        <div className="top-actions"><span className="model-tag">{state?.config?.agents?.defaults?.model || 'Modelo não selecionado'}</span>{view === 'chat' && selected && <><button title="Renomear conversa" onClick={() => { const value = window.prompt('Novo título', currentTitle); const session = state?.sessions.find(s => s.key === selected); if (value?.trim() && session) void act(session, 'rename', value.trim()); }}>Renomear</button><button title="Excluir conversa" onClick={() => { const session = state?.sessions.find(s => s.key === selected); if (session && window.confirm('Excluir esta conversa?')) void act(session, 'delete'); }}>Excluir</button></>}<button title="Autenticação" aria-label="Autenticação" onClick={() => setShowAuth(!showAuth)}><KeyRound size={17}/></button><button title="Atualizar" aria-label="Atualizar" onClick={() => void refresh()}><Activity size={17}/></button></div></header>
      {showAuth && <div className="auth-strip"><label>Token de API <input type="password" defaultValue={token()} placeholder="Token para esta sessão" onChange={event => setToken(event.target.value)}/></label><button onClick={() => { setShowAuth(false); void refresh(); }}>Aplicar</button></div>}
      {error && <div role="alert" className="error">{error}<button onClick={() => setError('')}>Fechar</button></div>}
      {view === 'chat' ? <div className="chat-layout"><div className="chat-body"><div className="conversation">{messages.length ? messages.map((message, index) => <div key={index} className={'message ' + message.role}><div className="avatar">{message.role === 'user' ? 'U' : <Bot size={17}/>}</div><div><small>{message.role === 'user' ? 'Você' : 'HAOSBOT'}</small><p>{message.content || (working && index === messages.length - 1 ? 'Pensando…' : '')}</p></div></div>) : <div className="welcome"><div className="hero-icon"><Bot size={32}/></div><h1>Como posso ajudar?</h1><p>Converse com o agente, acompanhe ferramentas e explore o seu workspace.</p><div className="welcome-links"><button onClick={() => setView('skills')}><Sparkles size={17}/> Explorar skills</button><button onClick={() => setView('memory')}><Layers3 size={17}/> Ver memória</button></div></div>}</div></div>
        <div className="composer-wrap"><div className="composer"><textarea value={draft} onChange={event => setDraft(event.target.value)} onKeyDown={event => { if (event.key === 'Enter' && !event.shiftKey) { event.preventDefault(); void send(); } }} placeholder="Envie uma mensagem para o HAOSBOT…" aria-label="Mensagem"/>{attachments.length > 0 && <div className="attachment-list">{attachments.map(item => <span key={item.path}>{item.name}<button title={'Remover ' + item.name} onClick={() => setAttachments(previous => previous.filter(other => other.path !== item.path))}>×</button></span>)}</div>}<div className="composer-footer"><label className="attach-button">+ Arquivo<input type="file" multiple hidden onChange={event => { void addFiles(event.target.files); event.target.value = ''; }}/></label><span>Enter para enviar · Shift+Enter para nova linha</span>{working ? <button onClick={() => controller.current?.abort()}>Parar</button> : <button className="send" onClick={() => void send()} disabled={!draft.trim() && !attachments.length}>Enviar ↑</button>}</div></div></div>
        {activity.length > 0 && <aside className="activity-panel"><h3>Atividade</h3>{activity.map((row, index) => <p key={index}>{row}</p>)}</aside>}
      </div> : <Workspace view={view} state={state} refresh={refresh} report={setError}/>}
    </main>
  </div>;
}

createRoot(document.getElementById('root')!).render(<App/>);
