'use strict';

// Run with: node --test tests/ui-contract.test.cjs
// Exercises the real bundled bridge and app in an isolated VM. It uses no network,
// browser executable, external test framework, or administrator credentials.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const ui = path.resolve(__dirname, '../ui');
const html = fs.readFileSync(path.join(ui, 'index.html'), 'utf8');
const bridgeSource = fs.readFileSync(path.join(ui, 'assets/bridge-v1.js'), 'utf8');
const appSource = fs.readFileSync(path.join(ui, 'assets/app.js'), 'utf8');
const MiB = 1024 * 1024;
const defaults = {
  compression_enabled: true, min_body_bytes: 1024, max_body_bytes: 512 * MiB,
  max_in_flight: 2, fallback_on_415: true, response_header_timeout_seconds: 600,
  proxy_mode: 'account',
  image_enabled: false,
  image_resize_mode: 'fit1080',
  image_jpeg_quality: 86,
  image_min_savings_bytes: 32768
};
const snapshot = {
  version: '1.1.0', algorithm: 'zstd', level: 3, config: defaults,
  stats: { total_requests: 3, compressed_requests: 2, original_bytes: 1000,
    wire_bytes: 1250, saved_bytes: -250, saving_percent: -25, fallbacks: 1 },
  cooldown_until: '', started_at: '2026-11-03T08:00:00Z'
};
const plain = value => JSON.parse(JSON.stringify(value));
const settle = () => new Promise(resolve => setImmediate(resolve));

function harness(options = {}) {
  const listeners = new Map(), timers = new Map(), intervals = new Map();
  const elements = new Map(), messages = [];
  let timerID = 0, storedConfig = plain(options.config || defaults);
  function makeElement(id, attrs = {}) {
    const events = new Map();
    let value = attrs.value || '';
    return {
      id, attrs, checked: 'checked' in attrs, disabled: 'disabled' in attrs,
      hidden: 'hidden' in attrs, textContent: '', className: '', scrollHeight: 900,
      get value() { return value; }, set value(next) { value = String(next); },
      set innerHTML(_) { throw new Error('UI must not interpolate host data as HTML'); },
      addEventListener(type, callback) { if (!events.has(type)) events.set(type, []); events.get(type).push(callback); },
      setAttribute(key, next) { attrs[key] = String(next); },
      focus() { this.focused = true; },
      fire(type) {
        if (this.disabled && type === 'click') return;
        const event = { target: this, preventDefault() { this.defaultPrevented = true; } };
        for (const callback of events.get(type) || []) callback(event);
      },
      getBoundingClientRect() { return { height: 900 }; },
      reportValidity() {
        for (const element of elements.values()) {
          if (element.attrs.type !== 'number') continue;
          const n = Number(element.value);
          if (!element.value || !Number.isFinite(n)) return false;
          if (element.attrs.min && n < Number(element.attrs.min)) return false;
          if (element.attrs.max && n > Number(element.attrs.max)) return false;
          if (element.attrs.step === '1' && !Number.isInteger(n)) return false;
        }
        return true;
      }
    };
  }
  for (const match of html.matchAll(/<([a-z]+)\b([^>]*\bid="([^"]+)"[^>]*)>/g)) {
    const attrs = {};
    for (const a of match[2].matchAll(/([a-z][a-z_-]*)(?:="([^"]*)")?/g)) attrs[a[1]] = a[2] === undefined ? '' : a[2];
    elements.set(match[3], makeElement(match[3], attrs));
  }
  const shell = makeElement('shell');
  const window = {
    location: { hash: options.missingToken ? '' : '#bridge_token=contract-token' },
    addEventListener(type, callback) { if (!listeners.has(type)) listeners.set(type, new Set()); listeners.get(type).add(callback); },
    removeEventListener(type, callback) { listeners.get(type)?.delete(callback); },
    setTimeout(callback) { timers.set(++timerID, callback); return timerID; },
    clearTimeout(id) { timers.delete(id); },
    setInterval(callback) { intervals.set(++timerID, callback); return timerID; },
    clearInterval(id) { intervals.delete(id); }
  };
  const dispatch = (type, event) => { for (const fn of listeners.get(type) || []) fn(event); };
  function reply(request, data = {}, source = window.parent) {
    dispatch('message', { source, data: {
      source: 'sub2api-plugin-host', bridge_token: 'contract-token',
      request_id: request.request_id, ok: true, ...data
    } });
  }
  window.parent = {
    postMessage(message, targetOrigin) {
      assert.equal(targetOrigin, '*'); // Opaque sandbox origins authenticate the window and bridge token.
      const request = plain(message);
      messages.push(request);
      if (!options.autoReply || !request.request_id) return;
      queueMicrotask(() => {
        const override = options.handlers?.[request.type];
        if (override) { const value = override(request); if (value !== undefined) reply(request, value); return; }
        if (request.type === 'config.load') reply(request, { config: storedConfig });
        if (request.type === 'config.save') { storedConfig = request.config; reply(request, { config: storedConfig }); }
        if (request.type === 'plugin.status') reply(request, { result: { healthy: true, status_json: JSON.stringify(snapshot) } });
        if (request.type === 'config.test') reply(request, { result: { success: true, message: '本地压缩自检通过', status_json: JSON.stringify(snapshot) } });
      });
    }
  };
  const document = {
    getElementById(id) { assert.ok(elements.has(id), `HTML must declare ${id}`); return elements.get(id); },
    querySelector(selector) { assert.equal(selector, '.shell'); return shell; },
    documentElement: shell
  };
  window.document = document;
  const context = vm.createContext({ window, document });
  vm.runInContext(bridgeSource, context, { filename: 'bridge-v1.js' });
  return {
    window, elements, messages, timers, intervals, listeners, reply,
    runApp() { vm.runInContext(appSource, context, { filename: 'app.js' }); },
    createBridge() { return window.Sub2ApiPluginBridge.create(); },
    expireTimeouts() { const callbacks = [...timers.values()]; timers.clear(); callbacks.forEach(fn => fn()); },
    disposeApp() { dispatch('pagehide', {}); },
    callTypes() { return messages.filter(m => m.request_id).map(m => m.type); }
  };
}

test('bridge authenticates parent, source, token and pending request ID', async () => {
  const h = harness(), bridge = h.createBridge();
  bridge.ready();
  assert.equal(h.messages[0].type, 'sub2api.plugin.ready');
  assert.equal(h.messages[0].source, 'sub2api-plugin-ui');
  let resolved = false;
  const pending = bridge.loadConfig().then(value => { resolved = true; return value; });
  const request = h.messages.at(-1);
  assert.ok(request.request_id);
  h.reply(request, { config: defaults }, {});
  h.reply(request, { source: 'wrong-host', config: defaults });
  h.reply(request, { bridge_token: 'wrong-token', config: defaults });
  h.reply(request, { request_id: 'stale-id', config: defaults });
  await settle();
  assert.equal(resolved, false);
  h.reply(request, { config: defaults });
  assert.deepEqual(plain(await pending), defaults);
  assert.equal(h.timers.size, 0);
  bridge.dispose();
});

test('bridge timeouts and disposal reject pending calls and remove listeners', async () => {
  const h = harness(), bridge = h.createBridge();
  const first = bridge.loadConfig();
  h.expireTimeouts();
  await assert.rejects(first, error => error.name === 'BridgeTimeoutError' && error.type === 'config.load');
  const second = bridge.status();
  bridge.dispose();
  await assert.rejects(second, /bridge disposed/);
  assert.equal(h.timers.size, 0);
  assert.equal(h.listeners.get('message').size, 0);
  assert.throws(() => harness({ missingToken: true }).createBridge(), /bridge_token missing/);
});

test('app loads saved configuration and read-only status automatically; negative savings remain visible', async () => {
  const h = harness({ autoReply: true }); h.runApp(); await settle();
  assert.deepEqual(h.callTypes(), ['config.load', 'plugin.status']);
  assert.equal(h.elements.get('max_body_mib').value, '512');
  assert.equal(h.elements.get('save').disabled, false);
  assert.equal(h.elements.get('stat-compressed').textContent, '2');
  assert.equal(h.elements.get('stat-saved').textContent, '-25.0%');
  assert.equal(h.elements.get('status-details').hidden, false);
  assert.match(html, /压缩请求体估算/);
  assert.match(html, /不是系统实测流量/);
  assert.match(html, /失败尝试可能未完整发送/);
  h.disposeApp();
  assert.equal(h.intervals.size, 0);
});

test('app saves exact byte units, accepts normalized config, and refreshes without saving edits', async () => {
  const h = harness({ autoReply: true }); h.runApp(); await settle();
  h.elements.get('max_body_mib').value = '8';
  h.elements.get('max_in_flight').value = '4';
  h.elements.get('config-form').fire('input');
  assert.match(h.elements.get('save-state').textContent, /未保存/);
  h.elements.get('config-form').fire('submit'); await settle();
  const save = h.messages.find(m => m.type === 'config.save');
  assert.equal(save.config.max_body_bytes, 8 * MiB);
  assert.equal(save.config.max_in_flight, 4);
  assert.equal(h.elements.get('save-state').textContent, '配置已同步');
  h.elements.get('min_body_bytes').value = '2048';
  h.elements.get('config-form').fire('input');
  const before = h.callTypes().length;
  h.elements.get('refresh').fire('click'); await settle();
  assert.deepEqual(h.callTypes().slice(before), ['plugin.status']);
  assert.equal(h.elements.get('min_body_bytes').value, '2048');
  assert.match(h.elements.get('save-state').textContent, /未保存/);
  h.disposeApp();
});

test('local test saves edits first; failed save never tests stale configuration', async () => {
  const h = harness({ autoReply: true }); h.runApp(); await settle();
  h.elements.get('min_body_bytes').value = '2048';
  h.elements.get('test').fire('click'); await settle();
  assert.deepEqual(h.callTypes().slice(-2), ['config.save', 'config.test']);
  assert.match(h.elements.get('notice').textContent, /尚未进行 OpenAI 上游实测/);
  h.disposeApp();
  const rejected = harness({ autoReply: true, handlers: { 'config.save': () => ({ ok: false, error: '验证未通过' }) } });
  rejected.runApp(); await settle();
  rejected.elements.get('min_body_bytes').value = '2048';
  rejected.elements.get('test').fire('click'); await settle();
  assert.equal(rejected.callTypes().includes('config.test'), false);
  assert.match(rejected.elements.get('notice').textContent, /验证未通过/);
  assert.equal(rejected.elements.get('save').disabled, false);
  rejected.disposeApp();
});

test('large and unlimited limits save without the former aggregate budget', async () => {
  for (const size of ['512', '1024', '0']) {
    const h = harness({ autoReply: true }); h.runApp(); await settle();
    h.elements.get('max_body_mib').value = size;
    h.elements.get('max_in_flight').value = '4';
    h.elements.get('config-form').fire('submit'); await settle();
    const saved = h.messages.find(m => m.type === 'config.save');
    assert.ok(saved, 'large/unlimited limit must be saveable');
    assert.equal(saved.config.max_body_bytes, Number(size) * MiB);
    assert.equal(h.elements.get('max_body_mib').value, size);
    h.disposeApp();
  }
});

test('old image settings migrate while the saved zstd cap stays at 16 MiB', async () => {
  const oldConfig = {...defaults, max_body_bytes:16 * MiB, image_enabled:true, image_jpeg_quality:92,
    image_max_long_edge:0, image_min_savings_percent:25, image_min_savings_bytes:262144};
  delete oldConfig.image_resize_mode;
  const h = harness({autoReply:true, config:oldConfig}); h.runApp(); await settle();
  assert.equal(h.elements.get('max_body_mib').value, '16');
  assert.equal(h.elements.get('image_enabled').checked, true);
  assert.equal(h.elements.get('image_resize_mode').value, 'fit1080');
  assert.equal(h.elements.get('image_jpeg_quality').value, '86');
  assert.equal(h.elements.get('image_min_savings_kib').value, '32');
  assert.equal(h.callTypes().includes('config.save'), false);
  h.elements.get('config-form').fire('submit'); await settle();
  const saved = h.messages.find(m => m.type === 'config.save').config;
  assert.deepEqual(saved, {...defaults, max_body_bytes:16 * MiB, image_enabled:true});
  h.disposeApp();
});

test('changing size presets switches default quality and preserves a custom quality', async () => {
  const h = harness({autoReply:true}); h.runApp(); await settle();
  const mode = h.elements.get('image_resize_mode');
  const quality = h.elements.get('image_jpeg_quality');
  mode.value = 'fit1440'; mode.fire('change');
  assert.equal(quality.value, '92');
  mode.value = 'original'; mode.fire('change');
  assert.equal(quality.value, '86');
  quality.value = '79';
  mode.value = 'fit1440'; mode.fire('change');
  assert.equal(quality.value, '79');
  h.elements.get('image_min_savings_kib').value = '0';
  h.elements.get('config-form').fire('submit'); await settle();
  const saved = h.messages.find(m => m.type === 'config.save').config;
  assert.deepEqual(saved, {...defaults, image_resize_mode:'fit1440', image_jpeg_quality:79, image_min_savings_bytes:0});
  h.disposeApp();
});

test('initial load failure leaves retry available and a later load restores the form', async () => {
  let loads = 0;
  const h = harness({ autoReply: true, handlers: { 'config.load': () => ++loads === 1
    ? { ok: false, error: '临时读取失败' } : { config: defaults } } });
  h.runApp(); await settle();
  assert.equal(h.elements.get('reload').disabled, false);
  assert.equal(h.elements.get('save').disabled, true);
  h.elements.get('reload').fire('click'); await settle();
  assert.equal(h.elements.get('save').disabled, false);
  assert.equal(h.elements.get('stat-compressed').textContent, '2');
  h.disposeApp();
});

test('invalid status clears stale cards and never announces successful refresh', async () => {
  let calls = 0;
  const h = harness({ autoReply: true, handlers: { 'plugin.status': () => ({ result: {
    healthy: true, status_json: ++calls === 1 ? JSON.stringify(snapshot) : '[]'
  } }) } });
  h.runApp(); await settle();
  assert.equal(h.elements.get('stat-compressed').textContent, '2');
  h.elements.get('refresh').fire('click'); await settle();
  assert.equal(h.elements.get('stat-compressed').textContent, '—');
  assert.equal(h.elements.get('status-details').hidden, true);
  assert.match(h.elements.get('notice').textContent, /没有返回可解析的统计/);
  h.disposeApp();
});

test('save timeout tells the user to reconcile actual state and leaves the form usable', async () => {
  const h = harness({ autoReply: true, handlers: { 'config.save': () => undefined } });
  h.runApp(); await settle();
  h.elements.get('config-form').fire('submit'); await settle();
  assert.equal(h.elements.get('save').disabled, true);
  h.expireTimeouts(); await settle();
  assert.match(h.elements.get('notice').textContent, /核对实际保存结果/);
  assert.equal(h.elements.get('reload').disabled, false);
  assert.equal(h.elements.get('save').disabled, false);
  h.disposeApp();
});
