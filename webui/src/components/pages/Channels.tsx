import { useEffect, useState } from 'react';
import { Cable, Search } from 'lucide-react';
import { patchConfig, request, type CatalogChannel, type State } from '../../api';
import { Panel } from '../ui';

export function Channels({ state, refresh, report }: { state: State; refresh: () => Promise<void>; report: (error: string) => void }) {
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
    try { const result = await request<{ status: string; message: string; checks: Array<{ label: string; status: string; message?: string }> }>(`/api/webui/channels/${encodeURIComponent(selected?.id || '')}/validate`);
      setStatus(`${result.status}: ${result.message} ${result.checks.map(check => `${check.label}: ${check.status}${check.message ? ` (${check.message})` : ''}`).join(' · ')}`); }
    catch (error) { setStatus(String(error)); }
  }
  return <Panel title="Canais" subtitle="Configure as integrações disponíveis no runtime e consulte as próximas do catálogo.">
    {selected ? <div className="settings-card"><button className="back" onClick={() => setSelected(null)}>← Voltar ao catálogo</button><h2>{selected.name}</h2><p>{selected.description}</p>{selected.setup?.external_dependency && <p>Requer serviço externo: {selected.setup.external_dependency}</p>}<div className="form-grid">{selected.setup?.fields.map(field => <label key={field.field}>{field.field}{field.required && ' *'}
      {field.kind === 'enum' || field.kind === 'bool' ? <select value={values[field.field] || ''} onChange={event => setValues({ ...values, [field.field]: event.target.value })}>{(field.kind === 'bool' ? ['true', 'false'] : field.choices).map(choice => <option key={choice}>{choice}</option>)}</select> :
        <input type={field.kind === 'secret' ? 'password' : field.kind === 'int' || field.kind === 'float' ? 'number' : 'text'} step={field.kind === 'float' ? 'any' : undefined} value={values[field.field] || ''} placeholder={field.kind === 'secret' && configured[selected.id]?.[field.field + 'Configured'] ? 'Configurado · deixe vazio para manter' : ''} onChange={event => setValues({ ...values, [field.field]: event.target.value })}/>}</label>)}</div><div className="form-actions"><button className="primary" onClick={() => void save()}>Salvar canal</button>{selected.setup?.verifies_connection && <button onClick={() => void validate()}>Testar conexão</button>}<span>{status}</span></div></div> : <><label className="search big"><Search size={16}/><input placeholder="Buscar integração" value={query} onChange={event => setQuery(event.target.value)}/></label><div className="tile-grid">{catalog.filter(item => item.name.toLowerCase().includes(query.toLowerCase())).map(item => <button className="tile" key={item.id} onClick={() => open(item)} disabled={!item.available}><Cable size={20}/><strong>{item.name}</strong><span>{item.description}</span><small className={item.available ? 'available' : ''}>{item.available ? 'Disponível · configurar' : 'Transporte indisponível'}</small></button>)}</div></>}
  </Panel>;
}
