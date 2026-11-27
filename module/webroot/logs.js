const logPanel = el('logs');
const logViewport = el('tab-panels');
const logTypeBar = el('log-type-bar');
const logFeed = el('log-feed');
const logList = el('entries');
const logFilters = Array.from(logTypeBar.querySelectorAll('[data-level]'));
const logLabels = { success: '信息', pending: '警告', error: '异常' };
const estimatedLogHeight = 82;
const logOverscan = 260;

let logFilter = 'all';
let logRows = [];
let logHeights = [];
let logOffsets = [0];
let logWidth = 0;
let logWindowStart = -1;
let logWindowEnd = -1;
let logFrame = 0;
let logEpoch = 0;
let logCursor = '';
let logLoading = false;
let logShownHead = null;
let logRetryAt = 0;

function logNode(record, index) {
  const item = document.createElement('li');
  const meta = document.createElement('div');
  const dot = document.createElement('span');
  const time = document.createElement('time');
  const duration = document.createElement('span');
  const title = document.createElement('strong');
  const detail = document.createElement('p');

  item.dataset.severity = record.severity;
  item.setAttribute('aria-posinset', index + 1);
  item.setAttribute('aria-setsize', logCursor ? -1 : logRows.length);
  meta.className = 'entry-meta';
  dot.className = 'entry-dot';
  dot.setAttribute('role', 'img');
  dot.setAttribute('aria-label', logLabels[record.severity] ?? logLabels.pending);
  time.textContent = record.time;
  duration.className = 'entry-duration';
  duration.textContent = record.duration;
  title.textContent = record.action + ' · ' + record.outcome;
  detail.textContent = record.reason;
  meta.append(dot, time, duration);
  item.append(meta, title, detail);
  return item;
}

function updateLogOffsets(start = 0) {
  logOffsets.length = logRows.length + 1;
  for (let index = start; index < logRows.length; index++) {
    logOffsets[index + 1] = logOffsets[index] + (logHeights[index] ?? estimatedLogHeight);
  }
  logFeed.style.height = logOffsets[logRows.length] + 'px';
}

function logIndexAt(offset) {
  let low = 0;
  let high = logRows.length;
  while (low < high) {
    const middle = Math.floor((low + high) / 2);
    if (logOffsets[middle + 1] <= offset) low = middle + 1;
    else high = middle;
  }
  return low;
}

function logViewportOffsets() {
  const viewportTop = logViewport.getBoundingClientRect().top;
  const visibleTop = Math.max(viewportTop, logTypeBar.getBoundingClientRect().bottom);
  const start = logFeed.getBoundingClientRect().top;
  return {
    top: Math.max(0, visibleTop - start),
    bottom: Math.max(0, viewportTop + logViewport.clientHeight - start),
  };
}

function updateLogLayout(start, top) {
  // Keep the same row in view as estimates are replaced by measured, wrapped heights.
  const anchor = logIndexAt(top);
  const previousOffset = logOffsets[anchor];
  const scrollTop = logViewport.scrollTop;
  updateLogOffsets(start);
  if (top > 0) logViewport.scrollTop = scrollTop + logOffsets[anchor] - previousOffset;
}

function queueLogWindow() {
  if (!logFrame && !logPanel.hidden) logFrame = requestAnimationFrame(paintLogWindow);
}

function paintLogWindow() {
  logFrame = 0;
  if (logPanel.hidden || !logRows.length) return;
  const width = logFeed.clientWidth;
  if (width !== logWidth) {
    logWidth = width;
    logHeights = [];
    logWindowStart = -1;
    updateLogLayout(0, logViewportOffsets().top);
  }

  const { top, bottom } = logViewportOffsets();
  if (logCursor && bottom + logOverscan >= logOffsets[logRows.length]) void loadLogPage();

  const first = logIndexAt(Math.max(0, top - logOverscan));
  const end = Math.min(logRows.length, logIndexAt(bottom + logOverscan) + 1);
  if (first === logWindowStart && end === logWindowEnd) return;
  logWindowStart = first;
  logWindowEnd = end;
  logList.start = first + 1;
  logList.replaceChildren(...logRows.slice(first, end).map((row, offset) => logNode(row, first + offset)));

  let firstChanged = end;
  for (const [offset, node] of Array.from(logList.children).entries()) {
    const index = first + offset;
    const height = node.getBoundingClientRect().height;
    if (height > 0 && height !== (logHeights[index] ?? estimatedLogHeight)) {
      logHeights[index] = height;
      firstChanged = Math.min(firstChanged, index);
    }
  }

  if (firstChanged < end) {
    updateLogLayout(firstChanged, top);
    queueLogWindow();
  }
  logList.style.transform = 'translateY(' + logOffsets[first] + 'px)';
}

function showLogFeedback(message = '') {
  const feedback = el('log-feedback');
  feedback.textContent = message;
  feedback.hidden = !message;
}

function logHead() {
  return state?.LogHead?.[logFilter] ?? 0;
}

function resetLogRows() {
  logEpoch++;
  logLoading = false;
  logCursor = '';
  logRows = [];
  logHeights = [];
  logWindowStart = -1;
  logShownHead = null;
  logRetryAt = 0;
  logList.replaceChildren();
  el('empty-logs').hidden = true;
  showLogFeedback();
  updateLogOffsets();
  void loadLogPage(true);
}

async function loadLogPage(first = false) {
  if (logLoading || logPanel.hidden || (!first && !logCursor) || Date.now() < logRetryAt) return;
  const epoch = logEpoch;
  const head = logHead();
  const cursor = first ? '' : logCursor;
  const refreshing = first && logRows.length > 0;
  logLoading = true;
  if (!refreshing) showLogFeedback('正在读取日志…');
  try {
    const reply = await request('logs', { level: logFilter, cursor });
    if (epoch !== logEpoch || (refreshing && logViewportOffsets().top > 0)) return;
    const rows = reply.entries ?? [];
    const start = first ? 0 : logRows.length;
    if (first) {
      logRows = rows;
      logHeights = [];
      logShownHead = head;
    } else {
      logRows.push(...rows);
    }
    logCursor = reply.cursor && reply.cursor !== cursor ? reply.cursor : '';
    logWindowStart = -1;
    logRetryAt = 0;
    if (!logRows.length) logList.replaceChildren();
    updateLogOffsets(start);
    const empty = el('empty-logs');
    empty.textContent = logFilter === 'all' ? '暂无运行记录' : '暂无此类记录';
    empty.hidden = logRows.length > 0;
    showLogFeedback();
  } catch (error) {
    if (epoch !== logEpoch) return;
    logRetryAt = Date.now() + 4000;
    showLogFeedback(error.message);
  } finally {
    if (epoch === logEpoch) {
      logLoading = false;
      queueLogWindow();
    }
  }
}

function refreshLogHead() {
  if (logPanel.hidden || document.hidden) return;
  if (logShownHead !== logHead() && logViewportOffsets().top === 0) void loadLogPage(true);
  if (logRetryAt) queueLogWindow();
}

for (const filter of logFilters) {
  filter.addEventListener('click', () => {
    if (logFilter === filter.dataset.level && !logRetryAt) return;
    logFilter = filter.dataset.level;
    for (const button of logFilters) button.setAttribute('aria-pressed', String(button === filter));
    const cardTop = logTypeBar.parentElement.getBoundingClientRect().top;
    const viewportTop = logViewport.getBoundingClientRect().top;
    logViewport.scrollTop = Math.min(logViewport.scrollTop, Math.max(0, logViewport.scrollTop + cardTop - viewportTop));
    resetLogRows();
  });
}

logViewport.addEventListener('scroll', queueLogWindow, { passive: true });
if (typeof ResizeObserver === 'function') new ResizeObserver(queueLogWindow).observe(logViewport);
else window.addEventListener('resize', queueLogWindow);
