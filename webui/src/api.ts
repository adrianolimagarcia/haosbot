export type Session = { key: string; session_id: string; title: string; pinned: boolean; archived: boolean; selectable: boolean; updated_at: string };
export type ChannelCapabilities = { text: boolean; media: boolean; threads: boolean; reactions: boolean; streaming: boolean; typing: boolean; groups: boolean };
export type CatalogChannel = { id: string; name: string; description: string; available: boolean; probe?: 'config' | 'dependency' | 'live'; capabilities?: ChannelCapabilities; setup?: { fields: Array<{ field: string; kind: string; choices: string[]; required: boolean; default_value?: string }>; verifies_connection?: boolean; external_dependency?: string } };
export type State = { sessions: Session[]; skills: Array<{ name: string; description: string; available: boolean; unavailable_reason?: string }>;
  config: Record<string, any>; metrics: Record<string, any>; capabilities: Record<string, boolean>; memory: { path: string; bytes: number }; workspace: string };

export const token = () => sessionStorage.getItem('haosbot_token') || '';
export const setToken = (value: string) => value ? sessionStorage.setItem('haosbot_token', value.trim()) : sessionStorage.removeItem('haosbot_token');
export async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers);
  if (init.body && !headers.has('Content-Type')) headers.set('Content-Type', 'application/json');
  if (token()) headers.set('Authorization', `Bearer ${token()}`);
  const response = await fetch(path, { ...init, headers });
  if (!response.ok) throw new Error(`${response.status}: ${(await response.text()).slice(0, 250)}`);
  return response.json() as Promise<T>;
}
export const patchConfig = (patch: object) => request<{ restartRequired: boolean }>('/api/config', { method: 'POST', body: JSON.stringify(patch) });
export const sessionAction = (key: string, action: string, value?: unknown) => request('/api/webui/session/action', { method: 'POST', body: JSON.stringify({ key, action, value }) });

export type TurnEvent = { type: string; delta?: string; content?: string; error?: string; toolName?: string; contextChars?: number };
export async function streamTurn(sessionId: string, message: string, media: string[], onEvent: (event: TurnEvent) => void, signal: AbortSignal) {
  const headers = new Headers({ 'Content-Type': 'application/json' });
  if (token()) headers.set('Authorization', `Bearer ${token()}`);
  const response = await fetch('/api/agent/turn/stream', { method: 'POST', headers, body: JSON.stringify({ sessionId, message, media }), signal });
  if (!response.ok || !response.body) throw new Error(`${response.status}: ${(await response.text()).slice(0, 250)}`);
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let pending = '';
  for (;;) {
    const { done, value } = await reader.read();
    pending += decoder.decode(value || new Uint8Array(), { stream: !done });
    const lines = pending.split('\n');
    pending = lines.pop() || '';
    for (const line of lines) if (line.trim()) onEvent(JSON.parse(line) as TurnEvent);
    if (done) { if (pending.trim()) onEvent(JSON.parse(pending) as TurnEvent); break; }
  }
}

export async function uploadAttachment(sessionId: string, file: File): Promise<{ name: string; path: string }> {
  const form = new FormData(); form.append('sessionId', sessionId); form.append('file', file);
  const headers = new Headers(); if (token()) headers.set('Authorization', `Bearer ${token()}`);
  const response = await fetch('/api/webui/attachment', { method: 'POST', headers, body: form });
  if (!response.ok) throw new Error(`${response.status}: ${(await response.text()).slice(0, 250)}`);
  return response.json();
}
