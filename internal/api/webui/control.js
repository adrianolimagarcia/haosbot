(() => {
  'use strict';

  let state = null;
  let showArchived = false;
  let activeKey = null;
  let activeView = null;
  // True while an editor form owns the panel. refreshState() re-renders the
  // active view (on every turn completion and on the refresh button), which
  // used to replace a draft the user was still filling in with the list view.
  let editorMounted = false;
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
    const text = await res.text();
    if (!text || !text.trim()) return null;
    return JSON.parse(text);
  }

  function errorMessage(err) {
    return err && err.message ? err.message : String(err);
  }

  // Failures used to surface only as unhandled promise rejections in the
  // console, leaving the panel looking like the click did nothing at all.
  function reportError(message) {
    console.warn('haosbot:', message);
    const stream = byId('chat-stream');
    if (!stream) return;
    stream.appendChild(el('div', 'session-loading', message));
    const container = byId('messages-container');
    if (container) container.scrollTop = container.scrollHeight;
  }

  async function refreshState() {
    try {
      state = await api('/api/webui/state');
      renderSessions();
      if (activeView && !editorMounted) renderView(activeView);
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
    editorMounted = true;
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
      // Only webui:* sessions can be opened here; the server marks the others
      // with selectable=false and ships no session_id. Rendering them as live
      // buttons made a click look broken instead of unavailable.
      if (!session.selectable || !session.session_id) {
        open.disabled = true;
        open.classList.add('session-open-disabled');
        open.title = 'Sessão de canal externo — não pode ser aberta no WebUI';
      }
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
    try {
      const data = await api('/api/webui/session?key=' + encodeURIComponent(key));
      activeKey = key;
      if (typeof window.activateWebSession === 'function') {
        await window.activateWebSession(sessionID, data.messages || [], sessionTitle(key));
      }
      closeView();
      renderSessions();
    } catch (err) {
      reportError('Falha ao abrir a sessão: ' + errorMessage(err));
    }
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
    editorMounted = false;
    const panel = byId('workspace-panel');
    if (panel) panel.classList.add('hidden');
  }

  function setPanelTitle(title, kicker = 'HAOSBOT') {
    if (byId('workspace-panel-title')) byId('workspace-panel-title').textContent = title;
    if (byId('workspace-panel-kicker')) byId('workspace-panel-kicker').textContent = kicker;
  }

  function renderView(view) {
    editorMounted = false;
    const root = byId('workspace-panel-content');
    if (!root) return;
    root.replaceChildren();
    if (!state) {
      root.appendChild(el('div', 'session-loading', 'Carregando…'));
      return;
    }
    if (view === 'apps') return renderApps(root);
    if (view === 'skills') return renderSkills(root);
    if (view === 'marketplace') return renderMarketplace(root);
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
    editorMounted = false;
    setPanelTitle('Skills', 'Capabilities');
    const toolbar = el('div', 'control-toolbar');
    const explore = el('button', 'control-button primary', 'Explorar Mercado de Skills');
    explore.type = 'button';
    explore.addEventListener('click', () => openView('marketplace'));
    const create = el('button', 'control-button', 'Nova skill local');
    create.type = 'button';
    create.addEventListener('click', () => editSkill('', '---\nname: new-skill\ndescription: Describe this skill.\n---\n\n# New Skill\n'));
    toolbar.append(explore, create);
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

  let marketplaceProvider = 'all';
  let marketplaceQuery = '';
  let marketplaceTimer = null;

  async function renderMarketplace(root) {
    editorMounted = false;
    setPanelTitle('Mercado de Skills & Plugins', 'Catálogo');

    const topBar = el('div', 'control-toolbar');
    const input = el('input', 'control-input');
    input.type = 'search';
    input.placeholder = 'Buscar skills e plugins (ex: image, git, web, research)…';
    input.style.flex = '1';
    input.value = marketplaceQuery;
    topBar.appendChild(input);
    root.appendChild(topBar);

    const providerBar = el('div', 'control-toolbar');
    const providers = [
      { id: 'all', label: 'Todos' },
      { id: 'skillhub', label: 'SkillHub' },
      { id: 'skills_sh', label: 'skills.sh' },
      { id: 'cliapps', label: 'CLI Apps' }
    ];
    for (const p of providers) {
      const btn = el('button', 'control-button' + (marketplaceProvider === p.id ? ' active' : ''), p.label);
      btn.type = 'button';
      btn.addEventListener('click', () => {
        marketplaceProvider = p.id;
        renderMarketplace(root);
      });
      providerBar.appendChild(btn);
    }
    root.appendChild(providerBar);

    const status = el('div', 'control-muted', 'Carregando catálogo…');
    status.style.marginBottom = '12px';
    const grid = el('div', 'control-grid');
    grid.style.gridTemplateColumns = 'repeat(auto-fill, minmax(280px, 1fr))';
    root.append(status, grid);

    // Responses are matched against the request that produced them: a slow
    // search must not append its cards on top of a newer render.
    let loadSeq = 0;
    async function loadItems() {
      const seq = ++loadSeq;
      const query = marketplaceQuery.trim();
      const provider = marketplaceProvider;
      status.textContent = 'Carregando catálogo…';
      try {
        let endpoint = '/api/webui/skills/marketplace/trending?provider=' + encodeURIComponent(provider);
        if (query.length >= 2) {
          endpoint = '/api/webui/skills/marketplace/search?q=' + encodeURIComponent(query) + '&provider=' + encodeURIComponent(provider);
        }
        const data = await api(endpoint);
        if (seq !== loadSeq) return;
        const skills = data?.skills || [];
        const installEnabled = data?.install_supported !== false;
        status.textContent = query.length >= 2
          ? `${skills.length} resultado(s) para "${query}"`
          : `Trending em destaque (${skills.length} disponíveis)`;
        if (!installEnabled) {
          status.textContent += ' — instalação remota desabilitada (tools.webuiAllowRemotePackageInstall)';
        }
        grid.replaceChildren();

        if (!skills.length) {
          grid.appendChild(el('div', 'control-muted', 'Nenhuma skill encontrada para este filtro.'));
          return;
        }

        for (const item of skills) {
          const card = el('div', 'control-card');
          card.style.display = 'flex';
          card.style.flexDirection = 'column';
          card.style.justifyContent = 'space-between';

          const head = el('div');
          const titleRow = el('div', '');
          titleRow.style.display = 'flex';
          titleRow.style.justifyContent = 'space-between';
          titleRow.style.alignItems = 'flex-start';
          titleRow.style.gap = '8px';
          titleRow.append(
            el('h3', '', item.name || item.skill_id),
            el('span', 'control-badge', item.provider || 'skill')
          );
          const desc = el('p', '', item.description || 'Sem descrição.');
          desc.style.marginTop = '6px';
          head.append(titleRow, desc);

          const footer = el('div');
          footer.style.marginTop = '14px';
          footer.style.display = 'flex';
          footer.style.justifyContent = 'space-between';
          footer.style.alignItems = 'center';

          const meta = el('div', 'control-muted');
          if (item.installs > 0) meta.textContent = `${item.installs.toLocaleString()} instalações`;
          else if (item.source) meta.textContent = item.source;

          const actionBtn = el('button', 'control-button' + (item.installed ? ' danger' : ' primary'));
          actionBtn.type = 'button';
          const itemInstallable = item.install_supported !== false;
          actionBtn.textContent = item.installed ? 'Desinstalar' : (itemInstallable ? 'Instalar' : 'Somente catálogo');
          if (!itemInstallable && !item.installed) {
            actionBtn.disabled = true;
            actionBtn.title = 'Este provedor é somente leitura';
          } else if (!installEnabled) {
            actionBtn.disabled = true;
            actionBtn.title = 'Instalação remota desabilitada pelo operador';
          }

          actionBtn.addEventListener('click', async () => {
            if (item.installed) {
              if (!window.confirm(`Desinstalar a skill ${item.name || item.skill_id}?`)) return;
              actionBtn.disabled = true;
              actionBtn.textContent = 'Removendo…';
              try {
                await api(`/api/webui/skills/marketplace/install?name=${encodeURIComponent(item.skill_id)}&provider=${encodeURIComponent(item.provider)}`, { method: 'DELETE' });
                item.installed = false;
                await refreshState();
                loadItems();
              } catch (err) {
                window.alert('Falha ao desinstalar: ' + err.message);
                actionBtn.disabled = false;
                actionBtn.textContent = 'Desinstalar';
              }
            } else {
              actionBtn.disabled = true;
              actionBtn.textContent = 'Instalando…';
              try {
                await api('/api/webui/skills/marketplace/install', {
                  method: 'POST',
                  headers: { 'Content-Type': 'application/json' },
                  body: JSON.stringify({
                    skill_id: item.skill_id,
                    provider: item.provider,
                    source: item.source,
                    version: item.version
                  })
                });
                item.installed = true;
                await refreshState();
                loadItems();
              } catch (err) {
                window.alert('Falha ao instalar: ' + err.message);
                actionBtn.disabled = false;
                actionBtn.textContent = 'Instalar';
              }
            }
          });

          footer.append(meta, actionBtn);
          card.append(head, footer);
          grid.appendChild(card);
        }
      } catch (err) {
        if (seq !== loadSeq) return;
        status.textContent = 'Erro ao carregar catálogo: ' + err.message;
      }
    }

    input.addEventListener('input', () => {
      clearTimeout(marketplaceTimer);
      marketplaceTimer = setTimeout(() => {
        marketplaceQuery = input.value;
        loadItems();
      }, 350);
    });

    loadItems();
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
    editorMounted = true;
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

  function automationSessionKey() {
    if (activeKey) return activeKey;
    const id = sessionStorage.getItem('haosbot_session_id');
    return id ? 'webui:' + id : '';
  }

  function scheduleLabel(schedule) {
    if (!schedule) return 'sem agenda';
    if (schedule.kind === 'every') {
      const ms = Number(schedule.everyMs || 0);
      if (ms % 3600000 === 0) return 'a cada ' + (ms / 3600000) + 'h';
      if (ms % 60000 === 0) return 'a cada ' + (ms / 60000) + 'm';
      if (ms % 1000 === 0) return 'a cada ' + (ms / 1000) + 's';
      return 'a cada ' + ms + 'ms';
    }
    if (schedule.kind === 'cron') return 'cron ' + schedule.expr + (schedule.tz ? ' · ' + schedule.tz : '');
    if (schedule.kind === 'at' && schedule.atMs) return 'uma vez · ' + new Date(schedule.atMs).toLocaleString();
    return schedule.kind || 'agenda';
  }

  function datetimeLocal(ms) {
    if (!ms) return '';
    const d = new Date(Number(ms));
    const pad = n => String(n).padStart(2, '0');
    return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) +
      'T' + pad(d.getHours()) + ':' + pad(d.getMinutes());
  }

  function automationEditor(root, job = null) {
    root.replaceChildren();
    editorMounted = true;
    setPanelTitle(job ? 'Editar automação' : 'Nova automação', 'Scheduler');

    const form = el('div', 'automation-editor');
    const name = el('input', 'control-input');
    name.placeholder = 'Nome';
    name.value = job?.name || '';

    const message = el('textarea', 'memory-editor');
    message.placeholder = 'Instrução que o HAOSBOT deve executar';
    message.value = job?.payload?.message || '';

    const session = el('input', 'control-input');
    session.placeholder = 'Sessão vinculada';
    session.value = job?.payload?.sessionKey || automationSessionKey();
    // PATCH /api/webui/automation does not accept session_key, so an edit here
    // would be silently dropped. Freeze it instead of pretending it is editable.
    if (job) {
      session.disabled = true;
      session.title = 'A sessão vinculada não pode ser alterada depois de criada';
    }

    const kind = el('select', 'control-input');
    for (const value of ['every', 'cron', 'at']) {
      const option = el('option', '', value === 'every' ? 'Intervalo' : value === 'cron' ? 'Cron' : 'Uma vez');
      option.value = value;
      kind.appendChild(option);
    }
    kind.value = job?.schedule?.kind || 'every';

    const every = el('input', 'control-input');
    every.type = 'number';
    every.min = '1';
    every.placeholder = 'Intervalo em segundos';
    every.value = job?.schedule?.everyMs ? String(Math.max(1, Math.round(job.schedule.everyMs / 1000))) : '3600';

    const expr = el('input', 'control-input');
    expr.placeholder = '0 9 * * 1-5';
    expr.value = job?.schedule?.expr || '';

    const timezone = el('input', 'control-input');
    timezone.placeholder = 'America/Sao_Paulo';
    timezone.value = job?.schedule?.tz || state?.config?.agents?.defaults?.timezone || 'UTC';

    const at = el('input', 'control-input');
    at.type = 'datetime-local';
    at.value = datetimeLocal(job?.schedule?.atMs);

    const deleteAfter = el('label', 'automation-check');
    const deleteCheckbox = document.createElement('input');
    deleteCheckbox.type = 'checkbox';
    deleteCheckbox.checked = job?.deleteAfterRun ?? (kind.value === 'at');
    deleteAfter.append(deleteCheckbox, document.createTextNode(' Excluir após executar'));

    const scheduleFields = el('div', 'automation-schedule-fields');
    const refreshScheduleFields = () => {
      scheduleFields.replaceChildren();
      if (kind.value === 'every') scheduleFields.append(every);
      if (kind.value === 'cron') scheduleFields.append(expr, timezone);
      if (kind.value === 'at') scheduleFields.append(at, deleteAfter);
    };
    kind.addEventListener('change', refreshScheduleFields);
    refreshScheduleFields();

    const toolbar = el('div', 'control-toolbar');
    const save = el('button', 'control-button primary', job ? 'Salvar' : 'Criar');
    save.type = 'button';
    const cancel = el('button', 'control-button', 'Cancelar');
    cancel.type = 'button';
    const status = el('span', 'control-muted', '');
    cancel.addEventListener('click', () => renderView('automations'));

    save.addEventListener('click', async () => {
      const schedule = { kind: kind.value };
      if (kind.value === 'every') {
        const seconds = Number(every.value);
        if (!Number.isFinite(seconds) || seconds <= 0) { status.textContent = 'Intervalo inválido'; return; }
        schedule.everyMs = Math.round(seconds * 1000);
      } else if (kind.value === 'cron') {
        schedule.expr = expr.value.trim();
        schedule.tz = timezone.value.trim() || 'UTC';
      } else {
        const ms = Date.parse(at.value);
        if (!Number.isFinite(ms)) { status.textContent = 'Data/hora inválida'; return; }
        schedule.atMs = ms;
      }

      const sessionKey = session.value.trim();
      if (!sessionKey || !sessionKey.includes(':')) { status.textContent = 'Sessão vinculada inválida'; return; }
      if (!name.value.trim() || !message.value.trim()) { status.textContent = 'Nome e instrução são obrigatórios'; return; }

      try {
        status.textContent = 'Salvando…';
        if (job) {
          await api('/api/webui/automation?id=' + encodeURIComponent(job.id), {
            method: 'PATCH',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({
              name: name.value.trim(),
              message: message.value.trim(),
              schedule,
              delete_after_run: kind.value === 'at' ? deleteCheckbox.checked : false
            })
          });
        } else {
          await api('/api/webui/automations', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({
              name: name.value.trim(),
              message: message.value.trim(),
              session_key: sessionKey,
              schedule,
              delete_after_run: kind.value === 'at' ? deleteCheckbox.checked : false
            })
          });
        }
        await renderAutomations(root);
      } catch (err) {
        status.textContent = err.message;
      }
    });

    toolbar.append(save, cancel, status);
    form.append(
      el('div', 'control-section-title', 'Identidade'),
      name,
      message,
      el('div', 'control-section-title', 'Sessão vinculada'),
      session,
      el('div', 'control-section-title', 'Agenda'),
      kind,
      scheduleFields,
      toolbar
    );
    root.appendChild(form);
  }

  async function showAutomationRun(root, job, run) {
    setPanelTitle(job.name, 'Run detail');
    root.replaceChildren(el('div', 'session-loading', 'Carregando execução…'));
    try {
      const detail = await api('/api/webui/automation/run?run_id=' + encodeURIComponent(run.runId));
      root.replaceChildren();
      const toolbar = el('div', 'control-toolbar');
      const back = el('button', 'control-button', '← Automação');
      back.type = 'button';
      back.addEventListener('click', () => showAutomation(root, job.id));
      toolbar.append(back, el('span', 'control-badge', detail.status || 'unknown'));
      root.append(
        toolbar,
        card('Execução', new Date(Number(detail.created_at_ms || 0)).toLocaleString(), (detail.duration_ms || 0) + ' ms'),
        el('div', 'control-section-title', detail.error ? 'Erro' : 'Resposta'),
        el('pre', 'control-pre', detail.error || detail.response || '(sem resposta)')
      );
    } catch (err) {
      root.replaceChildren(el('div', 'session-loading', err.message));
    }
  }

  async function showAutomation(root, id) {
    root.replaceChildren(el('div', 'session-loading', 'Carregando automação…'));
    try {
      const job = await api('/api/webui/automation?id=' + encodeURIComponent(id));
      setPanelTitle(job.name, 'Automation');
      root.replaceChildren();

      const toolbar = el('div', 'control-toolbar');
      const back = el('button', 'control-button', '← Automações');
      const run = el('button', 'control-button primary', job.state?.pending ? 'Executando…' : 'Run now');
      const toggle = el('button', 'control-button', job.enabled ? 'Pausar' : 'Retomar');
      const edit = el('button', 'control-button', 'Editar');
      const remove = el('button', 'control-button danger', 'Excluir');
      back.type = run.type = toggle.type = edit.type = remove.type = 'button';
      run.disabled = Boolean(job.state?.pending);
      back.addEventListener('click', () => renderView('automations'));
      run.addEventListener('click', async () => {
        try {
          run.disabled = true; run.textContent = 'Enfileirado…';
          await api('/api/webui/automation/run?id=' + encodeURIComponent(job.id), { method: 'POST' });
          setTimeout(() => showAutomation(root, job.id), 700);
        } catch (err) { run.disabled = false; run.textContent = err.message; }
      });
      toggle.addEventListener('click', async () => {
        await api('/api/webui/automation?id=' + encodeURIComponent(job.id), {
          method: 'PATCH', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ enabled: !job.enabled })
        });
        showAutomation(root, job.id);
      });
      edit.addEventListener('click', () => automationEditor(root, job));
      remove.addEventListener('click', async () => {
        if (!window.confirm('Excluir a automação ' + job.name + '?')) return;
        await api('/api/webui/automation?id=' + encodeURIComponent(job.id), { method: 'DELETE' });
        renderView('automations');
      });
      toolbar.append(back, run, toggle, edit, remove);

      const status = job.state?.pending ? 'executando' : job.enabled ? 'ativa' : 'pausada';
      const grid = el('div', 'control-grid');
      grid.append(
        card('Status', scheduleLabel(job.schedule), status),
        card('Sessão', job.payload?.sessionKey || '', job.payload?.originChannel || ''),
        card('Última execução', job.state?.lastRunAtMs ? new Date(job.state.lastRunAtMs).toLocaleString() : 'Nunca', job.state?.lastStatus || '—')
      );
      root.append(toolbar, grid, el('div', 'control-section-title', 'Instrução'), el('pre', 'control-pre', job.payload?.message || ''));

      root.appendChild(el('div', 'control-section-title', 'Histórico'));
      const history = el('div', 'control-list');
      const runs = Array.isArray(job.state?.runHistory) ? [...job.state.runHistory].reverse() : [];
      for (const item of runs) {
        const row = el('button', 'control-list-row automation-run-row');
        row.type = 'button';
        row.append(
          el('div', '', new Date(Number(item.runAtMs || 0)).toLocaleString()),
          el('div', 'control-muted', (item.status || 'unknown') + ' · ' + (item.durationMs || 0) + ' ms')
        );
        if (item.runId) row.addEventListener('click', () => showAutomationRun(root, job, item));
        else row.disabled = true;
        history.appendChild(row);
      }
      if (!runs.length) history.appendChild(el('div', 'session-loading', 'Nenhuma execução ainda.'));
      root.appendChild(history);
    } catch (err) {
      root.replaceChildren(el('div', 'session-loading', err.message));
    }
  }

  async function renderAutomations(root) {
    editorMounted = false;
    setPanelTitle('Automações', 'Scheduler');
    root.replaceChildren(el('div', 'session-loading', 'Carregando automações…'));
    try {
      const payload = await api('/api/webui/automations');
      root.replaceChildren();

      const toolbar = el('div', 'control-toolbar');
      const create = el('button', 'control-button primary', 'Nova automação');
      const refresh = el('button', 'control-button', 'Atualizar');
      create.type = refresh.type = 'button';
      create.addEventListener('click', () => automationEditor(root));
      refresh.addEventListener('click', () => renderAutomations(root));
      toolbar.append(create, refresh, el('span', 'control-badge', payload.running ? 'scheduler online' : 'scheduler parado'));
      root.appendChild(toolbar);

      const jobs = Array.isArray(payload.jobs) ? payload.jobs : [];
      const grid = el('div', 'control-grid');
      grid.append(
        card('Ativas', 'Automações habilitadas', jobs.filter(j => j.enabled).length),
        card('Executando', 'Runs em andamento', jobs.filter(j => j.state?.pending).length),
        card('Total', 'Jobs persistidos', jobs.length)
      );
      root.appendChild(grid);

      const list = el('div', 'control-list');
      for (const job of jobs) {
        const button = el('button', 'control-list-row automation-job-row');
        button.type = 'button';
        const left = el('div');
        left.append(el('div', '', job.name || job.id), el('div', 'control-muted', scheduleLabel(job.schedule)));
        const right = el('div', 'automation-job-status', job.state?.pending ? 'executando' : job.enabled ? 'ativa' : 'pausada');
        button.append(left, right);
        button.addEventListener('click', () => showAutomation(root, job.id));
        list.appendChild(button);
      }
      if (!jobs.length) list.appendChild(el('div', 'session-loading', 'Nenhuma automação criada.'));
      root.appendChild(list);
    } catch (err) {
      root.replaceChildren(el('div', 'session-loading', err.message));
    }
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
    try {
      await handleClick(event);
    } catch (err) {
      reportError('Falha na ação: ' + errorMessage(err));
    }
  });

  async function handleClick(event) {
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
      if (session?.session_id) {
        await openSession(session.key, session.session_id);
      } else {
        reportError('Sessão de canal externo — não pode ser aberta no WebUI.');
      }
    }
  }

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