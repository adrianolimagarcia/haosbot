import React, { useEffect, useMemo, useState } from 'react';
import { request } from '../../api';
import { Panel, Stat } from '../ui';

type AgentProfile = {
  id: string; name: string; role: string; instructions: string; model?: string; provider?: string;
  endpoint?: string; tool_allow?: string[]; delegate_to?: string[]; memory_scope: string; max_parallel?: number; enabled: boolean;
};

type AgentTask = {
  id: string; parent_task_id?: string; root_task_id: string; trace_id: string; agent_id: string;
  requested_by: string; depth: number; prompt: string; status: string; result?: string; error?: string;
  created_at: string; started_at?: string; completed_at?: string; deadline?: string;
};

type AgentState = {
  enabled: boolean;
  agents: AgentProfile[];
  limits?: { max_depth: number; max_parallel: number; max_children: number; max_tasks: number; task_timeout_seconds: number; retention_minutes: number };
};

export function Agents({ report }: { report: (error: string) => void }) {
  const [state, setState] = useState<AgentState>({ enabled: false, agents: [] });
  const [tasks, setTasks] = useState<AgentTask[]>([]);
  const [agent, setAgent] = useState('');
  const [prompt, setPrompt] = useState('');
  const [busy, setBusy] = useState(false);

  async function refresh() {
    try {
      const [next, taskData] = await Promise.all([
        request<AgentState>('/api/webui/agents'),
        request<{ tasks: AgentTask[] }>('/api/webui/agent-tasks'),
      ]);
      setState(next);
      setTasks(taskData.tasks || []);
      if (!agent && next.agents?.length) setAgent(next.agents.find(item => item.enabled)?.id || next.agents[0].id);
    } catch (cause) {
      report(String(cause));
    }
  }

  useEffect(() => {
    void refresh();
    const timer = window.setInterval(() => { void refresh(); }, 2500);
    return () => window.clearInterval(timer);
  }, []);

  async function delegate() {
    if (!agent || !prompt.trim() || busy) return;
    setBusy(true);
    try {
      await request('/api/webui/agent-task', {
        method: 'POST',
        body: JSON.stringify({ action: 'delegate', agent, prompt: prompt.trim() }),
      });
      setPrompt('');
      await refresh();
    } catch (cause) {
      report(String(cause));
    } finally {
      setBusy(false);
    }
  }

  async function cancel(taskId: string) {
    try {
      await request('/api/webui/agent-task', {
        method: 'POST',
        body: JSON.stringify({ action: 'cancel', task_id: taskId }),
      });
      await refresh();
    } catch (cause) {
      report(String(cause));
    }
  }

  const active = useMemo(() => tasks.filter(task => task.status === 'submitted' || task.status === 'working').length, [tasks]);
  const completed = useMemo(() => tasks.filter(task => task.status === 'completed').length, [tasks]);

  return <Panel title="Agents" subtitle="Squad nativo do HAOSBOT: workers locais em-processo e peers remotos via A2A 1.0.">
    <div className="stats-grid">
      <Stat title="runtime" value={state.enabled ? 'ativo' : 'desativado'}/>
      <Stat title="agents" value={state.agents.length}/>
      <Stat title="tasks ativas" value={active}/>
      <Stat title="concluídas" value={completed}/>
      <Stat title="paralelismo" value={state.limits?.max_parallel ?? '—'}/>
      <Stat title="profundidade" value={state.limits?.max_depth ?? '—'}/>
    </div>

    <div className="agent-grid">
      {state.agents.map(item => <article className="agent-card" key={item.id}>
        <div className="agent-card-head"><div><strong>{item.name || item.id}</strong><span>{item.role || item.id}</span></div><span className={item.enabled ? 'status-pill good' : 'status-pill'}>{item.enabled ? 'ativo' : 'off'}</span></div>
        <p>{item.instructions}</p>
        <small>{item.endpoint ? 'A2A remoto' : 'Local · in-process'} · memória {item.memory_scope || 'project'} · paralelo {item.max_parallel || state.limits?.max_parallel || 1}{item.delegate_to?.length ? ' · delega → ' + item.delegate_to.join(', ') : ''}{item.model ? ' · ' + item.model : ''}</small>
      </article>)}
    </div>

    <div className="agent-delegate">
      <h2>Delegar tarefa</h2>
      <div className="form-grid">
        <label>Agent<select value={agent} onChange={event => setAgent(event.target.value)}>{state.agents.filter(item => item.enabled).map(item => <option key={item.id} value={item.id}>{item.name || item.id}</option>)}</select></label>
        <label className="span-2">Tarefa<textarea value={prompt} onChange={event => setPrompt(event.target.value)} placeholder="Descreva uma tarefa autocontida para o worker."/></label>
      </div>
      <button className="primary-action" onClick={() => void delegate()} disabled={busy || !agent || !prompt.trim()}>{busy ? 'Delegando…' : 'Delegar em background'}</button>
    </div>

    <div className="agent-task-board">
      <h2>Task board</h2>
      {!tasks.length && <p className="empty-small">Nenhuma task multi-agent registrada.</p>}
      {tasks.map(task => <article className="agent-task" key={task.id}>
        <div className="agent-task-head"><div><strong>{task.agent_id}</strong><span>{task.status}</span></div><code>{task.id}</code></div>
        <p>{task.prompt}</p>
        <small>depth {task.depth} · por {task.requested_by} · trace {task.trace_id}</small>
        {task.result && <pre>{task.result}</pre>}
        {task.error && <div className="task-error">{task.error}</div>}
        {(task.status === 'submitted' || task.status === 'working') && <button onClick={() => void cancel(task.id)}>Cancelar</button>}
      </article>)}
    </div>
  </Panel>;
}
