import { useState } from 'react';
import { request } from '../../api';
import { Panel } from '../ui';

export function Files() {
  const [path, setPath] = useState(''); const [content, setContent] = useState(''); const [status, setStatus] = useState('');
  async function open() {
    try { const data = await request<{ directory?: boolean; entries?: Array<{ name: string; dir: boolean }>; binary?: boolean; content?: string; truncated?: boolean }>('/api/webui/file-preview?path=' + encodeURIComponent(path.trim()));
      setContent(data.directory ? (data.entries || []).map(entry => `${entry.dir ? '[dir]' : '     '} ${entry.name}`).join('\n') : data.binary ? '[Arquivo binário]' : data.content || '');
      setStatus(data.truncated ? 'Preview truncado' : 'Preview carregado'); }
    catch (error) { setStatus(String(error)); }
  }
  return <Panel title="Arquivos" subtitle="Prévia de arquivos e pastas do workspace."><div className="settings-card"><div className="form-actions"><input value={path} onChange={event => setPath(event.target.value)} onKeyDown={event => { if (event.key === 'Enter') void open(); }} placeholder="caminho/relativo/arquivo.md" aria-label="Caminho relativo"/><button className="primary" onClick={() => void open()}>Abrir</button><span>{status}</span></div><pre className="file-preview">{content}</pre></div></Panel>;
}
