'use strict';
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const web = require('./app.js');
const source = fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8');
const html = fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8');
const css = fs.readFileSync(path.join(__dirname, 'styles.css'), 'utf8');
const flush = async () => { for (let i = 0; i < 30; i++) await new Promise(resolve => setImmediate(resolve)); };

test('twelve stable catalog entries, distinct Whisper chains and dependencies', () => {
  assert.equal(web.STATIC_CATALOG.length, 12);
  assert.equal(new Set(web.STATIC_CATALOG.map(m => m.id)).size, 12);
  assert.deepEqual(web.STATIC_CATALOG.find(m => m.id === 'seed-vc').dependencies, ['seed-whisper', 'campplus', 'bigvgan']);
  assert.equal(web.STATIC_CATALOG.find(m => m.id === 'demucs-ft').components.length, 4);
  assert.match(web.STATIC_CATALOG.find(m => m.id === 'seed-whisper').purpose, /不负责.*转写/);
  assert.match(web.STATIC_CATALOG.find(m => m.id === 'haar-frontal').invocation, /Go 直接读取/);
});
test('BigInt binary sizes preserve null, zero, partial and very large byte values', () => {
  assert.equal(web.formatBytes(null), '未知');
  assert.equal(web.formatBytes('0'), '0.00 MiB');
  assert.equal(web.formatBytes('1048576'), '1.00 MiB');
  assert.equal(web.formatBytes('1073741824'), '1.00 GiB');
  assert.equal(web.formatBytes('1152921504606846976'), '1073741824.00 GiB');
  assert.equal(web.formatBytes('-1'), '未知');
  assert.match(web.metricText({ bytes: '0', complete: false }), /已扫描部分/);
  assert.match(web.metricText({ bytes: null, complete: true }), /未知/);
});
test('name/family/purpose search and category/runtime/status filters are AND combined', () => {
  const models = web.STATIC_CATALOG.map(m => ({ ...m, discovery_status: 'missing' }));
  assert.equal(web.filterModels(models, { search: 'WHISPER' }).length, 3);
  assert.equal(web.filterModels(models, { search: '音色', category: '音色处理', runtime: 'Python', status: 'missing' }).length > 0, true);
  assert.equal(web.filterModels(models, { search: 'Whisper', runtime: 'Go' }).length, 0);
  assert.equal(web.filterModels(models, {}).length, 12);
});
test('verification is explicit, current-page-only and rejects over 1000 without truncation', () => {
  const inventory = { id: 'inv' }, files = [{ id: 'a' }, { id: 'b' }];
  assert.deepEqual(web.verifyPayload(inventory, 'model', files, new Set(['a', 'other-page'])), { inventory_id: 'inv', model_id: 'model', file_ids: ['a'] });
  assert.throws(() => web.verifyPayload(inventory, 'm', files, new Set()), /选择/);
  assert.throws(() => web.verifyPayload({ ...inventory, stale: true }, 'm', files, new Set(['a'])), /过期/);
  const many = Array.from({ length: 1001 }, (_, i) => ({ id: String(i) }));
  assert.throws(() => web.verifyPayload(inventory, 'm', many, new Set(many.map(f => f.id))), /1000/);
});
test('all requests carry meta-derived token; writes never retry and errors use unified contract', async () => {
  const calls = [];
  const api = web.createAPI('session-only', async (...args) => { calls.push(args); return { ok: true, json: async () => ({ revision: 2 }) }; });
  await api('/api/state'); await api('/api/registrations/model', 'DELETE', { expected_revision: 1 });
  assert.equal(calls[0][1].headers['X-Model-Center-Token'], 'session-only');
  assert.equal(calls[1][1].body, '{"expected_revision":1}');
  assert.equal(calls[1][1].redirect, 'error');
  let attempts = 0;
  const failed = web.createAPI('t', async () => { attempts++; throw Error('offline'); });
  await assert.rejects(failed('/api/operations/scan', 'POST', {}), { code: 'network_error' });
  assert.equal(attempts, 1);
  const conflict = web.createAPI('t', async () => ({ ok: false, status: 409, json: async () => ({ error: { code: 'registry_conflict', message: '版本冲突' } }) }));
  await assert.rejects(conflict('/api/state'), { code: 'registry_conflict', message: '版本冲突', status: 409 });
});
test('polling waits one second, stops at every terminal status and stops on read failure', async () => {
  for (const status of ['completed', 'partial', 'cancelled', 'failed']) {
    const timers = [], seen = []; let reads = 0;
    const poller = web.createPoller(async () => ({ status: ++reads === 1 ? 'running' : status }), op => seen.push(op.status), assert.fail,
      (fn, delay) => { assert.equal(delay, 1000); timers.push(fn); return timers.length; }, () => {});
    poller.start('job'); await flush(); assert.equal(timers.length, 1);
    await timers.shift()(); await flush(); assert.deepEqual(seen, ['running', status]); assert.equal(timers.length, 0);
  }
  let errors = 0, scheduled = 0;
  web.createPoller(async () => { throw Error('offline'); }, assert.fail, () => errors++, () => scheduled++).start('x');
  await flush(); assert.equal(errors, 1); assert.equal(scheduled, 0);
});
test('stopping or replacing poller ignores late responses', async () => {
  let resolve, seen = 0;
  const poller = web.createPoller(() => new Promise(r => { resolve = r; }), () => seen++, assert.fail);
  poller.start('old'); poller.stop(); resolve({ status: 'running' }); await flush(); assert.equal(seen, 0);
});
test('static resources are local, no inline script/events/eval/HTML sinks; responsive tokens and accessibility', () => {
  assert.match(html, /name="model-center-token" content="__SESSION_TOKEN__"/);
  assert.deepEqual([...html.matchAll(/(?:src|href)="([^"#][^"]*)"/g)].map(m => m[1]), ['/styles.css', '/app.js']);
  assert.doesNotMatch(html, /<script(?![^>]*src=)|\son\w+=|<img|https?:\/\//i);
  assert.doesNotMatch(source, /innerHTML|outerHTML|insertAdjacentHTML|\beval\s*\(|new Function|localStorage|sessionStorage/);
  assert.match(html, /role="alert"/); assert.match(html, /aria-live="polite"/);
  assert.match(css, /^:root/); assert.match(css, /max-width: 899px/); assert.match(css, /max-width: 480px/);
  assert.match(css, /focus-visible/); assert.match(css, /prefers-reduced-motion/); assert.match(css, /overflow-wrap: anywhere/);
  for (const text of ['不写回', '不推理', 'RAM / VRAM', '关闭页面或客户端不会停止']) assert.ok(html.includes(text));
});

// A tiny DOM double exercises actual browser event handlers without installing a DOM package.
class Node {
  constructor(tag, document) { this.tagName = tag; this.document = document; this.children = []; this.listeners = {}; this.dataset = {}; this.value = ''; this.hidden = false; this.disabled = false; this._text = ''; }
  set id(value) { this._id = value; this.document.ids.set(value, this); } get id() { return this._id; }
  set textContent(value) { this._text = String(value); this.children = []; } get textContent() { return this._text + this.children.map(n => n.textContent).join(''); }
  append(...nodes) { this.children.push(...nodes); }
  replaceChildren(...nodes) { this._text = ''; this.children = nodes; }
  addEventListener(name, fn) { (this.listeners[name] ||= []).push(fn); }
  async fire(name) { if (this.disabled && name === 'click') return; for (const fn of this.listeners[name] || []) await fn({ preventDefault() {} }); await flush(); }
  setAttribute(name, value) { this[name] = value; }
  get options() { return this.children; }
  cloneNode() { const n = new Node(this.tagName, this.document); n.value = this.value; n.textContent = this._text; return n; }
  focus() { this.document.activeElement = this; }
  reportValidity() { return !!this.value; }
}
function harness(handler, clipboard) {
  const document = { ids: new Map(), activeElement: null };
  document.createElement = tag => new Node(tag, document);
  for (const [, tag, id] of html.matchAll(/<(\w+)[^>]*\bid="([^"]+)"/g)) { const n = document.createElement(tag); n.id = id; }
  for (const id of ['category', 'runtime', 'status']) document.ids.get(id).append(document.createElement('option'));
  document.getElementById = id => document.ids.get(id);
  const descendants = n => [n, ...n.children.flatMap(descendants)];
  document.querySelectorAll = selector => [...document.ids.values()].flatMap(descendants).filter(n => Object.hasOwn(n.dataset, selector.slice(6, -1)));
  document.querySelector = () => ({ content: 'from-meta' });
  const calls = [], timers = [];
  const window = { document, navigator: { clipboard }, addEventListener() {}, fetch: async (url, options) => {
    calls.push({ url, ...options, parsed: options.body ? JSON.parse(options.body) : undefined });
    const data = await handler(url, options);
    if (data instanceof Error) throw data;
    return { ok: !data?.error, status: data?.error ? 422 : 200, json: async () => data };
  } };
  vm.runInNewContext(source, { window, setTimeout: fn => { timers.push(fn); return timers.length; }, clearTimeout() {} });
  return { document, calls, timers, get: id => document.ids.get(id), all: n => descendants(n), find: (n, text) => descendants(n).find(x => x.tagName === 'button' && x.textContent === text) };
}
function inventory(id = 'inv', complete = true) {
  return { id, registry_revision: 1, complete, stale: false, models: web.STATIC_CATALOG.map(m => ({ ...m, discovery_status: 'found', size: { bytes: '1048576', complete }, file_count: 1 })), scopes: [], diagnostics: [], weight_size: { bytes: '1048576', complete }, started_at: '2026-11-08T00:00:00Z', finished_at: '2026-11-08T00:01:00Z' };
}
function snapshot(overrides = {}) { return { catalog: web.STATIC_CATALOG, registry: { revision: 1, registrations: {} }, project_root: 'W:\\fixture', current_inventory: 'inv', last_complete_inventory: 'inv', ...overrides }; }

test('browser initial API failure retains twelve static entries and safe text errors', async () => {
  const h = harness(async () => { throw Error('<img src=x onerror=alert(1)>'); }); await flush();
  assert.equal(h.get('models').children.length, 12);
  assert.equal(h.get('error').hidden, false);
  assert.equal(h.calls.length, 1); assert.equal(h.calls[0].headers['X-Model-Center-Token'], 'from-meta');
});
test('actual browser search/filter empty state clear and keyboard detail focus', async () => {
  const h = harness(async url => url === '/api/state' ? snapshot({ current_inventory: '', last_complete_inventory: '' }) : []); await flush();
  h.get('search').value = 'not-a-model'; await h.get('search').fire('input'); assert.equal(h.get('models').children.length, 0); assert.equal(h.get('empty').hidden, false);
  h.get('category').value = '音色处理'; await h.get('clear').fire('click');
  assert.equal(h.get('models').children.length, 12); assert.equal(h.get('category').value, ''); assert.equal(h.document.activeElement, h.get('search'));
  const detail = h.all(h.get('models')).find(n => n.tagName === 'button'); await detail.fire('click'); assert.equal(h.document.activeElement, h.get('detail-heading'));
});
test('alternative complete compositions render as safe text without implying runtime readiness', async () => {
  const inv = inventory();
  inv.models[0].component_alternatives = [['ggml-small.bin'], ['<img src=x onerror=alert(1)>', 'second.gguf']];
  const h = harness(async url => {
    if (url === '/api/state') return snapshot();
    if (url === '/api/inventories/inv') return inv;
    if (url === '/api/components') return [];
    if (url.includes('/files?')) return { files: [], next_cursor: '' };
    throw Error(url);
  });
  await flush();
  assert.match(h.get('detail-body').textContent, /任一组，同目录且非空.*ggml-small.bin/);
  assert.ok(h.get('detail-body').textContent.includes('<img src=x onerror=alert(1)> + second.gguf'));
  assert.equal(h.all(h.get('detail-body')).some(n => n.tagName === 'img'), false);
});

test('file pagination uses opaque cursor, safe long paths, clipboard failure keeps selectable text', async () => {
  const evil = 'W:\\' + 'long-directory\\'.repeat(30) + '<img src=x onerror=alert(1)>.bin';
  const h = harness(async url => {
    if (url === '/api/state') return snapshot();
    if (url === '/api/inventories/inv') return inventory();
    if (url === '/api/components') return [];
    if (url.includes('/files?')) return { files: [{ id: url.includes('cursor=next%2F%2B') ? 'second' : 'first', path: evil, bytes: '1', evidence: '<script>never execute</script>', hash: { status: 'stale', message: '历史指纹' } }], next_cursor: url.includes('cursor=next') ? '' : 'next/+' };
    throw Error(url);
  }, { writeText: async () => { throw Error('denied'); } }); await flush();
  assert.ok(h.get('files').textContent.includes(evil)); assert.equal(h.all(h.get('files')).some(n => n.tagName === 'img'), false);
  await h.find(h.get('files'), '复制路径').fire('click'); assert.match(h.get('notice').textContent, /复制失败/); assert.equal(h.document.activeElement.textContent, evil);
  await h.find(h.get('files'), '下一页').fire('click'); assert.ok(h.calls.some(c => c.url.endsWith('cursor=next%2F%2B&limit=200')));
  assert.match(h.get('files').textContent, /第 2 页/); assert.match(h.get('files').textContent, /已过期/);
});
test('runtime metadata uses actual Chinese statuses and text-only paths without execution or delivery claims', async () => {
  const evil = 'W:\\sidecar\\<img src=x onerror=alert(1)>\\Scripts\\python.exe';
  const message = '<script>metadata only</script>';
  const cases = [
    ['found', '已发现文件', '21'], ['empty_file', '空文件', '0'],
    ['missing', '未发现', null], ['unavailable', '不可访问', null], ['not_checked', '未检查', null]
  ];
  const inv = inventory();
  inv.models[0].dependency_status = 'not_applicable';
  inv.runtime_environments = cases.map(([status, , bytes]) => ({
    scope_id: status, kind: 'development_venv', interpreter_path: evil, interpreter_status: status,
    interpreter_bytes: bytes, delivery_status: 'pending_build', inference: 'not_tested', message
  }));
  const h = harness(async url => {
    if (url === '/api/state') return snapshot();
    if (url === '/api/inventories/inv') return inv;
    if (url.includes('/files?')) return { files: [], next_cursor: '' };
    if (url === '/api/components') return [];
    throw Error(url);
  }); await flush();
  const environments = h.all(h.get('scopes')).find(n => n.tagName === 'section');
  assert.ok(environments);
  assert.match(environments.textContent, /开发 venv 不是已交付的二进制组件.*不执行解释器或版本命令.*不验证推理/);
  const cards = environments.children.filter(n => n.tagName === 'article');
  assert.equal(cards.length, cases.length);
  for (const [i, [, translated, bytes]] of cases.entries()) {
    const card = cards[i];
    assert.ok(card.textContent.includes(evil));
    assert.ok(card.textContent.includes(message));
    assert.ok(h.all(card).some(n => n.tagName === 'code' && n.textContent === evil));
    assert.ok(h.all(card).some(n => n.tagName === 'dd' && n.textContent === translated));
    assert.match(card.textContent, /开发 venv（非交付组件）/);
    assert.match(card.textContent, /交付状态待构建 \/ 未交付/);
    assert.match(card.textContent, /推理验证未验证/);
    assert.ok(card.textContent.includes(bytes == null ? '未知 / 未测量' : `精确 ${bytes} 字节`));
  }
  assert.match(h.get('detail-body').textContent, /依赖：不适用/);
  assert.equal(h.all(environments).some(n => ['img', 'script'].includes(n.tagName)), false);
  assert.ok(h.calls.every(c => c.method === 'GET'));
  delete inv.runtime_environments;
  await h.get('reload').fire('click');
  assert.doesNotMatch(h.get('scopes').textContent, /运行环境元数据|开发 venv/);
  inv.runtime_environments = [];
  await h.get('reload').fire('click');
  assert.doesNotMatch(h.get('scopes').textContent, /运行环境元数据/);
  inv.runtime_environments = [{ kind: 'development_venv' }];
  await h.get('reload').fire('click');
  assert.match(h.get('scopes').textContent, /交付状态未检查.*推理验证未验证/);
  assert.doesNotMatch(h.get('scopes').textContent, /交付状态已交付|推理验证已通过/);
});
test('content equality requires an explicit group and current valid hash evidence, never duplicate candidates', () => {
  const file = { id: 'a', bytes: '21', link_kind: 'none', content_equal: 'inventory-local-group',
    hash: { file_id: 'a', bytes: '21', status: 'fingerprint_computed', sha256: 'a'.repeat(64) } };
  assert.match(web.contentEqualText(file, { stale: false }), /当前有效 SHA-256 摘要和长度相同（21 字节）/);
  assert.equal(web.contentEqualText(file, null), '');
  assert.equal(web.contentEqualText(file, { stale: true }), '');
  for (const changed of [
    { content_equal: '' }, { content_equal: undefined, duplicate_candidate: true }, { hash: null },
    { link_kind: 'symlink' }, { link_kind: undefined }, { bytes: 'unknown' },
    ...['stale', 'not_checked', 'cancelled', 'failed'].map(status => ({ hash: { ...file.hash, status } })),
    ...[{ file_id: 'other' }, { bytes: '20' }, { sha256: '' }, { sha256: 'g'.repeat(64) }, { sha256: 'a'.repeat(63) }].map(hash => ({ hash: { ...file.hash, ...hash } }))
  ]) assert.equal(web.contentEqualText({ ...file, ...changed }, { stale: false }), '', JSON.stringify(changed));
});
test('file content groups render per file as text; ordinary copies remain separate and no safe deletion is claimed', async () => {
  const evil = '<img src=x onerror=alert(1)>', group = `<script>${'g'.repeat(64)}</script>`;
  let stale = false;
  const files = ['first', 'second', 'candidate', 'unchecked', 'historical', 'no-group'].map(id => ({
    id, path: `W:\\${evil}\\${id}.bin`, bytes: '21', link_kind: 'none',
    content_equal: ['candidate', 'no-group'].includes(id) ? '' : group, duplicate_candidate: true,
    hash: id === 'unchecked' ? undefined : { file_id: id, bytes: '21', status: id === 'historical' ? 'stale' : 'fingerprint_computed', sha256: 'a'.repeat(64) }
  }));
  const h = harness(async url => {
    if (url === '/api/state') return snapshot();
    if (url === '/api/inventories/inv') return { ...inventory(), stale };
    if (url.includes('/files?')) return { files, next_cursor: '' };
    if (url === '/api/components') return [];
    throw Error(url);
  }); await flush();
  const cards = h.get('files').children.filter(n => n.tagName === 'article');
  assert.equal(cards.length, files.length);
  for (const [i, card] of cards.entries()) {
    assert.ok(card.textContent.includes(files[i].path));
    if (i < 2) {
      assert.ok(card.textContent.includes(group));
      assert.match(card.textContent, /当前有效 SHA-256 摘要和长度相同（21 字节）/);
      assert.match(card.textContent, /普通副本仍分别计量，不合并；不承诺可以安全删除/);
      assert.match(card.textContent, /不表示可节省的空间/);
    } else assert.doesNotMatch(card.textContent, /当前盘点内容组|摘要和长度相同|安全删除/);
  }
  assert.equal(h.all(h.get('files')).some(n => ['img', 'script'].includes(n.tagName)), false);
  assert.ok(h.calls.every(c => c.method === 'GET'));
  stale = true;
  await h.get('reload').fire('click');
  assert.doesNotMatch(h.get('files').textContent, /当前盘点内容组|摘要和长度相同/);
});
test('storage_error diagnostics permit repair PUT/DELETE; frozen writes stay guarded until state clears', async () => {
  const cases = [
    ['', false], ['registration_unavailable: whisper-asr: <b>missing</b>', false],
    ['registration_changed: whisper-asr: 入口变化; registration_unavailable: campplus: missing', false],
    ['registration_unavailable: W:\\registry_corrupt: 路径诊断不是冻结', false],
    ['registry_write_failed: temporary', false],
    ['registry_corrupt: <b>损坏</b>', true], ['registry_external_change: 磁盘已被改写', true],
    ['registry_commit_uncertain: 提交结果不确定', true], ['external_change: 冻结', true], ['commit_uncertain: 冻结', true]
  ];
  for (const [initialError, frozen] of cases) for (const method of ['PUT', 'DELETE']) {
    const registration = { input_path: 'W:\\old.bin', resolved_path: 'W:\\old.bin' };
    let storage_error = initialError;
    let registry = { revision: 1, registrations: { 'whisper-asr': registration } };
    const h = harness(async (url, options) => {
      if (url === '/api/state') return snapshot({ registry, storage_error });
      if (url === '/api/inventories/inv') return inventory();
      if (url === '/api/components') return [{ component_id: 'demucs-runner', version: 'v1', packaging: 'pyinstaller-onedir' }];
      if (url.includes('/files?')) return { files: [{ id: 'f', path: 'W:\\old.bin', bytes: '1' }], next_cursor: '' };
      if (url.startsWith('/api/registrations/')) {
        registry = { revision: 2, registrations: method === 'DELETE' ? {} : { 'whisper-asr': { input_path: 'W:\\new.bin' } } };
        storage_error = undefined; // Omitted by the real state JSON after repair; not present in the registry response.
        return registry;
      }
      if (url === '/api/operations/scan') return { id: 'scan2', kind: 'scan', status: 'running' };
      if (url === '/api/operations/scan2') return { id: 'scan2', kind: 'scan', status: 'running' };
      throw Error(url + options.method);
    }); await flush();
    const save = () => h.find(h.get('detail-body'), '保存登记并新扫描');
    const remove = () => h.find(h.get('detail-body'), '取消中心登记并新扫描');
    const verify = () => h.find(h.get('files'), '计算所选 SHA-256');
    const component = () => h.find(h.get('components'), '显式校验组件清单');
    const diagnostic = h.get('storage-diagnostic');
    assert.equal(diagnostic.hidden, !initialError, initialError);
    if (initialError) {
      assert.ok(diagnostic.textContent.includes(initialError));
      assert.match(diagnostic.textContent, frozen ? /存储已冻结/ : /可重新登记.*DELETE/);
      assert.equal(h.all(diagnostic).some(n => n.tagName === 'b'), false);
    }
    for (const control of [h.get('scan'), save(), remove(), verify(), component()]) assert.equal(control.disabled, frozen, initialError);
    assert.equal(h.get('reload').disabled, false);
    if (frozen) {
      // Bypass disabled presentation to verify the actual submission guards too.
      await h.all(h.get('detail-body')).find(n => n.tagName === 'form').fire('submit');
      await remove().listeners.click[0]();
      await h.get('scan').listeners.click[0]();
      await h.find(h.get('files'), '选择当前页').fire('click');
      await verify().listeners.click[0]();
      await component().listeners.click[0]();
      await flush();
      assert.equal(h.calls.filter(c => c.method !== 'GET').length, 0, initialError);
      await h.get('reload').fire('click');
      assert.equal(save().disabled, true); assert.ok(diagnostic.textContent.includes(initialError));
      storage_error = undefined;
      await h.get('reload').fire('click');
      assert.equal(diagnostic.hidden, true); assert.equal(diagnostic.textContent, '');
      for (const control of [h.get('scan'), save(), remove(), verify(), component()]) assert.equal(control.disabled, false);
    }
    if (method === 'PUT') { h.get('registration-path').value = 'W:\\new.bin'; await h.get('registration-path').fire('input'); const form = h.all(h.get('detail-body')).find(n => n.tagName === 'form'); await form.fire('submit'); }
    else await remove().fire('click');
    const write = h.calls.find(c => c.method === method); assert.ok(write); assert.equal(write.parsed.expected_revision, 1);
    assert.equal(h.calls.filter(c => c.method === method).length, 1);
    if (method === 'PUT') assert.equal(write.parsed.path, 'W:\\new.bin');
    const scan = h.calls.find(c => c.url === '/api/operations/scan'); assert.equal(scan.parsed.registry_revision, 2);
    assert.ok(h.calls.slice(h.calls.indexOf(write) + 1, h.calls.indexOf(scan)).some(c => c.url === '/api/state'));
    assert.equal(diagnostic.hidden, true); assert.equal(diagnostic.textContent, '');
    assert.match(h.get('history').textContent, /登记版本 2/);
    assert.equal(!!remove(), method !== 'DELETE');
  }
});
test('ambiguous registration never retries; reconciled storage_error freezes writes without losing drafts', async () => {
  for (const reconciledError of ['', 'registry_commit_uncertain: 无法确认提交', 'registration_changed: whisper-asr: 入口变化']) {
    let attempted = false;
    const h = harness(async (url, options) => {
      if (url === '/api/state') return snapshot({ storage_error: attempted ? reconciledError : '' });
      if (url === '/api/inventories/inv') return inventory();
      if (url === '/api/components') return [];
      if (url.includes('/files?')) return { files: [], next_cursor: '' };
      if (options.method === 'PUT') { attempted = true; throw Error('connection lost after commit'); }
      throw Error(url);
    }); await flush();
    h.get('registration-path').value = 'W:\\keep-my-input.bin'; await h.get('registration-path').fire('input');
    await h.all(h.get('detail-body')).find(n => n.tagName === 'form').fire('submit');
    assert.equal(h.calls.filter(c => c.method === 'PUT').length, 1);
    assert.equal(h.calls.filter(c => c.url === '/api/state').length, 2);
    assert.equal(h.calls.filter(c => c.url === '/api/operations/scan').length, 0);
    assert.equal(h.get('registration-path').value, 'W:\\keep-my-input.bin'); assert.match(h.get('notice').textContent, /没有重发/);
    assert.equal(h.find(h.get('detail-body'), '保存登记并新扫描').disabled, reconciledError.startsWith('registry_commit_uncertain'));
    assert.equal(h.get('storage-diagnostic').hidden, !reconciledError);
    if (reconciledError) assert.ok(h.get('storage-diagnostic').textContent.includes(reconciledError));
    assert.match(h.get('error').textContent, /network_error/);
  }
});
test('partial inventory and failed attempt preserve last complete reference and diagnostics', async () => {
  const h = harness(async url => {
    if (url === '/api/state') return snapshot({ current_inventory: 'partial', last_complete_inventory: 'complete', last_attempt: { kind: 'scan', status: 'failed', diagnostics: [{ code: 'scan_limit', message: '<b>部分保留</b>' }] } });
    if (url === '/api/inventories/partial') return inventory('partial', false);
    if (url === '/api/inventories/complete') return inventory('complete');
    if (url.includes('/files?')) return { files: [], next_cursor: '' };
    return [];
  }); await flush();
  assert.match(h.get('inventory-meta').textContent, /部分结果/); assert.match(h.get('history').textContent, /complete/);
  assert.match(h.get('scopes').textContent, /<b>部分保留<\/b>/);
  await h.find(h.get('history-actions'), '查看上次完整盘点').fire('click'); assert.match(h.get('inventory-meta').textContent, /完整盘点/);
});
test('component evidence has honest no-data states; explicit verifier sends only ID/version', async () => {
  let evidence = null;
  const h = harness(async url => {
    if (url === '/api/state') return snapshot({ current_inventory: '', last_complete_inventory: '' });
    if (url === '/api/components') return evidence;
    if (url === '/api/operations/verify-component') return { id: 'component-check', kind: 'verify-component', status: 'running' };
    if (url === '/api/operations/component-check') return { id: 'component-check', kind: 'verify-component', status: 'running' };
    throw Error(url);
  }); await flush();
  for (const emptyEvidence of [null, []]) {
    evidence = emptyEvidence;
    await h.get('reload').fire('click');
    assert.match(h.get('components').textContent, /没有取得组件交付证据.*未交付.*未验证/);
    assert.equal(h.all(h.get('components')).filter(n => n.tagName === 'button').length, 0);
    assert.match(h.get('stats').textContent, /运行环境逻辑大小未知 \/ 未测量/);
    assert.match(h.get('detail-body').textContent, /尚无盘点/);
    assert.equal(h.get('error').hidden, true);
  }
  evidence = [
    { component_id: 'whisper-stream', packaging: 'pending_build', files_state: 'missing', inference: 'not_tested' },
    { component_id: 'demucs-runner', version: 'v1', packaging: 'pyinstaller-onedir', integrity: 'not_checked', startup_test: 'passed', inference: 'not_tested' }
  ];
  await h.get('reload').fire('click');
  assert.equal(h.all(h.get('components')).filter(n => n.tagName === 'button').length, 1);
  await h.find(h.get('components'), '显式校验组件清单').fire('click');
  assert.deepEqual(h.calls.find(c => c.url === '/api/operations/verify-component').parsed, { component_id: 'demucs-runner', version: 'v1' });
  assert.match(h.get('components').textContent, /未交付/); assert.match(h.get('components').textContent, /未验证/);
  assert.match(h.get('components').textContent, /入口启动测试已通过（仅入口测试）/);
  assert.doesNotMatch(h.get('components').textContent, /推理验证已通过|\bpassed\b/);
});

test('component manifest details are text-only declarations, including malicious values and no execution links', async () => {
  const evil = '<img src=x onerror=alert(1)>', script = '<script>alert(1)</script>';
  const manifest = {
    schema: 1, os: evil, arch: script, entry_type: evil, entry: `bin/${evil}.exe`,
    files: [{ path: `runtime/${evil}.dll`, bytes: '9007199254740993', sha256: script }],
    runtime_dependencies: [{ component_id: evil, version: script }], model_refs: [evil, script],
    protocols: [script], driver_requirements: ['javascript:alert(1)'],
    build_inputs: [{ name: evil, version: script, sha256: evil, license: script }], startup_report_ref: `reports/${evil}.json`
  };
  let components = [{ component_id: 'seedvc-runner', version: '1', packaging: 'pyinstaller-onedir', manifest }];
  const h = harness(async url => {
    if (url === '/api/state') return snapshot({ current_inventory: '', last_complete_inventory: '' });
    if (url === '/api/components') return components;
    throw Error(url);
  }); await flush();
  const card = h.get('components').children[0], details = card.children.find(n => n.tagName === 'details');
  assert.ok(details);
  for (const value of [evil, script, manifest.entry, manifest.files[0].path, manifest.files[0].bytes, manifest.startup_report_ref, manifest.driver_requirements[0]]) assert.ok(details.textContent.includes(value), value);
  for (const title of ['声明平台 / 架构', '入口类型', '入口（组件根相对路径）', '交付文件 / 随包运行库', '运行库组件依赖', '权重引用', '协议声明（非实测）', '外部驱动要求', '构建输入（非供应链认证）', '入口测试报告引用']) assert.ok(details.textContent.includes(title), title);
  assert.match(details.textContent, /声明不等于验证/);
  assert.match(details.textContent, /不加载库.*权重引用不代表权重已发现或兼容/);
  assert.equal(h.all(details).some(n => ['img', 'script', 'svg', 'a', 'button', 'input', 'form'].includes(n.tagName)), false);
  assert.match(card.textContent, /按清单完整性未检查.*入口启动测试未检查.*推理验证未检查/);
  assert.ok(h.calls.every(c => c.method === 'GET'));
  // Empty optional fields are not interpreted as no dependencies, zero bytes or readiness.
  components = [{ component_id: 'whisper-batch', version: '1', packaging: 'native', manifest: {
    schema: 1, os: '', arch: null, entry: '', entry_type: '', files: [], runtime_dependencies: null,
    model_refs: [], protocols: [], driver_requirements: null, build_inputs: []
  } }];
  await h.get('reload').fire('click');
  assert.match(h.get('components').textContent, /入口（组件根相对路径）未声明/);
  assert.equal(h.all(h.get('components')).filter(n => n.tagName === 'p' && n.textContent === '未声明').length, 6);
  assert.doesNotMatch(h.get('components').textContent, /undefined|null|已就绪|无独立依赖/);
  // Invalid and undelivered records never display even an erroneously attached declaration.
  for (const packaging of ['invalid_manifest', 'pending_build']) {
    components = [{ component_id: 'whisper-stream', version: '1', packaging, manifest }];
    await h.get('reload').fire('click');
    assert.doesNotMatch(h.get('components').textContent, /onerror|9007199254740993/);
    assert.match(h.get('components').textContent, packaging === 'invalid_manifest' ? /清单无效；不展示/ : /未交付.*未声明/);
    assert.equal(h.all(h.get('components')).some(n => n.tagName === 'button'), false);
  }
  for (const manifest of [undefined, null]) {
    components = [{ component_id: 'whisper-batch', packaging: 'pending_build', manifest }];
    await h.get('reload').fire('click');
    assert.match(h.get('components').textContent, /未取得有效清单.*入口、运行库及权重引用未声明/);
  }
  assert.ok(h.calls.every(c => c.method === 'GET'));
});

test('actual verify and cancel workflow uses current IDs, empty cancel body, terminal polling stops', async () => {
  let cancelled = false;
  const h = harness(async (url, options) => {
    if (url === '/api/state') return snapshot();
    if (url === '/api/inventories/inv') return inventory();
    if (url === '/api/components') return [];
    if (url.includes('/files?')) return { files: [{ id: 'file-current', path: 'W:\\safe.bin', bytes: '1' }], next_cursor: '' };
    if (url === '/api/operations/verify') return { id: 'v', kind: 'verify', status: 'running' };
    if (url === '/api/operations/v/cancel') { cancelled = true; return { id: 'v', kind: 'verify', status: 'cancelling' }; }
    if (url === '/api/operations/v') return { id: 'v', kind: 'verify', status: cancelled ? 'cancelled' : 'running', hashes: [{ file_id: 'file-current', status: 'cancelled', message: '用户取消' }] };
    throw Error(url + options.method);
  }); await flush();
  await h.find(h.get('files'), '选择当前页').fire('click'); await h.find(h.get('files'), '计算所选 SHA-256').fire('click');
  const verify = h.calls.find(c => c.url === '/api/operations/verify'); assert.deepEqual(verify.parsed, { inventory_id: 'inv', model_id: 'whisper-asr', file_ids: ['file-current'] });
  await h.get('cancel').fire('click'); const cancel = h.calls.find(c => c.url.endsWith('/cancel')); assert.deepEqual(cancel.parsed, {});
  assert.match(h.get('notice').textContent, /已取消/); assert.match(h.get('files').textContent, /用户取消/);
});
