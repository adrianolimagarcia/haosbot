import { useEffect, useMemo, useState } from 'react';
import { memoryAction, memoryAdmin, request, type MemoryAdminSnapshot, type State } from '../../api';
import { Panel, Stat } from '../ui';

const fmtBytes = (value = 0) => {
  if (value < 1024) return `${value} B`;
  if (value < 1024 * 1024) return `${(value / 1024).toFixed(1)} KB`;
  return `${(value / (1024 * 1024)).toFixed(1)} MB`;
};

export function Memory({ state, report }: { state: State; report: (error: string) => void }) {
  const [value, setValue] = useState('');
  const [status, setStatus] = useState('');
  const [admin, setAdmin] = useState<MemoryAdminSnapshot | null>(null);
  const [busy, setBusy] = useState('');

  const refresh = async () => {
    try { setAdmin(await memoryAdmin()); } catch (error) { report(String(error)); }
  };

  useEffect(() => {
    request<{ content: string }>('/api/webui/memory')
      .then(data => setValue(data.content || ''))
      .catch(error => report(String(error)));
    void refresh();
  }, []);

  const totalRecords = useMemo(() => (admin?.namespaces || []).reduce((sum, item) => sum + item.records, 0), [admin]);

  const run = async (label: string, body: Record<string, unknown>) => {
    setBusy(label); setStatus('');
    try {
      const result = await memoryAction(body);
      if (result.snapshot) setAdmin(result.snapshot);
      else await refresh();
      setStatus(result.deleted !== undefined ? `${label}: ${result.deleted} registros removidos` : `${label}: concluído`);
    } catch (error) {
      setStatus(String(error));
    } finally {
      setBusy('');
    }
  };

  return <Panel title="Memory & GraphRAG" subtitle="Fonte canônica, namespaces físicos, projeções derivadas e manutenção operacional.">
    <div className="stats-grid">
      <Stat title="MEMORY.md" value={`${(state.memory.bytes / 1024).toFixed(1)} KB`} detail={state.memory.path}/>
      <Stat title="Registros canônicos" value={admin ? totalRecords : '—'} detail={admin ? fmtBytes(admin.disk_bytes) + ' em disco' : 'carregando'}/>
      <Stat title="Fila de projeção" value={admin?.stats.pending ?? state.metrics?.memory_pending ?? '—'} detail={admin ? `${admin.stats.running} running · ${admin.stats.dead} dead` : ''}/>
      <Stat title="Graph search" value={`${state.metrics?.graph_search_avg_ms ?? '—'} ms`} detail="retrieval híbrido"/>
    </div>

    <div className="settings-card">
      <h2>Memória de longo prazo</h2>
      <textarea className="memory-text" value={value} onChange={event => setValue(event.target.value)}/>
      <div className="form-actions">
        <button className="primary" onClick={async () => {
          try {
            await request('/api/webui/memory', { method: 'PUT', body: JSON.stringify({ content: value }) });
            setStatus('MEMORY.md salvo');
          } catch (error) { setStatus(String(error)); }
        }}>Salvar MEMORY.md</button>
        <span>{status}</span>
      </div>
    </div>

    <div className="settings-card">
      <h2>Namespaces físicos</h2>
      <p>private, team, project e global vivem em índices derivados separados. Legacy sem proveniência fica em <code>legacy-unassigned</code>.</p>
      <div className="stack-list">
        {(admin?.namespaces || []).map(item =>
          <div className="list-row" key={item.scope + ':' + item.owner}>
            <div><strong>{item.scope}</strong><div className="muted">{item.owner}</div></div>
            <div>{item.records} registros · {fmtBytes(item.bytes)}</div>
          </div>
        )}
        {admin && admin.namespaces.length === 0 && <div className="muted">Nenhum registro canônico ainda.</div>}
      </div>
    </div>

    <div className="settings-card">
      <h2>Projection / GC</h2>
      <p>Rebuild apaga apenas dados derivados e reprojeta a partir do canônico. Prune remove somente registros antigos cujas projeções já terminaram com sucesso; DLQ é preservada.</p>
      <div className="form-actions">
        <button disabled={!!busy} onClick={() => void run('Rebuild GraphRAG', { action: 'rebuild', projection: 'graph' })}>Rebuild GraphRAG</button>
        <button disabled={!!busy} onClick={() => void run('Rebuild completo', { action: 'rebuild', projection: 'all' })}>Rebuild completo</button>
        <button disabled={!!busy} onClick={() => void run('Prune 30d', { action: 'prune', before_days: 30, max_records: 1000 })}>Prune &gt;30 dias</button>
        <button disabled={!!busy} onClick={() => void run('VACUUM', { action: 'vacuum' })}>VACUUM</button>
        <button disabled={!!busy} onClick={() => void refresh()}>Atualizar</button>
      </div>
      {admin && <div className="muted">succeeded {admin.stats.succeeded} · dead {admin.stats.dead} · pending bytes {fmtBytes(admin.stats.pending_bytes)} · oldest {admin.stats.oldest_age_seconds}s</div>}
    </div>

    <div className="settings-card">
      <h2>Dead-letter queue</h2>
      <div className="stack-list">
        {(admin?.dead_jobs || []).map(job =>
          <div className="list-row" key={job.projection + ':' + job.id}>
            <div>
              <strong>{job.projection}</strong> · {job.scope}
              <div className="muted">{job.owner} · attempts {job.attempts}</div>
              <div className="muted">{job.last_error || 'sem erro registrado'}</div>
            </div>
            <button disabled={!!busy} onClick={() => void run('Retry DLQ', { action: 'retry_dead', projection: job.projection, job_id: job.id })}>Retry</button>
          </div>
        )}
        {admin && admin.dead_jobs.length === 0 && <div className="muted">DLQ vazia.</div>}
      </div>
    </div>
  </Panel>;
}
