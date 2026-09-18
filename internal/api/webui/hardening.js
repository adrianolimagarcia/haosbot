(() => {
  'use strict';

  const TOKEN_KEY = 'haosbot_token';
  const SESSION_KEY = 'haosbot_session_id';

  function randomSessionID() {
    if (globalThis.crypto && typeof globalThis.crypto.randomUUID === 'function') {
      return globalThis.crypto.randomUUID();
    }
    const bytes = new Uint8Array(16);
    globalThis.crypto.getRandomValues(bytes);
    return Array.from(bytes, b => b.toString(16).padStart(2, '0')).join('');
  }

  function getSessionID() {
    let id = sessionStorage.getItem(SESSION_KEY);
    if (!id) {
      id = randomSessionID();
      sessionStorage.setItem(SESSION_KEY, id);
    }
    return id;
  }

  // Credentials are session-scoped instead of persistent localStorage. They are
  // cleared when the browser session ends and never rendered back by /api/config.
  window.getToken = function getToken() {
    return sessionStorage.getItem(TOKEN_KEY) || '';
  };
  window.setToken = function setToken(token) {
    const value = (token || '').trim();
    if (value) sessionStorage.setItem(TOKEN_KEY, value);
    else sessionStorage.removeItem(TOKEN_KEY);
  };

  function authHeaders(extra = {}) {
    const headers = { ...extra };
    const token = window.getToken();
    if (token) headers.Authorization = `Bearer ${token}`;
    return headers;
  }

  async function appendTextMessage(stream, text, kind) {
    const wrapper = document.createElement('div');
    let assistantBody = null;
    if (kind === 'user') {
      wrapper.className = 'flex justify-end';
      const bubble = document.createElement('div');
      bubble.className = 'bg-gray-100 text-gray-900 px-4 py-2.5 rounded-2xl max-w-xl text-sm border border-gray-200 whitespace-pre-wrap';
      bubble.textContent = text;
      wrapper.appendChild(bubble);
    } else if (kind === 'error') {
      wrapper.className = 'p-3 bg-red-50 border border-red-200 text-red-700 rounded-lg text-xs whitespace-pre-wrap';
      wrapper.textContent = text;
    } else {
      wrapper.className = 'space-y-2';

      const top = document.createElement('div');
      top.className = 'flex items-center justify-between text-[11px] text-gray-400';
      const label = document.createElement('span');
      label.className = 'font-semibold text-gray-600';
      label.textContent = 'TEXT';
      const copy = document.createElement('button');
      copy.className = 'hover:text-black';
      copy.textContent = 'Copiar';
      copy.addEventListener('click', () => navigator.clipboard.writeText(text));
      top.append(label, copy);

      // LLM output is untrusted. The server renders it (internal/api/markdown.go)
      // and returns markup that is safe by construction, so the browser never
      // parses untrusted text into markup itself. The raw text is shown first as
      // inert text and replaced once the renderer answers; if the renderer is
      // unavailable the inert text simply stays.
      assistantBody = document.createElement('div');
      assistantBody.className = 'prose max-w-none text-gray-800 text-sm';
      assistantBody.textContent = text;

      const footer = document.createElement('div');
      footer.className = 'text-gray-400 text-[10px] pt-2';
      footer.textContent = 'Agora';
      wrapper.append(top, assistantBody, footer);
    }
    stream.appendChild(wrapper);

    if (assistantBody) {
      const html = await renderMarkdown(text);
      applyRenderedHTML(assistantBody, html);
    }
  }

  async function renderMarkdown(text) {
    try {
      const res = await fetch('/api/webui/render', {
        method: 'POST',
        headers: authHeaders({ 'Content-Type': 'application/json' }),
        body: JSON.stringify({ text }),
      });
      if (!res.ok) return null;
      const data = await res.json();
      return typeof data.html === 'string' ? data.html : null;
    } catch (err) {
      console.warn('Falha ao renderizar a resposta:', err);
      return null;
    }
  }

  function applyRenderedHTML(element, html) {
    if (html !== null) element.innerHTML = html;
  }

  function appendStreamingMessage(stream) {
    const wrapper = document.createElement('div');
    wrapper.className = 'space-y-2';
    const top = document.createElement('div');
    top.className = 'flex items-center justify-between text-[11px] text-gray-400';
    const label = document.createElement('span');
    label.className = 'font-semibold text-gray-600';
    label.textContent = 'TEXT';
    top.appendChild(label);
    const body = document.createElement('div');
    body.className = 'prose max-w-none text-gray-800 text-sm whitespace-pre-wrap';
    const footer = document.createElement('div');
    footer.className = 'text-gray-400 text-[10px] pt-2';
    footer.textContent = 'Gerando...';
    wrapper.append(top, body, footer);
    stream.appendChild(wrapper);
    return { body, footer };
  }

  async function consumeAgentStream(response, onEvent) {
    if (!response.body) throw new Error('resposta sem corpo de streaming');
    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let buffer = '';
    for (;;) {
      const { value, done } = await reader.read();
      buffer += decoder.decode(value || new Uint8Array(), { stream: !done });
      const lines = buffer.split('\n');
      buffer = lines.pop() || '';
      for (const line of lines) {
        const trimmed = line.trim();
        if (!trimmed) continue;
        onEvent(JSON.parse(trimmed));
      }
      if (done) break;
    }
    if (buffer.trim()) onEvent(JSON.parse(buffer.trim()));
  }

  window.newSession = function newSession() {
    sessionStorage.setItem(SESSION_KEY, randomSessionID());
    const stream = document.getElementById('chat-stream');
    if (stream) stream.replaceChildren();
    const title = document.getElementById('header-chat-title');
    if (title) title.textContent = 'Novo chat';
  };

  let sendInFlight = false;
  let turnAbort = null;

  // The Stop button aborts the in-flight request. The server derives the turn
  // context from the request context (internal/api/agent_turn_stream.go), so
  // aborting the request cancels the turn itself and not merely this browser's
  // view of it.
  function setTurnRunning(running) {
    const stopButton = document.getElementById('stop-button');
    const sendButton = document.getElementById('send-button');
    if (stopButton) {
      stopButton.classList.toggle('hidden', !running);
      stopButton.classList.toggle('flex', running);
    }
    if (sendButton) {
      sendButton.classList.toggle('hidden', running);
      sendButton.classList.toggle('flex', !running);
      sendButton.disabled = running;
    }
  }

  window.stopTurn = function stopTurn() {
    if (turnAbort) turnAbort.abort();
  };

  window.handleSend = async function handleSend() {
    if (sendInFlight) return;
    const input = document.getElementById('user-input');
    const text = input.value.trim();
    if (!text) return;

    sendInFlight = true;
    turnAbort = new AbortController();
    setTurnRunning(true);
    input.disabled = true;
    input.value = '';
    const stream = document.getElementById('chat-stream');
    await appendTextMessage(stream, text, 'user');

    const loader = document.createElement('div');
    loader.className = 'flex items-center space-x-2 text-xs text-gray-400';
    loader.textContent = 'Processando turno...';
    stream.appendChild(loader);

    try {
      const sessionID = getSessionID();
      const headers = authHeaders({
        'Content-Type': 'application/json',
        'X-HAOS-Session-ID': sessionID,
      });
      const res = await fetch('/api/agent/turn/stream', {
        method: 'POST',
        headers,
        body: JSON.stringify({ sessionId: sessionID, message: text }),
        signal: turnAbort ? turnAbort.signal : undefined,
      });
      if (!res.ok) {
        loader.remove();
        await appendTextMessage(stream, `Erro (${res.status}): ${await res.text()}`, 'error');
      } else {
        const assistant = appendStreamingMessage(stream);
        loader.remove();
        let content = '';
        let failed = null;
        await consumeAgentStream(res, event => {
          if (event.type === 'text_delta') {
            content += event.delta || '';
            assistant.body.textContent = content;
          } else if (event.type === 'reasoning_delta') {
            assistant.footer.textContent = 'Raciocinando...';
          } else if (event.type === 'tool_start') {
            assistant.footer.textContent = `Executando ${event.toolName || 'ferramenta'}...`;
          } else if (event.type === 'tool_end') {
            assistant.footer.textContent = 'Continuando...';
          } else if (event.type === 'done') {
            content = event.content || content || '(sem resposta)';
            assistant.body.textContent = content;
            assistant.footer.textContent = 'Agora';
          } else if (event.type === 'error') {
            failed = event.error || 'erro no turno';
          }
        });
        if (failed) {
          assistant.body.textContent = `Erro: ${failed}`;
          assistant.body.className = 'text-red-700 text-sm whitespace-pre-wrap';
        } else {
          const html = await renderMarkdown(content || '(sem resposta)');
          applyRenderedHTML(assistant.body, html);
        }
      }
    } catch (err) {
      if (loader.isConnected) loader.remove();
      if (err && err.name === 'AbortError') {
        await appendTextMessage(stream, 'Turno interrompido pelo usuário.', 'error');
      } else {
        await appendTextMessage(stream, `Erro de conexão: ${err.message}`, 'error');
      }
    } finally {
      sendInFlight = false;
      turnAbort = null;
      setTurnRunning(false);
      input.disabled = false;
      input.focus();
    }

    const container = document.getElementById('messages-container');
    container.scrollTop = container.scrollHeight;
  };

  window.fetchRemoteModels = async function fetchRemoteModels() {
    const base = document.getElementById('dlg-llm-base').value.trim();
    const key = document.getElementById('dlg-llm-key').value.trim();
    const feedback = document.getElementById('fetch-feedback');
    const select = document.getElementById('dlg-llm-model-select');
    const fetchBtn = document.getElementById('btn-fetch-models');
    if (!base) {
      feedback.textContent = 'Por favor, preencha a URL Base antes de buscar.';
      return;
    }

    feedback.textContent = 'Consultando endpoint...';
    fetchBtn.disabled = true;
    try {
      const res = await fetch('/api/fetch-models', {
        method: 'POST',
        headers: authHeaders({ 'Content-Type': 'application/json' }),
        body: JSON.stringify({ baseUrl: base, apiKey: key }),
      });
      if (!res.ok) {
        feedback.textContent = `Falha ao consultar modelos: ${await res.text()}`;
        return;
      }
      const data = await res.json();
      const models = Array.isArray(data.data) ? data.data : [];
      select.replaceChildren();
      const first = document.createElement('option');
      first.value = '';
      first.textContent = `-- Escolha um modelo da lista (${models.length} encontrados) --`;
      select.appendChild(first);
      for (const model of models) {
        const option = document.createElement('option');
        option.value = String(model.id || '');
        option.textContent = String(model.id || '');
        select.appendChild(option);
      }
      if (models.length) {
        select.value = String(models[0].id || '');
        document.getElementById('dlg-llm-model').value = select.value;
      }
      feedback.textContent = `${models.length} modelos carregados.`;
    } catch (err) {
      feedback.textContent = `Erro de conexão: ${err.message}`;
    } finally {
      fetchBtn.disabled = false;
    }
  };

  window.openSettings = async function openSettings() {
    document.getElementById('dlg-status').textContent = '';
    document.getElementById('settings-dialog').classList.remove('hidden');
    try {
      const res = await fetch('/api/config', { headers: authHeaders() });
      if (!res.ok) return;
      const cfg = await res.json();
      document.getElementById('dlg-llm-base').value = cfg.providers?.openai?.apiBase || '';
      document.getElementById('dlg-llm-key').value = '';
      document.getElementById('dlg-llm-key').placeholder = cfg.providers?.openai?.apiKeyConfigured
        ? 'Chave configurada — deixe vazio para manter'
        : 'API key';
      document.getElementById('dlg-llm-model').value = cfg.agents?.defaults?.model || 'deepseek-chat';
      document.getElementById('dlg-gateway-key').value = window.getToken();
      document.getElementById('dlg-gateway-key').placeholder = cfg.api?.apiKeyConfigured
        ? 'Token configurado — informe o token atual para autenticar'
        : 'Bearer token';
    } catch (err) {
      console.warn('Falha ao carregar configuração:', err);
    }
  };

  window.saveDialogSettings = async function saveDialogSettings() {
    const llmBase = document.getElementById('dlg-llm-base').value.trim();
    const llmKey = document.getElementById('dlg-llm-key').value.trim();
    const llmModel = document.getElementById('dlg-llm-model').value.trim() || 'deepseek-chat';
    const gatewayKey = document.getElementById('dlg-gateway-key').value.trim();

    const patch = {
      agents: { defaults: { model: llmModel } },
      providers: { openai: { apiBase: llmBase, apiKey: llmKey } },
      api: { apiKey: gatewayKey },
    };

    const headers = authHeaders({ 'Content-Type': 'application/json' });
    if (!headers.Authorization && gatewayKey) headers.Authorization = `Bearer ${gatewayKey}`;
    const status = document.getElementById('dlg-status');
    try {
      const res = await fetch('/api/config', {
        method: 'POST',
        headers,
        body: JSON.stringify(patch),
      });
      if (!res.ok) {
        status.textContent = `Erro ao salvar: ${await res.text()}`;
        return;
      }
      window.setToken(gatewayKey || window.getToken());
      document.getElementById('model-selector-badge').textContent = llmModel;
      const data = await res.json();
      status.textContent = data.restartRequired ? 'Salvo. Reinicie para aplicar.' : 'Salvo.';
    } catch (err) {
      status.textContent = `Erro de rede: ${err.message}`;
    }
  };

  // Remove the persistent legacy token if this browser used an older build.
  localStorage.removeItem(TOKEN_KEY);
  getSessionID();
})();
