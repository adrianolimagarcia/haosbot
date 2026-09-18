'use strict';

/*
 * UI plumbing for the Haosbot control center.
 *
 * This file is served by the application itself (see internal/api/webui.go) and
 * is loaded before /webui-hardening.js, which owns the security-sensitive data
 * paths: chat turns, sessions, config and model discovery.
 *
 * Every interactive element is wired through a `data-action` attribute handled
 * by the delegated listeners below instead of an inline `onclick` attribute.
 * The Content-Security-Policy sent with this page is `script-src 'self'` with
 * no 'unsafe-inline', and browsers block inline event handlers under such a
 * policy, so inline handlers would simply stop working.
 */

(() => {
  const byId = (id) => document.getElementById(id);

  // Security-sensitive behaviour lives in the hardening layer, which is loaded
  // after this file. Resolve it at call time so the override always wins.
  function call(name, ...args) {
    const fn = window[name];
    if (typeof fn !== 'function') {
      console.error(`haosbot: ${name}() is unavailable`);
      return undefined;
    }
    return fn(...args);
  }

  function switchTab(tab) {
    const panel = byId('sidebar-skills-panel');
    if (panel && tab === 'skills') panel.classList.toggle('hidden');
  }

  function toggleSidebar() {
    const aside = document.querySelector('aside');
    if (aside) aside.classList.toggle('hidden');
  }

  function focusSearch() {
    const filter = window.prompt('Filtrar comandos ou mensagens:');
    if (filter) byId('user-input').value = filter;
  }

  function onModelSelectChange(value) {
    if (value) byId('dlg-llm-model').value = value;
  }

  function closeSettings() {
    byId('settings-dialog').classList.add('hidden');
  }

  async function restartAgent() {
    if (!window.confirm('Deseja realmente reiniciar o serviço haosbot agora?')) return;
    const status = byId('dlg-status');
    status.textContent = 'Reiniciando...';
    status.className = 'text-xs text-amber-600 font-medium';
    const token = typeof window.getToken === 'function' ? window.getToken() : '';
    const headers = token ? { Authorization: `Bearer ${token}` } : {};
    try {
      await fetch('/api/restart', { method: 'POST', headers });
      status.textContent = 'Agente reiniciado! Reconectando...';
      setTimeout(() => window.location.reload(), 2000);
    } catch (err) {
      status.textContent = 'Agente reiniciando...';
      setTimeout(() => window.location.reload(), 2500);
    }
  }

  const actions = {
    'new-session': () => call('newSession'),
    'focus-search': () => focusSearch(),
    'switch-tab': (el) => switchTab(el.dataset.tab),
    'open-settings': () => call('openSettings'),
    'toggle-sidebar': () => toggleSidebar(),
    'close-settings': () => closeSettings(),
    'fetch-models': () => call('fetchRemoteModels'),
    'restart-agent': () => restartAgent(),
    'save-settings': () => call('saveDialogSettings'),
    send: () => call('handleSend'),
    stop: () => call('stopTurn'),
  };

  function closestAction(event, selector) {
    const target = event.target;
    return target instanceof Element ? target.closest(selector) : null;
  }

  document.addEventListener('click', (event) => {
    const el = closestAction(event, '[data-action]');
    if (!el) return;
    const action = actions[el.dataset.action];
    if (!action) return;
    event.preventDefault();
    action(el);
  });

  document.addEventListener('change', (event) => {
    const el = closestAction(event, '[data-action="model-select-change"]');
    if (el) onModelSelectChange(el.value);
  });

  const composer = document.querySelector('[data-role="composer"]');
  if (composer) {
    composer.addEventListener('keydown', (event) => {
      if (event.key === 'Enter' && !event.shiftKey) {
        event.preventDefault();
        call('handleSend');
      }
    });
  }
})();
