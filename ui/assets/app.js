(function () {
  'use strict';

  var MiB = 1024 * 1024;
  var defaults = {
    compression_enabled: true,
    min_body_bytes: 1024,
    max_body_bytes: 512 * MiB,
    max_in_flight: 4,
    zstd_level: 5,
    zstd_window_mib: 8,
    fallback_on_415: true,
    response_header_timeout_seconds: 600,
    proxy_mode: 'account',
    image_enabled: false,
    image_max_body_bytes: 0,
    image_resize_mode: 'fit1080',
    image_tool_resize_mode: 'fit1440',
    image_jpeg_quality: 78,
    image_min_savings_bytes: 8 * 1024,
    image_workers: 0,
    image_timeout_seconds: 30,
    image_png_background: 'white'
  };
  var bridge;
  var savedConfig = null;
  var busy = false;
  var disposed = false;
  var stopResize = null;
  var elements = {};
  var ids = [
    'notice', 'config-form', 'config-fields', 'compression_enabled',
    'min_body_bytes', 'max_body_mib', 'max_in_flight', 'zstd_level', 'zstd_window_mib', 'fallback_on_415',
    'response_header_timeout_seconds', 'proxy_mode', 'proxy-help',
    'reload', 'save', 'test', 'refresh', 'memory-budget', 'save-state', 'stats-time', 'test-message',
    'stat-compressed', 'stat-before', 'stat-after', 'stat-saved',
    'status-details', 'status-json',
    'image_enabled', 'image_max_body_mib', 'image_resize_mode', 'image_tool_resize_mode',
    'image_jpeg_quality', 'image_min_savings_kib', 'image_workers', 'image_timeout_seconds', 'image_png_background',
    'stat-image-before', 'stat-image-after', 'stat-image-saved', 'stat-image-wire',
    'stat-image-hit', 'stat-image-count', 'stat-image-replaced', 'stat-image-time',
    'image-skips', 'recent-empty', 'recent-table', 'recent-rows'
  ];
  ids.forEach(function (id) { elements[id] = document.getElementById(id); });

  function notice(message, kind) {
    elements.notice.textContent = message;
    elements.notice.className = 'notice' + (kind ? ' ' + kind : '');
  }

  function configWithDefaults(config) {
    if (!config || typeof config !== 'object' || Array.isArray(config)) {
      throw new Error('宿主返回的配置格式无效。');
    }
    var normalized = {};
    Object.keys(defaults).forEach(function (key) {
      normalized[key] = config[key] === undefined ? defaults[key] : config[key];
    });
    var legacy = config.image_resize_mode === undefined &&
      (config.image_max_long_edge !== undefined || config.image_min_savings_percent !== undefined);
    if (config.image_jpeg_quality === undefined || (legacy && config.image_jpeg_quality === 92)) {
      normalized.image_jpeg_quality = defaults.image_jpeg_quality;
    }
    if (legacy) normalized.image_min_savings_bytes = defaults.image_min_savings_bytes;
    return normalized;
  }

  function formConfig() {
    return {
      compression_enabled: elements.compression_enabled.checked,
      min_body_bytes: Number(elements.min_body_bytes.value),
      max_body_bytes: Number(elements.max_body_mib.value) * MiB,
      max_in_flight: Number(elements.max_in_flight.value),
      zstd_level: Number(elements.zstd_level.value),
      zstd_window_mib: Number(elements.zstd_window_mib.value),
      fallback_on_415: elements.fallback_on_415.checked,
      response_header_timeout_seconds: Number(elements.response_header_timeout_seconds.value),
      proxy_mode: elements.proxy_mode.value,
      image_enabled: elements.image_enabled.checked,
      image_max_body_bytes: Number(elements.image_max_body_mib.value) * MiB,
      image_resize_mode: elements.image_resize_mode.value,
      image_tool_resize_mode: elements.image_tool_resize_mode.value,
      image_jpeg_quality: Number(elements.image_jpeg_quality.value),
      image_min_savings_bytes: Number(elements.image_min_savings_kib.value) * 1024,
      image_workers: Number(elements.image_workers.value),
      image_timeout_seconds: Number(elements.image_timeout_seconds.value),
      image_png_background: elements.image_png_background.value
    };
  }

  function isDirty() {
    return savedConfig !== null && JSON.stringify(formConfig()) !== JSON.stringify(savedConfig);
  }

  function updateSaveState() {
    var dirty = isDirty();
    elements['save-state'].textContent = dirty ? '有未保存修改；自检前会先保存' : '配置已同步';
    elements['save-state'].className = 'save-state' + (dirty ? ' dirty' : '');
    elements['proxy-help'].textContent = elements.proxy_mode.value === 'direct'
      ? '直接连接上游，忽略账号代理。仅在服务器能够直接访问 ChatGPT 时使用。'
      : '使用 Sub2API 传入的账号代理；未配置代理的账号直接连接。';
    var maxMiB = Number(elements.max_body_mib.value);
    var concurrency = Number(elements.max_in_flight.value);
    elements['memory-budget'].textContent = (maxMiB === 0
      ? 'Zstd 请求体不限'
      : 'Zstd 请求体上限 ' + maxMiB + ' MiB') + '，大请求准备并发 ' + concurrency + '。每个缓冲超过 8 MiB 时溢写匿名临时文件。';
    elements['memory-budget'].className = 'hint budget';
  }

  function renderConfig(config) {
    var normalized = configWithDefaults(config);
    elements.compression_enabled.checked = normalized.compression_enabled;
    elements.min_body_bytes.value = normalized.min_body_bytes;
    elements.max_body_mib.value = normalized.max_body_bytes / MiB;
    elements.max_in_flight.value = normalized.max_in_flight;
    elements.zstd_level.value = normalized.zstd_level;
    elements.zstd_window_mib.value = normalized.zstd_window_mib;
    elements.fallback_on_415.checked = normalized.fallback_on_415;
    elements.response_header_timeout_seconds.value = normalized.response_header_timeout_seconds;
    elements.proxy_mode.value = normalized.proxy_mode;
    elements.image_enabled.checked = normalized.image_enabled;
    elements.image_max_body_mib.value = normalized.image_max_body_bytes / MiB;
    elements.image_resize_mode.value = normalized.image_resize_mode;
    elements.image_tool_resize_mode.value = normalized.image_tool_resize_mode;
    elements.image_jpeg_quality.value = normalized.image_jpeg_quality;
    elements.image_min_savings_kib.value = normalized.image_min_savings_bytes / 1024;
    elements.image_workers.value = normalized.image_workers;
    elements.image_timeout_seconds.value = normalized.image_timeout_seconds;
    elements.image_png_background.value = normalized.image_png_background;
    savedConfig = normalized;
    updateSaveState();
  }

  function setBusy(value) {
    busy = value;
    elements['config-fields'].disabled = value || savedConfig === null;
    elements.reload.disabled = value || !bridge;
    elements.save.disabled = value || savedConfig === null;
    elements.test.disabled = value || savedConfig === null;
    elements.refresh.disabled = value || !bridge;
    elements['config-form'].setAttribute('aria-busy', String(value));
  }

  function readValidatedForm() {
    if (!elements['config-form'].reportValidity()) return null;
    var config = formConfig();
    var numericKeys = ['min_body_bytes', 'max_body_bytes', 'max_in_flight', 'response_header_timeout_seconds',
      'zstd_level', 'zstd_window_mib', 'image_max_body_bytes', 'image_jpeg_quality',
      'image_min_savings_bytes', 'image_workers', 'image_timeout_seconds'];
    for (var i = 0; i < numericKeys.length; i++) {
      if (!Number.isSafeInteger(config[numericKeys[i]])) {
        notice('数值设置需要填写有效整数。', 'error');
        return null;
      }
    }
    if (config.max_body_bytes !== 0 && config.min_body_bytes > config.max_body_bytes) {
      notice('最小请求体不能大于最大压缩请求体。', 'error');
      elements.min_body_bytes.focus();
      return null;
    }
    return config;
  }

  function describeError(error) {
    if (error && error.name === 'BridgeTimeoutError') {
      if (error.type === 'plugin.status' || error.type === 'config.load') {
        return '读取等待超时。请稍后再次读取配置或刷新统计。';
      }
      return '操作等待超时。宿主的管理员验证可能仍未完成；完成验证后，请点击「读取配置」核对实际保存结果。';
    }
    var message = error && typeof error.message === 'string' ? error.message : String(error || '未知错误');
    return '操作未完成：' + message.slice(0, 450);
  }

  function parseStatus(raw) {
    if (typeof raw === 'string') {
      try { raw = JSON.parse(raw); } catch (_) { return null; }
    }
    return raw && typeof raw === 'object' && !Array.isArray(raw) ? raw : null;
  }

  function numberAt(object, keys) {
    for (var i = 0; i < keys.length; i++) {
      var value = object[keys[i]];
      if (typeof value === 'number' && Number.isFinite(value) && value >= 0) return value;
      if (typeof value === 'string' && /^\d+$/.test(value) && Number.isFinite(Number(value))) return Number(value);
    }
    return null;
  }

  function bytesLabel(value) {
    if (value === null) return '—';
    if (value < 1024) return value.toLocaleString('zh-CN') + ' B';
    if (value < MiB) return (value / 1024).toFixed(1) + ' KiB';
    if (value < 1024 * MiB) return (value / MiB).toFixed(2) + ' MiB';
    return (value / (1024 * MiB)).toFixed(2) + ' GiB';
  }

  var skipLabels = {
    request_limit: '请求体超限', part_limit: '图片项超限', format_or_limit: '格式或单图大小超限',
    external_url: '外链图片', no_gain: '节省不足', invalid_data_url: 'Data URL 无效',
    invalid_jpeg: 'JPEG 无效', invalid_png: 'PNG 无效', animated_png: '动画 PNG',
    timeout: '图片处理超时', scan_timeout: '扫描超时', invalid_json: 'JSON 扫描失败',
    read_error: '图片暂时读取失败', encode_error: '编码失败'
  };

  function skipsLabel(skips) {
    if (!skips || typeof skips !== 'object') return '';
    return Object.keys(skips).filter(function (reason) {
      return numberAt(skips, [reason]) > 0;
    }).sort(function (a, b) {
      return skips[b] - skips[a] || a.localeCompare(b);
    }).map(function (reason) {
      return (Object.hasOwn(skipLabels, reason) ? skipLabels[reason] : reason) + ' ' + Number(skips[reason]).toLocaleString('zh-CN');
    }).join(' · ');
  }

  function millisecondsLabel(value) {
    return value === null ? '—' : value.toFixed(1) + ' ms';
  }

  function renderRecent(records) {
    var rows = Array.isArray(records) ? records.filter(function (record) {
      return record && typeof record === 'object';
    }).slice(0, 30) : [];
    elements['recent-rows'].replaceChildren();
    elements['recent-table'].hidden = rows.length === 0;
    elements['recent-empty'].hidden = rows.length !== 0;
    rows.forEach(function (record) {
      var row = document.createElement('tr');
      function cell(text) {
        var td = document.createElement('td');
        td.textContent = text;
        row.appendChild(td);
        return td;
      }
      var at = new Date(record.at);
      var finalBytes = numberAt(record, ['final_bytes']);
      var sent = numberAt(record, ['wire_bytes']);
      cell(Number.isNaN(at.getTime()) ? '—' : at.toLocaleTimeString('zh-CN', { hour12: false }));
      cell(bytesLabel(numberAt(record, ['original_bytes'])));
      cell(bytesLabel(numberAt(record, ['after_image_bytes'])));
      var final = cell(record.attempts > 0 ? bytesLabel(finalBytes) : '—');
      if (record.attempts > 1) {
        var retry = document.createElement('small');
        retry.textContent = '两次合计 ' + bytesLabel(sent);
        final.appendChild(retry);
      }
      var images = cell(record.image_counts_known
        ? '换 ' + record.replaced + ' · 命中 ' + record.cache_hits + ' · 跳 ' + record.skipped + ' / ' + record.images
        : '—');
      var reasons = skipsLabel(record.skips);
      images.title = reasons;
      if (!record.image_counts_known && reasons) {
        images.textContent = reasons;
      } else if (record.images > 0 && numberAt(record, ['input_image_bytes']) !== null) {
        var imageBytes = document.createElement('small');
        imageBytes.textContent = '图片 ' + bytesLabel(numberAt(record, ['input_image_bytes'])) +
          ' · 跳过 ' + bytesLabel(numberAt(record, ['skipped_image_bytes']));
        images.appendChild(imageBytes);
      }
      cell(['image_milliseconds', 'image_encode_milliseconds', 'zstd_milliseconds'].map(function (key) {
        return millisecondsLabel(numberAt(record, [key]));
      }).join(' / '));
      elements['recent-rows'].appendChild(row);
    });
  }

  function renderImageStats(stats) {
    elements['stat-image-before'].textContent = bytesLabel(numberAt(stats, ['before_bytes']));
    elements['stat-image-after'].textContent = bytesLabel(numberAt(stats, ['after_bytes']));
    var saving = numberAt(stats, ['saving_percent']);
    elements['stat-image-saved'].textContent = saving === null ? '—' : saving.toFixed(1) + '%';
    elements['stat-image-wire'].textContent = bytesLabel(numberAt(stats, ['wire_bytes']));
    var hits = numberAt(stats, ['cache_hits']);
    var misses = numberAt(stats, ['cache_misses']);
    elements['stat-image-hit'].textContent = hits + misses > 0 ? (100 * hits / (hits + misses)).toFixed(1) + '%' : '—';
    var images = numberAt(stats, ['images']);
    elements['stat-image-count'].textContent = images === null ? '—' : images.toLocaleString('zh-CN');
    var count = numberAt(stats, ['replaced']);
    elements['stat-image-replaced'].textContent = count === null ? '—' : count.toLocaleString('zh-CN');
    var time = numberAt(stats, ['average_processing_milliseconds']);
    elements['stat-image-time'].textContent = millisecondsLabel(time);
    elements['image-skips'].textContent = skipsLabel(stats.skips) || '—';
  }

  function renderStats(result) {
    if (!result || typeof result !== 'object') throw new Error('宿主没有返回有效测试结果。');
    var snapshot = parseStatus(result.status_json || result.stats || result.status);
    if (!snapshot || (snapshot.stats !== undefined && (!snapshot.stats || typeof snapshot.stats !== 'object' || Array.isArray(snapshot.stats)))) {
      ['stat-compressed', 'stat-before', 'stat-after', 'stat-saved'].forEach(function (id) { elements[id].textContent = '—'; });
      renderImageStats({});
      renderRecent([]);
      elements['status-json'].textContent = '';
      elements['status-details'].hidden = true;
      elements['stats-time'].textContent = '没有可显示的统计';
      throw new Error('宿主没有返回可解析的统计快照。');
    }
    var stats = snapshot.stats && typeof snapshot.stats === 'object' ? snapshot.stats : snapshot;
    var compressed = numberAt(stats, ['compressed_requests']);
    var before = numberAt(stats, ['original_bytes']);
    var after = numberAt(stats, ['wire_bytes']);
    elements['stat-compressed'].textContent = compressed === null ? '—' : compressed.toLocaleString('zh-CN');
    elements['stat-before'].textContent = bytesLabel(before);
    elements['stat-after'].textContent = bytesLabel(after);
    var percentage = typeof stats.saving_percent === 'number' && Number.isFinite(stats.saving_percent)
      ? stats.saving_percent : before !== null && after !== null && before > 0 ? 100 * (before - after) / before : null;
    elements['stat-saved'].textContent = percentage === null ? '—' : percentage.toFixed(1) + '%';
    renderImageStats(snapshot.images || {});
    renderRecent(snapshot.recent_requests);
    elements['status-json'].textContent = JSON.stringify(snapshot, null, 2);
    elements['status-details'].hidden = false;
    elements['stats-time'].textContent = '更新于 ' + new Date().toLocaleTimeString('zh-CN', { hour12: false });
    if (typeof result.message === 'string' && result.success !== undefined) {
      elements['test-message'].textContent = result.message + ' 本地自检不访问 OpenAI，不代表上游实测。';
    }
  }

  async function refreshStatus() {
    if (busy || disposed || !bridge) return;
    setBusy(true);
    try {
      var result = await bridge.status();
      renderStats(result);
      notice(result.healthy === false ? '插件当前状态异常，请查看插件管理页。' : '已刷新当前进程统计，未修改配置。', result.healthy === false ? 'warning' : 'success');
    } catch (error) {
      if (!disposed) notice(describeError(error), 'error');
    } finally {
      if (!disposed) setBusy(false);
    }
  }

  async function loadConfig() {
    if (busy || disposed) return;
    setBusy(true);
    notice('正在读取已保存配置…');
    try {
      renderConfig(await bridge.loadConfig());
      notice('已读取配置。保存后对新请求生效。', 'success');
      try {
        var status = await bridge.status();
        renderStats(status);
        if (status.healthy === false) notice('配置已读取，但插件当前状态异常，请查看插件管理页。', 'warning');
      } catch (statusError) {
        if (!disposed) notice('配置已读取，统计暂不可用。' + describeError(statusError), 'warning');
      }
    } catch (error) {
      if (!disposed) notice(describeError(error), 'error');
    } finally {
      if (!disposed) setBusy(false);
    }
  }

  async function saveConfig() {
    if (busy || disposed || savedConfig === null) return;
    var config = readValidatedForm();
    if (!config) return;
    setBusy(true);
    notice('正在保存配置；如宿主弹出管理员验证，请完成验证。');
    try {
      renderConfig(await bridge.saveConfig(config));
      notice('配置已保存，对新请求生效。', 'success');
    } catch (error) {
      if (!disposed) notice(describeError(error), 'error');
    } finally {
      if (!disposed) setBusy(false);
    }
  }

  async function testConfig() {
    if (busy || disposed || savedConfig === null) return;
    var config = readValidatedForm();
    if (!config) return;
    var mustSave = isDirty();
    setBusy(true);
    try {
      if (mustSave) {
        notice('先保存当前修改；如宿主弹出管理员验证，请完成验证。');
        renderConfig(await bridge.saveConfig(config));
      }
      notice('正在对已保存配置进行本地压缩自检；如宿主弹出管理员验证，请完成验证。');
      var result = await bridge.testConfig();
      renderStats(result);
      if (result.success === false) {
        notice('本地自检未通过：' + (typeof result.message === 'string' ? result.message : '请查看详细统计。'), 'error');
      } else {
        notice('本地压缩自检已完成，统计已更新；尚未进行 OpenAI 上游实测。', 'success');
      }
    } catch (error) {
      if (!disposed) notice(describeError(error), 'error');
    } finally {
      if (!disposed) setBusy(false);
    }
  }

  function dispose() {
    if (disposed) return;
    disposed = true;
    if (stopResize) stopResize();
    if (bridge) bridge.dispose();
  }

  elements['config-form'].addEventListener('submit', function (event) { event.preventDefault(); saveConfig(); });
  elements['config-form'].addEventListener('input', updateSaveState);
  elements['config-form'].addEventListener('change', updateSaveState);
  elements.reload.addEventListener('click', loadConfig);
  elements.test.addEventListener('click', testConfig);
  elements.refresh.addEventListener('click', refreshStatus);
  window.addEventListener('pagehide', dispose);
  window.addEventListener('beforeunload', dispose);

  try {
    bridge = window.Sub2ApiPluginBridge.create();
    bridge.ready();
    stopResize = bridge.autoResize(document.querySelector('.shell'));
    loadConfig();
  } catch (_) {
    notice('请从 Sub2API 后台的插件配置页打开此页面；直接打开文件无法读取或保存配置。', 'warning');
    elements['save-state'].textContent = '尚未连接宿主';
    elements.reload.disabled = true;
    elements.refresh.disabled = true;
  }
})();
