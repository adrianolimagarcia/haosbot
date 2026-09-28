import type { State } from '../../api';
import { Panel, Stat } from '../ui';

export function Runtime({ state }: { state: State }) {
  const metrics = state.metrics || {};
  const chosen = ['pre_provider_avg_ms', 'provider_ttft_avg_ms', 'turn_total_avg_ms', 'persistence_avg_ms', 'graph_search_avg_ms', 'memory_pending', 'heap_alloc_bytes', 'total_tokens', 'prompt_tokens', 'completion_tokens'];
  return <Panel title="Runtime" subtitle="Latência, memória e atividade observadas no gateway."><div className="stats-grid">{chosen.map(key => <Stat key={key} title={key.replaceAll('_', ' ')} value={metrics[key] == null ? '—' : Number(metrics[key]).toLocaleString('pt-BR')} detail={key.endsWith('_ms') ? 'milissegundos' : undefined}/>)}</div></Panel>;
}
