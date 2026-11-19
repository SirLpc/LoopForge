"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");
const form = {dataset: {projectFor: "demo"}, elements: {workspaceMode: {value: "warm_pool"}}};
const ctx = vm.createContext({state: {currentUser: {id: 1}, data: {workflows: [], users: [], projects: [], repositories: []}},
  document: {addEventListener(){}}, window: {addEventListener(){}}, clone: structuredClone, defaultRunEval: () => ({}), $: () => form, userById: () => ({}), userDisplay: () => "demo",
  userAvatarHTML: () => "", escapeHTML: String, safeColor: () => "#abcdef", isPlatformAdmin: () => true});
vm.runInContext(fs.readFileSync("app/projects.js", "utf8"), ctx);
for (const draft of [ctx.blankProjectDraft(), ctx.projectDraftFrom({id: "demo"})]) {
  assert.equal(draft.workspaceMode, "checkout");
  ctx.state.projectDraft = draft;
  ctx.state.projectDraftFor = "demo";
  for (const mode of ["warm_pool", "checkout"]) {
    const base = structuredClone(draft);
    form.elements.workspaceMode.value = mode;
    ctx.captureProjectAdminForm();
    assert.ok(ctx.projectDraftDirty(base, draft));
    const saved = ctx.buildProjectPayload(base, {workspaceMode: mode});
    assert.equal(saved.workspaceMode, mode);
    const html = ctx.projectAdminFormHTML(ctx.projectDraftFrom(saved), {creating: !saved.id, repos: []});
    assert.match(html, /<select[^>]*name="workspaceMode"/);
    assert.match(html, new RegExp('value="' + mode + '" selected'));
    assert.match(html, /普通检出/);
    assert.match(html, /预热工作区池/);
  }
}
vm.runInContext(fs.readFileSync("app/runs.js", "utf8"), ctx);
const waiting = {id: "run", status: "running", warmWorkspaceWaiting: true};
assert.equal(ctx.runDisplayStatus(waiting), "等待预热工作区");
assert.match(ctx.runStatusBadgeHTML(waiting), /等待预热工作区/);
assert.equal(ctx.runDisplayStatus({...waiting, status: "cancelled"}), "已取消");
assert.equal(ctx.runDisplayStatus({status: "running"}), "运行中");
vm.runInContext(fs.readFileSync("i18n-en.js", "utf8"), ctx);
assert.equal(ctx.window.LF_I18N_EN["等待预热工作区"], "Waiting for a warm workspace");
assert.equal(ctx.window.LF_I18N_EN["取消排队"], "Cancel wait");
