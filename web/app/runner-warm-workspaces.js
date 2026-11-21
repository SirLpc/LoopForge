function runnerWarmWorkspaceButton(runner) {
  return `<button class="button ghost small runner-text-button" type="button" data-runner-warm-pool="${escapeHTML(runner.id)}" aria-haspopup="dialog">${lfT('预热工作区', 'Warm workspaces')}</button>`;
}

function runnerWarmWorkspaceOccupancy(slot, pool) {
  switch (slot.leaseState) {
    case 'ready': return [lfT('使用中', 'In use'), 'info'];
    case 'reserved': return [lfT('已分配', 'Assigned'), 'info'];
    case 'releasing': return [lfT('归还中', 'Returning'), 'warn'];
    case 'blocked': return [lfT('待检查', 'Needs inspection'), 'error'];
  }
  if (pool.runnerStatus === 'online' && pool.supportsWarmWorkspace) {
    switch (slot.inventoryState) {
      case 'available': return [lfT('空闲', 'Idle'), 'success'];
      case 'reserved': return [lfT('Runner 报告占用', 'Runner reports occupied'), 'info'];
      case 'blocked': return [lfT('待检查', 'Needs inspection'), 'error'];
    }
  }
  return [lfT('状态未知', 'Status unknown'), 'neutral'];
}

function runnerWarmWorkspaceTime(value) {
  return value ? new Date(value).toLocaleString(lfT('zh-CN', 'en')) : '—';
}

function runnerWarmWorkspacePoolHTML(pool) {
  const slots = pool.slots.map(slot => {
    const [status, tone] = runnerWarmWorkspaceOccupancy(slot, pool);
    const scheduling = slot.paused ? lfT('调度已暂停', 'Scheduling paused') : lfT('调度开启', 'Scheduling enabled');
    const action = slot.paused ? lfT('恢复调度', 'Resume scheduling') : lfT('暂停调度', 'Pause scheduling');
    const canManage = slot.paused || slot.inventoryState || slot.leaseState;
    return `<tr>
      <th scope="row"><code>${escapeHTML(slot.id)}</code></th>
      <td><code>${escapeHTML(slot.repositoryId || '—')}</code><small>${escapeHTML(slot.projectId || '')}</small></td>
      <td><span class="hui-badge" data-tone="${tone}" data-size="sm"><span class="hui-badge-label">${status}</span></span></td>
      <td>${scheduling}</td>
      <td>${canManage ? `<button class="button small" type="button" data-warm-pool-slot="${escapeHTML(slot.id)}" aria-label="${escapeHTML(action + ' · ' + slot.id)}">${action}</button>` : ''}</td>
    </tr>`;
  }).join('');
  const queue = pool.queue.map(wait => `<li>
    <span><code>${escapeHTML(wait.repositoryIds.join(lfT('、', ', ')))}</code><small>${escapeHTML(wait.projectId)}</small></span>
    <time datetime="${escapeHTML(wait.queuedAt)}">${runnerWarmWorkspaceTime(wait.queuedAt)}</time>
  </li>`).join('');
  const emptyInventory = pool.supportsWarmWorkspace
    ? lfT('Runner 当前没有上报预热槽位。槽位通过 Runner 本地配置登记。', 'The Runner is currently reporting no warm slots. Register slots in its local configuration.')
    : lfT('此 Runner 版本不支持预热工作区。', 'This Runner version does not support warm workspaces.');
  return `
    <h4>${lfT('槽位', 'Slots')} (${pool.slots.length})</h4>
    ${slots ? `<div class="runner-warm-pool-table"><table>
      <thead><tr>
        <th scope="col">${lfT('槽位 ID', 'Slot ID')}</th>
        <th scope="col">${lfT('仓库 / 项目', 'Repository / project')}</th>
        <th scope="col">${lfT('占用状态', 'Occupancy')}</th>
        <th scope="col">${lfT('调度', 'Scheduling')}</th>
        <th scope="col">${lfT('操作', 'Action')}</th>
      </tr></thead><tbody>${slots}</tbody>
    </table></div><p class="runner-warm-pool-muted">${lfT('暂停调度只停止新分配，已有租约继续使用和归还。', 'Pausing stops new assignments. Active leases continue and return normally.')}</p>` : `<p class="runner-warm-pool-muted">${emptyInventory}</p>`}
    <h4>${lfT('排队请求', 'Queued requests')} (${pool.queue.length})</h4>
    ${queue ? `<ol class="runner-warm-pool-queue">${queue}</ol>` : `<p class="runner-warm-pool-muted">${lfT('暂无排队请求', 'No queued requests')}</p>`}`;
}

function openRunnerWarmWorkspacePool(runner) {
  return afOpenDialog(modal => {
    const ok = $('#confirmOkBtn');
    const cancel = $('#confirmCancelBtn');
    const card = modal.querySelector('.confirm-modal-card');
    $('#confirmTitle').textContent = lfT('预热工作区', 'Warm workspaces');
    $('#confirmMessage').textContent = runner.name || runner.id;
    ok.textContent = lfT('关闭', 'Close');
    ok.className = 'button ghost';
    cancel.hidden = true;
    setConfirmIcon('info', 'layers');

    const panel = document.createElement('div');
    panel.className = 'runner-warm-pool-panel';
    panel.dataset.runnerWarmPoolPanel = '';
    panel.innerHTML = `<div class="runner-warm-pool-toolbar">
      <p class="runner-warm-pool-muted" data-warm-pool-heartbeat></p>
      <button class="button small" type="button" data-warm-pool-refresh>${lfT('刷新', 'Refresh')}</button>
    </div>
    <p class="runner-warm-pool-error" role="alert" hidden></p>
    <div data-warm-pool-results>${lfT('正在读取预热工作区…', 'Loading warm workspaces…')}</div>`;
    card.insertBefore(panel, modal.querySelector('.confirm-modal-footer'));
    card.classList.add('runner-warm-pool-dialog');

    const path = `/api/runners/${encodeURIComponent(runner.id)}/warm-workspaces`;
    const controller = new AbortController();
    const options = { silent: true, signal: controller.signal };
    const refresh = panel.querySelector('[data-warm-pool-refresh]');
    const heartbeat = panel.querySelector('[data-warm-pool-heartbeat]');
    const error = panel.querySelector('[role="alert"]');
    const results = panel.querySelector('[data-warm-pool-results]');
    let pool;
    let pending = false;

    async function load(change) {
      if (pending) return;
      pending = true;
      panel.setAttribute('aria-busy', 'true');
      const controls = [refresh, ...results.querySelectorAll('[data-warm-pool-slot]')];
      for (const button of controls) button.setAttribute('aria-disabled', 'true');
      error.hidden = true;
      try {
        if (change) await api(path, { ...options, method: 'PATCH', body: JSON.stringify(change) });
        const next = await api(path, options);
        if (controller.signal.aborted || JSON.stringify(next) === JSON.stringify(pool)) return;
        pool = next;
        const online = pool.runnerStatus === 'online';
        heartbeat.textContent = [
          online ? lfT('机器在线', 'Runner online') : lfT('机器离线', 'Runner offline'),
          lfT('最近心跳', 'Last heartbeat'),
          runnerWarmWorkspaceTime(pool.lastSeenAt),
        ].join(' · ');
        const focusedSlot = document.activeElement?.dataset.warmPoolSlot;
        results.innerHTML = runnerWarmWorkspacePoolHTML(pool);
        if (focusedSlot) {
          const buttons = [...results.querySelectorAll('[data-warm-pool-slot]')];
          (buttons.find(button => button.dataset.warmPoolSlot === focusedSlot) || refresh).focus({ preventScroll: true });
        }
      } catch (err) {
        if (controller.signal.aborted) return;
        if (!pool) results.textContent = '';
        error.textContent = err.message;
        error.hidden = false;
      } finally {
        pending = false;
        panel.removeAttribute('aria-busy');
        for (const button of controls) button.removeAttribute('aria-disabled');
      }
    }

    refresh.onclick = () => load();
    results.addEventListener('click', event => {
      const slotID = event.target.closest('[data-warm-pool-slot]')?.dataset.warmPoolSlot;
      const slot = pool?.slots.find(item => item.id === slotID);
      if (slot) load({ slotId: slot.id, paused: !slot.paused });
    });
    load();
    const timer = setInterval(() => {
      if (!document.hidden) load();
    }, 15000);

    return {
      focus: ok,
      value: () => {
        clearInterval(timer);
        controller.abort();
        panel.remove();
        card.classList.remove('runner-warm-pool-dialog');
      },
    };
  });
}
