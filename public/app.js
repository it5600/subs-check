/**
 * Sub-Store 管理面板前端 — Go Edition
 * Industrial Dashboard 风格 · 暗色主题 + 响应式布局
 *
 * 模块结构：
 *   App.api       — API 客户端（Cookie Session 鉴权、CSRF Token 管理、错误重试）
 *   App.state     — 全局状态管理
 *   App.toast     — 通知系统
 *   App.utils     — 工具函数
 *   App.dashboard — 仪表盘
 *   App.nodes     — 节点管理（分页、搜索、筛选）
 *   App.sources   — 订阅源管理
 *   App.substore  — Sub-Store 数据
 *   App.logs      — 日志查看（自动刷新）
 *   App.settings  — 设置
 *   App.nav       — 导航路由
 */
(function () {
  'use strict';

  // ===== 全局状态 =====
  const state = {
    currentNodes: [],
    nodePage: 1,
    nodePageSize: 100,
    nodeTotal: 0,
    nodeTotalPages: 1,
    lastSearch: '',
    csrfToken: null,
    loggedIn: false,
    logAutoRefresh: false,
    logRefreshTimer: null,
  };

  // ===== 工具函数 =====
  const utils = {
    escapeHtml(s) {
      if (s == null) return '';
      return String(s).replace(/[&<>"']/g, c => ({
        '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'
      }[c]));
    },

    formatTime(ts) {
      if (!ts) return '';
      const d = new Date(ts);
      return d.toLocaleString('zh-CN');
    },

    formatUptime(seconds) {
      const d = Math.floor(seconds / 86400);
      const h = Math.floor((seconds % 86400) / 3600);
      const m = Math.floor((seconds % 3600) / 60);
      if (d > 0) return `${d}天${h}小时`;
      if (h > 0) return `${h}小时${m}分钟`;
      return `${m}分钟`;
    },

    debounce(fn, delay) {
      let timer;
      return (...args) => {
        clearTimeout(timer);
        timer = setTimeout(() => fn(...args), delay);
      };
    },

    /** 渲染骨架屏 HTML */
    skeletonRows(count, cols) {
      const rows = Array.from({ length: count }, () =>
        `<tr>${Array.from({ length: cols }, () => '<td><div class="skeleton" style="width:60%;"></div></td>').join('')}</tr>`
      ).join('');
      return `<div class="table-wrap"><table><tbody>${rows}</tbody></table></div>`;
    },

    /** 渲染错误状态 + 重试按钮 */
    errorState(msg, retryFn) {
      return `
        <div class="error-state">
          <div class="icon">⚠️</div>
          <div class="msg">${utils.escapeHtml(msg)}</div>
          <button class="btn btn-ghost btn-sm" onclick="App.${retryFn || 'loadDashboard'}()">🔄 重试</button>
        </div>
      `;
    },
  };

  // ===== 通知系统 =====
  const toast = {
    show(msg, type = 'success', duration = 3000) {
      const el = document.getElementById('toast');
      if (!el) return;
      el.className = `toast toast-${type} show`;
      el.textContent = msg;
      setTimeout(() => el.classList.remove('show'), duration);
    },
    success(msg) { this.show(msg, 'success'); },
    error(msg) { this.show(msg, 'error', 5000); },
  };

  // ===== API 客户端 =====
  const api = {
    /**
     * 获取 CSRF Token
     */
    async getCsrfToken() {
      const res = await fetch('/api/csrf-token');
      const json = await res.json();
      if (json.code !== 0) throw new Error(json.message);
      state.csrfToken = json.data.csrfToken;
      return state.csrfToken;
    },

    /**
     * 登录 — 提交 API Key，服务端设置 Cookie Session
     */
    async login(apiKey) {
      await this.getCsrfToken();
      const res = await fetch('/api/login', {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'X-CSRF-Token': state.csrfToken,
        },
        credentials: 'same-origin',
        body: JSON.stringify({ apiKey }),
      });
      const json = await res.json();
      if (json.code !== 0) throw new Error(json.message);
      state.loggedIn = true;
      await this.getCsrfToken();
      return json.data;
    },

    /**
     * 登出
     */
    async logout() {
      try {
        const token = state.csrfToken || await this.getCsrfToken();
        await fetch('/api/logout', {
          method: 'POST',
          headers: { 'X-CSRF-Token': token },
          credentials: 'same-origin',
        });
      } catch { /* 忽略 */ }
      state.loggedIn = false;
      state.csrfToken = null;
      showLoginScreen();
    },

    /**
     * 统一请求方法
     */
    async request(path, method = 'GET', body = null, retry = true) {
      const headers = { 'Content-Type': 'application/json' };
      if (method === 'POST' || method === 'DELETE') {
        if (!state.csrfToken) await this.getCsrfToken();
        headers['X-CSRF-Token'] = state.csrfToken;
      }

      const opts = { method, headers, credentials: 'same-origin' };
      if (body) opts.body = JSON.stringify(body);

      const res = await fetch(path, opts);
      const json = await res.json();

      if (json.code === 403003 && retry) {
        await this.getCsrfToken();
        return this.request(path, method, body, false);
      }

      if (json.code === 401001) {
        state.loggedIn = false;
        showLoginScreen();
        throw new Error('未授权，请重新登录');
      }

      if (json.code !== 0) {
        throw new Error(json.message || '请求失败');
      }
      return json.data;
    },

    get(path) { return this.request(path, 'GET'); },
    post(path, body) { return this.request(path, 'POST', body); },
    del(path) { return this.request(path, 'DELETE'); },
  };

  // ===== 登录/认证界面 =====
  let originalPanelHTML = null;

  function showLoginScreen() {
    if (document.getElementById('login-form')) return;
    state.loggedIn = false;
    const mainDiv = document.querySelector('.main');
    if (!originalPanelHTML) originalPanelHTML = mainDiv.innerHTML;

    mainDiv.innerHTML = `
      <div style="display: flex; align-items: center; justify-content: center; min-height: 80vh;">
        <div class="login-card">
          <div class="logo">⚡ Sub<span>Store</span></div>
          <div class="subtitle">Go Edition · 管理面板</div>
          <form id="login-form" onsubmit="App.handleLogin(event)">
            <div class="mb-4">
              <label class="block text-sm text-muted mb-2">API Key</label>
              <input type="password" id="login-api-key" placeholder="输入 API Key" autocomplete="current-password" required>
            </div>
            <button type="submit" class="btn btn-primary w-full" id="login-btn">登录</button>
          </form>
        </div>
      </div>
    `;
    document.getElementById('login-api-key').focus();
  }

  async function handleLogin(event) {
    event.preventDefault();
    const keyInput = document.getElementById('login-api-key');
    const btn = document.getElementById('login-btn');
    const key = keyInput.value.trim();

    if (!key) {
      toast.error('请输入 API Key');
      return;
    }

    btn.disabled = true;
    btn.textContent = '登录中...';

    try {
      await api.login(key);
      toast.success('登录成功');
      restorePanel();
      loadDashboard();
    } catch (e) {
      toast.error('登录失败: ' + e.message);
      btn.disabled = false;
      btn.textContent = '登录';
    }
  }

  function restorePanel() {
    const mainDiv = document.querySelector('.main');
    if (originalPanelHTML) {
      mainDiv.innerHTML = originalPanelHTML;
    } else {
      mainDiv.innerHTML = '<section id="page-dashboard" class="page active"><div class="card"><h3>仪表盘</h3><div id="dashboard-stats"></div></div></section>';
    }
    setupNav();
  }

  // ===== 仪表盘 =====
  async function loadDashboard() {
    const statsEl = document.getElementById('dashboard-stats');
    if (!statsEl) return;
    statsEl.innerHTML = '<div class="loading-overlay"><div class="loading"></div><span>加载中...</span></div>';

    try {
      const [health, status, nodes] = await Promise.all([
        api.get('/api/health'),
        api.get('/api/status'),
        api.get('/api/nodes?pageSize=1'),
      ]);

      const pipeline = status.pipeline || {};
      const statCards = `
        <div class="stat-card" style="--accent-color: var(--color-success);">
          <div class="stat-card-row">
            <span class="icon">🟢</span>
            <div>
              <div class="num">${status.available || 0}</div>
              <div class="label">存活节点</div>
            </div>
          </div>
        </div>
        <div class="stat-card" style="--accent-color: var(--color-accent);">
          <div class="stat-card-row">
            <span class="icon">⚡</span>
            <div>
              <div class="num">${pipeline.speedPass || 0}</div>
              <div class="label">测速通过</div>
            </div>
          </div>
        </div>
        <div class="stat-card" style="--accent-color: var(--color-warning);">
          <div class="stat-card-row">
            <span class="icon">🔗</span>
            <div>
              <div class="num">${status.proxyCount || 0}</div>
              <div class="label">代理总数</div>
            </div>
          </div>
        </div>
        <div class="stat-card" style="--accent-color: ${status.checking ? 'var(--color-warning)' : 'var(--color-success)'};">
          <div class="stat-card-row">
            <span class="icon">${status.checking ? '🟡' : '🟢'}</span>
            <div>
              <div class="num" style="font-size: 1.2rem;">${status.checking ? '检测中' : '空闲'}</div>
              <div class="label">运行状态</div>
            </div>
          </div>
        </div>
      `;

      statsEl.innerHTML = statCards;

      // 更新服务状态指示器
      const statusDot = document.getElementById('service-status-dot');
      const statusText = document.getElementById('service-status-text');
      if (statusDot && statusText) {
        if (status.available != null && status.available > 0) {
          statusDot.className = 'status-dot online';
          statusText.textContent = `${status.available} 节点在线`;
        } else {
          statusDot.className = 'status-dot offline';
          statusText.textContent = '服务异常';
        }
      }

      // 节点概览
      const byType = nodes.byType || {};
      const typeBadges = Object.entries(byType).slice(0, 8).map(([t, c]) =>
        `<span class="badge badge-gray">${utils.escapeHtml(t)}: ${c}</span>`
      ).join(' ');
      document.getElementById('node-overview').innerHTML = `
        <div class="stat-card" style="margin-bottom: 0.8rem; --accent-color: var(--color-accent);">
          <div class="stat-card-row">
            <span class="icon">🌐</span>
            <div>
              <div class="num">${nodes.total}</div>
              <div class="label">总节点数</div>
            </div>
          </div>
        </div>
        <div class="flex flex-wrap gap-2">${typeBadges || '<span class="text-dim text-sm">无数据</span>'}</div>
      `;

      // Sub-Store 信息
      try {
        const env = await api.get('/api/substore/api/utils/env');
        document.getElementById('substore-info').innerHTML = `
          <div class="stat-card" style="margin-bottom: 0.6rem; --accent-color: var(--color-accent);">
            <div class="stat-card-row">
              <span class="icon">📦</span>
              <div>
                <div class="num" style="font-size: 1.2rem;">${utils.escapeHtml(env.version || 'N/A')}</div>
                <div class="label">版本</div>
              </div>
            </div>
          </div>
          <div class="text-xs text-muted mt-2">Backend: ${utils.escapeHtml(env.backend || 'N/A')}</div>
          <div class="text-xs text-muted">Features: ${utils.escapeHtml(env.feature ? Object.keys(env.feature).join(', ') : '')}</div>
        `;
      } catch {
        document.getElementById('substore-info').innerHTML = '<div class="text-dim text-sm">Sub-Store 不可达</div>';
      }

      // 系统信息
      const sysStats = health.stats || {};
      document.getElementById('sys-info').innerHTML = `
        <div class="text-xs text-muted space-y-1">
          <div>🕐 运行时长: ${utils.formatUptime(health.uptime || 0)}</div>
          <div>📡 请求数: <span class="font-mono tabular">${sysStats.requests || 0}</span></div>
          <div>⚠️ 错误数: <span class="font-mono tabular text-danger">${sysStats.errors || 0}</span></div>
          <div>🚦 限流数: <span class="font-mono tabular text-warning">${sysStats.rateLimited || 0}</span></div>
        </div>
      `;
    } catch (e) {
      // 401 未授权：不显示错误状态，让 init() 显示登录页
      if (e.message && e.message.includes('未授权')) {
        throw e;
      }
      statsEl.innerHTML = utils.errorState('加载失败: ' + e.message, 'loadDashboard');
      // 更新状态指示器
      const statusDot = document.getElementById('service-status-dot');
      const statusText = document.getElementById('service-status-text');
      if (statusDot && statusText) {
        statusDot.className = 'status-dot offline';
        statusText.textContent = '连接失败';
      }
    }
  }

  // ===== 节点管理 =====
  async function loadNodes() {
    const tableEl = document.getElementById('nodes-table');
    if (!tableEl) return;
    tableEl.innerHTML = '<div class="loading-overlay"><div class="loading"></div></div>';

    try {
      const search = document.getElementById('node-search')?.value || '';
      const type = document.getElementById('node-filter-type')?.value || '';
      const params = new URLSearchParams({
        page: state.nodePage,
        pageSize: state.nodePageSize,
      });
      if (search) params.set('search', search);
      if (type) params.set('type', type);

      const data = await api.get('/api/nodes?' + params.toString());
      state.currentNodes = data.nodes || [];
      state.nodeTotal = data.total || 0;
      state.nodeTotalPages = data.totalPages || 1;

      // 更新总数显示
      const totalEl = document.getElementById('nodes-total-count');
      if (totalEl) totalEl.textContent = state.nodeTotal;

      // 更新类型筛选下拉
      const typeSel = document.getElementById('node-filter-type');
      if (typeSel) {
        const currentVal = typeSel.value;
        typeSel.innerHTML = '<option value="">全部类型</option>' +
          Object.entries(data.byType || {}).map(([t, c]) =>
            `<option value="${utils.escapeHtml(t)}">${utils.escapeHtml(t)} (${c})</option>`
          ).join('');
        typeSel.value = currentVal;
      }

      renderNodes();
      renderPagination();
    } catch (e) {
      tableEl.innerHTML = utils.errorState('加载失败: ' + e.message, 'loadNodes');
    }
  }

  function renderNodes() {
    const tableEl = document.getElementById('nodes-table');
    if (!tableEl) return;
    const search = (document.getElementById('node-search')?.value || '').toLowerCase();
    const typeFilter = document.getElementById('node-filter-type')?.value || '';
    let nodes = state.currentNodes;

    // 检测搜索变化 → 重置到第 1 页
    if (state.lastSearch !== search) {
      state.lastSearch = search;
      state.nodePage = 1;
      debouncedLoadNodes();
    }

    // 前端二次过滤
    if (search) nodes = nodes.filter(n => (n.name || '').toLowerCase().includes(search));
    if (typeFilter) nodes = nodes.filter(n => n.type === typeFilter);

    if (nodes.length === 0) {
      tableEl.innerHTML = '<div class="empty-state"><div class="icon">🔍</div><div class="title">暂无匹配节点</div><div class="desc">尝试调整搜索条件或筛选器</div></div>';
      return;
    }

    tableEl.innerHTML = `
      <div class="text-xs text-muted mb-2">共 ${state.nodeTotal} 个节点，当前显示 ${nodes.length} 个</div>
      <div class="table-wrap">
        <table>
          <thead><tr><th>名称</th><th>类型</th><th>服务器</th><th>端口</th><th>TLS</th><th>网络</th></tr></thead>
          <tbody>
            ${nodes.map(n => `
              <tr>
                <td class="font-medium">${utils.escapeHtml(n.name)}</td>
                <td><span class="badge badge-gray">${utils.escapeHtml(n.type)}</span></td>
                <td class="text-xs text-muted">${utils.escapeHtml(n.server)}</td>
                <td class="text-xs font-mono tabular">${n.port}</td>
                <td>${n.tls ? '<span class="badge badge-green">TLS</span>' : '<span class="badge badge-gray">明文</span>'}</td>
                <td><span class="badge badge-blue">${utils.escapeHtml(n.network || 'tcp')}</span></td>
              </tr>
            `).join('')}
          </tbody>
        </table>
      </div>
    `;
  }

  const debouncedRenderNodes = utils.debounce(renderNodes, 300);
  const debouncedLoadNodes = utils.debounce(loadNodes, 400);

  function renderPagination() {
    const el = document.getElementById('nodes-pagination');
    if (!el) return;
    if (state.nodeTotalPages <= 1) {
      el.innerHTML = '';
      return;
    }

    el.innerHTML = `
      <div class="pagination">
        <button ${state.nodePage <= 1 ? 'disabled' : ''} onclick="App.goToNodePage(${state.nodePage - 1})">上一页</button>
        <span class="page-info">第 ${state.nodePage} / ${state.nodeTotalPages} 页（共 ${state.nodeTotal} 个）</span>
        <button ${state.nodePage >= state.nodeTotalPages ? 'disabled' : ''} onclick="App.goToNodePage(${state.nodePage + 1})">下一页</button>
      </div>
    `;
  }

  async function goToNodePage(page) {
    state.nodePage = page;
    await loadNodes();
  }

  // ===== 订阅源管理 =====
  async function loadSources() {
    const el = document.getElementById('sources-list');
    if (!el) return;
    el.innerHTML = '<div class="loading-overlay"><div class="loading"></div></div>';

    try {
      const data = await api.get('/api/sources');
      if (data.sources.length === 0) {
        el.innerHTML = '<div class="empty-state"><div class="icon">🔗</div><div class="title">暂无订阅源</div><div class="desc">在上方添加第一个订阅链接</div></div>';
        return;
      }
      el.innerHTML = `
        <div class="table-wrap">
          <table>
            <thead><tr><th>#</th><th>URL</th><th>状态</th><th>操作</th></tr></thead>
            <tbody>
              ${data.sources.map(s => `
                <tr>
                  <td class="font-mono tabular font-medium">${s.index + 1}</td>
                  <td class="text-xs text-muted">${utils.escapeHtml(s.url)}</td>
                  <td><span class="badge badge-green">启用</span></td>
                  <td><button class="btn btn-danger btn-sm" onclick="App.deleteSource(${s.index})">删除</button></td>
                </tr>
              `).join('')}
            </tbody>
          </table>
        </div>
      `;
    } catch (e) {
      el.innerHTML = utils.errorState('加载失败: ' + e.message, 'loadSources');
    }
  }

  async function addSource() {
    const input = document.getElementById('source-url');
    if (!input) return;
    const url = input.value.trim();
    if (!url) return;

    try {
      const data = await api.post('/api/sources', { url });
      toast.success(data.message || '添加成功');
      input.value = '';
      await loadSources();
    } catch (e) {
      toast.error('添加失败: ' + e.message);
    }
  }

  async function deleteSource(index) {
    if (!confirm(`确定删除第 ${index + 1} 个订阅源？`)) return;
    try {
      const data = await api.del(`/api/sources/${index}`);
      toast.success(data.message || '删除成功');
      await loadSources();
    } catch (e) {
      toast.error('删除失败: ' + e.message);
    }
  }

  // ===== Sub-Store =====
  async function loadSubstore() {
    const colsEl = document.getElementById('substore-collections');
    const subsEl = document.getElementById('substore-subs');
    if (!colsEl || !subsEl) return;
    colsEl.innerHTML = '<div class="loading-overlay"><div class="loading"></div></div>';
    subsEl.innerHTML = '<div class="loading-overlay"><div class="loading"></div></div>';

    try {
      const [cols, subs] = await Promise.all([
        api.get('/api/substore/api/collections'),
        api.get('/api/substore/api/subs'),
      ]);
      colsEl.innerHTML = renderSubstoreList(cols, 'type');
      subsEl.innerHTML = renderSubstoreList(subs, 'type');
    } catch (e) {
      const msg = utils.errorState('加载失败: ' + e.message, 'loadSubstore');
      colsEl.innerHTML = msg;
      subsEl.innerHTML = msg;
      toast.error('加载 Sub-Store 数据失败: ' + e.message);
    }
  }

  function renderSubstoreList(items, keyField) {
    if (!items || items.length === 0) {
      return '<div class="empty-state"><div class="icon">📦</div><div class="title">暂无数据</div></div>';
    }
    return `
      <div class="table-wrap">
        <table>
          <thead><tr><th>名称</th><th>类型</th></tr></thead>
          <tbody>
            ${items.map(item => `
              <tr>
                <td class="font-medium">${utils.escapeHtml(item.name || item.id)}</td>
                <td><span class="badge badge-blue">${utils.escapeHtml(item[keyField] || 'N/A')}</span></td>
              </tr>
            `).join('')}
          </tbody>
        </table>
      </div>
    `;
  }

  // ===== 日志 =====
  async function loadLogs() {
    try {
      const data = await api.get('/api/logs');
      const logs = data.logs || [];
      const html = logs.slice(-200).map(line => {
        const m = line.match(/^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}) (INF|ERR|WRN|DBG|FTL)?\s*(.*)$/);
        if (m) {
          const [, ts, lvl, msg] = m;
          return `<div class="log-line"><span class="ts">${ts}</span> <span class="lvl lvl-${lvl || 'INF'}">${lvl || ''}</span>${utils.escapeHtml(msg)}</div>`;
        }
        return `<div class="log-line">${utils.escapeHtml(line)}</div>`;
      }).join('');
      const el = document.getElementById('logs-container');
      el.innerHTML = html || '<div class="empty-state"><div class="icon">📋</div><div class="title">暂无日志</div></div>';
      el.scrollTop = 999999;
    } catch (e) {
      toast.error('加载日志失败: ' + e.message);
    }
  }

  function clearLogs() {
    const el = document.getElementById('logs-container');
    if (el) el.innerHTML = '';
  }

  function toggleLogAutoRefresh() {
    state.logAutoRefresh = !state.logAutoRefresh;
    const checkbox = document.getElementById('auto-refresh-logs');
    if (state.logAutoRefresh) {
      checkbox.checked = true;
      state.logRefreshTimer = setInterval(loadLogs, 5000);
    } else {
      checkbox.checked = false;
      if (state.logRefreshTimer) {
        clearInterval(state.logRefreshTimer);
        state.logRefreshTimer = null;
      }
    }
  }

  // ===== 设置 =====
  async function loadSettings() {
    try {
      const cfg = await api.get('/api/config');
      const text = cfg.content || cfg.raw || '无配置';
      document.getElementById('config-display').textContent = text;
    } catch (e) {
      toast.error('加载配置失败: ' + e.message);
    }
  }

  async function refreshCsrfToken() {
    try {
      await api.getCsrfToken();
      toast.success('CSRF Token 已刷新');
    } catch (e) {
      toast.error('刷新失败: ' + e.message);
    }
  }

  // ===== 导航 =====
  function setupNav() {
    document.querySelectorAll('.nav-link').forEach(a => {
      a.addEventListener('click', (e) => {
        e.preventDefault();
        const page = a.dataset.page;
        document.querySelectorAll('.nav-link').forEach(x => x.classList.remove('active'));
        a.classList.add('active');
        document.querySelectorAll('.page').forEach(p => p.classList.add('hidden'));
        document.getElementById('page-' + page).classList.remove('hidden');
        document.getElementById('page-' + page).classList.add('fade-in');

        document.getElementById('sidebar').classList.remove('open');

        if (page === 'dashboard') loadDashboard();
        if (page === 'nodes') loadNodes();
        if (page === 'sources') loadSources();
        if (page === 'substore') loadSubstore();
        if (page === 'logs') loadLogs();
        if (page === 'settings') loadSettings();
      });
    });

    // 键盘导航：Escape 关闭移动端侧边栏
    document.addEventListener('keydown', (e) => {
      if (e.key === 'Escape') {
        const sidebar = document.getElementById('sidebar');
        if (sidebar) sidebar.classList.remove('open');
      }
    });
  }

  // ===== 初始化 =====
  async function init() {
    setupNav();
    try {
      // 先检测是否已登录：调用 /api/health（无需认证）验证服务可达
      await api.get('/api/health');
      // 再尝试加载仪表盘数据（需要认证）
      await loadDashboard();
    } catch (e) {
      // 401 → 未登录，显示登录页
      if (e.message && e.message.includes('未授权')) {
        showLoginScreen();
        return;
      }
      // 其他错误 → 也显示登录页（服务可能未启动或网络不通）
      showLoginScreen();
    }
  }

  // 暴露到全局
  window.App = {
    api,
    loadDashboard,
    loadNodes, renderNodes, debouncedRenderNodes, renderPagination, goToNodePage,
    loadSources, addSource, deleteSource,
    loadSubstore,
    loadLogs, clearLogs, toggleLogAutoRefresh,
    loadSettings, refreshCsrfToken,
    handleLogin,
    showLoginScreen,
  };

  document.addEventListener('DOMContentLoaded', init);
})();
