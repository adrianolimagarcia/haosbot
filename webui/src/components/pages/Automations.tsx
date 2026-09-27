import { useEffect, useState } from 'react';
import { Clock3, Sparkles } from 'lucide-react';
import { request } from '../../api';
import { Panel, Stat } from '../ui';

export function Automations({ report }: { report: (error: string) => void }) {
  const [jobs, setJobs] = useState<any[]>([]); const [status, setStatus] = useState('');
  const [triggers, setTriggers] = useState<any[]>([]); const [tab, setTab] = useState<'jobs' | 'triggers'>('jobs');
  const [name, setName] = useState(''); const [message, setMessage] = useState(''); const [seconds, setSeconds] = useState('3600');
  const [selected, setSelected] = useState<any>(null); const [history, setHistory] = useState<any[]>([]);
  const reload = async () => { const data = await request<{ jobs: any[] }>('/api/webui/automations'); setJobs(data.jobs || []); };
  useEffect(() => { reload().catch(error => { setStatus('Scheduler indisponível'); report(String(error)); }); }, []);
  async function reloadTriggers() { const data = await request<{ triggers: any[] }>('/api/webui/triggers'); setTriggers(data.triggers || []); }
  useEffect(() => { if (tab === 'triggers') reloadTriggers().catch(error => report(String(error))); }, [tab]);
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
  async function createTrigger() {
    const sessionId = sessionStorage.getItem('haosbot_session_id') || crypto.randomUUID();
    try { await request('/api/webui/triggers', { method: 'POST', body: JSON.stringify({ name, session_key: `webui:${sessionId}` }) }); await reloadTriggers(); setName(''); setStatus('Trigger criado'); }
    catch (error) { setStatus(String(error)); }
  }
  async function updateTrigger(trigger: any, action: 'fire' | 'toggle' | 'delete') {
    try {
      if (action === 'fire') { const content = window.prompt('Conteúdo para disparar este trigger:'); if (content == null) return; await request('/api/webui/trigger/fire?id=' + encodeURIComponent(trigger.id), { method: 'POST', body: JSON.stringify({ content }) }); }
      else if (action === 'toggle') await request('/api/webui/trigger?id=' + encodeURIComponent(trigger.id), { method: 'PATCH', body: JSON.stringify({ enabled: !trigger.enabled }) });
      else await request('/api/webui/trigger?id=' + encodeURIComponent(trigger.id), { method: 'DELETE' });
      await reloadTriggers();
    } catch (error) { setStatus(String(error)); }
  }
  return <Panel title="Automações" subtitle="Agende turnos e gerencie triggers locais."><div className="form-actions"><button className={tab === 'jobs' ? 'primary' : ''} onClick={() => setTab('jobs')}>Jobs</button><button className={tab === 'triggers' ? 'primary' : ''} onClick={() => setTab('triggers')}>Triggers</button><span>{status}</span></div>{tab === 'jobs' && <><div className="stats-grid"><Stat title="Total" value={jobs.length}/><Stat title="Ativas" value={jobs.filter(job => job.enabled).length}/><Stat title="Executando" value={jobs.filter(job => job.state?.pending).length}/></div><h2>Tarefas</h2><div className="tile-grid">{jobs.map(job => <button className="tile" key={job.id} onClick={() => void select(job)}><Clock3 size={19}/><strong>{job.name || job.id}</strong><span>{job.schedule?.expr || (job.schedule?.everyMs ? `A cada ${job.schedule.everyMs / 1000} s` : job.schedule?.kind) || 'Agendamento'}</span><small>{job.state?.pending ? 'Executando' : job.enabled ? 'Ativa' : 'Pausada'}</small></button>)}{!jobs.length && <p className="muted">{status || 'Nenhuma automação cadastrada.'}</p>}</div>
    {selected && <div className="settings-card"><h2>{selected.name}</h2><div className="form-actions"><button className="primary" onClick={() => void action(selected, 'run')}>Executar agora</button><button onClick={() => { if (window.confirm('Excluir esta automação?')) void action(selected, 'delete'); }}>Excluir</button><button onClick={() => setSelected(null)}>Fechar</button></div><h2>Histórico</h2>{history.map((run, index) => <p className="muted" key={index}>{run.status} · {run.runId || run.run_id}</p>)}{!history.length && <p className="muted">Nenhuma execução.</p>}</div>}
    <div className="settings-card"><h2>Nova automação</h2><div className="form-grid"><label>Nome<input value={name} onChange={event => setName(event.target.value)}/></label><label>Intervalo (segundos)<input type="number" min="1" value={seconds} onChange={event => setSeconds(event.target.value)}/></label><label className="wide">Instrução<textarea value={message} onChange={event => setMessage(event.target.value)}/></label></div><div className="form-actions"><button className="primary" onClick={() => void create()}>Criar automação</button><span>{status}</span></div></div></>}{tab === 'triggers' && <><div className="tile-grid">{triggers.map(trigger => <article className="tile" key={trigger.id}><Sparkles size={19}/><strong>{trigger.name}</strong><span>{trigger.sessionKey || trigger.session_key}</span><small>{trigger.enabled ? 'Ativo' : 'Desativado'}</small><div className="form-actions"><button onClick={() => void updateTrigger(trigger, 'fire')}>Disparar</button><button onClick={() => void updateTrigger(trigger, 'toggle')}>{trigger.enabled ? 'Pausar' : 'Ativar'}</button><button onClick={() => void updateTrigger(trigger, 'delete')}>Excluir</button></div></article>)}</div><div className="settings-card"><h2>Novo trigger</h2><label>Nome<input value={name} onChange={event => setName(event.target.value)}/></label><div className="form-actions"><button className="primary" onClick={() => void createTrigger()}>Criar trigger</button><span>{status}</span></div></div></>}</Panel>;
}
