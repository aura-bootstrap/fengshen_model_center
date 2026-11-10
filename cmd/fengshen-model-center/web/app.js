'use strict';

// This file is both a browser script and a dependency-free Node test target.
(function (root) {
  const rows = [
    ['whisper-asr', 'Whisper · 高光转写', 'Whisper', '语音识别', '将台词转为带时间戳的文本，供高光分析使用。', 'Go → whisper.cpp 原生推理', '原生推理'],
    ['whisper-vad', 'Whisper VAD', 'VAD', '语音识别', '检测说话区间，辅助转写；不包含纯算法能量 VAD。', 'Go → whisper.cpp 原生推理', '原生推理'],
    ['local-text', '本地文本大模型', '可配置 LLM', '高光分析', '理解台词、评选高光并判断内容完整性。', 'Go → 本地 HTTP 模型服务', '本地模型服务'],
    ['local-vision', '本地视觉大模型', '可配置 VLM', '高光分析', '为高光候选补充画面理解和视觉审查。', 'Go → 本地 HTTP 模型服务', '本地模型服务'],
    ['demucs', 'Demucs · htdemucs', 'Demucs', '音色处理', '将音轨分离为人声与伴奏。', 'Go → Python / PyTorch', 'Python'],
    ['demucs-ft', 'Demucs · htdemucs_ft', 'Demucs', '音色处理', '通过四个微调子模型进行音源分离。', 'Go → Python / PyTorch', 'Python'],
    ['seed-vc', 'Seed-VC', 'Seed-VC', '音色处理', '以参考声音为条件转换原有人声音色；非 F0 分支。', 'Go → Python / PyTorch', 'Python'],
    ['seed-whisper', 'Whisper Small · 音色特征', 'Whisper', '音色处理', '为 Seed-VC 提取内容特征，不负责高光字幕转写。', 'Python → Transformers / PyTorch', 'Python'],
    ['campplus', 'CAMPPlus', 'CAMPPlus', '音色处理', '提取参考音频的说话人和音色特征。', 'Python → PyTorch', 'Python'],
    ['bigvgan', 'BigVGAN · 22 kHz', 'BigVGAN', '音色处理', '将声学特征还原为音频波形。', 'Python → PyTorch', 'Python'],
    ['haar-frontal', 'Haar · 正脸检测', 'OpenCV Haar', '人脸检测', '检测人物参考图是否包含正脸。', 'Go 直接读取 Haar 模型', 'Go'],
    ['haar-profile', 'Haar · 侧脸检测', 'OpenCV Haar', '人脸检测', '辅助区分侧脸与没有人脸。', 'Go 直接读取 Haar 模型', 'Go']
  ];
  const STATIC_CATALOG = rows.map(([id, name, family, category, purpose, invocation, runtime]) => ({
    id, name, family, category, purpose, invocation, runtime, optional: !id.startsWith('haar-'),
    dependencies: id === 'seed-vc' ? ['seed-whisper', 'campplus', 'bigvgan'] : [],
    components: id === 'demucs-ft' ? ['f7e0c4bc-*.th', 'd12395a8-*.th', '92cfc3b6-*.th', '04573f0d-*.th'] : [],
    identity_note: '型号未确认；规则匹配不是内容认证或推理验证。'
  }));
  const LABELS = {
    not_scanned: '尚未扫描', not_checked: '未检查', not_tested: '未验证', unknown: '未知',
    missing: '未发现', found: '已发现文件', discovered: '已发现文件', inaccessible: '不可访问', unavailable: '不可访问',
    empty: '空文件', empty_file: '空文件', incomplete: '不完整', partial: '部分完成', complete: '组成齐全',
    not_applicable: '不适用', development_venv: '开发 venv（非交付组件）', registered_identity_unconfirmed: '已登记、身份未确认',
    registered_unconfirmed: '已登记、身份未确认', unconfirmed: '身份未确认', recognized_by_rule: '规则识别（非认证）',
    recognized: '规则识别（非认证）', confirmed: '身份规则已匹配（非认证）',
    running: '进行中', cancelling: '正在取消', completed: '已完成', cancelled: '已取消（未完成）', failed: '失败',
    passed: '已通过（仅入口测试）', not_verified: '尚未验证',
    stale: '已过期', fingerprint_computed: '已计算指纹（非认证）', pending_build: '待构建 / 未交付',
    matched: '与清单匹配（非官方认证）', mismatch: '不匹配', not_declared: '未声明',
    declared_not_tested: '已声明、未执行验证', missing_required_declaration: '缺少必需协议声明',
    invalid_manifest: '清单无效', dependency_missing: '依赖缺失', dependency_cycle: '依赖循环',
    satisfied: '依赖文件已发现（未验证兼容）', not_required: '无独立依赖', none: '无',
    scan: '受限扫描', verify: '文件 SHA-256', 'verify-component': '组件清单校验',
    go: 'Go 二进制', native: '原生二进制', 'pyinstaller-onedir': '冻结打包（携带 Python 运行时）'
  };
  const label = value => LABELS[value] || (value ? String(value) : '未知');
  const isTerminal = op => !!op && ['completed', 'partial', 'cancelled', 'failed'].includes(op.status);
  function storageFrozen(error) {
    // Only storage-level freeze codes block writes; path diagnostics remain repairable.
    const code = typeof error === 'string' ? error.split(':', 1)[0].trim() : '';
    return ['registry_corrupt', 'registry_external_change', 'registry_commit_uncertain', 'external_change', 'commit_uncertain'].includes(code);
  }
  function formatBytes(value) {
    if (value === null || value === undefined || !/^\d+$/.test(String(value))) return '未知';
    const n = BigInt(value), unit = n >= 1073741824n ? 1073741824n : 1048576n;
    const hundredths = (n * 100n + unit / 2n) / unit;
    return `${hundredths / 100n}.${String(hundredths % 100n).padStart(2, '0')} ${unit === 1073741824n ? 'GiB' : 'MiB'}`;
  }
  function metricText(metric) {
    if (!metric || metric.bytes == null) return '未知 / 未测量';
    return `${formatBytes(metric.bytes)}${metric.complete ? '' : ' · 已扫描部分'}`;
  }
  function contentEqualText(file, inventory) {
    const hash = file.hash;
    // The group is inventory-local; candidates, aliases and historical hashes are not evidence.
    if (!inventory || inventory.stale || !file.content_equal || file.link_kind !== 'none' ||
      !hash || hash.status !== 'fingerprint_computed' || hash.file_id !== file.id ||
      hash.bytes !== file.bytes || !/^\d+$/.test(file.bytes) || !/^[a-f\d]{64}$/i.test(hash.sha256 || '')) return '';
    return `当前盘点内容组：${file.content_equal} · 当前有效 SHA-256 摘要和长度相同（${file.bytes} 字节）；不代表发行方认证或推理可用。`;
  }
  function filterModels(models, filters) {
    const query = (filters.search || '').trim().toLocaleLowerCase();
    return models.filter(m => (!query || [m.name, m.family, m.purpose].join(' ').toLocaleLowerCase().includes(query)) &&
      (!filters.category || m.category === filters.category) && (!filters.runtime || m.runtime === filters.runtime) &&
      (!filters.status || (m.discovery_status || 'not_scanned') === filters.status));
  }
  function verifyPayload(inventory, model, files, selected) {
    const ids = files.filter(f => selected.has(f.id)).map(f => f.id);
    if (!ids.length) throw new Error('请先选择当前页文件。');
    if (ids.length > 1000) throw new Error('单次最多 1000 个文件，请分批校验，不会自动截断。');
    if (!inventory || inventory.stale) throw new Error('盘点未知或已过期，请重新扫描。');
    return { inventory_id: inventory.id, model_id: model, file_ids: ids };
  }
  function createAPI(token, fetcher) {
    return async function api(path, method = 'GET', body) {
      const options = { method, credentials: 'same-origin', cache: 'no-store', redirect: 'error', headers: { 'X-Model-Center-Token': token } };
      if (body !== undefined) { options.headers['Content-Type'] = 'application/json'; options.body = JSON.stringify(body); }
      let response;
      try { response = await fetcher(path, options); }
      catch (_) { const error = new Error('无法连接中心服务；写入结果可能不确定，请读取状态核对，不要直接重复提交。'); error.code = 'network_error'; throw error; }
      let data;
      try { data = await response.json(); }
      catch (_) { const error = new Error('中心返回了无法解析的响应，请读取状态核对。'); error.code = 'response_invalid'; throw error; }
      if (!response.ok) {
        const error = new Error(data.error?.message || `请求失败（${response.status}）`);
        error.code = data.error?.code || 'request_failed'; error.status = response.status; throw error;
      }
      return data;
    };
  }
  function createPoller(read, onValue, onError, schedule = setTimeout, unschedule = clearTimeout) {
    let generation = 0, timer;
    return {
      stop() { generation++; if (timer !== undefined) unschedule(timer); timer = undefined; },
      start(id) {
        this.stop(); const current = generation;
        const tick = async () => {
          try {
            const op = await read(id);
            if (current !== generation) return;
            await onValue(op);
            if (current === generation && !isTerminal(op)) timer = schedule(tick, 1000);
          } catch (error) { if (current === generation) onError(error); }
        };
        void tick();
      }
    };
  }
  const exported = { STATIC_CATALOG, label, isTerminal, storageFrozen, formatBytes, metricText, contentEqualText, filterModels, verifyPayload, createAPI, createPoller };
  if (typeof module !== 'undefined' && module.exports) module.exports = exported;
  if (!root.document) return;

  const document = root.document;
  const $ = id => document.getElementById(id);
  const el = (tag, text, className) => { const node = document.createElement(tag); if (text !== undefined) node.textContent = String(text); if (className) node.className = className; return node; };
  const button = (text, action) => { const node = el('button', text); node.type = 'button'; node.addEventListener('click', action); return node; };
  const state = { catalog: STATIC_CATALOG, registry: { revision: 0, registrations: {} }, inventories: new Map(), inventory: null,
    lastCompleteInventory: null, lastAttempt: null, currentID: '', selected: STATIC_CATALOG[0].id, drafts: new Map(), active: null, connected: false,
    busy: false, sessionInvalid: false, storage_error: '', page: [], pageModel: '', pageInventory: '', cursors: [''], pageIndex: 0, nextCursor: '', selectedFiles: new Set(), fileGeneration: 0, filesLoading: false };
  const api = createAPI(document.querySelector('meta[name="model-center-token"]')?.content || '', root.fetch.bind(root));
  const idPath = value => encodeURIComponent(value);
  const date = value => value && !value.startsWith('0001-') ? new Date(value).toLocaleString('zh-CN') : '未取得时间';
  const diagnostic = value => typeof value === 'string' ? value : `${value.code || '诊断'}：${value.message || ''}`;
  function notice(text) { $('notice').textContent = text; }
  function showError(error) {
    $('error').hidden = false; $('error').textContent = `${error.code ? `${error.code}：` : ''}${error.message || error}`;
    if (error.status === 410) $('error').textContent += '\n该结果已回收，请重新读取状态或手动发起新扫描；仍保留当前可见数据作为历史证据。';
    if (error.status === 403 || error.code === 'session_invalid') {
      state.sessionInvalid = true; poller.stop(); $('error').textContent += '\n会话已失效或访问被拒绝，请重新打开当前中心入口；不会自动寻找凭据。';
    }
    controls();
  }
  function clearError() { $('error').hidden = true; $('error').textContent = ''; }
  function models() {
    const observed = new Map((state.inventory?.models || []).map(m => [m.id, m]));
    return state.catalog.map(m => ({ ...m, ...observed.get(m.id) }));
  }
  function locked() { return state.busy || !state.connected || state.sessionInvalid; }
  function active() { return state.active && !isTerminal(state.active); }
  function controls() {
    const frozen = storageFrozen(state.storage_error);
    $('scan').disabled = locked() || !!active() || frozen;
    $('reload').disabled = state.busy || state.sessionInvalid;
    $('cancel').disabled = locked() || !active() || state.active?.status === 'cancelling';
    document.querySelectorAll('[data-write]').forEach(n => { n.disabled = locked() || !!active() || frozen; });
    document.querySelectorAll('[data-verify]').forEach(n => { n.disabled = locked() || !!active() || frozen || !state.inventory || state.inventory.stale || state.pageInventory !== state.inventory.id || state.pageModel !== state.selected || state.filesLoading; });
    document.querySelectorAll('[data-component]').forEach(n => { n.disabled = locked() || !!active() || frozen; });
  }
  function renderOperation(op) {
    $('operation').hidden = !op;
    if (op) $('operation-text').textContent = `${label(op.kind)} · ${label(op.status)} · 已检查 ${op.checked_entries ?? 0} 项 · 已处理 ${formatBytes(op.processed_bytes)}`;
    $('cancel').hidden = !op || isTerminal(op); controls();
  }
  function renderOverview() {
    const inventory = state.inventory;
    const stats = $('stats'); stats.replaceChildren();
    for (const [name, value, hint] of [
      ['本地模型目录', `${state.catalog.length} 项`, '可选能力与必需依赖分开'],
      ['已识别权重逻辑大小', metricText(inventory?.weight_size), '不含隐式依赖合计'],
      ['资产范围逻辑大小', metricText(inventory?.asset_size), '含缓存与辅助文件'],
      ['运行环境逻辑大小', metricText(inventory?.runtime_size), '与权重分栏，不相加']
    ]) { const node = el('article', undefined, 'stat'); node.append(el('span', name, 'muted'), el('strong', value), el('small', hint)); stats.append(node); }
    $('inventory-meta').textContent = inventory ? `当前查看 ${inventory.id} · ${inventory.complete ? '完整盘点' : '部分结果 / 未完成'}${inventory.stale || inventory.registry_revision !== state.registry.revision ? ' · 已过期，需重新扫描' : ''} · 开始 ${date(inventory.started_at)} · 结束 ${date(inventory.finished_at)}` : '尚未扫描 · 十二项静态目录可供查阅';
    $('history').textContent = `清单版本 ${state.registry.revision} · 上次完整盘点：${state.lastCompleteInventory?.id || '无'} · 最近尝试：${state.lastAttempt ? `${label(state.lastAttempt.kind)} / ${label(state.lastAttempt.status)}` : '无'}`;
    const historyActions = $('history-actions'); historyActions.replaceChildren();
    for (const [title, id] of [['查看上次完整盘点', state.lastCompleteInventory?.id], ['查看最近部分 / 当前盘点', state.currentID]]) {
      if (id && id !== inventory?.id) historyActions.append(button(title, () => void viewInventory(id)));
    }
    const scopes = $('scopes'); scopes.replaceChildren();
    if (!inventory) scopes.append(el('p', '尚未扫描'));
    else {
      scopes.append(el('p', `Seed-VC 套件（依赖文件并集）：${metricText(inventory.seed_suite_size)}`));
      const list = el('ul');
      for (const scope of inventory.scopes || []) list.append(el('li', `${scope.path} · ${scope.kind} · ${label(scope.status)} · ${scope.complete ? '范围完整' : '范围未完成'}`, 'path'));
      scopes.append(list); appendDiagnostics(scopes, inventory.diagnostics);
      if (inventory.runtime_environments?.length) {
        const environments = section(scopes, '运行环境元数据');
        environments.append(el('p', '开发 venv 不是已交付的二进制组件；此处仅展示文件元数据，不执行解释器或版本命令，不验证推理。', 'muted'));
        for (const environment of inventory.runtime_environments) {
          const card = el('article', undefined, 'file');
          card.append(el('p', label(environment.kind), 'badge'));
          if (environment.interpreter_path) { card.append(el('p', '解释器路径', 'muted')); pathView(card, environment.interpreter_path); }
          const dl = el('dl');
          for (const [title, value] of [
            ['解释器发现状态', label(environment.interpreter_status || 'not_checked')],
            ['解释器文件字节', environment.interpreter_bytes == null ? '未知 / 未测量' : `${formatBytes(environment.interpreter_bytes)} · 精确 ${environment.interpreter_bytes} 字节`],
            ['交付状态', label(environment.delivery_status || 'not_checked')],
            ['推理验证', label(environment.inference || 'not_tested')]
          ]) pair(dl, title, value);
          card.append(dl);
          if (environment.message) card.append(el('p', environment.message, 'path'));
          environments.append(card);
        }
      }
    }
    const storage = $('storage-diagnostic');
    storage.hidden = !state.storage_error;
    storage.textContent = state.storage_error ? `${storageFrozen(state.storage_error) ? '模型清单已冻结：新写操作已禁用；请核对清单文件并重新读取状态。' : '模型清单诊断：请检查 ~/.fengshen/model_manifest.json 并重新读取状态。'}\n${state.storage_error}` : '';
    if (state.lastAttempt?.diagnostics?.length) appendDiagnostics(scopes, state.lastAttempt.diagnostics);
  }
  function appendDiagnostics(parent, diagnostics) {
    if (!diagnostics?.length) return;
    const list = el('ul'); for (const item of diagnostics) list.append(el('li', diagnostic(item))); parent.append(list);
  }
  function fillOptions(id, values) {
    const select = $(id), previous = select.value;
    const first = select.options[0].cloneNode(true); select.replaceChildren(first);
    for (const value of [...new Set(values)].sort()) { const option = el('option', id === 'status' ? label(value) : value); option.value = value; select.append(option); }
    select.value = [...select.options].some(o => o.value === previous) ? previous : '';
  }
  function renderList(updateOptions = false) {
    const all = models();
    if (updateOptions) { fillOptions('category', all.map(m => m.category)); fillOptions('runtime', all.map(m => m.runtime)); fillOptions('status', all.map(m => m.discovery_status || 'not_scanned')); }
    const filtered = filterModels(all, Object.fromEntries(['search', 'category', 'runtime', 'status'].map(id => [id, $(id).value])));
    $('model-count').textContent = `${filtered.length} / ${all.length}`; $('empty').hidden = filtered.length !== 0;
    $('models').replaceChildren();
    for (const m of filtered) {
      const card = el('article', undefined, `model-card${m.id === state.selected ? ' selected' : ''}`);
      const heading = el('h3'); const open = button(m.name, () => { state.selected = m.id; resetPage(); renderList(); renderDetail(); $('detail-heading').focus(); void loadFiles(); });
      open.setAttribute('aria-label', `查看 ${m.name} 详情`); open.setAttribute('aria-pressed', String(m.id === state.selected));
      heading.append(open, el('span', label(m.discovery_status || 'not_scanned'), 'badge'));
      card.append(heading, el('p', m.purpose), el('p', `${m.family} · ${m.category} · ${m.optional ? '可选能力' : '消费者数据资源'}`, 'muted'), el('p', m.invocation, 'muted'), el('p', `${metricText(m.size)} · ${m.file_count ?? '未知'} 个文件`, 'muted'));
      $('models').append(card);
    }
  }
  function pair(parent, title, text) { parent.append(el('dt', title), el('dd', text)); }
  function section(parent, title) { const node = el('section', undefined, 'detail-section'); node.append(el('h3', title)); parent.append(node); return node; }
  function pathView(parent, path) {
    const text = el('code', path, 'path'); text.tabIndex = 0; parent.append(text);
    parent.append(button('复制路径', async () => {
      try { if (!root.navigator.clipboard) throw new Error('不可用'); await root.navigator.clipboard.writeText(path); notice('路径已复制。'); }
      catch (_) { notice('复制失败；路径仍保留为可选择文本，请手动选择并复制。'); text.focus(); }
    }));
  }
  function resetPage() { state.fileGeneration++; state.page = []; state.pageModel = ''; state.pageInventory = ''; state.cursors = ['']; state.pageIndex = 0; state.nextCursor = ''; state.selectedFiles.clear(); state.filesLoading = false; }
  function renderDetail() {
    const m = models().find(m => m.id === state.selected); if (!m) return;
    $('detail-heading').textContent = m.name; const body = $('detail-body'); body.replaceChildren();
    body.append(el('p', m.purpose), el('p', m.description || m.identity_note, 'muted'));
    const dl = el('dl');
    for (const [title, value] of [['家族 / 类别', `${m.family} / ${m.category}`], ['调用链', m.invocation], ['运行环境 / 消费者', `${m.runtime} / ${m.consumer || '尚未检查交付组件'}`], ['可选性', m.optional ? '此功能可选；启用后仍需满足必需依赖' : '数据资源，由 Go 消费者读取'], ['身份边界', m.identity_note || '型号未确认']]) pair(dl, title, value);
    body.append(dl);
    body.append(el('p', `本项权重：${metricText(m.size)} · 计入 ${m.size?.counted_files ?? '未知'} 个文件对象 · 身份统计质量：${m.size?.identity_quality || '未知'}；不隐式加上依赖。`, 'muted'));
    const evidence = section(body, '正交证据');
    for (const [key, title] of [['discovery_status', '文件发现'], ['identity_status', '身份'], ['composition_status', '组成'], ['dependency_status', '依赖'], ['verification_status', '摘要'], ['inference', '推理']]) evidence.append(el('span', `${title}：${label(m[key] || (key === 'inference' ? 'not_tested' : 'not_checked'))}`, 'badge'));
    const composition = section(body, '依赖与组成');
    const dependencies = el('ul');
    for (const id of m.dependencies || []) { const dep = models().find(x => x.id === id); dependencies.append(el('li', `${dep?.name || id} · ${label(dep?.discovery_status || 'not_checked')} · 兼容性未验证`)); }
    if (!(m.dependencies || []).length) dependencies.append(el('li', '没有声明独立模型依赖；不表示运行环境已就绪。'));
    composition.append(dependencies);
    for (const rule of m.components || []) composition.append(el('p', `预期组成：${rule}`, 'path'));
    for (const parts of m.component_alternatives || []) composition.append(el('p', `可选完整组成（任一组，同目录且非空）：${parts.join(' + ')}`, 'path'));
    for (const [group, status] of Object.entries(m.groups || {})) composition.append(el('p', `组成组 ${group}：${label(status)}`, 'path'));
    composition.append(el('p', 'htdemucs_ft 须同组四份非空权重齐全；Seed-VC 主模型不代表三项配套依赖齐全。', 'muted'));
    const rules = el('details'); rules.append(el('summary', '识别规则与允许类型'));
    for (const rule of m.recognition_rules || []) rules.append(el('p', rule, 'path'));
    rules.append(el('p', `允许文件类型：${(m.allowed_extensions || []).join('、') || '以服务端规则为准'}`)); composition.append(rules);
    const files = section(body, '组成文件与 SHA-256'); files.id = 'files'; renderFiles(); controls();
  }
  function renderFiles() {
    const parent = $('files'); if (!parent) return; parent.replaceChildren(el('h3', '组成文件与 SHA-256'));
    parent.append(el('p', '每页 200 条，仅校验本页所选文件；单次不超过 1000 个，更多文件请逐页分批。指纹不代表发行方认证或推理通过。', 'muted'));
    if (!state.inventory) { parent.append(el('p', '尚无盘点，请先刷新扫描。')); return; }
    const actions = el('div', undefined, 'actions');
    const selectAll = button('选择当前页', () => { state.page.forEach(f => state.selectedFiles.add(f.id)); renderFiles(); });
    selectAll.disabled = state.filesLoading;
    const verify = button('计算所选 SHA-256', () => { try { const payload = verifyPayload(state.inventory, state.selected, state.page, state.selectedFiles); void startOperation('/api/operations/verify', payload); } catch (error) { showError(error); } }); verify.dataset.verify = '';
    actions.append(selectAll, button('清除所选', () => { state.selectedFiles.clear(); renderFiles(); }), verify); parent.append(actions);
    if (state.filesLoading) parent.append(el('p', '正在读取文件页…', 'muted'));
    else if (!state.page.length) parent.append(el('p', '本页没有文件；未发现不等于已验证容量为零。', 'muted'));
    for (const file of state.page) {
      const item = el('article', undefined, 'file'); const checkLabel = el('label'); const check = el('input'); check.type = 'checkbox'; check.checked = state.selectedFiles.has(file.id);
      check.addEventListener('change', () => { if (check.checked) state.selectedFiles.add(file.id); else state.selectedFiles.delete(file.id); });
      checkLabel.append(check, el('span', `选择文件 ${file.path}`)); item.append(checkLabel);
      const details = el('details'); details.append(el('summary', '展开完整路径、别名与证据')); pathView(details, file.path);
      for (const alias of file.aliases || []) details.append(el('p', `别名 / 共享路径：${alias}`, 'path'));
      details.append(el('p', `证据：${file.evidence || '未知'} · 组成组：${file.group || '未确认'}`, 'path')); item.append(details);
      item.append(el('p', `${formatBytes(file.bytes)} · 精确 ${file.bytes} 字节 · 修改 ${date(file.modified)}`, 'muted'));
      if (file.duplicate_candidate) item.append(el('p', '重复候选：仅为线索，未据此合并或删除。', 'muted'));
      const hash = file.hash; item.append(el('p', `摘要：${label(hash?.status || 'not_checked')}`));
      if (hash) { item.append(el('p', hash.sha256 || '无有效指纹', 'path'), el('p', `${date(hash.checked_at)} · ${hash.message || '仅为本次读取字节指纹'}`, 'muted')); }
      const equal = contentEqualText(file, state.inventory);
      if (equal) item.append(el('p', equal, 'path'), el('p', '普通副本仍分别计量，不合并；不承诺可以安全删除，也不表示可节省的空间。', 'muted'));
      parent.append(item);
    }
    const pages = el('div', undefined, 'actions');
    const previous = button('上一页', () => { state.pageIndex--; void loadFiles(); }); previous.disabled = state.pageIndex === 0 || state.filesLoading;
    const next = button('下一页', () => { state.cursors[state.pageIndex + 1] = state.nextCursor; state.pageIndex++; void loadFiles(); }); next.disabled = !state.nextCursor || state.filesLoading;
    pages.append(previous, el('span', `第 ${state.pageIndex + 1} 页 · 已选 ${state.selectedFiles.size} 项`, 'muted'), next); parent.append(pages); controls();
  }
  async function loadFiles() {
    if (!state.inventory) return;
    const generation = ++state.fileGeneration, inventory = state.inventory.id, model = state.selected;
    state.filesLoading = true; state.page = []; state.selectedFiles.clear(); renderFiles();
    try {
      const cursor = state.cursors[state.pageIndex] || '';
      const data = await api(`/api/inventories/${idPath(inventory)}/models/${idPath(model)}/files?cursor=${encodeURIComponent(cursor)}&limit=200`);
      if (generation !== state.fileGeneration) return;
      state.page = data.files || []; state.nextCursor = data.next_cursor || ''; state.pageInventory = inventory; state.pageModel = model;
    } catch (error) { if (generation === state.fileGeneration) { state.nextCursor = ''; showError(error); } }
    finally { if (generation === state.fileGeneration) { state.filesLoading = false; renderFiles(); } }
  }
  async function getInventory(id) {
    if (!id) return null;
    const inventory = await api(`/api/inventories/${idPath(id)}`); state.inventories.set(id, inventory); return inventory;
  }
  async function viewInventory(id) {
    try { const inventory = state.inventories.get(id) || await getInventory(id); state.inventory = inventory.registry_revision !== state.registry.revision ? { ...inventory, stale: true } : inventory; resetPage(); renderOverview(); renderList(true); renderDetail(); await loadFiles(); }
    catch (error) { showError(error); }
  }
  async function readState({ startPolling = true, preservePage = false } = {}) {
    const previousInventory = state.inventory?.id, previousModel = state.selected;
    const snapshot = await api('/api/state');
    state.connected = true; state.registry = snapshot.registry; state.storage_error = typeof snapshot.storage_error === 'string' ? snapshot.storage_error : '';
    state.catalog = snapshot.catalog?.length ? snapshot.catalog : STATIC_CATALOG;
    state.currentID = snapshot.current_inventory; state.lastAttempt = snapshot.last_attempt; state.active = snapshot.active_operation;
    $('project-root').textContent = snapshot.project_root || '未取得';
    // Fetch both references separately. A failed/partial attempt never erases the last complete view.
    if (snapshot.last_complete_inventory) {
      try { state.lastCompleteInventory = await getInventory(snapshot.last_complete_inventory); } catch (error) { showError(error); }
    }
    let current = null;
    if (snapshot.current_inventory) { try { current = await getInventory(snapshot.current_inventory); } catch (error) { showError(error); } }
    if (current) state.inventory = current;
    else if (!state.inventory && state.lastCompleteInventory) state.inventory = state.lastCompleteInventory;
    if (state.inventory && state.inventory.registry_revision !== state.registry.revision) state.inventory = { ...state.inventory, stale: true };
    if (!preservePage || previousInventory !== state.inventory?.id || previousModel !== state.selected) resetPage();
    renderOverview(); renderList(true); renderDetail(); renderOperation(state.active || state.lastAttempt);
    if (startPolling) { if (active()) poller.start(state.active.id); else poller.stop(); }
    await loadFiles();
  }
  function renderComponentManifest(card, component) {
    const details = el('details'); details.append(el('summary', '只读清单详情（声明不等于验证）'));
    card.append(details);
    const manifest = component.manifest;
    if (!manifest || component.packaging === 'pending_build' || component.packaging === 'invalid_manifest') {
      details.append(el('p', component.packaging === 'invalid_manifest' ? '清单无效；不展示未通过解析的声明。' : '未交付 / 未取得有效清单；入口、运行库及权重引用未声明。'));
      return;
    }
    const declared = value => value == null || value === '' ? '未声明' : String(value);
    const dl = el('dl');
    for (const [title, value] of [
      ['清单 schema', manifest.schema], ['声明平台 / 架构', `${declared(manifest.os)} / ${declared(manifest.arch)}`],
      ['入口类型', manifest.entry_type], ['入口（组件根相对路径）', manifest.entry],
      ['入口测试报告引用', manifest.startup_report_ref]
    ]) pair(dl, title, declared(value));
    details.append(dl, el('p', '以下仅为清单声明：文件表包含入口及随包运行库，不推断 DLL 用途、不加载库；权重引用不代表权重已发现或兼容。', 'muted'));
    const list = (title, values, describe) => {
      const part = section(details, title), items = Array.isArray(values) ? values : [];
      if (!items.length) { part.append(el('p', '未声明')); return; }
      const ul = el('ul');
      for (const item of items) ul.append(el('li', describe(item), 'path'));
      part.append(ul);
    };
    list('交付文件 / 随包运行库', manifest.files, f => `${declared(f.path)} · 清单字节 ${declared(f.bytes)} · 预期 SHA-256 ${declared(f.sha256)}`);
    list('运行库组件依赖', manifest.runtime_dependencies, d => `${declared(d.component_id)} / ${declared(d.version)}`);
    list('权重引用', manifest.model_refs, declared);
    list('协议声明（非实测）', manifest.protocols, declared);
    list('外部驱动要求', manifest.driver_requirements, declared);
    list('构建输入（非供应链认证）', manifest.build_inputs, b => `${declared(b.name)} · 版本 ${declared(b.version)} · SHA-256 ${declared(b.sha256)} · 许可证 ${declared(b.license)}`);
  }
  async function loadComponents() {
    try {
      const evidence = await api('/api/components'); const parent = $('components'); parent.replaceChildren();
      const components = Array.isArray(evidence) ? evidence : [];
      for (const component of components) {
        const card = el('article', undefined, 'component'); card.append(el('h3', component.component_id), el('p', `版本：${component.version || '未交付'}`, 'muted'));
        const dl = el('dl');
        for (const [key, title] of [['packaging', '交付方式'], ['platform', '平台'], ['files_state', '文件状态'], ['integrity', '按清单完整性'], ['startup_test', '入口启动测试'], ['protocol_evidence', '协议证据'], ['inference', '推理验证']]) pair(dl, title, label(component[key] || 'not_checked'));
        card.append(dl); appendDiagnostics(card, component.diagnostics);
        renderComponentManifest(card, component);
        if (component.version && component.packaging !== 'pending_build' && component.packaging !== 'invalid_manifest') {
          const verify = button('显式校验组件清单', () => void startOperation('/api/operations/verify-component', { component_id: component.component_id, version: component.version })); verify.dataset.component = ''; card.append(verify);
        }
        parent.append(card);
      }
      if (!components.length) parent.append(el('p', '没有取得组件交付证据；未交付 / 未验证。'));
      controls();
    } catch (error) { showError(error); }
  }
  async function reconcile(error) {
    // Never automatically replay a POST/PUT/DELETE, including on an ambiguous network failure.
    let checked = false;
    try { await readState(); checked = true; } catch (readError) { showError(readError); }
    showError(error); notice(checked ? '已重新读取 state 核对服务结果。没有重发写请求；请检查登记版本及最近操作，再决定是否手动重试。' : '尚无法核对执行结果。没有重发写请求；恢复连接后请先“重新读取状态”。');
  }
  async function startOperation(path, body) {
    if (locked() || active() || storageFrozen(state.storage_error)) return;
    state.busy = true; controls(); clearError();
    try { const op = await api(path, 'POST', body); state.active = op; state.lastAttempt = op; renderOperation(op); poller.start(op.id); }
    catch (error) { await reconcile(error); }
    finally { state.busy = false; controls(); }
  }
  const poller = createPoller(id => api(`/api/operations/${idPath(id)}`), async op => {
    state.active = op; state.lastAttempt = op; renderOperation(op);
    if (isTerminal(op)) {
      state.active = null;
      await readState({ startPolling: false, preservePage: true });
      // The result may be partial; retain the complete reference and label both.
      if (op.result_id && !state.inventories.has(op.result_id)) { try { await getInventory(op.result_id); } catch (error) { showError(error); } }
      if (op.hashes?.length && state.pageInventory === state.inventory?.id) {
        const hashes = new Map(op.hashes.map(hash => [hash.file_id, hash]));
        state.page = state.page.map(file => hashes.has(file.id) ? { ...file, hash: hashes.get(file.id) } : file); renderFiles();
      }
      appendDiagnostics($('scopes'), op.diagnostics); await loadComponents();
      notice(`${label(op.kind)}：${label(op.status)}。${op.status === 'completed' ? '完成仅指本次操作，不代表推理可用。' : '保留已取得证据及上次完整盘点，未完成不报告为成功。'}`);
    }
  }, error => { showError(error); notice('进度查询已停止；操作可能仍在服务端继续。请重新读取状态核对。'); });
  $('scan').addEventListener('click', () => void startOperation('/api/operations/scan', { registry_revision: state.registry.revision }));
  $('cancel').addEventListener('click', async () => {
    if (locked() || !active()) return;
    state.busy = true; controls();
    try { const op = await api(`/api/operations/${idPath(state.active.id)}/cancel`, 'POST', {}); state.active = op; renderOperation(op); poller.start(op.id); }
    catch (error) { await reconcile(error); }
    finally { state.busy = false; controls(); }
  });
  $('reload').addEventListener('click', async () => { if (state.busy) return; clearError(); state.busy = true; controls(); try { await readState(); await loadComponents(); notice('已读取服务状态，未自动扫描。'); } catch (error) { showError(error); } finally { state.busy = false; controls(); } });
  for (const id of ['search', 'category', 'runtime', 'status']) $(id).addEventListener(id === 'search' ? 'input' : 'change', () => renderList());
  $('clear').addEventListener('click', () => { for (const id of ['search', 'category', 'runtime', 'status']) $(id).value = ''; renderList(); $('search').focus(); });
  root.addEventListener('pagehide', () => poller.stop());
  renderOverview(); renderList(true); renderDetail(); controls();
  void (async () => { try { await readState(); await loadComponents(); } catch (error) { showError(error); } })();
})(typeof window !== 'undefined' ? window : globalThis);
