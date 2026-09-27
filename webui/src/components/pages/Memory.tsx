import { useEffect, useState } from 'react';
import { request, type State } from '../../api';
import { Panel, Stat } from '../ui';

export function Memory({ state, report }: { state: State; report: (error: string) => void }) {
  const [value, setValue] = useState(''); const [status, setStatus] = useState('');
  useEffect(() => { request<{ content: string }>('/api/webui/memory').then(data => setValue(data.content || '')).catch(error => report(String(error))); }, []);
  return <Panel title="Memory & GraphRAG" subtitle="Fonte canônica e projeção de busca do agente."><div className="stats-grid"><Stat title="MEMORY.md" value={`${(state.memory.bytes / 1024).toFixed(1)} KB`} detail={state.memory.path}/><Stat title="Fila de projeção" value={state.metrics?.memory_pending ?? '—'}/><Stat title="Graph search" value={`${state.metrics?.graph_search_avg_ms ?? '—'} ms`}/></div><div className="settings-card"><h2>Memória de longo prazo</h2><textarea className="memory-text" value={value} onChange={event => setValue(event.target.value)}/><div className="form-actions"><button className="primary" onClick={async () => { try { await request('/api/webui/memory', { method: 'PUT', body: JSON.stringify({ content: value }) }); setStatus('Salvo'); } catch (error) { setStatus(String(error)); } }}>Salvar memória</button><span>{status}</span></div></div></Panel>;
}
