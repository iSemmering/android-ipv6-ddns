const moduleCommand = '/data/adb/modules/ipv6_ddns/bin/ddns request';
const logPeriods = Array.from(document.querySelectorAll('[data-days]'));
const saveButton = el('settings').querySelector('button[type="submit"]');
const credentialToggles = document.querySelectorAll('[data-credential]');
const draftEdits = new Map();
let state = null;
let stateRevision = 0;
let requestNumber = 0;
let refreshing = false;
let lastStatusPoll = 0;
let connected = false;
let toggleBusy = false;
let checkSubmitting = false;
let saveBusy = false;
let logSettingsBusy = false;
let pendingBinding = null;
let configSession = 0;
let draftRevision = 0;
let draftRestored = false;
let draftRestoring = false;

function request(op, parameters = {}) {
  return new Promise((resolve, reject) => {
    const bridge = window.ksu;
    if (!bridge || typeof bridge.exec !== 'function') {
      reject(new Error('请从 KernelSU 模块的 WebUI 入口打开'));
      return;
    }
    const callbackName = 'ipv6DdnsReply' + (++requestNumber);
    const finish = () => {
      clearTimeout(timer);
      delete window[callbackName];
    };
    const timer = setTimeout(() => {
      finish();
      reject(new Error('模块后台响应超时'));
    }, 44000);
    window[callbackName] = (code, stdout) => {
      finish();
      let reply;
      try {
        reply = JSON.parse(stdout);
      } catch {
        reject(new Error('无法读取模块后台响应，请确认模块已安装并重启'));
        return;
      }
      if (code !== 0 || !reply.ok) reject(new Error(reply.error || '模块操作失败'));
      else resolve(reply);
    };
    try {
      const bytes = new TextEncoder().encode(JSON.stringify({ op, ...parameters }));
      const payload = btoa(String.fromCharCode(...bytes))
        .replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
      bridge.exec(moduleCommand, JSON.stringify({
        env: { DDNS_REQUEST: payload },
      }), callbackName);
    } catch {
      finish();
      reject(new Error('无法连接 KernelSU WebUI 本地接口'));
    }
  });
}

function render() {
  if (!state) return;
  el('status-hero').dataset.tone = state.Phase === 'waiting' ? 'working' : state.Phase;
  el('run-state').textContent = state.Label;
  el('run-toggle').setAttribute('aria-checked', state.Enabled);
  el('run-toggle').disabled = !connected || toggleBusy;
  el('bound-domain').textContent = state.Domain || '未绑定域名';
  el('copy-domain').hidden = !state.Domain;
  el('address').textContent = state.Address || '未获取到全局 IPv6';
  el('copy-address').disabled = !state.Address;
  el('summary-interval').textContent = state.Interval + ' s';
  el('summary-sync').textContent = state.LastSync || '尚未同步';
  const check = el('check');
  const checkBusy = checkSubmitting || state.ManualPending;
  check.disabled = !connected || !state.Enabled || checkBusy;
  check.setAttribute('aria-busy', Boolean(checkBusy));
  check.textContent = checkBusy ? '检查中…' : '立即检查';
  const reason = el('operation-reason');
  reason.textContent = state.Reason;
  reason.hidden = !state.Reason || !['error', 'waiting'].includes(state.Phase);
  el('logging').setAttribute('aria-checked', state.Logging);
  el('logging').disabled = !connected || logSettingsBusy;
  for (const period of logPeriods) {
    period.setAttribute('aria-pressed', Number(period.dataset.days) === state.Days);
    period.disabled = !connected || logSettingsBusy;
  }
}

function acceptStatus(next) {
  stateRevision++;
  state = next;
  connected = true;
  render();
  void restoreDraft();
}

function nextDraftRevision() {
  draftRevision = Math.max(Date.now() * 1000, draftRevision + 1);
  return draftRevision;
}

async function restoreDraft() {
  if (draftRestored || draftRestoring) return;
  draftRestoring = true;
  try {
    const { draft } = await request('getDraft');
    for (const id of credentialFields) {
      if (!draftEdits.has(id)) el(id).value = draft?.[id] ?? '';
    }
    draftRestored = true;
  } catch {
    // Retry after the next successful status response.
  } finally {
    draftRestoring = false;
  }
}

function zoneByID(id) {
  return state?.Zones.find(zone => zone.id === id);
}

function renderFullDomain() {
  const prefix = el('prefix').value.trim().toLowerCase();
  const zone = zoneByID(el('zone').value);
  const preview = el('full-domain');
  const name = zone && !prefixError(prefix) ? prefix + '.' + zone.name : '';
  preview.textContent = name;
  preview.hidden = !name;
}

function selectZone(id) {
  const zone = zoneByID(id);
  el('zone').value = zone?.id || '';
  el('zone-value').textContent = zone?.name || '选择域名';
  el('zone-value').classList.toggle('setting-placeholder', !zone);
  if (zone) showFieldError('choose-zone');
  renderFullDomain();
}

function hydrateConfig() {
  el('prefix').value = state?.Prefix ?? '';
  for (const [id, value] of [['ttl', state?.TTL], ['interval', state?.Interval]]) {
    const input = el(id);
    input.value = String(value ?? '');
    input.dataset.digits = input.value;
  }
  for (const id of [...settingsFields, 'choose-zone']) showFieldError(id);
  el('keep-data-on').setAttribute('aria-checked', Boolean(state?.KeepDataOn));
  el('airplane').setAttribute('aria-checked', Boolean(state?.Airplane));
  selectZone(state?.Zone.id || '');
}

function setCredentialVisibility(button, revealed) {
  const { credential } = button.dataset;
  el(credential).type = revealed ? 'text' : 'password';
  button.dataset.revealed = revealed;
  button.setAttribute('aria-label', (revealed ? '隐藏 ' : '显示 ') + credential.toUpperCase());
}

async function refreshStatus() {
  if (refreshing || document.hidden) return;
  refreshing = true;
  lastStatusPoll = Date.now();
  const before = stateRevision;
  try {
    const { status } = await request('status');
    if (before !== stateRevision) return;
    const first = !state;
    acceptStatus(status);
    if (first) hydrateConfig();
    refreshLogHead();
  } catch (error) {
    if (before !== stateRevision) return;
    if (connected || !state) {
      el('operation-reason').textContent = error.message;
      el('operation-reason').hidden = false;
    }
    connected = false;
    el('run-toggle').disabled = true;
    el('check').disabled = true;
  } finally {
    refreshing = false;
  }
}

async function updateLogs(parameters) {
  if (!connected || logSettingsBusy) return;
  logSettingsBusy = true;
  render();
  try {
    const { status } = await request('logSettings', parameters);
    acceptStatus(status);
    resetLogRows();
  } catch (error) {
    toast(error.message, 'error');
  } finally {
    logSettingsBusy = false;
    render();
  }
}

tabList.addEventListener('contextmenu', event => event.preventDefault());
for (const tab of tabs) {
  tab.addEventListener('click', () => {
    const name = tab.getAttribute('aria-controls');
    const wasConfigOpen = !el('config').hidden;
    const isConfigOpen = name === 'config';
    openTab(name);
    if (wasConfigOpen !== isConfigOpen) hydrateConfig();
    if (wasConfigOpen && !isConfigOpen) {
      configSession++;
      for (const button of credentialToggles) setCredentialVisibility(button, false);
    }
    if (name === 'logs') resetLogRows();
  });
  tab.addEventListener('keydown', event => {
    if (!['ArrowLeft', 'ArrowRight'].includes(event.key)) return;
    event.preventDefault();
    const offset = event.key === 'ArrowRight' ? 1 : -1;
    const next = tabs[(tabs.indexOf(tab) + offset + tabs.length) % tabs.length];
    next.click();
    next.focus();
  });
}

el('run-toggle').addEventListener('click', async () => {
  if (!connected || toggleBusy) return;
  toggleBusy = true;
  render();
  try {
    const { status } = await request('toggle', { enabled: !state.Enabled });
    acceptStatus(status);
    toast(status.Enabled ? '自动同步已开启' : '自动同步已停止');
  } catch (error) {
    toast(error.message, 'error');
  } finally {
    toggleBusy = false;
    render();
  }
});

el('check').addEventListener('click', async () => {
  if (!connected || !state.Enabled || checkSubmitting || state.ManualPending) return;
  checkSubmitting = true;
  render();
  const visibleFeedback = new Promise(resolve => setTimeout(resolve, 250));
  try {
    const { status } = await request('check');
    acceptStatus(status);
  } catch (error) {
    toast(error.message, 'error');
  } finally {
    await visibleFeedback;
    checkSubmitting = false;
    render();
  }
});

el('copy-address').addEventListener('click', event => {
  copyValue(state?.Address, el('address'), event.currentTarget, 'IPv6 地址');
});
el('copy-domain').addEventListener('click', event => {
  copyValue(state?.Domain, el('bound-domain'), event.currentTarget, '域名');
});
el('choose-provider').addEventListener('click', () => openPicker('provider', providerOptions));
el('choose-zone').addEventListener('click', () => {
  if (!state?.Zones.length) {
    toast('请先填写 AK 和 SK 并点击确定', 'warning');
    return;
  }
  const selected = el('zone').value;
  openPicker('zone', state.Zones.map(zone => ({
    label: zone.name, value: zone.id, selected: zone.id === selected,
  })), selectZone);
});
el('close-picker').addEventListener('click', () => pickerSheet.close());
el('prefix').addEventListener('input', renderFullDomain);
for (const id of ['keep-data-on', 'airplane']) {
  const button = el(id);
  button.addEventListener('click', () => {
    button.setAttribute('aria-checked', button.getAttribute('aria-checked') !== 'true');
  });
}
el('logging').addEventListener('click', () => { void updateLogs({ logging: !state?.Logging }); });
for (const button of logPeriods) {
  button.addEventListener('click', () => {
    const days = Number(button.dataset.days);
    if (days !== state?.Days) void updateLogs({ days });
  });
}

for (const id of ['ttl', 'interval']) {
  const input = el(id);
  input.dataset.digits = input.value;
  input.addEventListener('input', () => {
    if (/^[0-9]*$/.test(input.value)) input.dataset.digits = input.value;
    else input.value = input.dataset.digits;
  });
}
for (const id of Object.keys(fieldValidators)) {
  const input = el(id);
  input.addEventListener('blur', () => validateField(id));
  input.addEventListener('input', () => {
    if (input.hasAttribute('aria-invalid')) validateField(id);
  });
}
for (const id of credentialFields) {
  el(id).addEventListener('input', () => {
    showKeyResult('');
    const revision = nextDraftRevision();
    draftEdits.set(id, revision);
    void request('saveDraft', { draft: { [id]: el(id).value }, revision }).catch(error => {
      if (draftEdits.get(id) === revision) showKeyResult(error.message, true);
    });
  });
}
for (const button of credentialToggles) {
  const input = el(button.dataset.credential);
  button.addEventListener('click', () => {
    setCredentialVisibility(button, input.type === 'password');
  });
}

el('credentials').addEventListener('submit', async event => {
  event.preventDefault();
  if (queryBusy) return;
  showKeyResult('');
  const invalid = credentialFields.filter(id => !validateField(id));
  if (invalid.length) {
    el(invalid[0]).focus();
    return;
  }
  const revision = nextDraftRevision();
  setQueryBusy(true);
  for (const id of credentialFields) {
    el(id).readOnly = true;
    draftEdits.set(id, revision);
  }
  try {
    const { status } = await request('verify', {
      ak: el('ak').value, sk: el('sk').value, revision,
    });
    acceptStatus(status);
    selectZone(el('zone').value || status.Zone.id || '');
    showKeyResult('已获取 ' + status.Zones.length + ' 个公网域名');
  } catch (error) {
    showKeyResult(error.message, true);
  } finally {
    for (const id of credentialFields) el(id).readOnly = false;
    setQueryBusy(false);
  }
});

async function saveBinding(value) {
  if (saveBusy) return;
  const session = configSession;
  saveBusy = true;
  saveButton.disabled = true;
  try {
    const reply = await request('save', value);
    if (reply.confirm) {
      if (session !== configSession) return;
      pendingBinding = { ...value, confirm: reply.confirm.id };
      const dialog = el('reuse');
      dialog.returnValue = '';
      el('reuse-domain').textContent = reply.confirm.name;
      dialog.showModal();
      return;
    }
    acceptStatus(reply.status);
    hydrateConfig();
    toast('配置已保存');
  } catch (error) {
    toast(error.message, 'error');
  } finally {
    saveBusy = false;
    saveButton.disabled = false;
  }
}

el('settings').addEventListener('submit', event => {
  event.preventDefault();
  const zoneID = el('zone').value;
  showFieldError('choose-zone', zoneByID(zoneID) ? '' : '请选择公网域名');
  const invalid = settingsFields.filter(id => !validateField(id));
  if (!zoneByID(zoneID) || invalid.length) {
    el(zoneByID(zoneID) ? invalid[0] : 'choose-zone').focus();
    return;
  }
  void saveBinding({
    zoneId: zoneID, prefix: el('prefix').value.trim().toLowerCase(),
    ttl: Number(el('ttl').value), interval: Number(el('interval').value),
    keepDataOn: el('keep-data-on').getAttribute('aria-checked') === 'true',
    airplane: el('airplane').getAttribute('aria-checked') === 'true',
  });
});

el('reuse').addEventListener('close', () => {
  const value = pendingBinding;
  pendingBinding = null;
  if (value && el('reuse').returnValue === 'use') void saveBinding(value);
});

el('key-result').prepend(el('toast').querySelector('.toast-icon').cloneNode(true));
el('run-toggle').disabled = true;
el('logging').disabled = true;
for (const period of logPeriods) period.disabled = true;
setQueryBusy(false);
openTab('overview');
void restoreDraft();
void refreshStatus();
setInterval(() => {
  if (state?.ManualPending || Date.now() - lastStatusPoll >= 2500) void refreshStatus();
}, 500);
document.addEventListener('visibilitychange', () => {
  if (!document.hidden) void refreshStatus();
});
