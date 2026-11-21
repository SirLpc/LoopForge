'use strict';
const assert = require('node:assert/strict');
const { createDOM } = require('./hui_fake_dom.js');

async function main() {
  const dom = createDOM();
  const { document, window } = dom;
  const modal = dom.html(`<div id="confirmModal"><div class="confirm-modal-card">
    <h3 id="confirmTitle"></h3><p id="confirmMessage"></p>
    <div class="confirm-modal-footer">
      <button id="confirmCancelBtn">取消</button><button id="confirmOkBtn">确定</button>
    </div>
  </div></div>`);
  const calls = [];
  const pool = {
    runnerStatus: 'online', supportsWarmWorkspace: true, lastSeenAt: '2026-11-21T13:00:00Z',
    slots: [
      { id: 'busy/slot', projectId: 'game', repositoryId: '<code>', inventoryState: 'available', leaseState: 'ready', paused: false },
      { id: 'idle', projectId: 'game', repositoryId: 'game-ui', inventoryState: 'available', paused: false },
      { id: 'physical', projectId: 'game', repositoryId: 'game-ui', inventoryState: 'reserved', paused: false },
    ],
    queue: [{ projectId: 'game', repositoryIds: ['code', 'game-ui'], queuedAt: '2026-11-21T12:59:00Z' }],
  };
  let dialog, poll, rejectPatch, pendingRead;
  window.$ = selector => document.querySelector(selector);
  window.lfT = zh => zh;
  window.escapeHTML = value => String(value ?? '').replace(/[&<>"']/g, c => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  }[c]));
  window.setConfirmIcon = () => {};
  window.afOpenDialog = setup => { dialog = setup(modal); };
  window.setInterval = fn => { poll = fn; return 1; };
  window.clearInterval = () => { poll = null; };
  window.AbortController = AbortController;
  window.api = async (path, options) => {
    calls.push([path, options.method || 'GET', options.body]);
    if (options.method === 'PATCH') {
      if (rejectPatch) throw new Error('更新失败');
      const { slotId, paused } = JSON.parse(options.body);
      pool.slots.find(slot => slot.id === slotId).paused = paused;
    } else if (pendingRead) {
      await pendingRead;
    }
    return structuredClone(pool);
  };

  dom.load('app/runner-warm-workspaces.js');
  window.openRunnerWarmWorkspacePool({ id: 'runner/1', name: 'Test Runner' });
  await dom.settle();
  const panel = modal.querySelector('[data-runner-warm-pool-panel]');
  const button = () => panel.querySelector('button[data-warm-pool-slot="busy/slot"]');
  assert.equal(calls[0][0], '/api/runners/runner%2F1/warm-workspaces');
  assert.match(panel.textContent, /使用中/);
  assert.match(panel.textContent, /空闲/);
  assert.match(panel.textContent, /Runner 报告占用/);
  assert.match(panel.textContent, /code、game-ui/);
  assert.equal(panel.querySelector('td code')?.textContent, '<code>');
  assert.match(button().textContent, /暂停调度/);
  button().focus();
  button().click();
  await dom.settle();
  assert.deepEqual(JSON.parse(calls[1][2]), { slotId: 'busy/slot', paused: true });
  assert.match(panel.textContent, /使用中/);
  assert.match(button().textContent, /恢复调度/);
  assert.equal(document.activeElement, button());

  rejectPatch = true;
  button().click();
  await dom.settle();
  assert.match(panel.querySelector('[role="alert"]').textContent, /更新失败/);
  assert.match(button().textContent, /恢复调度/);

  pool.runnerStatus = 'offline';
  let finishRead;
  pendingRead = new Promise(resolve => { finishRead = resolve; });
  poll();
  assert.equal(button().getAttribute('aria-disabled'), 'true');
  finishRead();
  await dom.settle();
  assert.equal(button().hasAttribute('aria-disabled'), false);
  assert.match(panel.textContent, /离线/);
  assert.doesNotMatch(panel.textContent, /空闲/);
  assert.match(panel.textContent, /状态未知/);
  assert.match(panel.textContent, /使用中/);
  dialog.value();
  assert.equal(poll, null);
  assert.equal(panel.isConnected, false);
}

main().catch(error => { console.error(error); process.exitCode = 1; });
