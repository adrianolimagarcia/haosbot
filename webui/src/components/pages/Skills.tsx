import { useEffect, useState } from 'react';
import { Search, Sparkles } from 'lucide-react';
import { request, type State } from '../../api';
import { Panel } from '../ui';

export function Skills({ state, refresh, report }: { state: State; refresh: () => Promise<void>; report: (error: string) => void }) {
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
