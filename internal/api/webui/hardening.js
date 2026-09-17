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

  function appendTextMessage(stream, text, kind) {
    const wrapper = document.createElement('div');
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

      // LLM output is untrusted. Use textContent rather than marked.parse +
      // innerHTML, which allowed model/tool/document content to execute HTML.
      const body = document.createElement('pre');
      body.className = 'whitespace-pre-wrap font-sans text-gray-800 text-sm leading-relaxed bg-transparent border-0 p-0 m-0';
      body.textContent = text;

      const footer = document.createElement('div');
      footer.className = 'text-gray-400 text-[10px] pt-2';
      footer.textContent = 'Agora';
      wrapper.append(top, body, footer);
    }
    stream.appendChild(wrapper);
  }

  window.newSession = function newSession() {
    window.chatHistory = [];
    // The original page declares chatHistory with let, so keep it in sync by
    // mutating the lexical binding through the existing global function path
    // when available. handleSend below owns its own state to avoid dependence.
    sessionStorage.setItem(SESSION_KEY, randomSessionID());
    const stream = document.getElementById('chat-stream');
    if (stream) stream.replaceChildren();
    const title = document.getElementById('header-chat-title');
    if (title) title.textContent = 'Novo chat';
    hardeningHistory.length = 0;
  };

  const hardeningHistory = [];

  window.handleSend = async function handleSend() {
    const input = document.getElementById('user-input');
    const text = input.value.trim();
    if (!text) return;

    input.value = '';
    hardeningHistory.push({ role: 'user', content: text });
    const stream = document.getElementById('chat-stream');
    appendTextMessage(stream, text, 'user');

    const loader = document.createElement('div');
    loader.className = 'flex items-center space-x-2 text-xs text-gray-400';
    loader.textContent = 'Processando turno...';
    stream.appendChild(loader);

    try {
      const model = document.getElementById('model-selector-badge').textContent || 'deepseek-chat';
      const headers = authHeaders({
        'Content-Type': 'application/json',
        'X-HAOS-Session-ID': getSessionID(),
      });
      const res = await fetch('/v1/chat/completions', {
        method: 'POST',
        headers,
        body: JSON.stringify({ model, messages: hardeningHistory }),
      });
      loader.remove();

      if (!res.ok) {
        appendTextMessage(stream, `Erro (${res.status}): ${await res.text()}`, 'error');
      } else {
        const data = await res.json();
        const reply = data.choices?.[0]?.message?.content || '(sem resposta)';
        hardeningHistory.push({ role: 'assistant', content: reply });
        appendTextMessage(stream, reply, 'assistant');
      }
    } catch (err) {
      loader.remove();
      appendTextMessage(stream, `Erro de conexão: ${err.message}`, 'error');
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

    let headers = authHeaders({ 'Content-Type': 'application/json' });
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
