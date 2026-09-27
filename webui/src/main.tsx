import React, { useEffect, useRef, useState } from 'react';
import { createRoot } from 'react-dom/client';
import { Activity, Archive, Bot, Cable, ChevronLeft, Clock3, KeyRound, Layers3, MessageSquare, Plus, Search, Settings2, Sparkles, Wrench, type LucideIcon } from 'lucide-react';
import { patchConfig, request, sessionAction, setToken, streamTurn, token, type CatalogChannel, type Session, type State } from './api';
import './style.css';

type View = 'chat' | 'apps' | 'skills' | 'automations' | 'channels' | 'models' | 'memory' | 'files' | 'runtime' | 'settings';
type Message = { role: string; content: string };
const nav: Array<{ view: View; label: string; icon: LucideIcon }> = [
  { view: 'apps', label: 'Apps & MCP', icon: Cable }, { view: 'skills', label: 'Skills', icon: Sparkles },
  { view: 'automations', label: 'Automações', icon: Clock3 }, { view: 'channels', label: 'Canais', icon: MessageSquare },
  { view: 'models', label: 'Models & Providers', icon: Bot }, { view: 'memory', label: 'Memory & GraphRAG', icon: Layers3 },
  { view: 'files', label: 'Arquivos', icon: Wrench },
  { view: 'runtime', label: 'Runtime', icon: Activity }, { view: 'settings', label: 'Configurações', icon: Settings2 },
];
function Panel({ title, subtitle, children }: React.PropsWithChildren<{ title: string; subtitle?: string }>) {
  return <section className="panel"><div className="panel-heading"><p className="eyebrow">HAOSBOT CONTROL PLANE</p><h1>{title}</h1>{subtitle && <p>{subtitle}</p>}</div>{children}</section>;
}
function Stat({ title, value, detail }: { title: string; value: React.ReactNode; detail?: string }) {
  return <div className="stat"><span>{title}</span><strong>{value ?? '—'}</strong>{detail && <small>{detail}</small>}</div>;
}
function App() {
  const [state, setState] = useState<State | null>(null);
  const [view, setView] = useState<View>('chat');
  const [selected, setSelected] = useState<string>('');
  const [messages, setMessages] = useState<Message[]>([]);
  const [draft, setDraft] = useState('');
  const [query, setQuery] = useState('');
  const [activity, setActivity] = useState<string[]>([]);
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
      setView('chat'); setActivity([]); setError('');
    } catch (cause) { setError(String(cause)); }
  }
  function newChat() {
    controller.current?.abort();
    const id = crypto.randomUUID();
    sessionStorage.setItem('haosbot_session_id', id);
    setSelected(''); setMessages([]); setActivity([]); setView('chat'); setError('');
  }
  async function send() {
    const text = draft.trim(); if (!text || working) return;
    setDraft(''); setWorking(true); setError(''); setActivity([]);
    setMessages(previous => [...previous, { role: 'user', content: text }, { role: 'assistant', content: '' }]);
    const abort = new AbortController(); controller.current = abort;
    let partial = '';
    try {
      await streamTurn(sessionId, text, event => {
        if (event.type === 'text_delta') {
          partial += event.delta || '';
          setMessages(previous => previous.map((message, index) => index === previous.length - 1 ? { ...message, content: partial } : message));
        } else if (event.type === 'tool_start') setActivity(previous => [...previous, `Ferramenta: ${event.toolName || 'tool'}`]);
        else if (event.type === 'context_snapshot') setActivity(previous => [...previous, `Contexto: ${event.contextChars || 0} caracteres`]);
        else if (event.type === 'done') {
          setMessages(previous => previous.map((message, index) => index === previous.length - 1 ? { ...message, content: event.content || partial } : message));
        } else if (event.type === 'error') throw new Error(event.error || 'Falha no turno');
      }, abort.signal);
      await refresh();
    } catch (cause) { if (!abort.signal.aborted) setError(String(cause)); }
    finally { setWorking(false); controller.current = null; }
  }
  async function act(session: Session, action: string, value?: unknown) {
    try { await sessionAction(session.key, action, value); if (action === 'delete' && session.key === selected) newChat(); await refresh(); }
    catch (cause) { setError(String(cause)); }
  }
  const currentTitle = state?.sessions?.find(s => s.key === selected)?.title || 'Novo chat';
  const sessions = (state?.sessions || []).filter(s => !s.archived && s.title.toLowerCase().includes(query.toLowerCase()));
  return <div className="shell">
    <aside className="sidebar">
      <div className="brand"><div className="brand-icon"><Bot size={21}/></div><div><strong>HAOSBOT</strong><span>Agent workspace</span></div></div>
      <button className="new-chat" onClick={newChat}><Plus size={17}/> Novo chat</button>
      <nav aria-label="Navegação principal">{nav.slice(0, 4).map(({ view: item, icon: Icon, label }) => <button key={item} className={view === item ? 'nav-link active' : 'nav-link'} onClick={() => setView(item)}><Icon size={17}/>{label}</button>)}</nav>
      <div className="section-caption">CONVERSAS</div>
      <label className="search"><Search size={15}/><input value={query} onChange={event => setQuery(event.target.value)} placeholder="Buscar conversas" aria-label="Buscar conversas"/></label>
      <div className="sessions">{sessions.map(session => <div key={session.key} className={selected === session.key && view === 'chat' ? 'session active' : 'session'}>
        <button title={session.title} onClick={() => void openSession(session)}><MessageSquare size={15}/><span>{session.title}</span></button>
        <button className="session-more" title="Arquivar" aria-label={'Arquivar ' + session.title} onClick={() => void act(session, 'archive', true)}><Archive size={14}/></button>
      </div>)}{!sessions.length && <div className="empty-small">Nenhuma conversa encontrada.</div>}</div>
      <div className="sidebar-bottom">{nav.slice(4).map(({ view: item, icon: Icon, label }) => <button key={item} className={view === item ? 'nav-link active' : 'nav-link'} onClick={() => setView(item)}><Icon size={17}/>{label}</button>)}<a className="old-ui" href="/">WebUI anterior <ChevronLeft size={15}/></a></div>
    </aside>
    <main className="main">
      <header className="topbar"><div className="breadcrumbs">Workspace <span>/</span> <strong>{view === 'chat' ? currentTitle : nav.find(item => item.view === view)?.label}</strong></div>
        <div className="top-actions"><span className="model-tag">{state?.config?.agents?.defaults?.model || 'Modelo não selecionado'}</span><button title="Autenticação" aria-label="Autenticação" onClick={() => setShowAuth(!showAuth)}><KeyRound size={17}/></button><button title="Atualizar" aria-label="Atualizar" onClick={() => void refresh()}><Activity size={17}/></button></div></header>
      {showAuth && <div className="auth-strip"><label>Token de API <input type="password" defaultValue={token()} placeholder="Token para esta sessão" onChange={event => setToken(event.target.value)}/></label><button onClick={() => { setShowAuth(false); void refresh(); }}>Aplicar</button></div>}
      {error && <div role="alert" className="error">{error}<button onClick={() => setError('')}>Fechar</button></div>}
      {view === 'chat' ? <div className="chat-layout"><div className="chat-body"><div className="conversation">{messages.length ? messages.map((message, index) => <div key={index} className={'message ' + message.role}><div className="avatar">{message.role === 'user' ? 'U' : <Bot size={17}/>}</div><div><small>{message.role === 'user' ? 'Você' : 'HAOSBOT'}</small><p>{message.content || (working && index === messages.length - 1 ? 'Pensando…' : '')}</p></div></div>) : <div className="welcome"><div className="hero-icon"><Bot size={32}/></div><h1>Como posso ajudar?</h1><p>Converse com o agente, acompanhe ferramentas e explore o seu workspace.</p><div className="welcome-links"><button onClick={() => setView('skills')}><Sparkles size={17}/> Explorar skills</button><button onClick={() => setView('memory')}><Layers3 size={17}/> Ver memória</button></div></div>}</div></div>
        <div className="composer-wrap"><div className="composer"><textarea value={draft} onChange={event => setDraft(event.target.value)} onKeyDown={event => { if (event.key === 'Enter' && !event.shiftKey) { event.preventDefault(); void send(); } }} placeholder="Envie uma mensagem para o HAOSBOT…" aria-label="Mensagem"/><div className="composer-footer"><span>Enter para enviar · Shift+Enter para nova linha</span>{working ? <button onClick={() => controller.current?.abort()}>Parar</button> : <button className="send" onClick={() => void send()} disabled={!draft.trim()}>Enviar ↑</button>}</div></div></div>
        {activity.length > 0 && <aside className="activity-panel"><h3>Atividade</h3>{activity.map((row, index) => <p key={index}>{row}</p>)}</aside>}
      </div> : <Workspace view={view} state={state} refresh={refresh} report={setError}/>}
    </main>
  </div>;
}

function Workspace({ view, state, refresh, report }: { view: View; state: State | null; refresh: () => Promise<void>; report: (error: string) => void }) {
  if (!state) return <Panel title="Carregando…"/>;
  switch (view) {
    case 'channels': return <Channels state={state} refresh={refresh} report={report}/>;
    case 'apps': return <Apps state={state} refresh={refresh} report={report}/>;
    case 'skills': return <Skills state={state} refresh={refresh} report={report}/>;
    case 'automations': return <Automations report={report}/>;
    case 'models': return <Models state={state} refresh={refresh} report={report}/>;
    case 'memory': return <Memory state={state} report={report}/>;
    case 'files': return <Files/>;
    case 'runtime': return <Runtime state={state}/>;
    default: return <Settings state={state}/>;
  }
}

function Channels({ state, refresh, report }: { state: State; refresh: () => Promise<void>; report: (error: string) => void }) {
  const [catalog, setCatalog] = useState<CatalogChannel[]>([]);
  const [query, setQuery] = useState('');
  const [selected, setSelected] = useState<CatalogChannel | null>(null);
  const [values, setValues] = useState<Record<string, string>>({});
  const [status, setStatus] = useState('');
  useEffect(() => { request<{ channels: CatalogChannel[] }>('/api/webui/channels/catalog').then(data => setCatalog(data.channels)).catch(error => report(String(error))); }, []);
  const configured = state.config.channels || {};
  const open = (channel: CatalogChannel) => {
    if (!channel.available) return;
    const current = configured[channel.id] || {};
    setValues(Object.fromEntries((channel.setup?.fields || []).map(field => [field.field,
      field.kind === 'secret' ? '' : Array.isArray(current[field.field]) ? current[field.field].join(', ') : String(current[field.field] ?? field.default_value ?? '')])));
    setSelected(channel); setStatus('');
  };
  async function save() {
    if (!selected) return;
    const patch: Record<string, unknown> = {};
    for (const field of selected.setup?.fields || []) {
      const value = (values[field.field] || '').trim();
      if (field.kind === 'secret' && !value) continue;
      if ((field.kind === 'int' || field.kind === 'float') && !value) continue;
      patch[field.field] = field.kind === 'list' ? value.split(',').map(x => x.trim()).filter(Boolean) : field.kind === 'bool' ? value === 'true' :
        field.kind === 'int' || field.kind === 'float' ? Number(value) : value;
    }
    try { await patchConfig({ channels: { [selected.id]: patch } }); setStatus('Salvo · reinício necessário'); await refresh(); }
    catch (error) { setStatus(String(error)); }
  }
  async function validate() {
    try { const result = await request<{ status: string; message: string; checks: Array<{ label: string; status: string; message?: string }> }>('/api/webui/channels/telegram/validate');
      setStatus(`${result.status}: ${result.message} ${result.checks.map(check => `${check.label}: ${check.status}${check.message ? ` (${check.message})` : ''}`).join(' · ')}`); }
    catch (error) { setStatus(String(error)); }
  }
  return <Panel title="Canais" subtitle="Configure as integrações disponíveis no runtime e consulte as próximas do catálogo.">
    {selected ? <div className="settings-card"><button className="back" onClick={() => setSelected(null)}>← Voltar ao catálogo</button><h2>{selected.name}</h2><p>{selected.description}</p><div className="form-grid">{selected.setup?.fields.map(field => <label key={field.field}>{field.field}{field.required && ' *'}
      {field.kind === 'enum' || field.kind === 'bool' ? <select value={values[field.field] || ''} onChange={event => setValues({ ...values, [field.field]: event.target.value })}>{(field.kind === 'bool' ? ['true', 'false'] : field.choices).map(choice => <option key={choice}>{choice}</option>)}</select> :
        <input type={field.kind === 'secret' ? 'password' : field.kind === 'int' || field.kind === 'float' ? 'number' : 'text'} step={field.kind === 'float' ? 'any' : undefined} value={values[field.field] || ''} placeholder={field.kind === 'secret' && configured[selected.id]?.[field.field + 'Configured'] ? 'Configurado · deixe vazio para manter' : ''} onChange={event => setValues({ ...values, [field.field]: event.target.value })}/>}</label>)}</div><div className="form-actions"><button className="primary" onClick={() => void save()}>Salvar canal</button><button onClick={() => void validate()}>Testar conexão</button><span>{status}</span></div></div> : <><label className="search big"><Search size={16}/><input placeholder="Buscar integração" value={query} onChange={event => setQuery(event.target.value)}/></label><div className="tile-grid">{catalog.filter(item => item.name.toLowerCase().includes(query.toLowerCase())).map(item => <button className="tile" key={item.id} onClick={() => open(item)} disabled={!item.available}><Cable size={20}/><strong>{item.name}</strong><span>{item.description}</span><small className={item.available ? 'available' : ''}>{item.available ? 'Disponível · configurar' : 'Transporte indisponível'}</small></button>)}</div></>}
  </Panel>;
}

function Apps({ state, refresh, report }: { state: State; refresh: () => Promise<void>; report: (error: string) => void }) {
  const mcp = state.config.tools?.mcpServers || {};
  const [name, setName] = useState(''); const [command, setCommand] = useState(''); const [args, setArgs] = useState(''); const [status, setStatus] = useState('');
  async function save() {
    if (!/^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$/.test(name) || !command.trim()) { setStatus('Informe nome válido e comando.'); return; }
    try { await patchConfig({ tools: { mcpServers: { [name]: { ...mcp[name], type: 'stdio', command: command.trim(), args: args.split('\n').map(x => x.trim()).filter(Boolean) } } } }); setStatus('Salvo · reinício necessário'); await refresh(); }
    catch (error) { report(String(error)); }
  }
  return <Panel title="Apps & MCP" subtitle="Ferramentas de execução e servidores MCP disponíveis para o agente.">
    <div className="stats-grid">{Object.entries(state.capabilities).map(([key, enabled]) => <Stat key={key} title={key.replaceAll('_', ' ')} value={enabled ? 'Ativo' : 'Desativado'}/>)}</div>
    <h2>Servidores MCP</h2><div className="tile-grid">{Object.entries(mcp).map(([key, cfg]) => <button className="tile" key={key} onClick={() => { setName(key); setCommand((cfg as any).command || ''); setArgs(((cfg as any).args || []).join('\n')); }}><Wrench size={19}/><strong>{key}</strong><span>{(cfg as any).command || (cfg as any).url || 'Sem endpoint'}</span><small>{(cfg as any).type || 'stdio'}</small></button>)}{!Object.keys(mcp).length && <p className="muted">Nenhum servidor configurado.</p>}</div>
    <div className="settings-card"><h2>Configurar servidor stdio</h2><p>O runtime registra ferramentas MCP via stdio. Outros transportes exigem suporte no backend.</p><div className="form-grid"><label>Nome<input value={name} onChange={event => setName(event.target.value)}/></label><label>Comando<input value={command} onChange={event => setCommand(event.target.value)}/></label><label className="wide">Argumentos, um por linha<textarea value={args} onChange={event => setArgs(event.target.value)}/></label></div><div className="form-actions"><button className="primary" onClick={() => void save()}>Salvar servidor</button><span>{status}</span></div></div>
    <p className="muted">CLI Apps: {state.config.tools?.cliApps?.enable === false ? 'desativado' : 'habilitado'} · Marketplace de apps ainda sem API de gerenciamento.</p>
  </Panel>;
}

function Skills({ state, refresh, report }: { state: State; refresh: () => Promise<void>; report: (error: string) => void }) {
  const [query, setQuery] = useState(''); const [selected, setSelected] = useState(''); const [content, setContent] = useState('');
  const [market, setMarket] = useState<Array<{ skill_id: string; name: string; description: string; provider: string; installed: boolean; install_supported: boolean }>>([]);
  const [tab, setTab] = useState<'installed' | 'market'>('installed'); const [status, setStatus] = useState('');
  async function openSkill(name: string) {
    try { const data = await request<{ content: string }>('/api/webui/skill?name=' + encodeURIComponent(name)); setContent(data.content || ''); setSelected(name); }
    catch (error) { report(String(error)); }
  }
  async function loadMarket(term: string) {
    try { const url = term.trim().length >= 2 ? '/api/webui/skills/marketplace/search?q=' + encodeURIComponent(term.trim()) : '/api/webui/skills/marketplace/trending';
      const data = await request<{ skills: typeof market }>(url); setMarket(data.skills || []); setStatus(''); }
    catch (error) { setStatus(String(error)); }
  }
  useEffect(() => { if (tab === 'market') void loadMarket(query); }, [tab]);
  async function saveSkill() {
    if (!/^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$/.test(selected)) { setStatus('Nome inválido.'); return; }
    try { await request('/api/webui/skill?name=' + encodeURIComponent(selected), { method: 'PUT', body: JSON.stringify({ content }) }); await refresh(); setStatus('Skill salva'); }
    catch (error) { setStatus(String(error)); }
  }
  async function install(skill: typeof market[number]) {
    try { await request('/api/webui/skills/marketplace/install', { method: 'POST', body: JSON.stringify({ provider: skill.provider, skill_id: skill.skill_id }) }); await refresh(); await loadMarket(query); setStatus('Skill instalada'); }
    catch (error) { setStatus(String(error)); }
  }
  return <Panel title="Skills" subtitle="Edite skills locais ou explore o catálogo remoto."><div className="form-actions"><button className={tab === 'installed' ? 'primary' : ''} onClick={() => setTab('installed')}>Instaladas</button><button className={tab === 'market' ? 'primary' : ''} onClick={() => setTab('market')}>Marketplace</button><span>{status}</span></div><label className="search big"><Search size={16}/><input value={query} onChange={event => setQuery(event.target.value)} placeholder="Buscar skill" onKeyDown={event => { if (event.key === 'Enter' && tab === 'market') void loadMarket(query); }}/></label>
    {tab === 'installed' ? <><div className="form-actions"><button onClick={() => { setSelected('nova-skill'); setContent('---\nname: nova-skill\ndescription: Descreva a skill.\n---\n\n# Nova skill\n'); }}>Nova skill local</button></div><div className="tile-grid">{state.skills.filter(skill => skill.name.toLowerCase().includes(query.toLowerCase())).map(skill => <button className="tile" key={skill.name} onClick={() => void openSkill(skill.name)}><Sparkles size={19}/><strong>{skill.name}</strong><span>{skill.description || 'Sem descrição'}</span><small className={skill.available ? 'available' : ''}>{skill.available ? 'Disponível' : skill.unavailable_reason || 'Indisponível'}</small></button>)}</div></> : <div className="tile-grid">{market.map(skill => <article className="tile" key={skill.provider + skill.skill_id}><Sparkles size={19}/><strong>{skill.name}</strong><span>{skill.description}</span><small>{skill.provider} · {skill.installed ? 'Instalada' : 'Disponível'}</small>{!skill.installed && skill.install_supported && <button onClick={() => void install(skill)}>Instalar</button>}</article>)}</div>}
    {selected && tab === 'installed' && <div className="settings-card"><h2>Editar {selected}</h2><label>Nome da skill<input disabled={state.skills.some(skill => skill.name === selected)} value={selected} onChange={event => setSelected(event.target.value)}/></label><textarea className="memory-text" value={content} onChange={event => setContent(event.target.value)}/><div className="form-actions"><button className="primary" onClick={() => void saveSkill()}>Salvar skill</button><button onClick={() => setSelected('')}>Fechar</button></div></div>}</Panel>;
}

function Automations({ report }: { report: (error: string) => void }) {
  const [jobs, setJobs] = useState<any[]>([]); const [status, setStatus] = useState('');
  const [name, setName] = useState(''); const [message, setMessage] = useState(''); const [seconds, setSeconds] = useState('3600');
  const [selected, setSelected] = useState<any>(null); const [history, setHistory] = useState<any[]>([]);
  const reload = async () => { const data = await request<{ jobs: any[] }>('/api/webui/automations'); setJobs(data.jobs || []); };
  useEffect(() => { reload().catch(error => { setStatus('Scheduler indisponível'); report(String(error)); }); }, []);
  async function create() {
    const interval = Number(seconds);
    if (!name.trim() || !message.trim() || !Number.isInteger(interval) || interval < 1) { setStatus('Informe nome, instrução e intervalo válido.'); return; }
    const sessionId = sessionStorage.getItem('haosbot_session_id') || crypto.randomUUID();
    try { await request('/api/webui/automations', { method: 'POST', body: JSON.stringify({ name, message, session_key: `webui:${sessionId}`, schedule: { kind: 'every', everyMs: interval * 1000 } }) });
      await reload(); setName(''); setMessage(''); setStatus('Automação criada'); }
    catch (error) { setStatus(String(error)); }
  }
  async function select(job: any) {
    setSelected(job);
    try { const data = await request<{ history: any[] }>('/api/webui/automation/history?id=' + encodeURIComponent(job.id)); setHistory(data.history || []); }
    catch (error) { report(String(error)); }
  }
  async function action(job: any, verb: 'run' | 'delete') {
    try { if (verb === 'run') await request('/api/webui/automation/run?id=' + encodeURIComponent(job.id), { method: 'POST' });
      else await request('/api/webui/automation?id=' + encodeURIComponent(job.id), { method: 'DELETE' });
      await reload(); setSelected(null); setStatus(verb === 'run' ? 'Execução solicitada' : 'Automação removida'); }
    catch (error) { setStatus(String(error)); }
  }
  return <Panel title="Automações" subtitle="Agende turnos e acompanhe execuções persistidas."><div className="stats-grid"><Stat title="Total" value={jobs.length}/><Stat title="Ativas" value={jobs.filter(job => job.enabled).length}/><Stat title="Executando" value={jobs.filter(job => job.state?.pending).length}/></div><h2>Tarefas</h2><div className="tile-grid">{jobs.map(job => <button className="tile" key={job.id} onClick={() => void select(job)}><Clock3 size={19}/><strong>{job.name || job.id}</strong><span>{job.schedule?.expr || (job.schedule?.everyMs ? `A cada ${job.schedule.everyMs / 1000} s` : job.schedule?.kind) || 'Agendamento'}</span><small>{job.state?.pending ? 'Executando' : job.enabled ? 'Ativa' : 'Pausada'}</small></button>)}{!jobs.length && <p className="muted">{status || 'Nenhuma automação cadastrada.'}</p>}</div>
    {selected && <div className="settings-card"><h2>{selected.name}</h2><div className="form-actions"><button className="primary" onClick={() => void action(selected, 'run')}>Executar agora</button><button onClick={() => { if (window.confirm('Excluir esta automação?')) void action(selected, 'delete'); }}>Excluir</button><button onClick={() => setSelected(null)}>Fechar</button></div><h2>Histórico</h2>{history.map((run, index) => <p className="muted" key={index}>{run.status} · {run.runId || run.run_id}</p>)}{!history.length && <p className="muted">Nenhuma execução.</p>}</div>}
    <div className="settings-card"><h2>Nova automação</h2><div className="form-grid"><label>Nome<input value={name} onChange={event => setName(event.target.value)}/></label><label>Intervalo (segundos)<input type="number" min="1" value={seconds} onChange={event => setSeconds(event.target.value)}/></label><label className="wide">Instrução<textarea value={message} onChange={event => setMessage(event.target.value)}/></label></div><div className="form-actions"><button className="primary" onClick={() => void create()}>Criar automação</button><span>{status}</span></div></div></Panel>;
}

function Models({ state, refresh, report }: { state: State; refresh: () => Promise<void>; report: (error: string) => void }) {
  const providers = state.config.providers || {};
  const [query, setQuery] = useState(''); const [selected, setSelected] = useState(''); const [key, setKey] = useState(''); const [base, setBase] = useState(''); const [model, setModel] = useState(state.config.agents?.defaults?.model || ''); const [status, setStatus] = useState('');
  const names = ['openai', 'anthropic', 'gemini', 'deepseek', 'openrouter', 'xiaomiMimo', 'ollama', 'lmStudio', 'custom', ...Object.keys(providers).filter(name => !['openai', 'anthropic', 'gemini', 'deepseek', 'openrouter', 'xiaomiMimo', 'ollama', 'lmStudio', 'custom'].includes(name))];
  async function save() {
    try { await patchConfig({ providers: { [selected]: { apiBase: base || null, ...(key ? { apiKey: key } : {}) } } }); setStatus('Salvo · reinício necessário'); setKey(''); await refresh(); }
    catch (error) { report(String(error)); }
  }
  async function saveModel() {
    try { await patchConfig({ agents: { defaults: { model: model.trim() } } }); setStatus('Modelo salvo · reinício necessário'); await refresh(); }
    catch (error) { report(String(error)); }
  }
  return <Panel title="Models & Providers" subtitle="Selecione o modelo principal e configure as conexões do runtime."><div className="settings-card"><h2>Modelo principal</h2><div className="form-actions"><input value={model} onChange={event => setModel(event.target.value)} aria-label="Modelo principal"/><button className="primary" onClick={() => void saveModel()}>Salvar modelo</button></div></div><label className="search big"><Search size={16}/><input value={query} onChange={event => setQuery(event.target.value)} placeholder="Buscar provider"/></label><div className="tile-grid">{names.filter(name => name.toLowerCase().includes(query.toLowerCase())).map(name => <button key={name} className="tile" onClick={() => { setSelected(name); setBase(providers[name]?.apiBase || ''); setKey(''); setStatus(''); }}><Bot size={19}/><strong>{name}</strong><span>{providers[name]?.apiBase || 'API ou endpoint local'}</span><small className={providers[name]?.apiKeyConfigured ? 'available' : ''}>{providers[name]?.apiKeyConfigured ? 'Chave configurada' : 'Configurar'}</small></button>)}</div>{selected && <div className="settings-card"><h2>{selected}</h2><div className="form-grid"><label>Chave de API<input type="password" value={key} placeholder={providers[selected]?.apiKeyConfigured ? 'Configurada · deixe vazio para manter' : ''} onChange={event => setKey(event.target.value)}/></label><label>Endpoint<input value={base} onChange={event => setBase(event.target.value)}/></label></div><div className="form-actions"><button className="primary" onClick={() => void save()}>Salvar provider</button><span>{status}</span></div></div>}</Panel>;
}

function Memory({ state, report }: { state: State; report: (error: string) => void }) {
  const [value, setValue] = useState(''); const [status, setStatus] = useState('');
  useEffect(() => { request<{ content: string }>('/api/webui/memory').then(data => setValue(data.content || '')).catch(error => report(String(error))); }, []);
  return <Panel title="Memory & GraphRAG" subtitle="Fonte canônica e projeção de busca do agente."><div className="stats-grid"><Stat title="MEMORY.md" value={`${(state.memory.bytes / 1024).toFixed(1)} KB`} detail={state.memory.path}/><Stat title="Fila de projeção" value={state.metrics?.memory_pending ?? '—'}/><Stat title="Graph search" value={`${state.metrics?.graph_search_avg_ms ?? '—'} ms`}/></div><div className="settings-card"><h2>Memória de longo prazo</h2><textarea className="memory-text" value={value} onChange={event => setValue(event.target.value)}/><div className="form-actions"><button className="primary" onClick={async () => { try { await request('/api/webui/memory', { method: 'PUT', body: JSON.stringify({ content: value }) }); setStatus('Salvo'); } catch (error) { setStatus(String(error)); } }}>Salvar memória</button><span>{status}</span></div></div></Panel>;
}

function Files() {
  const [path, setPath] = useState(''); const [content, setContent] = useState(''); const [status, setStatus] = useState('');
  async function open() {
    try { const data = await request<{ directory?: boolean; entries?: Array<{ name: string; dir: boolean }>; binary?: boolean; content?: string; truncated?: boolean }>('/api/webui/file-preview?path=' + encodeURIComponent(path.trim()));
      setContent(data.directory ? (data.entries || []).map(entry => `${entry.dir ? '[dir]' : '     '} ${entry.name}`).join('\n') : data.binary ? '[Arquivo binário]' : data.content || '');
      setStatus(data.truncated ? 'Preview truncado' : 'Preview carregado'); }
    catch (error) { setStatus(String(error)); }
  }
  return <Panel title="Arquivos" subtitle="Prévia de arquivos e pastas do workspace."><div className="settings-card"><div className="form-actions"><input value={path} onChange={event => setPath(event.target.value)} onKeyDown={event => { if (event.key === 'Enter') void open(); }} placeholder="caminho/relativo/arquivo.md" aria-label="Caminho relativo"/><button className="primary" onClick={() => void open()}>Abrir</button><span>{status}</span></div><pre className="file-preview">{content}</pre></div></Panel>;
}

function Runtime({ state }: { state: State }) {
  const metrics = state.metrics || {};
  const chosen = ['pre_provider_avg_ms', 'provider_ttft_avg_ms', 'turn_total_avg_ms', 'persistence_avg_ms', 'graph_search_avg_ms', 'memory_pending', 'heap_alloc_bytes', 'total_tokens', 'prompt_tokens', 'completion_tokens'];
  return <Panel title="Runtime" subtitle="Latência, memória e atividade observadas no gateway."><div className="stats-grid">{chosen.map(key => <Stat key={key} title={key.replaceAll('_', ' ')} value={metrics[key] == null ? '—' : Number(metrics[key]).toLocaleString('pt-BR')} detail={key.endsWith('_ms') ? 'milissegundos' : undefined}/>)}</div></Panel>;
}

function Settings({ state }: { state: State }) {
  return <Panel title="Configurações" subtitle="Informações do ambiente e aparência do control plane."><div className="stats-grid"><Stat title="Workspace" value={state.workspace}/><Stat title="Sessões" value={state.sessions?.length || 0}/><Stat title="Skills" value={state.skills?.length || 0}/></div><div className="settings-card"><h2>Sobre</h2><p>Esta versão usa React e TypeScript compilados no binário Go. A WebUI anterior permanece disponível durante a migração das operações avançadas.</p><a href="/">Abrir WebUI anterior →</a></div></Panel>;
}

createRoot(document.getElementById('root')!).render(<App/>);
