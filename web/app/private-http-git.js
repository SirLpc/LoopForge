// Project address preview for private HTTP GitLab. The API validates the allowlist.
globalThis.LoopForgeGitPrivateHTTP = function parsePrivateHTTPGitRepository(raw) {
  const address = String(raw || '').trim().replace(/^git clone\s+/i, '');
  if (!/^http:\/\//i.test(address) || /[\s\\?#]/.test(address)) return null;
  try {
    const url = new URL(address);
    if (url.username || url.password) return null;
    const rawAuthority = address.slice('http://'.length).split('/')[0];
    const ip = rawAuthority.match(/^(\d+\.\d+\.\d+\.\d+|\[[a-f0-9:]+\])(?::[1-9]\d*)?$/i)?.[1];
    const privateIPv4 = ip === url.hostname && /^(10\.|192\.168\.|172\.(1[6-9]|2\d|3[01])\.)/.test(ip);
    const privateIPv6 = ip?.startsWith('[') && /^\[f[cd][a-f0-9]{2}:/i.test(url.hostname);
    if (!privateIPv4 && !privateIPv6) return null;
    const parts = url.pathname.replace(/^\/+|\/+$/g, '').split('/');
    if (parts.some(part => !part || ['.', '..'].includes(decodeURIComponent(part)))) return null;
    const name = parts[parts.length - 1].replace(/\.git$/, '');
    if (!name) return null;
    const path = parts.join('/').replace(/\.git$/, '');
    const webUrl = `${url.origin}/${path}`;
    return { provider: 'https', host: url.host, project: `${url.host}/${path}`, name, cloneUrl: `${webUrl}.git`, webUrl };
  } catch {
    return null;
  }
};
