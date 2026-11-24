const el = (id) => document.getElementById(id);
const tabList = document.querySelector('.tabs');
const tabs = Array.from(tabList.querySelectorAll('[role="tab"]'));
const periods = Array.from(document.querySelectorAll('[data-days]'));
const secondsLimits = { ttl: [1, 2147483647], interval: [2, 2147483] };
const providerOptions = [
  { label: '华为云', icon: 'provider-huawei', selected: true },
  { label: '阿里云', icon: 'provider-alibaba', disabled: true },
  { label: '腾讯云', icon: 'provider-tencent', disabled: true },
  { label: '京东云', icon: 'provider-jd', disabled: true },
];
let toastTimer;
let queryBusy = false;

function toast(message, tone = 'success') {
  clearTimeout(toastTimer);
  const notification = el('toast');
  notification.dataset.tone = tone;
  el('toast-message').textContent = message;
  notification.hidden = false;
  toastTimer = setTimeout(() => { notification.hidden = true; }, 2500);
}

function openTab(name) {
  el('tab-panels').scrollTop = 0;
  for (const [index, tab] of tabs.entries()) {
    const panel = el(tab.getAttribute('aria-controls'));
    const selected = panel.id === name;
    panel.hidden = !selected;
    tab.setAttribute('aria-selected', String(selected));
    tab.tabIndex = selected ? 0 : -1;
    if (selected) tabList.style.setProperty('--active-tab', index);
  }
}

function openPicker(title, options, onSelect) {
  const sheet = el('picker-sheet');
  el('picker-title').textContent = title;
  const rows = options.map(({ label, icon, value = label, disabled = false, selected = false }) => {
    const row = document.createElement('button');
    row.type = 'button';
    row.className = 'picker-option';
    row.disabled = disabled;
    row.setAttribute('aria-pressed', selected);
    if (icon) {
      const namespace = 'http://www.w3.org/2000/svg';
      const svg = document.createElementNS(namespace, 'svg');
      svg.setAttribute('class', 'provider-icon');
      svg.setAttribute('aria-hidden', 'true');
      svg.setAttribute('focusable', 'false');
      const use = document.createElementNS(namespace, 'use');
      use.setAttribute('href', '#' + icon);
      svg.append(use);
      row.append(svg);
    }
    const text = document.createElement('span');
    text.className = 'picker-label';
    text.textContent = label;
    row.append(text);
    if (selected) {
      const mark = document.createElement('span');
      mark.className = 'picker-check';
      mark.setAttribute('aria-hidden', 'true');
      row.append(mark);
    }
    row.addEventListener('click', () => {
      onSelect?.(value);
      sheet.close();
    });
    return row;
  });
  el('picker-options').replaceChildren(...rows);
  sheet.showModal();
}

function secondsError(id, value) {
  const [min, max] = secondsLimits[id];
  if (!value) return '请填写秒数';
  if (!/^[0-9]+$/.test(value)) return '请输入整数秒';
  const seconds = Number(value);
  if (seconds < min) return '至少填写 ' + min + ' 秒';
  if (seconds > max) return '最多填写 ' + max + ' 秒';
  return '';
}

function prefixError(value) {
  const prefix = value.trim().toLowerCase();
  if (!prefix) return '请填写子域名前缀';
  return /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(prefix)
    ? ''
    : '仅限字母、数字或连字符，首尾须为字母或数字';
}

const fieldValidators = {
  ak: (value) => value.trim() ? '' : '请输入 AK',
  sk: (value) => value.trim() ? '' : '请输入 SK',
  prefix: prefixError,
  ttl: (value) => secondsError('ttl', value),
  interval: (value) => secondsError('interval', value),
};
const credentialFields = ['ak', 'sk'];
const settingsFields = ['prefix', 'ttl', 'interval'];

function showFieldError(id, message = '') {
  const input = el(id);
  const error = el(id + '-error');
  error.textContent = message;
  error.hidden = !message;
  if (message) input.setAttribute('aria-invalid', 'true');
  else input.removeAttribute('aria-invalid');
}

function validateField(id) {
  const message = fieldValidators[id](el(id).value);
  showFieldError(id, message);
  return !message;
}

function showKeyResult(message, error = false) {
  const result = el('key-result');
  const detail = el('key-result-message');
  if (!result.hidden && el('toast-message').textContent === detail.textContent) {
    clearTimeout(toastTimer);
    el('toast').hidden = true;
  }
  detail.textContent = message;
  result.classList.toggle('error', error);
  result.hidden = !message;
  if (message && !error) toast(message);
}

function setQueryBusy(busy) {
  queryBusy = busy;
  const button = el('fetch-zones');
  button.disabled = busy;
  button.textContent = busy ? '正在获取…' : '确定';
  button.setAttribute('aria-busy', busy);
}

function copyWithSelection(value, button) {
  const field = document.createElement('textarea');
  field.value = value;
  field.readOnly = true;
  field.tabIndex = -1;
  field.style.cssText = 'position:fixed;inset:0 auto auto 0;opacity:0;pointer-events:none';
  document.body.append(field);
  field.focus({ preventScroll: true });
  field.select();
  field.setSelectionRange(0, value.length);
  try {
    return document.execCommand('copy');
  } catch {
    return false;
  } finally {
    field.remove();
    button.focus({ preventScroll: true });
  }
}

async function copyValue(value, source, button, label) {
  if (!value) return;

  try {
    await navigator.clipboard.writeText(value);
  } catch {
    if (!copyWithSelection(value, button)) {
      const selection = window.getSelection();
      if (selection) {
        const range = document.createRange();
        range.selectNodeContents(source);
        selection.removeAllRanges();
        selection.addRange(range);
        toast(label + '已选中，可手动复制', 'warning');
      } else {
        toast('复制失败，请选中' + label + '后手动复制', 'error');
      }
      return;
    }
  }
  toast(label + '已复制');
}

