import { useState } from 'react';
import { Bot, Search } from 'lucide-react';
import { patchConfig, type State } from '../../api';
import { Panel } from '../ui';

export function Models({ state, refresh, report }: { state: State; refresh: () => Promise<void>; report: (error: string) => void }) {
  const providers = state.config.providers || {};
  const presets = state.config.modelPresets || {};
  const [query, setQuery] = useState(''); const [selected, setSelected] = useState(''); const [key, setKey] = useState(''); const [base, setBase] = useState(''); const [model, setModel] = useState(state.config.agents?.defaults?.model || ''); const [status, setStatus] = useState('');
  const defaults = state.config.agents?.defaults || {};
  const [provider, setProvider] = useState(defaults.provider || '');
  const [maxTokens, setMaxTokens] = useState(String(defaults.maxTokens || ''));
  const [contextWindow, setContextWindow] = useState(String(defaults.contextWindowTokens || ''));
  const [temperature, setTemperature] = useState(String(defaults.temperature ?? ''));
  const [reasoning, setReasoning] = useState(defaults.reasoningEffort || '');
  const [fallback, setFallback] = useState<string>((defaults.fallbackModels || []).filter((value: unknown) => typeof value === 'string').join(', '));
  const names = ['openai', 'anthropic', 'gemini', 'deepseek', 'openrouter', 'xiaomiMimo', 'ollama', 'lmStudio', 'custom', ...Object.keys(providers).filter(name => !['openai', 'anthropic', 'gemini', 'deepseek', 'openrouter', 'xiaomiMimo', 'ollama', 'lmStudio', 'custom'].includes(name))];
  async function save() {
    try { await patchConfig({ providers: { [selected]: { apiBase: base || null, ...(key ? { apiKey: key } : {}) } } }); setStatus('Salvo · reinício necessário'); setKey(''); await refresh(); }
    catch (error) { report(String(error)); }
  }
  async function saveModel() {
    const numeric = [maxTokens, contextWindow, temperature];
    if (numeric.some(value => value && !Number.isFinite(Number(value)))) { setStatus('Parâmetros numéricos inválidos.'); return; }
    try { await patchConfig({ agents: { defaults: { model: model.trim(), provider: provider.trim(),
      ...(maxTokens ? { maxTokens: Number(maxTokens) } : {}), ...(contextWindow ? { contextWindowTokens: Number(contextWindow) } : {}),
      ...(temperature ? { temperature: Number(temperature) } : {}), reasoningEffort: reasoning || null,
      fallbackModels: [...fallback.split(',').map(value => value.trim()).filter(Boolean), ...(defaults.fallbackModels || []).filter((value: unknown) => typeof value === 'object' && value !== null)] } } }); setStatus('Modelo salvo · reinício necessário'); await refresh(); }
    catch (error) { report(String(error)); }
  }
  function applyPreset(name: string) {
    const preset = presets[name]; if (!preset) return;
    setModel(preset.model || ''); setProvider(preset.provider || '');
    setMaxTokens(String(preset.maxTokens || '')); setContextWindow(String(preset.contextWindowTokens || ''));
    setTemperature(String(preset.temperature ?? '')); setReasoning(preset.reasoningEffort || '');
    setStatus('Preset aplicado: ' + name);
  }
  return <Panel title="Models & Providers" subtitle="Selecione o modelo principal, fallback e parâmetros do runtime."><div className="settings-card"><h2>Modelo principal</h2><div className="form-grid">{Object.keys(presets).length > 0 && <label>Preset<select defaultValue="" onChange={event => applyPreset(event.target.value)}><option value="">Selecionar preset</option>{Object.keys(presets).sort().map(name => <option key={name}>{name}</option>)}</select></label>}<label>Modelo<input value={model} onChange={event => setModel(event.target.value)}/></label><label>Provider<input value={provider} onChange={event => setProvider(event.target.value)}/></label><label>Max tokens<input type="number" value={maxTokens} onChange={event => setMaxTokens(event.target.value)}/></label><label>Context window<input type="number" value={contextWindow} onChange={event => setContextWindow(event.target.value)}/></label><label>Temperature<input type="number" step="any" value={temperature} onChange={event => setTemperature(event.target.value)}/></label><label>Reasoning effort<input value={reasoning} onChange={event => setReasoning(event.target.value)}/></label><label className="wide">Fallback models, separados por vírgula<input value={fallback} onChange={event => setFallback(event.target.value)}/></label></div><div className="form-actions"><button className="primary" onClick={() => void saveModel()}>Salvar modelo</button><span>{status}</span></div></div><h2>Providers</h2><label className="search big"><Search size={16}/><input value={query} onChange={event => setQuery(event.target.value)} placeholder="Buscar provider"/></label><div className="tile-grid">{names.filter(name => name.toLowerCase().includes(query.toLowerCase())).map(name => <button key={name} className="tile" onClick={() => { setSelected(name); setBase(providers[name]?.apiBase || ''); setKey(''); setStatus(''); }}><Bot size={19}/><strong>{name}</strong><span>{providers[name]?.apiBase || 'API ou endpoint local'}</span><small className={providers[name]?.apiKeyConfigured ? 'available' : ''}>{providers[name]?.apiKeyConfigured ? 'Chave configurada' : 'Configurar'}</small></button>)}</div>{selected && <div className="settings-card"><h2>{selected}</h2><div className="form-grid"><label>Chave de API<input type="password" value={key} placeholder={providers[selected]?.apiKeyConfigured ? 'Configurada · deixe vazio para manter' : ''} onChange={event => setKey(event.target.value)}/></label><label>Endpoint<input value={base} onChange={event => setBase(event.target.value)}/></label></div><div className="form-actions"><button className="primary" onClick={() => void save()}>Salvar provider</button><span>{status}</span></div></div>}</Panel>;
}
