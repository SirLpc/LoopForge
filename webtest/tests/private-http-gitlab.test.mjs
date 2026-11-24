import test from 'node:test';
import assert from 'node:assert/strict';
import { loadApp, settle } from '../lib/load-app.mjs';

test('private HTTP GitLab uses the same explicit address in settings and repository forms', async (t) => {
  const app = await loadApp();
  t.after(app.close);
  const endpoint = '/api/users/1/git-http-credentials';
  const origin = 'http://192.168.50.10:8443';
  const remote = `${origin}/group/repo.git`;
  app.router.get(endpoint, []);
  app.router.put(endpoint, [{ id: 'local', host: origin, platform: 'gitlab', name: origin }]);
  app.evaluate(`
    state.currentUser={...state.data.users[0],id:1,isGuest:false};
    openGitHTTPCredentials();
  `);
  await settle(6);
  const dialog = app.document.querySelector('#gitHTTPCredentialDialog');
  const form = dialog.querySelector('form');
  dialog.querySelector('[data-http-cred-add]').click();
  form.elements.host.value = origin;
  form.elements.host.dispatchEvent(new app.window.Event('change', { bubbles: true }));
  assert.equal(form.elements.platform.value, 'gitlab');
  form.elements.token.value = 'glpat-settings-test';
  form.dispatchEvent(new app.window.Event('submit', { bubbles: true, cancelable: true }));
  await settle(6);
  assert.equal(app.router.calls.find(call => call.method === 'PUT' && call.path === endpoint).body.host, origin);
  assert.equal(app.evaluate(`parseGongfengRepoURL(${JSON.stringify(remote)})?.cloneUrl`), remote);
  assert.equal(app.evaluate(`parseGongfengRepoURL(${JSON.stringify(`${origin}/group/repo`)})?.cloneUrl`), remote);
  assert.equal(app.evaluate(`parseGongfengRepoURL('http://8.8.8.8/group/repo.git')`), null);
});
