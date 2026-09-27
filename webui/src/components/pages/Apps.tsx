import { useEffect, useState } from 'react';
import { Search, Wrench } from 'lucide-react';
import { patchConfig, request, type State } from '../../api';
import { Panel, Stat } from '../ui';

export function Apps({ state, refresh, report }: { state: State; refresh: () => Promise<void>; report: (error: string) => void }) {
  const mcp = state.config.tools?.mcpServers || {};
  const [name, setName] = useState(''); const [command, setCommand] = useState(''); const [args, setArgs] = useState(''); const [status, setStatus] = useState('');
  const [type, setType] = useState('stdio'); const [url, setURL] = useState('');
  const [headerName, setHeaderName] = useState('Authorization'); const [headerValue, setHeaderValue] = useState('');
  const [query, setQuery] = useState(''); const [catalog, setCatalog] = useState<Array<{ skill_id: string; name: string; description: string; url?: string; version?: string; install_supported: boolean }>>([]);
  const [catalogStatus, setCatalogStatus] = useState('');
  async function loadCatalog(term = query) {
    try {
      const path = term.trim().length >= 2 ? '/api/webui/skills/marketplace/search?q=' + encodeURIComponent(term.trim()) + '&provider=cliapps' : '/api/webui/skills/marketplace/trending?provider=cliapps';
      const data = await request<{ skills: typeof catalog }>(path); setCatalog(data.skills || []); setCatalogStatus('');
    } catch (error) { setCatalogStatus(String(error)); }
  }
  useEffect(() => { void loadCatalog(''); }, []);
  async function save() {
    if (!/^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$/.test(name) || !(type === 'stdio' ? command.trim() : url.trim())) { setStatus('Informe nome válido e endpoint.'); return; }
    const headers = { ...(mcp[name]?.headers || {}) };
    if (type !== 'stdio' && headerName.trim() && headerValue.trim()) headers[headerName.trim()] = headerValue.trim();
    try { await patchConfig({ tools: { mcpServers: { [name]: { ...mcp[name], type, command: type === 'stdio' ? command.trim() : '', url: type === 'stdio' ? '' : url.trim(), args: type === 'stdio' ? args.split('\n').map(x => x.trim()).filter(Boolean) : [], headers } } } }); setStatus('Salvo · reinício necessário'); setHeaderValue(''); await refresh(); }
    catch (error) { report(String(error)); }
  }
  async function testServer() {
    try { const data = await request<{ tools: string[] }>('/api/webui/mcp/test?name=' + encodeURIComponent(name), { method: 'POST' });
      setStatus(`Conectado · ${(data.tools || []).length} ferramentas: ${(data.tools || []).join(', ')}`); }
    catch (error) { setStatus(String(error)); }
  }
  async function removeServer() {
    if (!mcp[name] || !window.confirm(`Remover o servidor MCP “${name}”?`)) return;
    try { await patchConfig({ tools: { mcpServers: { [name]: null } } }); setStatus('Servidor removido'); setName(''); await refresh(); }
    catch (error) { report(String(error)); }
  }
  return <Panel title="Apps & MCP" subtitle="Ferramentas de execução e servidores MCP disponíveis para o agente.">
    <div className="stats-grid">{Object.entries(state.capabilities).map(([key, enabled]) => <Stat key={key} title={key.replaceAll('_', ' ')} value={enabled ? 'Ativo' : 'Desativado'}/>)}</div>
    <h2>Servidores MCP</h2><div className="tile-grid">{Object.entries(mcp).map(([key, cfg]) => <button className="tile" key={key} onClick={() => { setName(key); setCommand((cfg as any).command || ''); setURL((cfg as any).url || ''); setType((cfg as any).type || ((cfg as any).url ? 'streamableHttp' : 'stdio')); setArgs(((cfg as any).args || []).join('\n')); setHeaderName('Authorization'); setHeaderValue(''); }}><Wrench size={19}/><strong>{key}</strong><span>{(cfg as any).command || (cfg as any).url || 'Sem endpoint'}</span><small>{(cfg as any).type || 'stdio'}</small></button>)}{!Object.keys(mcp).length && <p className="muted">Nenhum servidor configurado.</p>}</div>
    <div className="settings-card"><h2>Configurar servidor MCP</h2><div className="form-grid"><label>Nome<input value={name} onChange={event => setName(event.target.value)}/></label><label>Transporte<select value={type} onChange={event => setType(event.target.value)}><option value="stdio">stdio</option><option value="streamableHttp">Streamable HTTP</option><option value="sse">SSE</option></select></label>{type === 'stdio' ? <><label>Comando<input value={command} onChange={event => setCommand(event.target.value)}/></label><label className="wide">Argumentos, um por linha<textarea value={args} onChange={event => setArgs(event.target.value)}/></label></> : <><label className="wide">URL<input value={url} onChange={event => setURL(event.target.value)}/></label><label>Header de autenticação<input value={headerName} onChange={event => setHeaderName(event.target.value)}/></label><label>Valor secreto<input type="password" value={headerValue} placeholder="Vazio para manter configuração atual" onChange={event => setHeaderValue(event.target.value)}/></label><p className="muted wide">Use headers de autenticação estáticos. OAuth ainda não está disponível neste runtime.</p></>}</div><div className="form-actions"><button className="primary" onClick={() => void save()}>Salvar servidor</button><button disabled={!mcp[name]} onClick={() => void testServer()}>Testar conexão</button><button disabled={!mcp[name]} onClick={() => void removeServer()}>Remover</button><span>{status}</span></div></div>
    <h2>CLI Apps</h2><p className="muted">{state.config.tools?.cliApps?.enable === false ? 'Execução de CLI Apps desativada.' : 'Execução de CLI Apps habilitada. O catálogo abaixo é informativo: a origem não disponibiliza um instalador compatível.'}</p>
    <label className="search big"><Search size={16}/><input value={query} onChange={event => setQuery(event.target.value)} onKeyDown={event => { if (event.key === 'Enter') void loadCatalog(); }} placeholder="Buscar CLI App"/><button onClick={() => void loadCatalog()}>Buscar</button></label>
    <div className="tile-grid">{catalog.map(app => <article className="tile" key={app.skill_id}><Wrench size={19}/><strong>{app.name}</strong><span>{app.description}</span><small>{app.version || 'CLI-Anything'} · instalação indisponível</small>{app.url && <a href={app.url} target="_blank" rel="noreferrer">Projeto</a>}</article>)}{!catalog.length && <p className="muted">{catalogStatus || 'Nenhum app no catálogo.'}</p>}</div>
  </Panel>;
}
