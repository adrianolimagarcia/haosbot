(() => {
  'use strict';

  let state = null;
  let showArchived = false;
  let activeKey = null;
  let activeView = null;
  let searchTimer = null;

  const byId = id => document.getElementById(id);
  const el = (tag, className, text) => {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined) node.textContent = text;
    return node;
  };

  async function api(path, options = {}) {
    const headers = typeof window.webuiAuthHeaders === 'function'
      ? window.webuiAuthHeaders(options.headers || {})
      : (options.headers || {});
    const res = await fetch(path, { ...options, headers });
    if (!res.ok) throw new Error(`HTTP ${res.status}: ${await res.text()}`);
    if (res.status === 204) return null;
    return res.json();
  }

  async function refreshState() {
    try {
      state = await api('/api/webui/state');
      renderSessions();
      if (activeView) renderView(activeView);
      const model = state?.config?.agents?.defaults?.model;
      if (model && byId('model-selector-badge')) byId('model-selector-badge').textContent = model;
      const usage = Number(state?.metrics?.total_tokens || 0);
      if (byId('token-indicator')) byId('token-indicator').textContent = usage ? usage.toLocaleString() + ' tokens' : 'tokens —';
    } catch (err) {
      const list = byId('session-list');
      if (list) {
        list.replaceChildren(el('div', 'session-loading', 'Falha ao carregar: ' + err.message));
      }
    }
  }

  function applyTheme(theme) {
    const resolved = theme === 'dark' ? 'dark' : 'light';
    document.documentElement.dataset.theme = resolved;
    localStorage.setItem('haosbot-theme', resolved);
  }

  function configEditor(title, key, value) {
    const wrap = el('div', 'control-editor-block');
    wrap.appendChild(el('div', 'control-section-title', title));
    const area = el('textarea', 'memory-editor');
    area.spellcheck = false;
    area.value = JSON.stringify(value || {}, null, 2);
    const toolbar = el('div', 'control-toolbar');
    const save = el('button', 'control-button primary', 'Salvar');
    save.type = 'button';
    const status = el('span', 'control-muted', '');
    save.addEventListener('click', async () => {
      try {
        const parsed = JSON.parse(area.value || '{}');
        status.textContent = 'Salvando…';
        const payload = await api('/api/config', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ [key]: parsed })
        });
        status.textContent = payload?.restartRequired ? 'Salvo · reinício necessário' : 'Salvo';
        await refreshState();
      } catch (err) {
        status.textContent = err.message;
      }
    });
    toolbar.append(save, status);
    wrap.append(area, toolbar);
    return wrap;
  }

  function relativeTime(value) {
    const ts = Date.parse(value || '');
    if (!Number.isFinite(ts)) return '';
    const seconds = Math.max(0, Math.floor((Date.now() - ts) / 1000));
    if (seconds < 60) return 'agora';
    if (seconds < 3600) return Math.floor(seconds / 60) + ' min';
    if (seconds < 86400) return Math.floor(seconds / 3600) + ' h';
    return Math.floor(seconds / 86400) + ' d';
  }

  function actionButton(label, action, session, value) {
    const button = el('button', 'session-action', label);
    button.type = 'button';
    button.title = action;
    button.dataset.sessionAction = action;
    button.dataset.sessionKey = session.key;
    if (value !== undefined) button.dataset.value = String(value);
    return button;
  }

  function renderSessions() {
    const list = byId('session-list');
    if (!list) return;
    list.replaceChildren();
    const sessions = Array.isArray(state?.sessions) ? state.sessions : [];
    const visible = sessions.filter(s => showArchived || !s.archived);
    for (const session of visible) {
      const row = el('div', 'session-row' + (session.key === activeKey ? ' active' : ''));
      const open = el('button', 'session-open');
      open.type = 'button';
      open.dataset.sessionKey = session.key;
      open.dataset.sessionId = session.session_id || '';
      const title = el('span', 'session-title', session.title || 'Novo chat');
      const meta = el('span', 'session-meta');
      meta.append(
        el('span', '', relativeTime(session.updated_at)),
        el('span', '', String(session.messages || 0) + ' msgs')
      );
      if (session.pinned) meta.append(el('span', '', 'fixado'));
      open.append(title, meta);
      const actions = el('div', 'session-actions');
      actions.append(
        actionButton(session.pinned ? '◆' : '◇', 'pin', session, !session.pinned),
        actionButton('✎', 'rename', session),
        actionButton(session.archived ? '↥' : '↧', 'archive', session, !session.archived),
        actionButton('×', 'delete', session)
      );
      row.append(open, actions);
      list.appendChild(row);
    }
    if (!visible.length) list.appendChild(el('div', 'session-loading', 'Nenhuma sessão.'));
    const toggle = byId('toggle-archived');
    if (toggle) {
      const count = sessions.filter(s => s.archived).length;
      toggle.classList.toggle('hidden', count === 0);
      toggle.textContent = showArchived ? 'Ocultar arquivados' : `Mostrar arquivados (${count})`;
    }
  }

  async function openSession(key, sessionID) {
    if (!sessionID) return;
    const data = await api('/api/webui/session?key=' + encodeURIComponent(key));
    activeKey = key;
    if (typeof window.activateWebSession === 'function') {
      await window.activateWebSession(sessionID, data.messages || [], sessionTitle(key));
    }
    closeView();
    renderSessions();
  }

  function sessionTitle(key) {
    return state?.sessions?.find(s => s.key === key)?.title || 'Novo chat';
  }

  async function mutateSession(action, key, value) {
    await api('/api/webui/session/action', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ action, key, value })
    });
    if (action === 'delete' && activeKey === key) {
      activeKey = null;
      if (typeof window.newSession === 'function') window.newSession();
    }
    await refreshState();
  }

  function openView(view) {
    activeView = view;
    const panel = byId('workspace-panel');
    if (panel) panel.classList.remove('hidden');
    renderView(view);
  }

  function closeView() {
    activeView = null;
    const panel = byId('workspace-panel');
    if (panel) panel.classList.add('hidden');
  }

  function setPanelTitle(title, kicker = 'HAOSBOT') {
    if (byId('workspace-panel-title')) byId('workspace-panel-title').textContent = title;
    if (byId('workspace-panel-kicker')) byId('workspace-panel-kicker').textContent = kicker;
  }

  function renderView(view) {
    const root = byId('workspace-panel-content');
    if (!root) return;
    root.replaceChildren();
    if (!state) {
      root.appendChild(el('div', 'session-loading', 'Carregando…'));
      return;
    }
    if (view === 'apps') return renderApps(root);
    if (view === 'skills') return renderSkills(root);
    if (view === 'automations') return renderAutomations(root);
    if (view === 'channels') return renderChannels(root);
    if (view === 'runtime') return renderRuntime(root);
    return renderSettings(root);
  }

  function card(title, body, stat) {
    const c = el('div', 'control-card');
    c.append(el('h3', '', title), el('p', '', body || ''));
    if (stat !== undefined) c.append(el('div', 'control-stat', String(stat)));
    return c;
  }

  function renderApps(root) {
    setPanelTitle('Apps & Capabilities', 'Workspace');
    const grid = el('div', 'control-grid');
    const caps = state.capabilities || {};
    const labels = {
      files: ['Arquivos', 'Leitura e edição no workspace'],
      exec: ['Exec / Python', 'Ferramentas locais de execução'],
      web: ['Web / A2A', 'Capacidades de rede habilitadas'],
      memory_search: ['Memory Search', 'GraphRAG profundo sob demanda'],
      graph_async: ['Async GraphRAG', 'Indexação fora do hot path'],
      streaming: ['Streaming', 'SSE token/delta em tempo real']
    };
    for (const [key, value] of Object.entries(caps)) {
      const meta = labels[key] || [key, ''];
      grid.appendChild(card(meta[0], meta[1], value ? 'Ativo' : 'Desativado'));
    }
    root.appendChild(grid);

    const toolsCfg = state.config?.tools || {};
    root.appendChild(el('div', 'control-section-title', 'CLI Apps / MCP'));
    const integrations = el('div', 'control-grid');
    const cli = toolsCfg.cliApps || {};
    const mcp = toolsCfg.mcpServers || {};
    integrations.append(
      card('CLI Apps', 'Execução de apps/CLIs configurados no runtime.', cli.enable === false ? 'Desativado' : 'Ativo'),
      card('MCP Servers', Object.keys(mcp).length ? Object.keys(mcp).join(', ') : 'Nenhum servidor MCP configurado.', String(Object.keys(mcp).length))
    );
    root.appendChild(integrations);

    root.appendChild(el('div', 'control-section-title', 'Preview seguro do workspace'));
    const toolbar = el('div', 'control-toolbar');
    const input = el('input', 'control-input');
    input.type = 'text';
    input.placeholder = 'caminho/relativo/arquivo.md';
    const open = el('button', 'control-button primary', 'Abrir');
    const status = el('span', 'control-muted', '');
    const preview = el('pre', 'control-pre');
    toolbar.append(input, open, status);
    root.append(toolbar, preview);
    open.addEventListener('click', async () => {
      status.textContent = 'Carregando…';
      preview.textContent = '';
      try {
        const item = await api('/api/webui/file-preview?path=' + encodeURIComponent(input.value.trim()));
        if (item.directory) {
          preview.textContent = (item.entries || []).map(e => (e.dir ? '[dir] ' : '      ') + e.name).join('\n');
        } else if (item.binary) {
          preview.textContent = '[arquivo binário — preview textual indisponível]';
        } else {
          preview.textContent = item.content || '';
        }
        status.textContent = item.truncated ? 'Preview truncado' : humanBytes(item.bytes || 0);
      } catch (err) {
        status.textContent = err.message;
      }
    });
    root.appendChild(configEditor('Tools / MCP / CLI Apps', 'tools', toolsCfg));
  }

  function renderSkills(root) {
    setPanelTitle('Skills', 'Capabilities');
    const toolbar = el('div', 'control-toolbar');
    const create = el('button', 'control-button primary', 'Nova skill');
    create.type = 'button';
    create.addEventListener('click', () => editSkill('', '---\nname: new-skill\ndescription: Describe this skill.\n---\n\n# New Skill\n'));
    toolbar.appendChild(create);
    root.appendChild(toolbar);
    const list = el('div', 'control-list');
    for (const skill of state.skills || []) {
      const row = el('div', 'control-list-row');
      const button = el('button');
      button.type = 'button';
      button.dataset.skillName = skill.name;
      button.append(
        el('div', '', skill.name),
        el('div', 'control-muted', skill.description || skill.path || '')
      );
      const badges = el('div');
      badges.append(
        el('span', 'control-badge', skill.source || 'skill'),
        el('span', 'control-badge', skill.available ? 'disponível' : 'indisponível')
      );
      row.append(button, badges);
      list.appendChild(row);
    }
    root.appendChild(list);
  }

  async function showSkill(name) {
    const root = byId('workspace-panel-content');
    if (!root) return;
    setPanelTitle(name, 'Skill');
    root.replaceChildren(el('div', 'session-loading', 'Carregando skill…'));
    try {
      const data = await api('/api/webui/skill?name=' + encodeURIComponent(name));
      root.replaceChildren();
      const toolbar = el('div', 'control-toolbar');
      const back = el('button', 'control-button', '← Skills');
      back.type = 'button';
      back.addEventListener('click', () => renderView('skills'));
      toolbar.append(back, el('span', 'control-badge', data.description || name));
      if (data.source === 'workspace') {
        const edit = el('button', 'control-button', 'Editar');
        edit.type = 'button';
        edit.addEventListener('click', () => editSkill(data.name, data.content || ''));
        const remove = el('button', 'control-button danger', 'Excluir');
        remove.type = 'button';
        remove.addEventListener('click', async () => {
          if (!window.confirm('Excluir a skill ' + data.name + '?')) return;
          await api('/api/webui/skill?name=' + encodeURIComponent(data.name), { method: 'DELETE' });
          await refreshState();
          renderView('skills');
        });
        toolbar.append(edit, remove);
      }
      root.append(toolbar, el('pre', 'control-pre', data.content || ''));
    } catch (err) {
      root.replaceChildren(el('div', 'session-loading', err.message));
    }
  }

  function editSkill(existingName, content) {
    const root = byId('workspace-panel-content');
    if (!root) return;
    setPanelTitle(existingName || 'Nova skill', 'Skill editor');
    root.replaceChildren();
    const nameInput = el('input', 'control-input');
    nameInput.type = 'text';
    nameInput.placeholder = 'skill-name';
    nameInput.value = existingName || '';
    nameInput.disabled = Boolean(existingName);
    const area = el('textarea', 'memory-editor');
    area.spellcheck = false;
    area.value = content || '';
    const toolbar = el('div', 'control-toolbar');
    const save = el('button', 'control-button primary', 'Salvar skill');
    save.type = 'button';
    const cancel = el('button', 'control-button', 'Cancelar');
    cancel.type = 'button';
    const status = el('span', 'control-muted', '');
    cancel.addEventListener('click', () => renderView('skills'));
    save.addEventListener('click', async () => {
      const name = nameInput.value.trim();
      if (!name) { status.textContent = 'Nome obrigatório'; return; }
      try {
        status.textContent = 'Salvando…';
        await api('/api/webui/skill?name=' + encodeURIComponent(name), {
          method: 'PUT', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ content: area.value })
        });
        await refreshState();
        showSkill(name);
      } catch (err) { status.textContent = err.message; }
    });
    toolbar.append(save, cancel, status);
    root.append(nameInput, area, toolbar);
  }

  function renderAutomations(root) {
    setPanelTitle('Automações', 'Runtime');
    const cron = (state.skills || []).find(skill => skill.name === 'cron');
    const grid = el('div', 'control-grid');
    grid.append(
      card('Cron skill', cron ? (cron.available ? 'Disponível para o agente.' : 'Instalada, com requisito ausente.') : 'Skill cron não encontrada.', cron ? 'Detectada' : 'Ausente'),
      card('Background memory', 'GraphRAG e sumarização automática rodam fora do caminho crítico.', 'Assíncrono'),
      card('Scheduler nativo', 'Este runtime não expõe um CRUD de scheduler equivalente ao nanobot WebUI; a UI não inventa automações inexistentes.', 'N/A')
    );
    root.appendChild(grid);
  }

  function renderChannels(root) {
    setPanelTitle('Canais', 'Integrations');
    const channels = state.config?.channels || {};
    root.append(
      card('Configuração de canais', 'Edite a configuração redigida. Campos secretos em branco preservam os valores existentes.', 'Reinício para aplicar'),
      configEditor('channels', 'channels', channels)
    );
  }

  function renderRuntime(root) {
    setPanelTitle('Runtime', 'Observability');
    const m = state.metrics || {};
    const grid = el('div', 'control-grid');
    const stats = [
      ['Pre-provider', 'Overhead médio HAOS antes do provider', fmtMs(m.pre_provider_avg_ms)],
      ['Provider TTFT', 'Tempo médio do provider até o primeiro token', fmtMs(m.provider_ttft_avg_ms)],
      ['Turno total', 'Duração média fim a fim', fmtMs(m.turn_total_avg_ms)],
      ['Persistência', 'I/O médio de sessão', fmtMs(m.persistence_avg_ms)],
      ['Graph search', 'Busca GraphRAG explícita', fmtMs(m.graph_search_avg_ms)],
      ['Fila memória', 'Jobs de projeção pendentes', m.memory_pending ?? 0],
      ['Graph/vector', 'Embedder local', m.embedder_loaded ? 'Ativo' : 'FTS/graph'],
      ['Tokens total', 'Uso acumulado observado pelo runtime', Number(m.total_tokens || 0).toLocaleString()],
      ['Prompt', 'Tokens de entrada acumulados', Number(m.prompt_tokens || 0).toLocaleString()],
      ['Completion', 'Tokens de saída acumulados', Number(m.completion_tokens || 0).toLocaleString()],
      ['Cache', 'Tokens reportados como cached', Number(m.cached_tokens || 0).toLocaleString()],
      ['Reasoning', 'Tokens de reasoning reportados', Number(m.reasoning_tokens || 0).toLocaleString()],
      ['Heap', 'Memória alocada', humanBytes(m.heap_alloc_bytes || 0)]
    ];
    for (const stat of stats) grid.appendChild(card(stat[0], stat[1], stat[2]));
    root.appendChild(grid);
  }

  function renderSettings(root) {
    setPanelTitle('Configurações', 'HAOSBOT');
    const toolbar = el('div', 'control-toolbar');
    const models = el('button', 'control-button primary', 'Modelos & Endpoint');
    models.type = 'button';
    models.addEventListener('click', () => typeof window.openSettings === 'function' && window.openSettings());
    const runtime = el('button', 'control-button', 'Runtime');
    runtime.type = 'button';
    runtime.addEventListener('click', () => openView('runtime'));
    const theme = el('button', 'control-button', document.documentElement.dataset.theme === 'dark' ? 'Tema claro' : 'Tema escuro');
    theme.type = 'button';
    theme.addEventListener('click', () => {
      const next = document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark';
      applyTheme(next);
      theme.textContent = next === 'dark' ? 'Tema claro' : 'Tema escuro';
    });
    toolbar.append(models, runtime, theme);
    root.appendChild(toolbar);

    const grid = el('div', 'control-grid');
    grid.append(
      card('Workspace', state.workspace || '', String(state.sessions?.length || 0) + ' sessões'),
      card('Memória canônica', state.memory?.path || 'memory/MEMORY.md', humanBytes(state.memory?.bytes || 0)),
      card('GraphRAG', 'Índice derivado, reconstruível e assíncrono.', 'Background')
    );
    root.append(grid);
    root.appendChild(el('div', 'control-section-title', 'Providers configurados'));
    const providers = state.config?.providers || {};
    const providerList = el('div', 'control-list');
    for (const [name, cfg] of Object.entries(providers)) {
      if (!cfg || typeof cfg !== 'object') continue;
      const row = el('div', 'control-list-row');
      row.append(
        el('div', '', name),
        el('div', 'control-muted', cfg.baseUrl || cfg.base_url || cfg.apiType || cfg.api_type || '')
      );
      providerList.appendChild(row);
    }
    if (!providerList.childNodes.length) providerList.appendChild(el('div', 'session-loading', 'Nenhum provider configurado.'));
    root.appendChild(providerList);
    root.appendChild(configEditor('Providers avançados', 'providers', providers));
    root.appendChild(configEditor('Agent defaults', 'agents', state.config?.agents || {}));
    root.appendChild(el('div', 'control-section-title', 'Long-term Memory · MEMORY.md'));
    const actions = el('div', 'control-toolbar');
    const load = el('button', 'control-button', 'Recarregar');
    const save = el('button', 'control-button primary', 'Salvar memória');
    const status = el('span', 'control-muted', '');
    actions.append(load, save, status);
    const editor = el('textarea', 'memory-editor');
    editor.id = 'memory-editor';
    editor.spellcheck = false;
    root.append(actions, editor);
    const loadMemory = async () => {
      status.textContent = 'Carregando…';
      try {
        const data = await api('/api/webui/memory');
        editor.value = data.content || '';
        status.textContent = humanBytes(editor.value.length);
      } catch (err) { status.textContent = err.message; }
    };
    load.addEventListener('click', loadMemory);
    save.addEventListener('click', async () => {
      status.textContent = 'Salvando…';
      try {
        await api('/api/webui/memory', {
          method: 'PUT', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ content: editor.value })
        });
        status.textContent = 'Salvo. Próximo prompt usará o snapshot atualizado.';
        await refreshState();
      } catch (err) { status.textContent = err.message; }
    });
    loadMemory();
  }

  function fmtMs(value) {
    const n = Number(value || 0);
    return n < 1 ? n.toFixed(2) + ' ms' : n.toFixed(1) + ' ms';
  }

  function humanBytes(value) {
    let n = Number(value || 0);
    if (n < 1024) return Math.round(n) + ' B';
    if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
    return (n / 1024 / 1024).toFixed(1) + ' MB';
  }

  async function runSearch(query) {
    const target = byId('session-search-results');
    if (!target) return;
    const q = String(query || '').trim();
    if (!q) { target.classList.add('hidden'); target.replaceChildren(); return; }
    try {
      const data = await api('/api/webui/search?q=' + encodeURIComponent(q));
      target.replaceChildren();
      for (const result of data.results || []) {
        const button = el('button', 'search-result');
        button.type = 'button';
        button.dataset.searchSessionKey = result.key;
        button.append(
          el('span', 'search-result-title', result.title || 'Chat'),
          el('span', 'search-result-snippet', result.snippet || '')
        );
        target.appendChild(button);
      }
      if (!(data.results || []).length) target.appendChild(el('div', 'session-loading', 'Sem resultados.'));
      target.classList.remove('hidden');
    } catch (err) {
      target.replaceChildren(el('div', 'session-loading', err.message));
      target.classList.remove('hidden');
    }
  }

  document.addEventListener('click', async event => {
    const target = event.target instanceof Element ? event.target : null;
    if (!target) return;

    const actionEl = target.closest('[data-action]');
    const action = actionEl?.dataset.action;
    if (action === 'open-view') { event.preventDefault(); openView(actionEl.dataset.view || 'settings'); return; }
    if (action === 'close-view') { event.preventDefault(); closeView(); return; }
    if (action === 'refresh-state') { event.preventDefault(); await refreshState(); return; }
    if (action === 'toggle-archived') { event.preventDefault(); showArchived = !showArchived; renderSessions(); return; }

    const open = target.closest('[data-session-key]:not([data-session-action])');
    if (open && open.dataset.sessionKey && open.dataset.sessionId) {
      event.preventDefault();
      await openSession(open.dataset.sessionKey, open.dataset.sessionId);
      return;
    }

    const sessionAction = target.closest('[data-session-action]');
    if (sessionAction) {
      event.preventDefault();
      const key = sessionAction.dataset.sessionKey;
      const kind = sessionAction.dataset.sessionAction;
      if (kind === 'rename') {
        const title = window.prompt('Novo nome do chat:', sessionTitle(key));
        if (title !== null) await mutateSession('rename', key, title);
      } else if (kind === 'delete') {
        if (window.confirm('Excluir esta sessão permanentemente?')) await mutateSession('delete', key, true);
      } else {
        await mutateSession(kind, key, sessionAction.dataset.value === 'true');
      }
      return;
    }

    const skill = target.closest('[data-skill-name]');
    if (skill) { event.preventDefault(); await showSkill(skill.dataset.skillName); return; }

    const searchResult = target.closest('[data-search-session-key]');
    if (searchResult) {
      const session = state?.sessions?.find(s => s.key === searchResult.dataset.searchSessionKey);
      if (session?.session_id) await openSession(session.key, session.session_id);
    }
  });

  document.addEventListener('input', event => {
    if (event.target?.id !== 'session-search') return;
    clearTimeout(searchTimer);
    searchTimer = setTimeout(() => runSearch(event.target.value), 180);
  });

  window.addEventListener('haosbot:turn-complete', refreshState);
  window.addEventListener('haosbot:session-changed', refreshState);
  window.addEventListener('DOMContentLoaded', () => {
    const saved = localStorage.getItem('haosbot-theme');
    const preferred = saved || (window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light');
    applyTheme(preferred);
    refreshState();
  });
})();