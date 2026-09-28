import { useEffect, useState } from 'react';
import { patchConfig, request, type State } from '../../api';
import { Panel, Stat } from '../ui';

export function Settings({ state }: { state: State }) {
  const [tab, setTab] = useState('Overview');
  const [theme, setTheme] = useState(localStorage.getItem('haosbot-next-theme') || 'light');
  const [advanced, setAdvanced] = useState(JSON.stringify(state.config, null, 2));
  const [status, setStatus] = useState('');
  useEffect(() => { document.documentElement.dataset.theme = theme; localStorage.setItem('haosbot-next-theme', theme); }, [theme]);
  return <Panel title="Configurações" subtitle="Aparência, capacidades e ajustes avançados do control plane.">
    <div className="settings-tabs">{['Overview', 'Appearance', 'Models', 'Capabilities', 'System', 'Advanced', 'About'].map(item => <button key={item} className={tab === item ? 'active' : ''} onClick={() => setTab(item)}>{item}</button>)}</div>
    {tab === 'Overview' && <div className="stats-grid"><Stat title="Workspace" value={state.workspace}/><Stat title="Sessões" value={state.sessions?.length || 0}/><Stat title="Skills" value={state.skills?.length || 0}/></div>}
    {tab === 'Appearance' && <div className="settings-card"><h2>Tema</h2><div className="form-actions"><button className={theme === 'light' ? 'primary' : ''} onClick={() => setTheme('light')}>Claro</button><button className={theme === 'dark' ? 'primary' : ''} onClick={() => setTheme('dark')}>Escuro</button></div></div>}
    {tab === 'Models' && <div className="settings-card"><h2>Modelo atual</h2><p>{state.config.agents?.defaults?.model || 'Não configurado'} · {state.config.agents?.defaults?.provider || 'Provider automático'}</p><p>Os parâmetros e fallback estão em Models & Providers na navegação.</p></div>}
    {tab === 'Capabilities' && <div className="stats-grid">{Object.entries(state.capabilities).map(([key, value]) => <Stat key={key} title={key.replaceAll('_', ' ')} value={value ? 'Ativo' : 'Desativado'}/>)}</div>}
    {tab === 'System' && <div className="settings-card"><h2>Gateway</h2><p>Workspace: {state.workspace}</p><div className="form-actions"><button onClick={async () => { if (!window.confirm('Reiniciar o gateway agora?')) return; try { await request('/api/restart', { method: 'POST' }); setStatus('Reiniciando…'); } catch (error) { setStatus(String(error)); } }}>Reiniciar gateway</button><span>{status}</span></div></div>}
    {tab === 'Advanced' && <div className="settings-card"><h2>Configuração avançada</h2><p>Campos secretos redigidos são preservados ao salvar.</p><textarea className="memory-text" value={advanced} onChange={event => setAdvanced(event.target.value)}/><div className="form-actions"><button className="primary" onClick={async () => { try { await patchConfig(JSON.parse(advanced)); setStatus('Salvo · reinício necessário'); } catch (error) { setStatus(String(error)); } }}>Salvar JSON</button><span>{status}</span></div></div>}
    {tab === 'About' && <div className="settings-card"><h2>HAOSBOT</h2><p>Control Plane compilado em React/TypeScript e servido pelo binário Go. A interface anterior permanece disponível para operações que ainda estão sendo migradas.</p><a href="/classic/">Abrir WebUI anterior →</a></div>}
  </Panel>;
}
