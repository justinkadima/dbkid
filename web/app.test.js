'use strict';

// Dependency-free browser interaction tests. They start a disposable HTTP API and
// a separate headless Chrome profile; they never contact a database.
const { test, before, after } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const os = require('node:os');
const path = require('node:path');
const { spawn } = require('node:child_process');
const { setTimeout: delay } = require('node:timers/promises');

const webRoot = __dirname;
const chromeCandidates = [
  process.env.CHROME_BIN,
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  '/Applications/Chromium.app/Contents/MacOS/Chromium',
  '/usr/bin/google-chrome',
  '/usr/bin/chromium',
  '/usr/bin/chromium-browser',
].filter(Boolean);
let chrome;
let profile;
let debugPort;

async function waitFor(predicate, timeout = 4000) {
  const end = Date.now() + timeout;
  do {
    if (await predicate()) return true;
    await delay(25);
  } while (Date.now() < end);
  return false;
}

class DevTools {
  constructor(socket) {
    this.socket = socket;
    this.sequence = 0;
    this.pending = new Map();
    this.handlers = new Map();
    socket.addEventListener('message', ({ data }) => {
      const message = JSON.parse(data);
      if (!message.id) {
        const handler = this.handlers.get(message.method);
        if (handler) handler(message.params);
        return;
      }
      const pending = this.pending.get(message.id);
      if (!pending) return;
      this.pending.delete(message.id);
      if (message.error) pending.reject(new Error(message.error.message));
      else pending.resolve(message.result);
    });
  }

  static async connect(url) {
    const socket = new WebSocket(url);
    await new Promise((resolve, reject) => {
      socket.addEventListener('open', resolve, { once: true });
      socket.addEventListener('error', reject, { once: true });
    });
    return new DevTools(socket);
  }

  call(method, params = {}) {
    const id = ++this.sequence;
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      this.socket.send(JSON.stringify({ id, method, params }));
    });
  }

  async evaluate(expression) {
    const response = await this.call('Runtime.evaluate', {
      expression,
      awaitPromise: true,
      returnByValue: true,
      userGesture: true,
    });
    if (response.exceptionDetails) {
      const detail = response.exceptionDetails.exception && response.exceptionDetails.exception.description
        ? response.exceptionDetails.exception.description
        : response.exceptionDetails.text;
      throw new Error(detail);
    }
    return response.result.value;
  }

  close() {
    this.socket.close();
  }
}

before(async () => {
  const executable = chromeCandidates.find((candidate) => fs.existsSync(candidate));
  assert.ok(executable, 'Chrome/Chromium is required; set CHROME_BIN to its executable');
  assert.equal(typeof WebSocket, 'function', 'Node must provide a built-in WebSocket');
  const temporaryDirectory = process.env.DBKID_TEST_TMPDIR || os.tmpdir();
  profile = fs.mkdtempSync(path.join(temporaryDirectory, 'dbkid-test-chrome-'));
  chrome = spawn(executable, [
    '--headless=new',
    '--remote-debugging-port=0',
    '--no-first-run',
    '--no-default-browser-check',
    '--disable-background-networking',
    '--disable-extensions',
    '--disable-gpu',
    '--no-sandbox',
    `--user-data-dir=${profile}`,
    'about:blank',
  ], { stdio: 'ignore', env: { ...process.env, TMPDIR: temporaryDirectory, TMP: temporaryDirectory, TEMP: temporaryDirectory } });
  const portFile = path.join(profile, 'DevToolsActivePort');
  assert.ok(await waitFor(() => fs.existsSync(portFile), 15000), 'headless Chrome did not start');
  debugPort = Number(fs.readFileSync(portFile, 'utf8').split('\n')[0]);
});

after(async () => {
  if (chrome && chrome.exitCode === null) {
    const exited = new Promise((resolve) => chrome.once('exit', resolve));
    chrome.kill('SIGTERM');
    await Promise.race([exited, delay(3000)]);
    if (chrome.exitCode === null) chrome.kill('SIGKILL');
  }
  // Preserve the disposable test profile; cleanup requires explicit approval.
});

async function fixture(options = {}) {
  const calls = [];
  const session = {
    csrf_token: 'fixture-csrf',
    generation: 7,
    connected: false,
    host: '',
    port: 0,
    user: '',
    server_version: '',
    ...options.session,
  };
  const server = http.createServer(async (req, res) => {
    const url = new URL(req.url, 'http://127.0.0.1');
    res.setHeader('Cache-Control', 'no-store');
    if (options.closeConnections) res.setHeader('Connection', 'close');
    if (url.pathname.startsWith('/api/')) {
      const chunks = [];
      for await (const chunk of req) chunks.push(chunk);
      const raw = Buffer.concat(chunks).toString('utf8');
      const body = raw ? JSON.parse(raw) : null;
      const call = { method: req.method, url, headers: req.headers, body };
      calls.push(call);
      res.setHeader('Content-Type', 'application/json');
      if (url.pathname === '/api/session') {
        res.setHeader('Set-Cookie', 'dbkid-fixture=1; HttpOnly; SameSite=Strict; Path=/');
        const override = options.sessionHandler && await options.sessionHandler(call, { calls, session });
        if (override) {
          if (override.drop) { req.socket.destroy(); return; }
          res.statusCode = override.status || 200;
          if (override.raw !== undefined) res.end(override.raw);
          else res.end(JSON.stringify(override.body));
        } else {
          res.end(JSON.stringify(session));
        }
        return;
      }
      const reply = options.handler && await options.handler(call, { calls, session });
      if (reply) {
        if (reply.drop) { req.socket.destroy(); return; }
        res.statusCode = reply.status || 200;
        if (reply.raw !== undefined) res.end(reply.raw);
        else res.end(JSON.stringify(reply.body));
        return;
      }
      res.statusCode = 404;
      res.end(JSON.stringify({ error: 'Fixture route not implemented' }));
      return;
    }
    const files = { '/': 'index.html', '/style.css': 'style.css', '/app.js': 'app.js' };
    const filename = files[url.pathname];
    if (filename && fs.existsSync(path.join(webRoot, filename))) {
      res.setHeader('Content-Type', filename.endsWith('.css') ? 'text/css' : filename.endsWith('.js') ? 'text/javascript' : 'text/html');
      res.end(fs.readFileSync(path.join(webRoot, filename)));
    } else if (url.pathname === '/') {
      res.setHeader('Content-Type', 'text/html');
      res.end('<!doctype html><html><body></body></html>');
    } else {
      res.statusCode = 404;
      res.end();
    }
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  return {
    calls,
    url: `http://127.0.0.1:${server.address().port}/`,
    close: () => new Promise((resolve) => {
      server.close(resolve);
      // A closed Chrome target can leave an in-flight fixture socket open.
      // Only tear down this disposable test server's connections.
      server.closeAllConnections();
    }),
  };
}

async function openPage(url) {
  const target = await fetch(`http://127.0.0.1:${debugPort}/json/new?about:blank`, { method: 'PUT' }).then((res) => res.json());
  const cdp = await DevTools.connect(target.webSocketDebuggerUrl);
  await cdp.call('Page.enable');
  await cdp.call('Runtime.enable');
  await cdp.call('DOM.enable');
  await cdp.call('Page.navigate', { url });
  return {
    evaluate: (expression) => cdp.evaluate(expression),
    setViewport: (width, height) => cdp.call('Emulation.setDeviceMetricsOverride', { width, height, deviceScaleFactor: 1, mobile: false }),
    setFiles: async (selector, files) => {
      const domDocument = await cdp.call('DOM.getDocument', { depth: 0 });
      const found = await cdp.call('DOM.querySelector', { nodeId: domDocument.root.nodeId, selector });
      if (!found || !found.nodeId) throw new Error(`file input not found: ${selector}`);
      await cdp.call('DOM.setFileInputFiles', { files, nodeId: found.nodeId });
    },
    screenshot: async (filename) => {
      const image = await cdp.call('Page.captureScreenshot', { format: 'png', fromSurface: true });
      fs.writeFileSync(filename, Buffer.from(image.data, 'base64'));
    },
    setOffline: async (offline) => {
      await cdp.call('Network.enable');
      await cdp.call('Network.emulateNetworkConditions', { offline, latency: 0, downloadThroughput: -1, uploadThroughput: -1 });
    },
    onDialog: (handler) => cdp.handlers.set('Page.javascriptDialogOpening', (params) => {
      cdp.call('Page.handleJavaScriptDialog', { accept: handler(params) });
    }),
    close: async () => {
      cdp.close();
      await fetch(`http://127.0.0.1:${debugPort}/json/close/${target.id}`);
    },
  };
}

test('a disconnected session bootstraps into the connection screen', async (t) => {
  const api = await fixture();
  const page = await openPage(api.url);
  t.after(async () => { await page.close(); await api.close(); });

  assert.ok(await waitFor(() => api.calls.some((call) => call.url.pathname === '/api/session')),
    'the interface must request the session before connecting');
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#connect-view")?.hidden === false')),
    'a disconnected session must show the connection form');
  assert.equal(await page.evaluate('document.querySelector("#host")?.value'), '127.0.0.1');
  assert.equal(await page.evaluate('document.querySelector("#tls")?.value'), 'disable');
  if (process.env.DBKID_SCREENSHOT_DIR) {
    await page.setViewport(1200, 800);
    await page.screenshot(path.join(process.env.DBKID_SCREENSHOT_DIR, 'dbkid-connect-desktop.png'));
    await page.setViewport(320, 800);
    await page.screenshot(path.join(process.env.DBKID_SCREENSHOT_DIR, 'dbkid-connect-mobile.png'));
  }
});

test('connecting posts with CSRF and cookie, then clears the password', async (t) => {
  const api = await fixture({ handler: (call) => {
    if (call.url.pathname === '/api/connect') return { body: {
      connected: true, generation: 7, host: call.body.host, port: call.body.port,
      user: call.body.user, server_version: 'fixture-8.0',
    } };
  } });
  const page = await openPage(api.url);
  t.after(async () => { await page.close(); await api.close(); });
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#connect-view")?.hidden === false')));
  await page.evaluate(`(() => {
    document.querySelector('#user').value = 'fixture-user';
    document.querySelector('#password').value = crypto.randomUUID();
    document.querySelector('#connect-form').requestSubmit();
  })()`);
  assert.ok(await waitFor(() => api.calls.some((call) => call.url.pathname === '/api/connect')),
    'submitting the form must POST /api/connect');
  const call = api.calls.find((entry) => entry.url.pathname === '/api/connect');
  assert.equal(call.method, 'POST');
  assert.equal(call.headers['x-csrf-token'], 'fixture-csrf');
  assert.match(call.headers.cookie, /dbkid-fixture=1/);
  assert.deepEqual({ ...call.body, password: undefined }, {
    host: '127.0.0.1', port: 3306, user: 'fixture-user', password: undefined,
    tls: 'disable', allow_insecure: false,
  });
  assert.ok(typeof call.body.password === 'string' && call.body.password.length > 0);
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#app-view")?.hidden === false')),
    'a successful connection must open the workspace');
  assert.equal(await page.evaluate('document.querySelector("#password").value'), '', 'password must be cleared');
  assert.equal(await page.evaluate('localStorage.length + sessionStorage.length'), 0, 'credentials must not be persisted');
});

test('remote plaintext needs native confirmation and explicit insecure approval', async (t) => {
  const api = await fixture({ handler: (call) => {
    if (call.url.pathname === '/api/connect') return { body: {
      connected: true, generation: 7, host: call.body.host, port: call.body.port,
      user: call.body.user, server_version: 'fixture-8.0',
    } };
  } });
  const page = await openPage(api.url);
  t.after(async () => { await page.close(); await api.close(); });
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#connect-view")?.hidden === false')));
  await page.evaluate(`(() => {
    const host = document.querySelector('#host');
    host.value = 'database.example.test';
    host.dispatchEvent(new Event('input', { bubbles: true }));
    document.querySelector('#user').value = 'fixture-user';
  })()`);
  assert.equal(await page.evaluate('document.querySelector("#tls").value'), 'verify',
    'remote hosts should default to certificate verification');
  await page.evaluate(`(() => {
    const tls = document.querySelector('#tls');
    tls.value = 'disable';
    tls.dispatchEvent(new Event('change', { bubbles: true }));
    document.querySelector('#password').value = crypto.randomUUID();
  })()`);
  const prompts = [];
  let approve = false;
  page.onDialog((dialog) => { prompts.push(dialog); return approve; });
  await page.evaluate('document.querySelector("#connect-form").requestSubmit()');
  assert.equal(prompts.length, 1, 'a native confirmation must be shown');
  assert.equal(prompts[0].type, 'confirm');
  assert.match(prompts[0].message, /unencrypted/i);
  assert.equal(api.calls.filter((call) => call.url.pathname === '/api/connect').length, 0,
    'cancelling must not send credentials');
  approve = true;
  await page.evaluate(`(() => {
    document.querySelector('#password').value = crypto.randomUUID();
    document.querySelector('#connect-form').requestSubmit();
  })()`);
  assert.ok(await waitFor(() => api.calls.some((call) => call.url.pathname === '/api/connect')));
  assert.equal(prompts.length, 2);
  const call = api.calls.find((entry) => entry.url.pathname === '/api/connect');
  assert.equal(call.body.tls, 'disable');
  assert.equal(call.body.allow_insecure, true);
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#password").value === ""')));
});

test('connected sessions list databases and safely load encoded table names', async (t) => {
  const database = 'prod /? & <img src=x onerror=window.__dbkid_xss=1>';
  const table = 'users <svg onload=window.__dbkid_xss=2>';
  const api = await fixture({
    session: { connected: true, generation: 7, host: '127.0.0.1', port: 3306, user: 'fixture-user', server_version: 'fixture-8.0' },
    handler: (call) => {
      if (call.url.pathname === '/api/databases') return { body: { databases: [database] } };
      if (call.url.pathname === '/api/tables') return { body: { tables: [{ name: table, type: 'VIEW' }] } };
    },
  });
  const page = await openPage(api.url);
  t.after(async () => { await page.close(); await api.close(); });
  assert.ok(await waitFor(() => api.calls.some((call) => call.url.pathname === '/api/databases')),
    'connected sessions must load databases');
  assert.ok(await waitFor(() => page.evaluate(`Array.from(document.querySelectorAll('.database-button')).some(b => b.dataset.database === ${JSON.stringify(database)})`)),
    'the database must be visible in the navigation');
  await page.evaluate(`Array.from(document.querySelectorAll('.database-button')).find(b => b.dataset.database === ${JSON.stringify(database)}).click()`);
  assert.ok(await waitFor(() => api.calls.some((call) => call.url.pathname === '/api/tables')));
  const tablesCall = api.calls.find((call) => call.url.pathname === '/api/tables');
  assert.equal(tablesCall.url.searchParams.get('database'), database);
  assert.match(tablesCall.url.search, /%2F/);
  assert.equal(tablesCall.headers['x-csrf-token'], 'fixture-csrf');
  assert.match(tablesCall.headers.cookie, /dbkid-fixture=1/);
  assert.ok(await waitFor(() => page.evaluate(`Array.from(document.querySelectorAll('.table-button')).some(b => b.dataset.table === ${JSON.stringify(table)})`)));
  assert.equal(await page.evaluate('document.querySelector("#database-list img, #database-list svg") === null'), true);
  assert.equal(await page.evaluate('window.__dbkid_xss === undefined'), true);
  assert.ok(await page.evaluate(`document.querySelector('#database-list').textContent.includes(${JSON.stringify(table)})`));
});

test('table browsing encodes identifiers, shows NULL distinctly, and pages safely', async (t) => {
  const database = 'catalog / &';
  const table = 'items ? /';
  const unsafe = '<img src=x onerror=window.__row_xss=1>';
  const api = await fixture({
    session: { connected: true, generation: 7, host: '127.0.0.1', port: 3306, user: 'fixture-user', server_version: 'fixture-8.0' },
    handler: (call) => {
      if (call.url.pathname === '/api/databases') return { body: { databases: [database] } };
      if (call.url.pathname === '/api/tables') return { body: { tables: [{ name: table, type: 'BASE TABLE' }] } };
      if (call.url.pathname === '/api/rows') {
        const page = Number(call.url.searchParams.get('page'));
        return { body: { columns: ['value', 'nullable'], rows: page === 1 ? [[unsafe, null], ['NULL', '']] : [['second page', 'done']], page, page_size: Number(call.url.searchParams.get('page_size')), has_next: page === 1 } };
      }
    },
  });
  const page = await openPage(api.url);
  t.after(async () => { await page.close(); await api.close(); });
  assert.ok(await waitFor(() => page.evaluate('!!document.querySelector(".database-button")')));
  await page.evaluate('document.querySelector(".database-button").click()');
  assert.ok(await waitFor(() => page.evaluate('!!document.querySelector(".table-button")')));
  await page.evaluate('document.querySelector(".table-button").click()');
  assert.ok(await waitFor(() => api.calls.some((call) => call.url.pathname === '/api/rows')),
    'selecting a table must load its first page');
  const first = api.calls.find((call) => call.url.pathname === '/api/rows');
  assert.equal(first.url.searchParams.get('database'), database);
  assert.equal(first.url.searchParams.get('table'), table);
  assert.equal(first.url.searchParams.get('page'), '1');
  assert.equal(first.url.searchParams.get('page_size'), '50');
  assert.ok(await waitFor(() => page.evaluate('document.querySelectorAll("#browse-result tbody tr").length === 2')));
  assert.equal(await page.evaluate('document.querySelectorAll("#browse-result .null-value").length'), 1);
  assert.equal(await page.evaluate('document.querySelector("#browse-result img") === null && window.__row_xss === undefined'), true);
  assert.ok(await page.evaluate(`document.querySelector('#browse-result').textContent.includes(${JSON.stringify(unsafe)})`));
  assert.equal(await page.evaluate('document.querySelector("#prev-page").disabled'), true);
  assert.equal(await page.evaluate('document.querySelector("#next-page").disabled'), false);
  await page.evaluate('document.querySelector("#next-page").click()');
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#page-label").textContent.includes("Page 2")')));
  assert.ok(await page.evaluate('document.querySelector("#browse-result").textContent.includes("second page")'));
  assert.equal(await page.evaluate('document.querySelector("#next-page").disabled'), true);
  assert.equal(await page.evaluate('document.querySelector("#prev-page").disabled'), false);
  await page.evaluate('document.querySelector("#prev-page").click()');
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#page-label").textContent.includes("Page 1")')));
  await page.evaluate(`(() => { const select = document.querySelector('#page-size'); select.value = '200'; select.dispatchEvent(new Event('change', { bubbles: true })); })()`);
  assert.ok(await waitFor(() => api.calls.some((call) => call.url.pathname === '/api/rows' && call.url.searchParams.get('page_size') === '200' && call.url.searchParams.get('page') === '1')),
    'changing page size must restart at page one within the 200-row limit');
});

test('browse reports optional cell truncation and clears it on later pages', async (t) => {
  const api = await fixture({
    session: { connected: true, generation: 7, host: '127.0.0.1', port: 3306, user: 'fixture-user', server_version: 'fixture-8.0' },
    handler: (call) => {
      if (call.url.pathname === '/api/databases') return { body: { databases: ['fixture_db'] } };
      if (call.url.pathname === '/api/tables') return { body: { tables: [{ name: 'items', type: 'BASE TABLE' }] } };
      if (call.url.pathname === '/api/rows') {
        const page = Number(call.url.searchParams.get('page'));
        return { body: { columns: ['value'], rows: [[String(page)]], page, has_next: page === 1,
          ...(page === 1 ? { cells_truncated: true } : {}) } };
      }
    },
  });
  const page = await openPage(api.url);
  t.after(async () => { await page.close(); await api.close(); });
  assert.ok(await waitFor(() => page.evaluate('!!document.querySelector(".database-button")')));
  await page.evaluate('document.querySelector(".database-button").click()');
  assert.ok(await waitFor(() => page.evaluate('!!document.querySelector(".table-button")')));
  await page.evaluate('document.querySelector(".table-button").click()');
  assert.ok(await waitFor(() => page.evaluate('!document.querySelector("#next-page").disabled')));
  const firstStatus = await page.evaluate('document.querySelector("#browse-status").textContent');
  assert.match(firstStatus, /cell.*truncat/i);
  assert.doesNotMatch(firstStatus, /500/);
  assert.ok(await page.evaluate('document.querySelector("#browse-status").getClientRects().length > 0'));
  assert.equal(await page.evaluate('document.querySelector("#browse-status").getAttribute("role")'), 'status');
  await page.evaluate('document.querySelector("#next-page").click()');
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#page-label").textContent.includes("Page 2")')));
  assert.doesNotMatch(await page.evaluate('document.querySelector("#browse-status").textContent'), /truncat/i);
});

test('browse warns that pages can shift without stable ordering', async (t) => {
  const api = await fixture({
    session: { connected: true, generation: 7, host: '127.0.0.1', port: 3306, user: 'fixture-user', server_version: 'fixture-8.0' },
    handler: (call) => {
      if (call.url.pathname === '/api/databases') return { body: { databases: ['fixture_db'] } };
      if (call.url.pathname === '/api/tables') return { body: { tables: [{ name: 'items', type: 'BASE TABLE' }] } };
      if (call.url.pathname === '/api/rows') return { body: { columns: ['id'], rows: [['1']], page: 1, has_next: false } };
    },
  });
  const page = await openPage(api.url);
  t.after(async () => { await page.close(); await api.close(); });
  assert.ok(await waitFor(() => page.evaluate('!!document.querySelector(".database-button")')));
  await page.evaluate('document.querySelector(".database-button").click()');
  assert.ok(await waitFor(() => page.evaluate('!!document.querySelector(".table-button")')));
  await page.evaluate('document.querySelector(".table-button").click()');
  assert.ok(await waitFor(() => page.evaluate('document.querySelectorAll("#browse-result tbody tr").length === 1')));
  const warning = await page.evaluate(`(() => Array.from(document.querySelectorAll('#browse-panel p'))
    .filter(p => p.getClientRects().length > 0).map(p => p.textContent).find(text => /stable order/i.test(text)) || '')()`);
  assert.match(warning, /without.*stable order/i);
  assert.match(warning, /pag(?:e|ination).*shift/i);
  assert.match(warning, /data changes/i);
  assert.ok(await page.evaluate(`['prev-page', 'next-page'].every(id =>
    document.getElementById(id).getAttribute('aria-describedby').split(/\\s+/).includes('browse-pagination-help'))`),
    'pagination controls must describe the ordering warning to assistive technology');
});

test('structure renders column metadata safely for the selected table', async (t) => {
  const database = 'schema / &';
  const table = 't? /';
  const unsafe = '<img src=x onerror=window.__structure_xss=1>';
  const api = await fixture({
    session: { connected: true, generation: 7, host: '127.0.0.1', port: 3306, user: 'fixture-user', server_version: 'fixture-8.0' },
    handler: (call) => {
      if (call.url.pathname === '/api/databases') return { body: { databases: [database] } };
      if (call.url.pathname === '/api/tables') return { body: { tables: [{ name: table, type: 'BASE TABLE' }] } };
      if (call.url.pathname === '/api/rows') return { body: { columns: ['id'], rows: [], page: 1, page_size: 50, has_next: false } };
      if (call.url.pathname === '/api/structure') return { body: { columns: [
        { name: unsafe, type: 'varchar(30)', nullable: true, key: 'PRI', default: null, extra: 'auto_increment' },
        { name: 'ordinary', type: 'int', nullable: false, key: '', default: '0', extra: '' },
      ] } };
    },
  });
  const page = await openPage(api.url);
  t.after(async () => { await page.close(); await api.close(); });
  assert.ok(await waitFor(() => page.evaluate('!!document.querySelector(".database-button")')));
  await page.evaluate('document.querySelector(".database-button").click()');
  assert.ok(await waitFor(() => page.evaluate('!!document.querySelector(".table-button")')));
  await page.evaluate('document.querySelector(".table-button").click()');
  await page.evaluate('document.querySelector("#tab-structure").click()');
  assert.ok(await waitFor(() => api.calls.some((call) => call.url.pathname === '/api/structure')),
    'opening Structure must request column metadata');
  const call = api.calls.find((entry) => entry.url.pathname === '/api/structure');
  assert.equal(call.url.searchParams.get('database'), database);
  assert.equal(call.url.searchParams.get('table'), table);
  assert.equal(call.headers['x-csrf-token'], 'fixture-csrf');
  assert.ok(await waitFor(() => page.evaluate('document.querySelectorAll("#structure-result tbody tr").length === 2')));
  assert.ok(await page.evaluate(`document.querySelector('#structure-result').textContent.includes(${JSON.stringify(unsafe)})`));
  assert.ok(await page.evaluate('document.querySelector("#structure-result").textContent.includes("PRI") && document.querySelector("#structure-result").textContent.includes("auto_increment")'));
  assert.equal(await page.evaluate('document.querySelectorAll("#structure-result .null-value").length'), 1);
  assert.equal(await page.evaluate('document.querySelector("#structure-result img") === null && window.__structure_xss === undefined'), true);
});

test('SQL works without tables and always requires native confirmation', async (t) => {
  const database = 'empty / database';
  const unsafe = '<img src=x onerror=window.__query_xss=1>';
  let queryCount = 0;
  const api = await fixture({
    session: { connected: true, generation: 7, host: '127.0.0.1', port: 3306, user: 'fixture-user', server_version: 'fixture-8.0' },
    handler: (call) => {
      if (call.url.pathname === '/api/databases') return { body: { databases: [database] } };
      if (call.url.pathname === '/api/tables') return { body: { tables: [] } };
      if (call.url.pathname === '/api/query') {
        queryCount++;
        return { body: queryCount === 1
          ? { columns: ['value', 'nullable'], rows: [[unsafe, null]], affected_rows: null, truncated: true, duration_ms: 17 }
          : { columns: [], rows: [], affected_rows: 3, truncated: false, duration_ms: 4 } };
      }
    },
  });
  const page = await openPage(api.url);
  t.after(async () => { await page.close(); await api.close(); });
  assert.ok(await waitFor(() => page.evaluate('!!document.querySelector(".database-button")')));
  await page.evaluate('document.querySelector(".database-button").click()');
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#database-list").textContent.includes("No tables")')));
  assert.equal(await page.evaluate('document.querySelector("#tab-sql").disabled'), false);
  await page.evaluate('document.querySelector("#tab-sql").click()');
  assert.equal(await page.evaluate('document.querySelector("#sql-panel")?.hidden'), false,
    'SQL must be usable even when the database has no tables');
  await page.evaluate('document.querySelector("#sql-editor").value = "SELECT 1"');
  let approve = false;
  const prompts = [];
  page.onDialog((dialog) => { prompts.push(dialog); return approve; });
  await page.evaluate('document.querySelector("#sql-form").requestSubmit()');
  assert.equal(prompts.length, 1);
  assert.equal(prompts[0].type, 'confirm');
  assert.match(prompts[0].message, /modify|delete/i);
  assert.equal(api.calls.filter((call) => call.url.pathname === '/api/query').length, 0,
    'cancelling must not run SQL');
  approve = true;
  await page.evaluate(`document.querySelector('#sql-editor').dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', ctrlKey: true, bubbles: true, cancelable: true }))`);
  assert.ok(await waitFor(() => api.calls.some((call) => call.url.pathname === '/api/query')));
  const call = api.calls.find((entry) => entry.url.pathname === '/api/query');
  assert.equal(call.method, 'POST');
  assert.equal(call.headers['x-csrf-token'], 'fixture-csrf');
  assert.match(call.headers['x-dbkid-request-id'] || '', /^[A-Za-z0-9_-]{22,64}$/, 'SQL requests need a unique replay-protection ID');
  assert.match(call.headers.cookie, /dbkid-fixture=1/);
  assert.deepEqual(call.body, { database, sql: 'SELECT 1', confirmed: true });
  assert.ok(await waitFor(() => page.evaluate('document.querySelectorAll("#query-result tbody tr").length === 1')));
  assert.ok(await page.evaluate('document.querySelector("#query-status").textContent.toLowerCase().includes("truncat")'));
  assert.equal(await page.evaluate('document.querySelectorAll("#query-result .null-value").length'), 1);
  assert.equal(await page.evaluate('document.querySelector("#query-result img") === null && window.__query_xss === undefined'), true);
  assert.ok(await page.evaluate(`document.querySelector('#query-result').textContent.includes(${JSON.stringify(unsafe)})`));
  await page.evaluate(`(() => { document.querySelector('#sql-editor').value = 'UPDATE items SET x = 1'; document.querySelector('#sql-form').requestSubmit(); })()`);
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#query-status").textContent.includes("3 rows affected")')));
  const queryCalls = api.calls.filter((entry) => entry.url.pathname === '/api/query');
  assert.equal(queryCalls.length, 2);
  assert.notEqual(queryCalls[0].headers['x-dbkid-request-id'], queryCalls[1].headers['x-dbkid-request-id'], 'separately confirmed executions need distinct IDs');
  assert.equal(prompts.length, 3, 'each SQL execution must be separately confirmed');
});

test('SQL help explains fresh connections and the scope of SQL effects', async (t) => {
  const api = await fixture({
    session: { connected: true, generation: 7, host: 'mysql.example.test', port: 3307, user: 'fixture-user', server_version: 'fixture-8.0' },
    handler: (call) => {
      if (call.url.pathname === '/api/databases') return { body: { databases: ['fixture_db'] } };
      if (call.url.pathname === '/api/tables') return { body: { tables: [] } };
    },
  });
  const page = await openPage(api.url);
  t.after(async () => { await page.close(); await api.close(); });
  assert.ok(await waitFor(() => page.evaluate('!!document.querySelector(".database-button")')));
  await page.evaluate(`(() => { document.querySelector('.database-button').click(); document.querySelector('#tab-sql').click(); })()`);
  assert.ok(await page.evaluate('document.querySelector("#sql-help").getClientRects().length > 0'), 'SQL help must be visible');
  assert.ok(await page.evaluate('document.querySelector("#sql-editor").getAttribute("aria-describedby").split(/\\s+/).includes("sql-help")'));
  const help = await page.evaluate('document.querySelector("#sql-help").textContent');
  assert.match(help, /each statement/i);
  assert.match(help, /fresh (?:database )?connection/i);
  assert.match(help, /transaction/i);
  assert.match(help, /session/i);
  assert.match(help, /not persist/i);
  assert.match(help, /separate requests/i);
  assert.match(help, /other databases/i);
  assert.match(help, /files/i);
  assert.match(help, /mysql permissions/i);
  assert.match(help, /modify/i);
  assert.match(help, /delete/i);
});

test('SQL confirmation identifies the selected database and server', async (t) => {
  const api = await fixture({
    session: { connected: true, generation: 7, host: 'mysql.example.test', port: 3307, user: 'fixture-user', server_version: 'fixture-8.0' },
    handler: (call) => {
      if (call.url.pathname === '/api/databases') return { body: { databases: ['alpha_db', 'beta_db'] } };
      if (call.url.pathname === '/api/tables') return { body: { tables: [] } };
    },
  });
  const page = await openPage(api.url);
  t.after(async () => { await page.close(); await api.close(); });
  assert.ok(await waitFor(() => page.evaluate('document.querySelectorAll(".database-button").length === 2')));
  const prompts = [];
  page.onDialog((dialog) => { prompts.push(dialog); return false; });
  for (const database of ['alpha_db', 'beta_db']) {
    await page.evaluate(`(() => {
      Array.from(document.querySelectorAll('.database-button')).find(b => b.dataset.database === ${JSON.stringify(database)}).click();
      document.querySelector('#tab-sql').click();
      document.querySelector('#sql-editor').value = 'SELECT 1';
      document.querySelector('#sql-form').requestSubmit();
    })()`);
    const prompt = prompts[prompts.length - 1];
    assert.equal(prompt.type, 'confirm');
    assert.ok(prompt.message.includes(database), 'confirmation must identify the currently selected database');
    assert.ok(prompt.message.includes('mysql.example.test:3307'), 'confirmation must identify the connected server');
    assert.match(prompt.message, /modify/i);
    assert.match(prompt.message, /delete/i);
  }
  assert.equal(prompts.length, 2);
  assert.equal(api.calls.filter((call) => call.url.pathname === '/api/query').length, 0,
    'cancelling either confirmation must not run SQL');
});

test('SQL errors and lost responses warn to inspect before retrying', async (t) => {
  const timeoutMessage = 'SQL timed out <img src=x onerror=window.__query_error_xss=1>';
  const replies = [
    { status: 504, body: { error: timeoutMessage } },
    { drop: true },
    { raw: 'not-json' },
  ];
  let queryCount = 0;
  const api = await fixture({
    // Chromium may transparently retry a POST after a reused connection resets;
    // fresh connections let this test check the application's own retry behavior.
    closeConnections: true,
    session: { connected: true, generation: 7, host: 'mysql.example.test', port: 3307, user: 'fixture-user', server_version: 'fixture-8.0' },
    handler: (call) => {
      if (call.url.pathname === '/api/databases') return { body: { databases: ['fixture_db'] } };
      if (call.url.pathname === '/api/tables') return { body: { tables: [] } };
      if (call.url.pathname === '/api/query') return replies[queryCount++];
    },
  });
  const page = await openPage(api.url);
  t.after(async () => { await page.close(); await api.close(); });
  assert.ok(await waitFor(() => page.evaluate('!!document.querySelector(".database-button")')));
  await page.evaluate(`(() => {
    document.querySelector('.database-button').click();
    document.querySelector('#tab-sql').click();
    document.querySelector('#sql-editor').value = 'UPDATE items SET value = 1';
  })()`);
  let confirmationCount = 0;
  page.onDialog(() => { confirmationCount++; return true; });
  for (const [index, expectedError] of [timeoutMessage, 'Could not reach DBKid', 'invalid response'].entries()) {
    await page.evaluate('document.querySelector("#sql-form").requestSubmit()');
    assert.ok(await waitFor(() => queryCount >= index + 1));
    assert.ok(await waitFor(() => page.evaluate('document.querySelector("#query-status").classList.contains("is-error") && !document.querySelector("#run-query").disabled')));
    const status = await page.evaluate('document.querySelector("#query-status").textContent');
    assert.ok(status.includes(expectedError), `response ${index}: expected ${JSON.stringify(expectedError)} in ${JSON.stringify(status)}`);
    assert.match(status, /outcome may be unknown/i);
    assert.match(status, /inspect.*before retry/i);
    assert.doesNotMatch(status, /try again/i, 'network errors must not suggest retrying before inspection');
    assert.equal(await page.evaluate('document.querySelector("#query-status").getAttribute("role")'), 'status');
    assert.equal(await page.evaluate('document.querySelector("#query-status img") === null && window.__query_error_xss === undefined'), true);
    await delay(200);
    assert.equal(queryCount, index + 1, 'the application must not automatically retry a failed query');
    assert.equal(confirmationCount, index + 1, 'each query must be confirmed by the user');
  }
});

test('a delayed SQL failure remains visible after changing databases', async (t) => {
  const database = 'alpha_db <img src=x onerror=window.__warning_xss=1>';
  let releaseQuery;
  const pending = new Promise((resolve) => { releaseQuery = resolve; });
  const api = await fixture({
    closeConnections: true,
    session: { connected: true, generation: 7, host: 'mysql.example.test', port: 3307, user: 'fixture-user', server_version: 'fixture-8.0' },
    handler: async (call) => {
      if (call.url.pathname === '/api/databases') return { body: { databases: [database, 'beta_db'] } };
      if (call.url.pathname === '/api/tables') return { body: { tables: [] } };
      if (call.url.pathname === '/api/query') {
        await pending;
        return { body: { columns: 'invalid' } };
      }
    },
  });
  const page = await openPage(api.url);
  t.after(async () => { releaseQuery(); await page.close(); await api.close(); });
  assert.ok(await waitFor(() => page.evaluate('document.querySelectorAll(".database-button").length === 2')));
  await page.evaluate(`(() => {
    Array.from(document.querySelectorAll('.database-button')).find(b => b.dataset.database === ${JSON.stringify(database)}).click();
    document.querySelector('#tab-sql').click();
    document.querySelector('#sql-editor').value = 'UPDATE items SET value = 1';
  })()`);
  page.onDialog(() => true);
  await page.evaluate('document.querySelector("#sql-form").requestSubmit()');
  assert.ok(await waitFor(() => api.calls.some((call) => call.url.pathname === '/api/query')));
  await page.evaluate(`Array.from(document.querySelectorAll('.database-button')).find(b => b.dataset.database === 'beta_db').click()`);
  releaseQuery();
  const visibleAlert = `Array.from(document.querySelectorAll('[role="alert"]')).find(e => e.getClientRects().length > 0 && /outcome may be unknown/i.test(e.textContent))`;
  assert.ok(await waitFor(() => page.evaluate(`!!(${visibleAlert})`)), 'a late SQL failure must remain visible after switching databases');
  const warning = await page.evaluate(`(${visibleAlert}).textContent`);
  assert.ok(warning.includes(database));
  assert.match(warning, /mysql\.example\.test:3307/);
  assert.match(warning, /inspect.*before retry/i);
  assert.equal(await page.evaluate(`(${visibleAlert}).querySelector('img') === null && window.__warning_xss === undefined`), true);
  await page.setViewport(320, 800);
  assert.equal(await page.evaluate('document.documentElement.scrollWidth <= innerWidth'), true,
    'background SQL warnings must not overflow a narrow viewport');
  assert.equal(api.calls.filter((call) => call.url.pathname === '/api/query').length, 1);
  await page.evaluate('document.querySelector("#tab-sql").click()');
  assert.equal(await page.evaluate('document.querySelector("#query-status").textContent'), 'No query run yet.',
    'the late failure must not overwrite the new database query status');
  await page.evaluate(`(${visibleAlert}).querySelector('button').click()`);
  assert.equal(await page.evaluate(`!!(${visibleAlert})`), false, 'the warning must be dismissible');
});

test('a SQL failure is announced when Browse is open', async (t) => {
  let releaseQuery;
  const pending = new Promise((resolve) => { releaseQuery = resolve; });
  const api = await fixture({
    closeConnections: true,
    session: { connected: true, generation: 7, host: 'mysql.example.test', port: 3307, user: 'fixture-user', server_version: 'fixture-8.0' },
    handler: async (call) => {
      if (call.url.pathname === '/api/databases') return { body: { databases: ['fixture_db'] } };
      if (call.url.pathname === '/api/tables') return { body: { tables: [{ name: 'items', type: 'BASE TABLE' }] } };
      if (call.url.pathname === '/api/rows') return { body: { columns: ['id'], rows: [], page: 1, has_next: false } };
      if (call.url.pathname === '/api/query') {
        await pending;
        return { status: 504, body: { error: 'SQL timed out' } };
      }
    },
  });
  const page = await openPage(api.url);
  t.after(async () => { releaseQuery(); await page.close(); await api.close(); });
  assert.ok(await waitFor(() => page.evaluate('!!document.querySelector(".database-button")')));
  await page.evaluate('document.querySelector(".database-button").click()');
  assert.ok(await waitFor(() => page.evaluate('!!document.querySelector(".table-button")')));
  await page.evaluate(`(() => {
    document.querySelector('.table-button').click();
    document.querySelector('#tab-sql').click();
    document.querySelector('#sql-editor').value = 'UPDATE items SET value = 1';
  })()`);
  page.onDialog(() => true);
  await page.evaluate('document.querySelector("#sql-form").requestSubmit()');
  assert.ok(await waitFor(() => api.calls.some((call) => call.url.pathname === '/api/query')));
  await page.evaluate('document.querySelector("#tab-browse").click()');
  releaseQuery();
  assert.ok(await waitFor(() => page.evaluate(`Array.from(document.querySelectorAll('[role="alert"]')).some(e =>
    e.getClientRects().length > 0 && /outcome may be unknown/i.test(e.textContent) && /inspect.*before retry/i.test(e.textContent))`)));
  await page.evaluate('document.querySelector("#tab-sql").click()');
  assert.match(await page.evaluate('document.querySelector("#query-status").textContent'), /outcome may be unknown/i);
});

test('a SQL failure is announced after disconnecting', async (t) => {
  let releaseQuery;
  const pending = new Promise((resolve) => { releaseQuery = resolve; });
  const api = await fixture({
    closeConnections: true,
    session: { connected: true, generation: 7, host: 'mysql.example.test', port: 3307, user: 'fixture-user', server_version: 'fixture-8.0' },
    handler: async (call, { session }) => {
      if (call.url.pathname === '/api/databases') return { body: { databases: ['fixture_db'] } };
      if (call.url.pathname === '/api/tables') return { body: { tables: [] } };
      if (call.url.pathname === '/api/disconnect') { session.connected = false; return { body: { connected: false } }; }
      if (call.url.pathname === '/api/query') {
        await pending;
        return { status: 504, body: { error: 'SQL timed out' } };
      }
    },
  });
  const page = await openPage(api.url);
  t.after(async () => { releaseQuery(); await page.close(); await api.close(); });
  assert.ok(await waitFor(() => page.evaluate('!!document.querySelector(".database-button")')));
  await page.evaluate(`(() => {
    document.querySelector('.database-button').click();
    document.querySelector('#tab-sql').click();
    document.querySelector('#sql-editor').value = 'UPDATE items SET value = 1';
  })()`);
  page.onDialog(() => true);
  await page.evaluate('document.querySelector("#sql-form").requestSubmit()');
  assert.ok(await waitFor(() => api.calls.some((call) => call.url.pathname === '/api/query')));
  await page.evaluate('document.querySelector("#disconnect-button").click()');
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#connect-view").hidden === false')));
  releaseQuery();
  assert.ok(await waitFor(() => page.evaluate(`Array.from(document.querySelectorAll('[role="alert"]')).some(e =>
    e.getClientRects().length > 0 && /fixture_db/i.test(e.textContent) && /outcome may be unknown/i.test(e.textContent))`)));
  assert.equal(api.calls.filter((call) => call.url.pathname === '/api/query').length, 1);
});

test('SQL reports row and optional cell truncation without assuming a limit', async (t) => {
  const replies = [
    { columns: ['value'], rows: [['one']], affected_rows: null, truncated: true, cells_truncated: true, duration_ms: 11 },
    { columns: ['value'], rows: [['two']], affected_rows: null, duration_ms: 12 },
    { columns: ['value'], rows: [['three']], affected_rows: null, truncated: false, cells_truncated: false, duration_ms: 13 },
    { columns: ['value'], rows: [['four']], affected_rows: null, truncated: false, cells_truncated: true, duration_ms: 14 },
  ];
  let queryCount = 0;
  const api = await fixture({
    session: { connected: true, generation: 7, host: 'mysql.example.test', port: 3307, user: 'fixture-user', server_version: 'fixture-8.0' },
    handler: (call) => {
      if (call.url.pathname === '/api/databases') return { body: { databases: ['fixture_db'] } };
      if (call.url.pathname === '/api/tables') return { body: { tables: [] } };
      if (call.url.pathname === '/api/query') return { body: replies[queryCount++] };
    },
  });
  const page = await openPage(api.url);
  t.after(async () => { await page.close(); await api.close(); });
  assert.ok(await waitFor(() => page.evaluate('!!document.querySelector(".database-button")')));
  await page.evaluate(`(() => {
    document.querySelector('.database-button').click();
    document.querySelector('#tab-sql').click();
    document.querySelector('#sql-editor').value = 'SELECT 1';
  })()`);
  page.onDialog(() => true);
  for (let index = 0; index < replies.length; index++) {
    await page.evaluate('document.querySelector("#sql-form").requestSubmit()');
    assert.ok(await waitFor(() => queryCount >= index + 1));
    assert.ok(await waitFor(() => page.evaluate('!document.querySelector("#run-query").disabled')));
    const status = await page.evaluate('document.querySelector("#query-status").textContent');
    assert.equal(await page.evaluate('document.querySelector("#query-status").classList.contains("is-error")'), false, status);
    assert.ok(await page.evaluate('document.querySelector("#query-status").getClientRects().length > 0'));
    if (index === 0) {
      assert.match(status, /truncat/i);
      assert.match(status, /result limit reached/i);
      assert.doesNotMatch(status, /500/);
      assert.match(status, /cell.*truncat/i);
    } else if (index === 3) {
      assert.match(status, /cell.*truncat/i);
      assert.doesNotMatch(status, /result limit/i);
    } else {
      assert.doesNotMatch(status, /truncat|result limit/i);
    }
    assert.equal(await page.evaluate('document.querySelector("#query-result tbody td").textContent'), replies[index].rows[0][0]);
  }
});

test('disconnect uses CSRF, returns to the connection screen, and clears workspace data', async (t) => {
  const api = await fixture({
    session: { connected: true, generation: 7, host: '127.0.0.1', port: 3306, user: 'fixture-user', server_version: 'fixture-8.0' },
    handler: (call, { session }) => {
      if (call.url.pathname === '/api/databases') return { body: { databases: ['fixture_db'] } };
      if (call.url.pathname === '/api/tables') return { body: { tables: [] } };
      if (call.url.pathname === '/api/disconnect') {
        session.connected = false;
        return { body: { connected: false } };
      }
    },
  });
  const page = await openPage(api.url);
  t.after(async () => { await page.close(); await api.close(); });
  assert.ok(await waitFor(() => page.evaluate('!!document.querySelector(".database-button")')));
  await page.evaluate('document.querySelector(".database-button").click()');
  await page.evaluate(`(() => { document.querySelector('#tab-sql').click(); document.querySelector('#sql-editor').value = 'SELECT sensitive_fixture'; document.querySelector('#disconnect-button').click(); })()`);
  assert.ok(await waitFor(() => api.calls.some((call) => call.url.pathname === '/api/disconnect')),
    'Disconnect must POST to the session');
  const call = api.calls.find((entry) => entry.url.pathname === '/api/disconnect');
  assert.equal(call.method, 'POST');
  assert.equal(call.headers['x-csrf-token'], 'fixture-csrf');
  assert.match(call.headers.cookie, /dbkid-fixture=1/);
  assert.equal(call.headers['content-type'], 'application/json');
  assert.deepEqual(call.body, {});
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#connect-view").hidden === false')));
  assert.equal(await page.evaluate('document.querySelector("#app-view").hidden'), true);
  assert.equal(await page.evaluate('document.querySelector("#sql-editor").value'), '');
  assert.equal(await page.evaluate('document.querySelector("#database-list").textContent'), '');
  assert.equal(await page.evaluate('document.querySelector("#password").value'), '');
});

test('malformed session responses show a retryable, useful error', async (t) => {
  let attempts = 0;
  const api = await fixture({ sessionHandler: () => {
    attempts++;
    if (attempts === 1) return { raw: 'null' };
    if (attempts === 2) return { raw: 'not-json' };
  } });
  const page = await openPage(api.url);
  t.after(async () => { await page.close(); await api.close(); });
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#retry-bootstrap").hidden === false')));
  assert.match(await page.evaluate('document.querySelector("#boot-message").textContent'), /incomplete/i,
    'a null session should not expose a JavaScript exception');
  await page.evaluate('document.querySelector("#retry-bootstrap").click()');
  assert.ok(await waitFor(() => attempts >= 2));
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#boot-message").textContent.includes("invalid response")')));
  await page.evaluate('document.querySelector("#retry-bootstrap").click()');
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#connect-view").hidden === false')));
  assert.equal(attempts, 3);
});

test('keyboard focus remains on the chosen navigation item after rendering', async (t) => {
  const api = await fixture({
    session: { connected: true, generation: 7, host: '127.0.0.1', port: 3306, user: 'fixture-user', server_version: 'fixture-8.0' },
    handler: (call) => {
      if (call.url.pathname === '/api/databases') return { body: { databases: ['keyboard_db'] } };
      if (call.url.pathname === '/api/tables') return { body: { tables: [{ name: 'keyboard_table', type: 'BASE TABLE' }] } };
      if (call.url.pathname === '/api/rows') return { body: { columns: ['id'], rows: [], page: 1, page_size: 50, has_next: false } };
    },
  });
  const page = await openPage(api.url);
  t.after(async () => { await page.close(); await api.close(); });
  assert.ok(await waitFor(() => page.evaluate('!!document.querySelector(".database-button")')));
  await page.evaluate(`(() => { const b = document.querySelector('.database-button'); b.focus(); b.click(); })()`);
  assert.ok(await waitFor(() => page.evaluate('!!document.querySelector(".table-button")')));
  assert.equal(await page.evaluate('document.activeElement.dataset.database'), 'keyboard_db',
    'opening a database must not throw keyboard focus away');
  await page.evaluate(`(() => { const b = document.querySelector('.table-button'); b.focus(); b.click(); })()`);
  assert.equal(await page.evaluate('document.activeElement.dataset.table'), 'keyboard_table',
    'selecting a table must not throw keyboard focus away');
});

test('a failed row page can be retried without resetting the requested page', async (t) => {
  let pageTwoAttempts = 0;
  const api = await fixture({
    session: { connected: true, generation: 7, host: '127.0.0.1', port: 3306, user: 'fixture-user', server_version: 'fixture-8.0' },
    handler: (call) => {
      if (call.url.pathname === '/api/databases') return { body: { databases: ['d'] } };
      if (call.url.pathname === '/api/tables') return { body: { tables: [{ name: 't', type: 'BASE TABLE' }] } };
      if (call.url.pathname === '/api/rows') {
        const page = Number(call.url.searchParams.get('page'));
        if (page === 2 && ++pageTwoAttempts === 1) return { status: 503, body: { error: 'Fixture temporary outage' } };
        return { body: { columns: ['id'], rows: [[String(page)]], page, page_size: 50, has_next: page === 1 } };
      }
    },
  });
  const page = await openPage(api.url);
  t.after(async () => { await page.close(); await api.close(); });
  assert.ok(await waitFor(() => page.evaluate('!!document.querySelector(".database-button")')));
  await page.evaluate('document.querySelector(".database-button").click()');
  assert.ok(await waitFor(() => page.evaluate('!!document.querySelector(".table-button")')));
  await page.evaluate('document.querySelector(".table-button").click()');
  assert.ok(await waitFor(() => page.evaluate('!document.querySelector("#next-page").disabled')));
  await page.evaluate('document.querySelector("#next-page").click()');
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#browse-status").textContent.includes("Fixture temporary outage")')));
  assert.ok(await page.evaluate('document.querySelector("#retry-rows") && !document.querySelector("#retry-rows").hidden'),
    'row failures must offer a retry');
  await page.evaluate('document.querySelector("#retry-rows").click()');
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#page-label").textContent.includes("Page 2")')));
  assert.equal(pageTwoAttempts, 2);
});

test('connection errors and broken responses clear the password and allow retries', async (t) => {
  let attempts = 0;
  const message = 'Denied <img src=x onerror=window.__error_xss=1>';
  const api = await fixture({ handler: (call) => {
    if (call.url.pathname !== '/api/connect') return;
    attempts++;
    if (attempts === 1) return { status: 403, body: { error: message } };
    return { raw: 'not-json' };
  } });
  const page = await openPage(api.url);
  t.after(async () => { await page.close(); await api.close(); });
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#connect-view").hidden === false')));
  await page.evaluate('document.querySelector("#user").value = "fixture-user"');
  async function submit() {
    await page.evaluate(`(() => { document.querySelector('#password').value = crypto.randomUUID(); document.querySelector('#connect-form').requestSubmit(); })()`);
  }
  await submit();
  assert.ok(await waitFor(() => page.evaluate(`document.querySelector('#connect-error').textContent === ${JSON.stringify(message)} && !document.querySelector('#connect-error').hidden`)));
  assert.equal(await page.evaluate('document.querySelector("#password").value'), '');
  assert.equal(await page.evaluate('document.querySelector("#connect-error img") === null && window.__error_xss === undefined'), true);
  await page.setOffline(true);
  await submit();
  const networkErrorShown = await waitFor(() => page.evaluate('document.querySelector("#connect-error").textContent.includes("Could not reach") && !document.querySelector("#connect-error").hidden'));
  assert.ok(networkErrorShown, 'a network failure must be explained');
  assert.equal(await page.evaluate('document.querySelector("#password").value'), '');
  await page.setOffline(false);
  await submit();
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#connect-error").textContent.includes("invalid response") && !document.querySelector("#connect-error").hidden')));
  assert.equal(await page.evaluate('document.querySelector("#password").value'), '');
  assert.equal(attempts, 2);
});

test('desktop and narrow layouts keep navigation and grids usable', async (t) => {
  const api = await fixture({
    session: { connected: true, generation: 7, host: '127.0.0.1', port: 3306, user: 'fixture-user', server_version: 'fixture-8.0' },
    handler: (call) => {
      if (call.url.pathname === '/api/databases') return { body: { databases: ['inventory', 'archive'] } };
      if (call.url.pathname === '/api/tables') return { body: { tables: [
        { name: 'customers', type: 'BASE TABLE' }, { name: 'recent_orders', type: 'VIEW' },
      ] } };
      if (call.url.pathname === '/api/rows') return { body: {
        columns: ['id', 'name', 'email', 'note'],
        rows: [['1', 'Example One', 'one@example.test', null], ['2', 'Example Two', 'two@example.test', 'Active']],
        page: 1, page_size: 50, has_next: false,
      } };
    },
  });
  const page = await openPage(api.url);
  t.after(async () => { await page.close(); await api.close(); });
  await page.setViewport(1440, 900);
  assert.ok(await waitFor(() => page.evaluate('!!document.querySelector(".database-button")')));
  await page.evaluate('document.querySelector(".database-button").click()');
  assert.ok(await waitFor(() => page.evaluate('!!document.querySelector(".table-button")')));
  await page.evaluate('document.querySelector(".table-button").click()');
  assert.ok(await waitFor(() => page.evaluate('document.querySelectorAll("#browse-result tbody tr").length === 2')));
  assert.equal(await page.evaluate('document.documentElement.scrollWidth <= innerWidth'), true);
  if (process.env.DBKID_SCREENSHOT_DIR) await page.screenshot(path.join(process.env.DBKID_SCREENSHOT_DIR, 'dbkid-desktop.png'));
  await page.setViewport(320, 800);
  assert.equal(await page.evaluate('document.documentElement.scrollWidth <= innerWidth'), true,
    'the page should not overflow horizontally at a narrow viewport');
  assert.ok(await page.evaluate('document.querySelector("#browse-panel h2").getBoundingClientRect().height < 26'),
    'the browse heading should remain readable instead of being squeezed into two lines');
  assert.equal(await page.evaluate('getComputedStyle(document.querySelector(".database-navigation summary")).display !== "none"'), true);
  if (process.env.DBKID_SCREENSHOT_DIR) await page.screenshot(path.join(process.env.DBKID_SCREENSHOT_DIR, 'dbkid-mobile.png'));
  await page.evaluate('document.querySelector(".database-navigation summary").click()');
  assert.equal(await page.evaluate('document.querySelector(".database-navigation").open'), false,
    'mobile database navigation can be collapsed');
  await page.evaluate('document.querySelector("#tab-sql").click()');
  assert.equal(await page.evaluate('document.querySelector("#sql-panel").hidden'), false);
  assert.ok(await page.evaluate('document.querySelector("#sql-help").getClientRects().length > 0'));
  assert.equal(await page.evaluate('document.documentElement.scrollWidth <= innerWidth'), true,
    'SQL safety text should wrap without horizontal overflow at a narrow viewport');
  await page.setViewport(1440, 900);
  assert.notEqual(await page.evaluate('getComputedStyle(document.querySelector(".database-navigation summary")).display'), 'none',
    'a sidebar collapsed on mobile must remain reopenable on desktop');
  await page.evaluate('document.querySelector(".database-navigation summary").click()');
  assert.equal(await page.evaluate('document.querySelector(".database-navigation").open'), true);
});

test('disconnect refreshes the session token before a new connection', async (t) => {
  const api = await fixture({
    session: { connected: true, generation: 7, host: '127.0.0.1', port: 3306, user: 'fixture-user', server_version: 'fixture-8.0' },
    handler: (call, { session }) => {
      if (call.url.pathname === '/api/databases') return { body: { databases: [] } };
      if (call.url.pathname === '/api/disconnect') {
        session.connected = false;
        session.csrf_token = 'rotated-fixture-csrf';
        return { body: { connected: false } };
      }
      if (call.url.pathname === '/api/connect') return { body: {
        connected: true, generation: 7, host: call.body.host, port: call.body.port,
        user: call.body.user, server_version: 'fixture-8.0',
      } };
    },
  });
  const page = await openPage(api.url);
  t.after(async () => { await page.close(); await api.close(); });
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#app-view").hidden === false')));
  await page.evaluate('document.querySelector("#disconnect-button").click()');
  assert.ok(await waitFor(() => api.calls.filter((call) => call.url.pathname === '/api/session').length === 2),
    'disconnect must obtain a fresh session before allowing reconnection');
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#connect-view").hidden === false')));
  await page.evaluate(`(() => { document.querySelector('#user').value = 'fixture-user'; document.querySelector('#password').value = crypto.randomUUID(); document.querySelector('#connect-form').requestSubmit(); })()`);
  assert.ok(await waitFor(() => api.calls.some((call) => call.url.pathname === '/api/connect')));
  assert.equal(api.calls.find((call) => call.url.pathname === '/api/connect').headers['x-csrf-token'], 'rotated-fixture-csrf');
});

test('refreshing away a database discards its pending query and editor state', async (t) => {
  let listCount = 0;
  let releaseQuery;
  const pending = new Promise((resolve) => { releaseQuery = resolve; });
  const api = await fixture({
    session: { connected: true, generation: 7, host: '127.0.0.1', port: 3306, user: 'fixture-user', server_version: 'fixture-8.0' },
    handler: async (call) => {
      if (call.url.pathname === '/api/databases') return { body: { databases: ++listCount === 1 ? ['vanishing'] : [] } };
      if (call.url.pathname === '/api/tables') return { body: { tables: [] } };
      if (call.url.pathname === '/api/query') {
        await pending;
        return { body: { columns: ['old'], rows: [['stale result']], affected_rows: null, truncated: false, duration_ms: 1 } };
      }
    },
  });
  const page = await openPage(api.url);
  t.after(async () => { releaseQuery(); await page.close(); await api.close(); });
  assert.ok(await waitFor(() => page.evaluate('!!document.querySelector(".database-button")')));
  await page.evaluate('document.querySelector(".database-button").click()');
  await page.evaluate(`(() => { document.querySelector('#tab-sql').click(); document.querySelector('#sql-editor').value = 'SELECT old'; })()`);
  page.onDialog(() => true);
  await page.evaluate('document.querySelector("#sql-form").requestSubmit()');
  assert.ok(await waitFor(() => api.calls.some((call) => call.url.pathname === '/api/query')));
  await page.evaluate('document.querySelector("#refresh-databases").click()');
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#database-list").textContent.includes("No databases available")')));
  assert.equal(await page.evaluate('document.querySelector("#tab-sql").disabled'), true);
  assert.equal(await page.evaluate('document.querySelector("#sql-editor").value'), '',
    'a removed database must not leave its SQL draft in the workspace');
  releaseQuery();
  await delay(150);
  assert.equal(await page.evaluate('document.querySelector("#query-result").textContent'), '',
    'a late query response must not reappear after its database was removed');
});

test('host input is validated instead of silently rewritten', async (t) => {
  const api = await fixture();
  const page = await openPage(api.url);
  t.after(async () => { await page.close(); await api.close(); });
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#connect-view").hidden === false')));
  await page.evaluate(`(() => {
    document.querySelector('#host').value = ' 127.0.0.1 ';
    document.querySelector('#user').value = 'fixture-user';
    document.querySelector('#password').value = crypto.randomUUID();
    document.querySelector('#connect-form').requestSubmit();
  })()`);
  await delay(150);
  assert.equal(api.calls.filter((call) => call.url.pathname === '/api/connect').length, 0,
    'an invalid host must not be silently trimmed and sent');
  assert.ok(await page.evaluate('!document.querySelector("#connect-error").hidden && document.querySelector("#connect-error").textContent.toLowerCase().includes("host")'));
  assert.equal(await page.evaluate('document.querySelector("#password").value'), '');
});

test('the connection form cannot fall back to putting credentials in a URL', async (t) => {
  const api = await fixture();
  const page = await openPage(api.url);
  t.after(async () => { await page.close(); await api.close(); });
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#connect-view").hidden === false')));
  assert.equal(await page.evaluate('document.querySelector("#connect-form").method.toLowerCase()'), 'post',
    'the native form fallback must not use GET');
  assert.equal(await page.evaluate('new URL(document.querySelector("#connect-form").action).pathname'), '/api/connect');
});

test('loopback plaintext does not grant the non-loopback override', async (t) => {
  const api = await fixture({ handler: (call) => {
    if (call.url.pathname === '/api/connect') return { status: 403, body: { error: 'Fixture rejected connection' } };
  } });
  const page = await openPage(api.url);
  t.after(async () => { await page.close(); await api.close(); });
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#connect-view").hidden === false')));
  const prompts = [];
  page.onDialog((dialog) => { prompts.push(dialog); return false; });
  for (const [index, host] of ['localhost', '127.12.34.56', '::1'].entries()) {
    await page.evaluate(`(() => {
      const host = document.querySelector('#host');
      host.value = ${JSON.stringify(host)};
      host.dispatchEvent(new Event('input', { bubbles: true }));
      const tls = document.querySelector('#tls');
      tls.value = 'disable';
      tls.dispatchEvent(new Event('change', { bubbles: true }));
      document.querySelector('#user').value = 'fixture-user';
      document.querySelector('#password').value = crypto.randomUUID();
      document.querySelector('#connect-form').requestSubmit();
    })()`);
    assert.ok(await waitFor(() => api.calls.filter((call) => call.url.pathname === '/api/connect').length === index + 1));
    assert.ok(await waitFor(() => page.evaluate('!document.querySelector("#connect-error").hidden')));
    const call = api.calls.filter((entry) => entry.url.pathname === '/api/connect')[index];
    assert.equal(call.body.host, host);
    assert.equal(call.body.allow_insecure, false);
  }
  assert.equal(prompts.length, 0);
});

test('a remote autofilled host cannot inherit the local plaintext default', async (t) => {
  const api = await fixture({ handler: (call) => {
    if (call.url.pathname === '/api/connect') return { status: 403, body: { error: 'Fixture rejected connection' } };
  } });
  const page = await openPage(api.url);
  t.after(async () => { await page.close(); await api.close(); });
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#connect-view").hidden === false')));
  const prompts = [];
  page.onDialog((dialog) => { prompts.push(dialog); return false; });
  await page.evaluate(`(() => {
    document.querySelector('#host').value = 'remote.example.test';
    document.querySelector('#user').value = 'fixture-user';
    document.querySelector('#password').value = crypto.randomUUID();
    document.querySelector('#connect-form').requestSubmit();
  })()`);
  assert.ok(await waitFor(() => api.calls.some((call) => call.url.pathname === '/api/connect')),
    'a host supplied without an input event should default to TLS verification');
  const call = api.calls.find((entry) => entry.url.pathname === '/api/connect');
  assert.equal(call.body.tls, 'verify');
  assert.equal(call.body.allow_insecure, false);
  assert.equal(prompts.length, 0);
});

test('late table lists do not overwrite a newer database selection', async (t) => {
  let releaseSlow;
  const pending = new Promise((resolve) => { releaseSlow = resolve; });
  const api = await fixture({
    session: { connected: true, generation: 7, host: '127.0.0.1', port: 3306, user: 'fixture-user', server_version: 'fixture-8.0' },
    handler: async (call) => {
      if (call.url.pathname === '/api/databases') return { body: { databases: ['slow', 'fast'] } };
      if (call.url.pathname === '/api/tables') {
        if (call.url.searchParams.get('database') === 'slow') {
          await pending;
          return { body: { tables: [{ name: 'old_table', type: 'BASE TABLE' }] } };
        }
        return { body: { tables: [{ name: 'new_table', type: 'BASE TABLE' }] } };
      }
    },
  });
  const page = await openPage(api.url);
  t.after(async () => { releaseSlow(); await page.close(); await api.close(); });
  assert.ok(await waitFor(() => page.evaluate('document.querySelectorAll(".database-button").length === 2')));
  await page.evaluate(`Array.from(document.querySelectorAll('.database-button')).find(b => b.dataset.database === 'slow').click()`);
  assert.ok(await waitFor(() => api.calls.some((call) => call.url.pathname === '/api/tables' && call.url.searchParams.get('database') === 'slow')));
  await page.evaluate(`Array.from(document.querySelectorAll('.database-button')).find(b => b.dataset.database === 'fast').click()`);
  assert.ok(await waitFor(() => page.evaluate('document.querySelector(".table-button")?.dataset.table === "new_table"')));
  releaseSlow();
  await delay(150);
  assert.equal(await page.evaluate('document.querySelector("#workspace-title").textContent'), 'fast');
  assert.equal(await page.evaluate('document.querySelector(".table-button").dataset.table'), 'new_table');
});

// ---------------------------------------------------------------------------
// DBKid v2 frontend tests: sort/filter browse, row editor, structure/index
// management, objects, search, status, history/EXPLAIN, export/import,
// blob links, recent connections — all against the mock API above.

const V2_SESSION = { connected: true, generation: 7, host: '127.0.0.1', port: 3306, user: 'fixture-user', server_version: 'fixture-8.0' };
const V2_DATABASE = 'shop';
const V2_TABLE = 'items';
const V2_STRUCTURE = [
  { name: 'id', type: 'int', nullable: false, key: 'PRI', default: null, extra: 'auto_increment' },
  { name: 'name', type: 'varchar(80)', nullable: true, key: '', default: null, extra: '' },
  { name: 'photo', type: 'blob', nullable: true, key: '', default: null, extra: '' },
];
const V2_GRID_COLUMNS = ['id', 'name', 'photo'];

function makeV2Api(options = {}) {
  const api = {
    databases: options.databases ?? [V2_DATABASE],
    tables: options.tables ?? [{ name: V2_TABLE, type: 'BASE TABLE' }],
    structure: options.structure ?? V2_STRUCTURE,
    rows: options.rows ?? [
      ['1', 'wallet', null],
      ['2', 'socks', 'deadbeef'],
    ],
    cellsTruncated: options.cellsTruncated ?? false,
    row: options.row ?? { values: ['1', 'wallet', 'deadbeef'], binary: [false, false, true], truncated: [false, false, true] },
    indexes: options.indexes ?? [{ name: 'PRIMARY', unique: true, columns: ['id'], type: 'BTREE' }],
    foreignKeys: options.foreignKeys ?? [],
    users: options.users ?? [{ user: 'admin', host: 'localhost' }, { user: 'reader', host: '127.0.0.1' }],
    usersRestricted: options.usersRestricted ?? false,
    history: options.history ?? [],
    recent: options.recent ?? [],
  };
  const handler = (call) => {
    const pathname = call.url.pathname;
    const query = call.url.searchParams;
    if (pathname === '/api/databases') return { body: { databases: api.databases } };
    if (pathname === '/api/tables') return { body: { tables: api.tables } };
    if (pathname === '/api/rows') return { body: {
      columns: V2_GRID_COLUMNS,
      rows: api.rows,
      page: Number(query.get('page')) || 1,
      page_size: Number(query.get('page_size')) || 50,
      has_next: false,
      ...(api.cellsTruncated ? { cells_truncated: true } : {}),
    } };
    if (pathname === '/api/structure') return { body: { columns: api.structure } };
    if (pathname === '/api/indexes') return { body: { indexes: api.indexes } };
    if (pathname === '/api/row') return { body: {
      columns: api.structure,
      values: api.row.values,
      binary: api.row.binary,
      truncated: api.row.truncated,
    } };
    if (pathname === '/api/row/insert' || pathname === '/api/row/update' || pathname === '/api/row/delete') {
      return { body: { affected_rows: 1 } };
    }
    if (pathname === '/api/query') return { body: { columns: [], rows: [], affected_rows: 0, truncated: false, duration_ms: 5 } };
    if (pathname === '/api/history') return { body: { history: api.history } };
    if (pathname === '/api/history/clear') { api.history = []; return { body: { cleared: true } }; }
    if (pathname === '/api/recent') return { body: { recent: api.recent } };
    if (pathname === '/api/blob/token') return { body: { url: '/api/blob?token=blob-token-1' } };
    if (pathname === '/api/blob') return { raw: 'fixture-blob-bytes' };
    if (pathname === '/api/export/token') return { body: { url: '/api/export?token=export-token-1' } };
    if (pathname === '/api/export') return { raw: 'a,b\n1,2\n' };
    if (pathname === '/api/import/sql') return { body: { executed: 3, failed: 0, errors: [], stopped: false } };
    if (pathname === '/api/import/csv') return { body: { inserted: 2, failed: 0, errors: [], stopped: false } };
    if (pathname === '/api/routines') return { body: { routines: [
      { name: 'add_total', type: 'procedure', created: '2026-01-01 10:00:00', last_altered: '2026-01-02 11:00:00' },
      { name: 'double_it', type: 'function', created: '2026-01-01 09:00:00', last_altered: '2026-01-01 09:00:00' },
    ] } };
    if (pathname === '/api/triggers') return { body: { triggers: [
      { name: 'items_before_insert', timing: 'BEFORE', event: 'INSERT', table: V2_TABLE },
    ] } };
    if (pathname === '/api/events') return { body: { events: [
      { name: 'nightly_cleanup', status: 'ENABLED', type: 'RECURRING', interval_value: '1', interval_field: 'DAY' },
    ] } };
    if (pathname === '/api/routine') return { body: {
      name: query.get('name'),
      definition: 'CREATE PROCEDURE add_total() BEGIN SELECT 1; END',
      truncated: false,
    } };
    if (pathname === '/api/object/definition') {
      const kind = query.get('kind');
      const name = query.get('name');
      const definition = `CREATE ${kind.toUpperCase()} ${name}() BEGIN SELECT 1; END`;
      const key = { procedure: 'create procedure', trigger: 'sql original statement', event: 'create event' }[kind];
      return { body: { database: query.get('database'), name, kind, generation: 7, definition, truncated: false,
        metadata: { [kind]: name, [key]: definition, sql_mode: '', character_set_client: 'utf8mb4', collation_connection: 'utf8mb4_general_ci', 'database collation': 'utf8mb4_general_ci', ...(kind === 'event' ? { time_zone: 'UTC' } : {}) } } };
    }
    if (pathname === '/api/object/replace') return { body: { stage: 'replaced' } };
    if (pathname === '/api/object/create') return { body: { stage: 'created', affected_rows: 0 } };
    if (pathname === '/api/routine/drop') return { body: { affected_rows: 0 } };
    if (pathname === '/api/search') return { body: { results: [
      { table: V2_TABLE, columns: V2_GRID_COLUMNS, rows: [['1', 'wallet', null]], more: false, matched_column_count: 1 },
    ] } };
    if (pathname === '/api/status') return { body: {
      status: [['Uptime', '42']],
      variables: [['max_connections', '100']],
      processes: [['5', 'fixture-user', 'localhost:12345', V2_DATABASE, 'Query', '3', 'executing', 'SELECT 1']],
    } };
    if (pathname === '/api/process/kill') return { body: { affected_rows: 0 } };
    if (pathname === '/api/database/create') {
      api.databases = [...api.databases, call.body.name];
      return { body: { affected_rows: 1 } };
    }
    if (pathname === '/api/database/drop') {
      api.databases = api.databases.filter((database) => database !== call.body.database);
      return { body: { affected_rows: 0 } };
    }
    if (pathname === '/api/table/drop') {
      api.tables = api.tables.filter((table) => table.name !== call.body.table);
      return { body: { affected_rows: 0 } };
    }
    if (pathname === '/api/table/rename') {
      api.tables = api.tables.map((table) => table.name === call.body.table ? { ...table, name: call.body.new_name } : table);
      return { body: { affected_rows: 0 } };
    }
    if (pathname === '/api/column/add' || pathname === '/api/column/modify' || pathname === '/api/column/drop') {
      return { body: { affected_rows: 0 } };
    }
    if (pathname === '/api/index/create' || pathname === '/api/index/drop') return { body: { affected_rows: 0 } };
    if (pathname === '/api/table/create') {
      api.tables = [...api.tables, { name: call.body.name, type: 'BASE TABLE' }];
      return { body: { affected_rows: 0 } };
    }
    if (pathname === '/api/foreign-keys') return { body: { foreign_keys: api.foreignKeys } };
    if (pathname === '/api/foreign-key/add') {
      api.foreignKeys = [...api.foreignKeys, {
        name: call.body.name || 'fk_generated', columns: call.body.columns,
        referenced_database: call.body.referenced_database, referenced_table: call.body.referenced_table,
        referenced_columns: call.body.referenced_columns,
        on_delete: call.body.on_delete || 'RESTRICT', on_update: call.body.on_update || 'RESTRICT',
      }];
      return { body: { affected_rows: 0 } };
    }
    if (pathname === '/api/foreign-key/drop') {
      api.foreignKeys = api.foreignKeys.filter((fk) => fk.name !== call.body.name);
      return { body: { affected_rows: 0 } };
    }
    if (pathname === '/api/users') return { body: { users: api.users, restricted: api.usersRestricted } };
    if (pathname === '/api/grants') return { body: { grants: query.get('user')
      ? [`GRANT SELECT ON ${JSON.stringify(V2_DATABASE)}.* TO ${JSON.stringify(query.get('user'))}@${JSON.stringify(query.get('host'))}`,
        'GRANT USAGE ON *.* TO the account']
      : ['GRANT USAGE ON *.* TO the connected account'] } };
    if (pathname === '/api/user/create') {
      api.users = [...api.users, { user: call.body.user, host: call.body.host }];
      return { body: { affected_rows: 0 } };
    }
    if (pathname === '/api/user/password') return { body: { affected_rows: 0 } };
    if (pathname === '/api/user/drop') {
      api.users = api.users.filter((account) => !(account.user === call.body.user && account.host === call.body.host));
      return { body: { affected_rows: 0 } };
    }
    if (pathname === '/api/grant' || pathname === '/api/revoke') return { body: { affected_rows: 0 } };
  };
  return { api, handler };
}

async function openTablePage(fixtureUrl) {
  const page = await openPage(fixtureUrl);
  assert.ok(await waitFor(() => page.evaluate('!!document.querySelector(".database-button")')));
  await page.evaluate('document.querySelector(".database-button").click()');
  assert.ok(await waitFor(() => page.evaluate('!!document.querySelector(".table-button")')));
  await page.evaluate('document.querySelector(".table-button").click()');
  return page;
}

async function waitForRowActions(page) {
  assert.ok(await waitFor(() => page.evaluate(`(() => {
    const buttons = Array.from(document.querySelectorAll('#browse-result .row-actions button'));
    return buttons.length >= 3 && buttons.some((button) => button.textContent === 'Edit' && !button.disabled);
  })()`)), 'row action buttons must enable once structure loads');
}

async function v2Fixture(options) {
  const made = makeV2Api(options || {});
  const fixtureApi = await fixture({ session: V2_SESSION, handler: made.handler });
  return { made, fixtureApi };
}

test('browse column headers toggle sort parameters', async (t) => {
  const { fixtureApi } = await v2Fixture({});
  const page = await openTablePage(fixtureApi.url);
  t.after(async () => { await page.close(); await fixtureApi.close(); });
  await waitForRowActions(page);
  await page.evaluate(`document.querySelectorAll('#browse-result thead th button')[1].click()`);
  assert.ok(await waitFor(() => fixtureApi.calls.some((call) => call.url.pathname === '/api/rows' && call.url.searchParams.get('sort') === 'name')),
    'clicking a header must send sort');
  const first = fixtureApi.calls.filter((call) => call.url.pathname === '/api/rows' && call.url.searchParams.get('sort')).pop();
  assert.equal(first.url.searchParams.get('dir'), 'asc');
  assert.equal(first.url.searchParams.get('page'), '1');
  await waitForRowActions(page);
  await page.evaluate(`document.querySelectorAll('#browse-result thead th button')[1].click()`);
  assert.ok(await waitFor(() => fixtureApi.calls.some((call) => call.url.pathname === '/api/rows' && call.url.searchParams.get('dir') === 'desc')),
    'clicking again must flip the direction');
  assert.equal(await page.evaluate(`document.querySelectorAll('#browse-result thead th')[1].getAttribute('aria-sort')`), 'descending');
});

test('the filter bar builds the JSON filter parameter and clears it', async (t) => {
  const { fixtureApi } = await v2Fixture({});
  const page = await openTablePage(fixtureApi.url);
  t.after(async () => { await page.close(); await fixtureApi.close(); });
  await waitForRowActions(page);
  assert.ok(await waitFor(() => page.evaluate(`document.querySelector('#filter-column-0 option[value="name"]') !== null`)),
    'filter columns must come from the table structure');
  await page.evaluate(`(() => {
    document.querySelector('#filter-column-0').value = 'name';
    document.querySelector('#filter-op-0').value = 'like';
    document.querySelector('#filter-value-0').value = 'wal';
    document.querySelector('#filter-apply').click();
  })()`);
  assert.ok(await waitFor(() => fixtureApi.calls.some((call) => call.url.pathname === '/api/rows' && call.url.searchParams.get('filter'))),
    'apply must send the filter parameter');
  const applied = fixtureApi.calls.filter((call) => call.url.pathname === '/api/rows' && call.url.searchParams.get('filter')).pop();
  assert.deepEqual(JSON.parse(applied.url.searchParams.get('filter')),
    [{ column: 'name', op: 'like', value: 'wal' }]);
  assert.equal(applied.url.searchParams.get('page'), '1', 'applying a filter must restart at page one');
  assert.ok(await waitFor(() => page.evaluate(`document.querySelector('#browse-status').textContent !== 'Loading rows…'`)));
  await page.evaluate('document.querySelector("#filter-clear").click()');
  assert.ok(await waitFor(() => {
    const rowsCalls = fixtureApi.calls.filter((call) => call.url.pathname === '/api/rows');
    return rowsCalls.length && rowsCalls[rowsCalls.length - 1].url.searchParams.get('filter') === null;
  }), 'clear must reload without the filter');
});

test('the row editor shows binary hex hints, truncated read-only cells, and NULL toggles', async (t) => {
  const { fixtureApi } = await v2Fixture({});
  const page = await openTablePage(fixtureApi.url);
  t.after(async () => { await page.close(); await fixtureApi.close(); });
  await waitForRowActions(page);
  await page.evaluate(`Array.from(document.querySelectorAll('#browse-result .row-actions button')).find((button) => button.textContent === 'Edit').click()`);
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#row-editor-modal").hidden === false')),
    'Edit must open the row editor');
  const rowCall = fixtureApi.calls.find((call) => call.url.pathname === '/api/row');
  assert.ok(rowCall, 'Edit must fetch the full row by primary key');
  assert.equal(rowCall.url.searchParams.get('database'), V2_DATABASE);
  assert.equal(rowCall.url.searchParams.get('table'), V2_TABLE);
  assert.deepEqual(JSON.parse(rowCall.url.searchParams.get('key')), [['id', '1']]);
  assert.ok(await waitFor(() => page.evaluate('document.querySelectorAll("#row-editor-form .row-editor-field").length === 3')));
  assert.equal(await page.evaluate('document.querySelector("#row-editor-save").disabled'), true,
    'saving an unmodified row must be prevented');
  assert.ok(await page.evaluate(`Array.from(document.querySelectorAll('#row-editor-form .row-editor-field')).some((field) =>
    field.textContent.includes('photo') && field.textContent.includes('hex'))`),
    'binary columns must be labeled as hex');
  assert.equal(await page.evaluate(`Array.from(document.querySelectorAll('#row-editor-form .row-editor-field')).find((field) =>
    field.textContent.includes('photo')).querySelector('input').readOnly`), true,
    'truncated cells must be read-only');
  assert.ok(await page.evaluate(`Array.from(document.querySelectorAll('#row-editor-form .truncated-note')).some((note) => /truncated/i.test(note.textContent))`),
    'truncated cells must explain why they are read-only');
  await page.evaluate(`(() => {
    const field = Array.from(document.querySelectorAll('#row-editor-form .row-editor-field')).find((entry) => entry.textContent.includes('name') && entry.querySelector('input[type="checkbox"]'));
    field.querySelector('input[type="checkbox"]').click();
  })()`);
  assert.equal(await page.evaluate(`Array.from(document.querySelectorAll('#row-editor-form .row-editor-field')).find((field) =>
    field.textContent.includes('name')).querySelector('input').disabled`), true,
    'checking NULL must disable the value input');
  assert.equal(await page.evaluate('document.querySelector("#row-editor-save").disabled'), false,
    'toggling NULL counts as a change');
  await page.evaluate('document.querySelector("#row-editor-cancel").click()');
  assert.equal(await page.evaluate('document.querySelector("#row-editor-modal").hidden'), true);
});

test('a truncated cell in the page disables Copy with a value-too-large hint', async (t) => {
  const { fixtureApi } = await v2Fixture({ cellsTruncated: true });
  const page = await openTablePage(fixtureApi.url);
  t.after(async () => { await page.close(); await fixtureApi.close(); });
  await waitForRowActions(page);
  assert.ok(await waitFor(() => page.evaluate(`Array.from(document.querySelectorAll('#browse-result .row-actions button'))
    .filter((button) => button.textContent === 'Copy').every((button) => button.disabled)`)),
    'Copy must be disabled when cells were truncated');
  assert.equal(await page.evaluate(`Array.from(document.querySelectorAll('#browse-result .row-actions button'))
    .filter((button) => button.textContent === 'Copy').every((button) => /value too large/i.test(button.title))`), true,
    'Copy must explain the value-too-large reason');
  assert.equal(await page.evaluate(`Array.from(document.querySelectorAll('#browse-result .row-actions button'))
    .filter((button) => button.textContent === 'Edit' || button.textContent === 'Delete').every((button) => !button.disabled)`), true,
    'Edit and Delete stay available with a primary key');
});

test('rows without a primary key disable row actions with a hint', async (t) => {
  const structure = V2_STRUCTURE.map((column) => ({ ...column, key: '' }));
  const { fixtureApi } = await v2Fixture({ structure });
  const page = await openTablePage(fixtureApi.url);
  t.after(async () => { await page.close(); await fixtureApi.close(); });
  assert.ok(await waitFor(() => page.evaluate(`Array.from(document.querySelectorAll('#browse-result .row-actions button'))
    .every((button) => button.disabled)`)), 'all row actions must be disabled without a primary key');
  // The hint follows the structure callback, which can land after the disabled
  // buttons render; wait for it instead of asserting immediately.
  assert.ok(await waitFor(() => page.evaluate(`!document.querySelector('#row-actions-hint').hidden && /no primary key/i.test(document.querySelector('#row-actions-hint').textContent)`)),
    'a visible hint must point users to SQL');
});

test('insert, update and delete confirm the database and table and send request IDs', async (t) => {
  const { fixtureApi } = await v2Fixture({});
  const page = await openTablePage(fixtureApi.url);
  t.after(async () => { await page.close(); await fixtureApi.close(); });
  await waitForRowActions(page);
  const confirms = [];
  page.onDialog((dialog) => { confirms.push(dialog.message); return true; });

  // Update: edit the first row's name and save.
  await page.evaluate(`Array.from(document.querySelectorAll('#browse-result .row-actions button')).find((button) => button.textContent === 'Edit').click()`);
  assert.ok(await waitFor(() => page.evaluate('document.querySelectorAll("#row-editor-form .row-editor-field").length === 3')));
  await page.evaluate(`(() => {
    const input = document.querySelector('#row-editor-field-1-value');
    input.value = 'renamed wallet';
    input.dispatchEvent(new Event('input', { bubbles: true }));
  })()`);
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#row-editor-save").disabled === false')));
  await page.evaluate('document.querySelector("#row-editor-save").click()');
  assert.ok(await waitFor(() => fixtureApi.calls.some((call) => call.url.pathname === '/api/row/update')));
  assert.match(confirms[0], /save changes/i);
  assert.ok(confirms[0].includes(JSON.stringify(V2_TABLE)), 'update confirm must name the table');
  assert.ok(confirms[0].includes(JSON.stringify(V2_DATABASE)), 'update confirm must name the database');
  const updateCall = fixtureApi.calls.find((call) => call.url.pathname === '/api/row/update');
  assert.deepEqual(updateCall.body, {
    database: V2_DATABASE,
    table: V2_TABLE,
    key: { id: '1' },
    values: { name: { null: false, value: 'renamed wallet', binary: false } },
    confirmed: true,
  });
  assert.match(updateCall.headers['x-dbkid-request-id'] || '', /^[A-Za-z0-9_-]{22,64}$/, 'update needs a replay-protection ID');

  // Insert: the toolbar button opens a blank editor.
  await page.evaluate('document.querySelector("#insert-row-button").click()');
  assert.ok(await waitFor(() => page.evaluate('document.querySelectorAll("#row-editor-form .row-editor-field").length === 3')));
  await page.evaluate(`(() => {
    const input = document.querySelector('#row-editor-field-1-value');
    input.value = 'brand new';
    input.dispatchEvent(new Event('input', { bubbles: true }));
  })()`);
  // Capture a node from the pre-insert grid BEFORE saving: the loadRows refresh
  // after the insert detaches it, which is the reliable settle signal before the
  // next click (a plain button count is satisfied by the stale DOM).
  await page.evaluate(`(() => {
    window.__staleDelete = Array.from(document.querySelectorAll('#browse-result .row-actions button'))
      .filter((button) => button.textContent === 'Delete')[1];
    window.__lastConfirm = undefined;
    const original = window.confirm;
    window.confirm = (message) => { window.__lastConfirm = message; return original(message); };
  })()`);
  await page.evaluate('document.querySelector("#row-editor-save").click()');
  assert.ok(await waitFor(() => fixtureApi.calls.some((call) => call.url.pathname === '/api/row/insert')),
    await page.evaluate(`(() => 'confirm seen: ' + JSON.stringify(window.__lastConfirm) +
      ' / modal hidden: ' + document.querySelector('#row-editor-modal').hidden +
      ' / status: ' + JSON.stringify(document.querySelector('#row-editor-status').textContent))()`));
  assert.match(confirms[1], /insert a new row/i);
  assert.ok(confirms[1].includes(JSON.stringify(V2_TABLE)) && confirms[1].includes(JSON.stringify(V2_DATABASE)));
  const insertCall = fixtureApi.calls.find((call) => call.url.pathname === '/api/row/insert');
  assert.deepEqual(insertCall.body, {
    database: V2_DATABASE,
    table: V2_TABLE,
    values: { name: { null: false, value: 'brand new', binary: false } },
    confirmed: true,
  });
  assert.match(insertCall.headers['x-dbkid-request-id'] || '', /^[A-Za-z0-9_-]{22,64}$/);

  // Delete the second row. The async loadRows refresh after the insert replaces
  // the grid DOM; wait for the captured pre-insert node to be detached first.
  assert.ok(await waitFor(() => page.evaluate(`(() =>
    window.__staleDelete && !window.__staleDelete.isConnected &&
    Array.from(document.querySelectorAll('#browse-result .row-actions button'))
      .filter((button) => button.textContent === 'Delete').length === 2)()`)),
    'browse must re-render with row actions after insert');
  await page.evaluate(`Array.from(document.querySelectorAll('#browse-result .row-actions button')).filter((button) => button.textContent === 'Delete')[1].click()`);
  assert.ok(await waitFor(() => fixtureApi.calls.some((call) => call.url.pathname === '/api/row/delete')));
  assert.match(confirms[2], /delete a row/i);
  assert.ok(confirms[2].includes(JSON.stringify(V2_TABLE)) && confirms[2].includes(JSON.stringify(V2_DATABASE)));
  assert.match(confirms[2], /cannot be undone/i);
  const deleteCall = fixtureApi.calls.find((call) => call.url.pathname === '/api/row/delete');
  assert.deepEqual(deleteCall.body, { database: V2_DATABASE, table: V2_TABLE, key: { id: '2' }, confirmed: true });
  assert.match(deleteCall.headers['x-dbkid-request-id'] || '', /^[A-Za-z0-9_-]{22,64}$/);
  assert.notEqual(updateCall.headers['x-dbkid-request-id'], deleteCall.headers['x-dbkid-request-id'],
    'each mutation needs its own request ID');
});

test('dropping a table needs the exact typed name and a confirmation', async (t) => {
  const { fixtureApi } = await v2Fixture({});
  const page = await openTablePage(fixtureApi.url);
  t.after(async () => { await page.close(); await fixtureApi.close(); });
  await waitForRowActions(page);
  const confirms = [];
  page.onDialog((dialog) => { confirms.push(dialog.message); return true; });
  await page.evaluate('document.querySelector("#tab-structure").click()');
  await page.evaluate('document.querySelector("#drop-table-button").click()');
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#drop-table-form").hidden === false')),
    'the two-step drop form must appear');
  await page.evaluate(`(() => {
    const input = document.querySelector('#drop-table-input');
    input.value = 'item';
    input.dispatchEvent(new Event('input', { bubbles: true }));
  })()`);
  assert.equal(await page.evaluate('document.querySelector("#drop-table-confirm").disabled'), true,
    'a wrong name must not enable the drop button');
  await page.evaluate(`(() => {
    const input = document.querySelector('#drop-table-input');
    input.value = ${JSON.stringify(V2_TABLE)};
    input.dispatchEvent(new Event('input', { bubbles: true }));
  })()`);
  assert.equal(await page.evaluate('document.querySelector("#drop-table-confirm").disabled'), false,
    'the exact table name must enable the drop button');
  await page.evaluate('document.querySelector("#drop-table-confirm").click()');
  assert.ok(await waitFor(() => fixtureApi.calls.some((call) => call.url.pathname === '/api/table/drop')));
  assert.ok(confirms[0].includes(JSON.stringify(V2_TABLE)) && confirms[0].includes(JSON.stringify(V2_DATABASE)),
    'the drop confirmation must name the table and database');
  assert.match(confirms[0], /cannot be undone/i);
  const call = fixtureApi.calls.find((entry) => entry.url.pathname === '/api/table/drop');
  assert.deepEqual(call.body, { database: V2_DATABASE, table: V2_TABLE, confirmed: true });
  assert.match(call.headers['x-dbkid-request-id'] || '', /^[A-Za-z0-9_-]{22,64}$/);
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#drop-table-form").hidden === true')),
    'the form must close after the drop');
});

test('dropping a database also needs the exact typed name', async (t) => {
  const { fixtureApi } = await v2Fixture({});
  const page = await openTablePage(fixtureApi.url);
  t.after(async () => { await page.close(); await fixtureApi.close(); });
  await waitForRowActions(page);
  const confirms = [];
  page.onDialog((dialog) => { confirms.push(dialog.message); return true; });
  await page.evaluate(`document.querySelector('#sidebar-tools .sidebar-button-danger').click()`);
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#drop-database-name") !== null')),
    'the drop-database form must appear in the sidebar');
  await page.evaluate(`(() => {
    const input = document.querySelector('#drop-database-name');
    input.value = 'shop2';
    input.dispatchEvent(new Event('input', { bubbles: true }));
  })()`);
  assert.equal(await page.evaluate('document.querySelector("#drop-database-confirm").disabled'), true);
  await page.evaluate(`(() => {
    const input = document.querySelector('#drop-database-name');
    input.value = ${JSON.stringify(V2_DATABASE)};
    input.dispatchEvent(new Event('input', { bubbles: true }));
  })()`);
  assert.equal(await page.evaluate('document.querySelector("#drop-database-confirm").disabled'), false);
  await page.evaluate('document.querySelector("#drop-database-confirm").click()');
  assert.ok(await waitFor(() => fixtureApi.calls.some((call) => call.url.pathname === '/api/database/drop')));
  assert.ok(confirms[0].includes(JSON.stringify(V2_DATABASE)));
  assert.match(confirms[0], /cannot be undone/i);
  const call = fixtureApi.calls.find((entry) => entry.url.pathname === '/api/database/drop');
  assert.deepEqual(call.body, { database: V2_DATABASE, confirmed: true });
  assert.match(call.headers['x-dbkid-request-id'] || '', /^[A-Za-z0-9_-]{22,64}$/);
});

test('history lists statements newest first and clicking one loads the editor', async (t) => {
  const history = [
    { sql: 'SELECT older FROM alpha', at: 1000, duration_ms: 3, affected: 1 },
    { sql: 'SELECT newer FROM beta', at: 2000, duration_ms: 5, rows: 2 },
  ];
  const { fixtureApi } = await v2Fixture({ history });
  const page = await openTablePage(fixtureApi.url);
  t.after(async () => { await page.close(); await fixtureApi.close(); });
  page.onDialog(() => true);
  await page.evaluate('document.querySelector("#tab-sql").click()');
  assert.ok(await waitFor(() => fixtureApi.calls.some((call) => call.url.pathname === '/api/history')));
  assert.ok(await waitFor(() => page.evaluate('document.querySelectorAll("#history-list .history-item").length === 2')),
    'history must render after opening the SQL tab');
  assert.ok(await page.evaluate(`document.querySelector('#history-list .history-item').textContent.includes('SELECT newer FROM beta')`),
    'history must be newest first');
  await page.evaluate('document.querySelector("#sql-editor").value = "SELECT 1"');
  await page.evaluate('document.querySelector("#sql-form").requestSubmit()');
  assert.ok(await waitFor(() => fixtureApi.calls.filter((call) => call.url.pathname === '/api/history').length >= 2),
    'history must refresh after each query');
  await page.evaluate('document.querySelector("#history-list .history-item").click()');
  assert.equal(await page.evaluate('document.querySelector("#sql-editor").value'), 'SELECT newer FROM beta',
    'clicking a history entry must load it into the editor');
  const confirmBeforeClear = [];
  page.onDialog((dialog) => { confirmBeforeClear.push(dialog.message); return true; });
  await page.evaluate('document.querySelector("#clear-history").click()');
  assert.ok(await waitFor(() => fixtureApi.calls.some((call) => call.url.pathname === '/api/history/clear')));
  assert.equal(confirmBeforeClear.length, 1, 'clearing history needs its own confirmation');
  assert.ok(await waitFor(() => page.evaluate('document.querySelectorAll("#history-list .history-item").length === 0')));
});

test('the Explain button prefixes EXPLAIN and runs the confirmed flow', async (t) => {
  const { fixtureApi } = await v2Fixture({});
  const page = await openTablePage(fixtureApi.url);
  t.after(async () => { await page.close(); await fixtureApi.close(); });
  const confirms = [];
  page.onDialog((dialog) => { confirms.push(dialog.message); return true; });
  await page.evaluate('document.querySelector("#tab-sql").click()');
  await page.evaluate('document.querySelector("#sql-editor").value = "SELECT * FROM items"');
  await page.evaluate('document.querySelector("#explain-query").click()');
  assert.ok(await waitFor(() => fixtureApi.calls.some((call) => call.url.pathname === '/api/query')));
  const call = fixtureApi.calls.find((entry) => entry.url.pathname === '/api/query');
  assert.equal(call.body.sql, 'EXPLAIN SELECT * FROM items');
  assert.equal(call.body.confirmed, true);
  assert.match(call.headers['x-dbkid-request-id'] || '', /^[A-Za-z0-9_-]{22,64}$/);
  assert.equal(await page.evaluate('document.querySelector("#sql-editor").value'), 'EXPLAIN SELECT * FROM items',
    'the editor must keep the prefixed statement');
  assert.equal(confirms.length, 1, 'EXPLAIN must go through the normal confirmation');
});

test('export issues a token and then navigates to the download URL', async (t) => {
  const { fixtureApi } = await v2Fixture({});
  const page = await openTablePage(fixtureApi.url);
  t.after(async () => { await page.close(); await fixtureApi.close(); });
  await waitForRowActions(page);
  // Sort first so the export carries the current ordering.
  await page.evaluate(`document.querySelectorAll('#browse-result thead th button')[1].click()`);
  assert.ok(await waitFor(() => fixtureApi.calls.some((call) => call.url.pathname === '/api/rows' && call.url.searchParams.get('sort') === 'name')));
  await page.evaluate('document.querySelector("#browse-export-button").click()');
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#export-modal").hidden === false')));
  assert.equal(await page.evaluate('document.querySelector("#export-format").value'), 'csv');
  await page.evaluate('document.querySelector("#export-submit").click()');
  assert.ok(await waitFor(() => fixtureApi.calls.some((call) => call.url.pathname === '/api/export/token')));
  const tokenCall = fixtureApi.calls.find((call) => call.url.pathname === '/api/export/token');
  assert.deepEqual(tokenCall.body, {
    database: V2_DATABASE,
    table: V2_TABLE,
    format: 'csv',
    what: 'data',
    sort: 'name',
    dir: 'asc',
    filter: '',
  });
  assert.match(tokenCall.headers['x-dbkid-request-id'] || '', /^[A-Za-z0-9_-]{22,64}$/);
  assert.ok(await waitFor(() => fixtureApi.calls.some((call) => call.url.pathname === '/api/export' && call.url.searchParams.get('token') === 'export-token-1')),
    'the browser must navigate to the tokenized export URL');
});

test('import previews the file client-side and posts the exact payload', async (t) => {
  const { fixtureApi } = await v2Fixture({});
  const page = await openTablePage(fixtureApi.url);
  t.after(async () => { await page.close(); await fixtureApi.close(); });
  await waitForRowActions(page);
  const confirms = [];
  page.onDialog((dialog) => { confirms.push(dialog.message); return true; });
  const temporaryDirectory = process.env.DBKID_TEST_TMPDIR || os.tmpdir();
  const csvFile = path.join(temporaryDirectory, 'dbkid-import-fixture.csv');
  const csvContent = 'id,name\n1,first\n2,second\n';
  fs.writeFileSync(csvFile, csvContent);

  await page.evaluate('document.querySelector("#browse-import-button").click()');
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#import-modal").hidden === false')));
  await page.setFiles('#import-file', [csvFile]);
  assert.ok(await waitFor(() => page.evaluate(`document.querySelector('#import-preview').textContent.includes('dbkid-import-fixture.csv')`)),
    'the preview must show the file name');
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#import-preview").textContent.includes("First line: id,name")')),
    'the preview must show the first line');
  assert.ok(await page.evaluate('document.querySelector("#import-preview").textContent.includes("bytes")'),
    'the preview must show the file size');
  assert.ok(await waitFor(() => page.evaluate('document.querySelectorAll("#import-mapping select").length === 2')),
    'the CSV field count must drive the mapping rows');
  assert.deepEqual(await page.evaluate(`Array.from(document.querySelectorAll('#import-mapping select')).map((select) => select.value)`),
    ['id', 'name'], 'mapping defaults to the table columns positionally');
  await page.evaluate('document.querySelector("#import-submit").click()');
  assert.ok(await waitFor(() => fixtureApi.calls.some((call) => call.url.pathname === '/api/import/csv')));
  assert.ok(confirms[0].includes(JSON.stringify(V2_DATABASE)) && confirms[0].includes(JSON.stringify(V2_TABLE)),
    'the import confirmation must name the database and table');
  const csvCall = fixtureApi.calls.find((call) => call.url.pathname === '/api/import/csv');
  assert.deepEqual({ ...csvCall.body, content: undefined }, {
    database: V2_DATABASE,
    table: V2_TABLE,
    content: undefined,
    has_header: true,
    delimiter: ',',
    quote: '"',
    columns: ['id', 'name'],
    confirmed: true,
  });
  assert.equal(csvCall.body.content, csvContent, 'the posted content must be the file read as UTF-8');
  assert.match(csvCall.headers['x-dbkid-request-id'] || '', /^[A-Za-z0-9_-]{22,64}$/);
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#import-status").textContent.includes("2 rows inserted")')));

  // The same dialog handles SQL scripts.
  const sqlFile = path.join(temporaryDirectory, 'dbkid-import-fixture.sql');
  const sqlContent = 'INSERT INTO items (name) VALUES (\'sql row\');\nUPDATE items SET name = \'x\' WHERE id = 1;\n';
  fs.writeFileSync(sqlFile, sqlContent);
  await page.setFiles('#import-file', [sqlFile]);
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#import-format").value === "sql"')),
    'a .sql file must switch the format automatically');
  await page.evaluate('document.querySelector("#import-submit").click()');
  assert.ok(await waitFor(() => fixtureApi.calls.some((call) => call.url.pathname === '/api/import/sql')));
  const sqlCall = fixtureApi.calls.find((call) => call.url.pathname === '/api/import/sql');
  assert.deepEqual({ ...sqlCall.body, content: undefined }, {
    database: V2_DATABASE,
    content: undefined,
    stop_on_error: true,
    confirmed: true,
  });
  assert.equal(sqlCall.body.content, sqlContent);
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#import-status").textContent.includes("3 statements executed")')));
});

test('binary cells request blob tokens and open them safely', async (t) => {
  const rows = [
    ['1', 'wallet', null],
    ['2', 'image item', '89504e470d0a1a0a0000000d49484452'],
    ['3', 'plain item', 'deadbeef'],
  ];
  const { fixtureApi } = await v2Fixture({ rows });
  const page = await openTablePage(fixtureApi.url);
  t.after(async () => { await page.close(); await fixtureApi.close(); });
  await waitForRowActions(page);
  // The short PNG-hex cell renders an inline preview through a one-time token.
  assert.ok(await waitFor(() => page.evaluate(`(() => {
    const images = Array.from(document.querySelectorAll('#browse-result img.blob-thumb'));
    return images.length === 1 && images[0].src.includes('/api/blob?token=');
  })()`)), 'an image-magic binary cell must render a small preview from a token URL');
  const imageTokenCall = fixtureApi.calls.find((call) => call.url.pathname === '/api/blob/token');
  assert.deepEqual(imageTokenCall.body, {
    database: V2_DATABASE,
    table: V2_TABLE,
    column: 'photo',
    key: [['id', '2']],
  });
  // Other binary cells get an open link that opens the tokenized URL.
  await page.evaluate('window.__opened = []; window.open = (url) => { window.__opened.push(String(url)); return null; };');
  await page.evaluate(`Array.from(document.querySelectorAll('#browse-result .blob-link'))[0].click()`);
  assert.ok(await waitFor(() => page.evaluate('window.__opened.length === 1')));
  assert.equal(await page.evaluate('window.__opened[0]'), '/api/blob?token=blob-token-1');
  const linkTokenCall = fixtureApi.calls.filter((call) => call.url.pathname === '/api/blob/token').pop();
  assert.deepEqual(linkTokenCall.body, {
    database: V2_DATABASE,
    table: V2_TABLE,
    column: 'photo',
    key: [['id', '3']],
  });
});

test('recent connections prefill the form without the password', async (t) => {
  const recent = [
    { host: '127.0.0.1', port: 3307, user: 'alice', tls: 'verify' },
    { host: '127.0.0.1', port: 3306, user: 'bob', tls: 'disable' },
  ];
  const api = await fixture({ handler: (call) => {
    if (call.url.pathname === '/api/recent') return { body: { recent } };
  } });
  const page = await openPage(api.url);
  t.after(async () => { await page.close(); await api.close(); });
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#connect-view").hidden === false')));
  assert.ok(await waitFor(() => page.evaluate('document.querySelectorAll(".recent-chip").length === 2')),
    'recent connections must appear as chips');
  await page.evaluate('document.querySelector(".recent-chip").click()');
  assert.equal(await page.evaluate('document.querySelector("#host").value'), '127.0.0.1');
  assert.equal(await page.evaluate('document.querySelector("#port").value'), '3307');
  assert.equal(await page.evaluate('document.querySelector("#user").value'), 'alice');
  assert.equal(await page.evaluate('document.querySelector("#tls").value'), 'verify');
  assert.equal(await page.evaluate('document.querySelector("#password").value'), '',
    'the password must stay empty — credentials are never stored');
  assert.ok(await page.evaluate('document.querySelector("#recent-connections").textContent.toLowerCase().includes("password")'),
    'the recent-connections hint must mention the password requirement');
});

test('the frontend never renders data through innerHTML', async () => {
  for (const filename of ['app.js', 'index.html', 'style.css']) {
    const source = fs.readFileSync(path.join(webRoot, filename), 'utf8');
    assert.equal(source.includes('innerHTML'), false, `${filename} must not use innerHTML`);
    assert.equal(source.includes('insertAdjacentHTML'), false, `${filename} must not use insertAdjacentHTML`);
    assert.equal(source.includes('outerHTML'), false, `${filename} must not use outerHTML`);
    assert.equal(source.includes('document.write'), false, `${filename} must not use document.write`);
  }
});

test('the structure tab manages indexes with confirmations and request IDs', async (t) => {
  const { fixtureApi } = await v2Fixture({});
  const page = await openTablePage(fixtureApi.url);
  t.after(async () => { await page.close(); await fixtureApi.close(); });
  await waitForRowActions(page);
  const confirms = [];
  page.onDialog((dialog) => { confirms.push(dialog.message); return true; });
  await page.evaluate('document.querySelector("#tab-structure").click()');
  assert.ok(await waitFor(() => page.evaluate('document.querySelectorAll("#structure-result tbody tr").length === 3')));
  assert.ok(await waitFor(() => page.evaluate('document.querySelectorAll("#indexes-result .index-item").length === 1')),
    'the existing index must render');
  await page.evaluate('document.querySelector("#add-index-button").click()');
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#index-form").hidden === false')));
  await page.evaluate(`(() => {
    document.querySelector('#index-form-name').value = 'idx_name';
    const checkbox = Array.from(document.querySelectorAll('#index-form-columns input[type="checkbox"]'))
      .find((box) => box.value === 'name');
    checkbox.click();
  })()`);
  await page.evaluate('document.querySelector("#index-form-submit").click()');
  assert.ok(await waitFor(() => fixtureApi.calls.some((call) => call.url.pathname === '/api/index/create')));
  assert.ok(confirms[0].includes(JSON.stringify(V2_DATABASE)) && confirms[0].includes(JSON.stringify(V2_TABLE)),
    'the index confirmation must name the database and table');
  const createCall = fixtureApi.calls.find((call) => call.url.pathname === '/api/index/create');
  assert.deepEqual(createCall.body, {
    database: V2_DATABASE, table: V2_TABLE, name: 'idx_name', columns: ['name'], unique: false, confirmed: true,
  });
  assert.match(createCall.headers['x-dbkid-request-id'] || '', /^[A-Za-z0-9_-]{22,64}$/);
  await page.evaluate(`Array.from(document.querySelectorAll('#indexes-result .index-item button'))[0].click()`);
  assert.ok(await waitFor(() => fixtureApi.calls.some((call) => call.url.pathname === '/api/index/drop')));
  assert.match(confirms[1], /cannot be undone/i);
  const dropCall = fixtureApi.calls.find((call) => call.url.pathname === '/api/index/drop');
  assert.deepEqual(dropCall.body, { database: V2_DATABASE, table: V2_TABLE, name: 'PRIMARY', confirmed: true });
});

test('the structure tab modifies and drops columns', async (t) => {
  const { fixtureApi } = await v2Fixture({});
  const page = await openTablePage(fixtureApi.url);
  t.after(async () => { await page.close(); await fixtureApi.close(); });
  await waitForRowActions(page);
  const confirms = [];
  page.onDialog((dialog) => { confirms.push(dialog.message); return true; });
  await page.evaluate('document.querySelector("#tab-structure").click()');
  assert.ok(await waitFor(() => page.evaluate('document.querySelectorAll("#structure-result tbody tr").length === 3')));
  await page.evaluate(`Array.from(document.querySelectorAll('#structure-result tbody tr'))[1]
    .querySelector('.row-actions button').click()`);
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#column-form").hidden === false')));
  assert.equal(await page.evaluate('document.querySelector("#column-form-name").value'), 'name',
    'modify must prefill the column name');
  assert.equal(await page.evaluate('document.querySelector("#column-form-type").value'), 'varchar');
  assert.equal(await page.evaluate('document.querySelector("#column-form-length").value'), '80',
    'the length must be parsed out of the type');
  await page.evaluate(`(() => {
    const input = document.querySelector('#column-form-length');
    input.value = '120';
    input.dispatchEvent(new Event('input', { bubbles: true }));
  })()`);
  // The modify triggers a loadStructure refresh that replaces the grid; capture
  // a pre-modify row so the refresh is detectable as a detach before the next
  // click (a row count alone is satisfied by the stale DOM).
  await page.evaluate(`(() => {
    window.__staleStructureRow = document.querySelectorAll('#structure-result tbody tr')[2];
  })()`);
  await page.evaluate('document.querySelector("#column-form-submit").click()');
  assert.ok(await waitFor(() => fixtureApi.calls.some((call) => call.url.pathname === '/api/column/modify')));
  assert.ok(confirms[0].includes(JSON.stringify(V2_TABLE)) && confirms[0].includes(JSON.stringify(V2_DATABASE)),
    'the column confirmation must name the table and database');
  const modifyCall = fixtureApi.calls.find((call) => call.url.pathname === '/api/column/modify');
  assert.equal(modifyCall.body.old_name, 'name');
  assert.deepEqual(modifyCall.body.column, {
    name: 'name', type: 'varchar', length: '120', decimals: '', nullable: true,
    auto_increment: false, primary: false, unique: false, default: '', default_null: true,
  });
  assert.equal(modifyCall.body.confirmed, true);
  assert.ok(await waitFor(() => page.evaluate(`(() =>
    window.__staleStructureRow && !window.__staleStructureRow.isConnected &&
    (() => { const rows = document.querySelectorAll('#structure-result tbody tr');
      return rows.length === 3 && rows[2].querySelector('.row-actions .button-danger') !== null; })())()`)),
    'structure must re-render after modify');
  await page.evaluate(`Array.from(document.querySelectorAll('#structure-result tbody tr'))[2]
    .querySelector('.row-actions .button-danger').click()`);
  assert.ok(await waitFor(() => fixtureApi.calls.some((call) => call.url.pathname === '/api/column/drop')));
  assert.match(confirms[1], /cannot be undone/i);
  const dropCall = fixtureApi.calls.find((call) => call.url.pathname === '/api/column/drop');
  assert.deepEqual(dropCall.body, { database: V2_DATABASE, table: V2_TABLE, column: 'photo', confirmed: true });
});

test('replacement sends the complete snapshot and exact SQL with security headers', async (t) => {
  const sql = 'CREATE PROCEDURE add_total() BEGIN SELECT 2; END';
  const original = { database: 'shop', name: 'add_total', kind: 'procedure', generation: 7,
    definition: 'CREATE PROCEDURE add_total() BEGIN SELECT 1; END', truncated: false,
    metadata: { procedure: 'add_total', 'create procedure': 'CREATE PROCEDURE add_total() BEGIN SELECT 1; END', sql_mode: '', character_set_client: 'utf8mb4', collation_connection: 'utf8mb4_general_ci', 'database collation': 'utf8mb4_general_ci' } };
  const { page, fixtureApi } = await objectTestPage(t, (call) => {
    if (call.url.pathname === '/api/object/definition') return { body: original };
    if (call.url.pathname === '/api/object/replace') return { body: { stage: 'replaced' } };
  });
  const confirms = [];
  page.onDialog((dialog) => { confirms.push(dialog.message); return true; });
  await openFirstObjectEditor(page);
  await page.evaluate(`document.querySelector('#object-sql').value = ${JSON.stringify(sql)}; document.querySelector('#object-editor-submit').click()`);
  assert.ok(await waitFor(() => fixtureApi.calls.some(c => c.url.pathname === '/api/object/replace')));
  const call = fixtureApi.calls.find(c => c.url.pathname === '/api/object/replace');
  assert.deepEqual(call.body, { original, sql, confirmed: true });
  assert.equal(call.headers['x-csrf-token'], 'fixture-csrf');
  assert.match(call.headers['x-dbkid-request-id'], /^[a-f0-9]{32}$/);
  assert.match(confirms[0], /procedure.*add_total.*shop.*127\.0\.0\.1:3306/is);
  assert.match(confirms[0], /non.atomic/i);
  assert.ok(await waitFor(() => page.evaluate(`/Replacement completed successfully/i.test(document.querySelector('#query-outcome-warnings').textContent)`)),
    'the completed replacement must be announced after the dialog closes');
});

test('object editor loads complete definition for replacement', async (t) => {
  const { fixtureApi } = await v2Fixture({});
  const page = await openTablePage(fixtureApi.url);
  t.after(async () => { await page.close(); await fixtureApi.close(); });
  await page.evaluate('document.querySelector("#tab-objects").click()');
  assert.ok(await waitFor(() => page.evaluate('document.querySelectorAll("#objects-result .object-item").length === 4')));
  assert.equal(await page.evaluate(`Array.from(document.querySelectorAll('#objects-result .object-item'))[0].querySelectorAll('button').length >= 3`), true, 'procedure must offer edit');
  await page.evaluate(`Array.from(document.querySelectorAll('#objects-result .object-item'))[0].querySelector('[data-action="edit"]').click()`);
  assert.ok(await waitFor(() => page.evaluate(`document.querySelector('#object-original').value.includes('CREATE PROCEDURE add_total')`)));
  assert.equal(await page.evaluate(`document.querySelector('#object-sql').value`), 'CREATE PROCEDURE add_total() BEGIN SELECT 1; END');
  assert.equal(await page.evaluate(`document.querySelector('#object-editor-modal').contains(document.activeElement)`), true, 'focus must enter the edit dialog');
});

async function objectTestPage(t, override = () => null, session = V2_SESSION) {
  const made = makeV2Api();
  const fixtureApi = await fixture({ session, handler: async (call, context) =>
    await override(call, context) || made.handler(call) });
  const page = await openTablePage(fixtureApi.url);
  t.after(async () => { await page.close(); await fixtureApi.close(); });
  await page.evaluate('document.querySelector("#tab-objects").click()');
  assert.ok(await waitFor(() => page.evaluate('document.querySelectorAll("#objects-result .object-item").length === 4')));
  return { page, fixtureApi };
}

async function openFirstObjectEditor(page) {
  await page.evaluate(`document.querySelector('#objects-result [data-action="edit"]').click()`);
  assert.ok(await waitFor(() => page.evaluate(`document.querySelector('#object-original').value.includes('CREATE PROCEDURE')`)));
}

test('definition modal ignores stale response after close and reopen', async (t) => {
  let reads = 0;
  const { page } = await objectTestPage(t, async (call) => {
    if (call.url.pathname === '/api/routine') {
      if (++reads === 1) { await delay(350); return { body: { name: 'add_total', definition: 'OLD' } }; }
      return { body: { name: 'add_total', definition: 'NEW' } };
    }
  });
  await page.evaluate(`document.querySelector('#objects-result .object-item button').click(); document.querySelector('#definition-cancel').click(); document.querySelector('#objects-result .object-item button').click()`);
  await delay(500);
  assert.equal(await page.evaluate(`document.querySelector('#definition-code').textContent`), 'NEW');
});

test('object editor traps tab focus and restores its opener', async (t) => {
  const { page } = await objectTestPage(t);
  await page.evaluate(`document.querySelector('#object-create-event').focus(); document.querySelector('#object-create-event').click(); document.querySelector('#object-editor-cancel').focus(); document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Tab', bubbles: true, cancelable: true }))`);
  assert.equal(await page.evaluate(`document.activeElement.id`), 'object-editor-close');
  await page.evaluate(`document.querySelector('#object-editor-cancel').click()`);
  assert.equal(await page.evaluate(`document.activeElement.id`), 'object-create-event');
});

test('replacement stage errors lock retries except explicit not_started', async (t) => {
  for (const [stage, expected, locked] of [
    ['not_started', /did not start/i, false], ['drop_unknown', /DROP outcome is unknown/i, true],
    ['dropped_create_failed', /DROP completed but CREATE failed/i, true],
    ['create_unknown', /DROP completed but CREATE outcome is unknown/i, true],
    ['replay_unknown', /previous attempt.*outcome.*unknown/i, true],
    ['created_unverified', /CREATE completed.*could not be verified/i, true],
    [null, /Replacement outcome is unknown/i, true],
  ]) await t.test(String(stage), async (t) => {
    const { page, fixtureApi } = await objectTestPage(t, (call) => call.url.pathname === '/api/object/replace' ?
      { status: stage === null ? 400 : 502, body: { error: 'rejected', ...(stage ? { stage } : {}) } } : null);
    page.onDialog(() => true);
    await openFirstObjectEditor(page);
    await page.evaluate(`document.querySelector('#object-editor-submit').click()`);
    assert.ok(await waitFor(() => page.evaluate(`document.querySelector('#object-editor-status').textContent.includes('rejected')`)));
    assert.match(await page.evaluate(`document.querySelector('#object-editor-status').textContent`), expected);
    assert.equal(await page.evaluate(`document.querySelector('#object-editor-submit').disabled`), locked);
    await page.evaluate(`document.querySelector('#object-editor-cancel').click()`);
    assert.match(await page.evaluate(`document.querySelector('#query-outcome-warnings').textContent`), /Original definition retained/);
    assert.match(await page.evaluate(`document.querySelector('#query-outcome-warnings textarea').value`), /CREATE PROCEDURE/);
    await openFirstObjectEditor(page);
    assert.equal(await page.evaluate(`document.querySelector('#object-editor-submit').disabled`), locked);
    if (locked) {
      await page.evaluate(`document.querySelector('#object-editor-submit').disabled = false; document.querySelector('#object-editor-submit').click()`);
      await delay(100);
      assert.equal(fixtureApi.calls.filter(c => c.url.pathname === '/api/object/replace').length, 1);
    }
  });
});

test('object name updates untouched SQL template without overwriting edits', async (t) => {
  const { page } = await objectTestPage(t);
  await page.evaluate(`document.querySelector('#object-create-procedure').click();
    const name = document.querySelector('#object-name');
    name.value = 'night` + '`' + `audit'; name.dispatchEvent(new Event('input', {bubbles:true}))`);
  assert.match(await page.evaluate(`document.querySelector('#object-sql').value`), /^CREATE PROCEDURE `night``audit`\(\)/);
  await page.evaluate(`document.querySelector('#object-sql').value += '\\n-- custom body';
    document.querySelector('#object-name').value = 'different'; document.querySelector('#object-name').dispatchEvent(new Event('input', {bubbles:true}))`);
  assert.match(await page.evaluate(`document.querySelector('#object-sql').value`), /-- custom body$/);
  assert.match(await page.evaluate(`document.querySelector('#object-sql').value`), /^CREATE PROCEDURE `night``audit`\(\)/);
});

test('object create templates use explicit confirmation and send exact SQL and security headers', async (t) => {
  const { page, fixtureApi } = await objectTestPage(t);
  const confirms = [];
  let accept = false;
  page.onDialog((dialog) => { confirms.push(dialog.message); return accept; });
  for (const kind of ['procedure', 'trigger', 'event']) {
    await page.evaluate(`document.querySelector('#object-create-${kind}').click()`);
    assert.match(await page.evaluate(`document.querySelector('#object-sql').value`), new RegExp(`CREATE ${kind}`, 'i'));
    await page.evaluate(`document.querySelector('#object-editor-submit').click()`);
    assert.equal(fixtureApi.calls.filter((c) => c.url.pathname === '/api/object/create').length, 0);
    await page.evaluate(`document.querySelector('#object-editor-cancel').click()`);
  }
  accept = true;
  await page.evaluate(`document.querySelector('#object-create-trigger').click(); document.querySelector('#object-name').value = 'tricky'; document.querySelector('#object-sql').value = 'CREATE TRIGGER tricky BEFORE INSERT ON items FOR EACH ROW BEGIN SET @x = 1; SET @y = 2; END'; document.querySelector('#object-editor-submit').click()`);
  assert.ok(await waitFor(() => fixtureApi.calls.some((c) => c.url.pathname === '/api/object/create')));
  const call = fixtureApi.calls.find((c) => c.url.pathname === '/api/object/create');
  assert.deepEqual(call.body, { database: 'shop', name: 'tricky', kind: 'trigger', generation: 7, sql: 'CREATE TRIGGER tricky BEFORE INSERT ON items FOR EACH ROW BEGIN SET @x = 1; SET @y = 2; END', confirmed: true });
  assert.equal(call.headers['x-csrf-token'], 'fixture-csrf');
  assert.match(call.headers['x-dbkid-request-id'], /^[a-f0-9]{32}$/);
  assert.match(confirms.at(-1), /trigger.*tricky.*shop.*127\.0\.0\.1:3306/is);
  assert.match(confirms.at(-1), /unsandboxed/i);
});

test('replacement sends proposed SQL for server validation without direct DROP or query', async (t) => {
  const { page, fixtureApi } = await objectTestPage(t, (call) => call.url.pathname === '/api/object/replace' ?
    { status: 400, body: { error: 'invalid CREATE target', stage: 'not_started' } } : null);
  page.onDialog(() => true);
  await openFirstObjectEditor(page);
  await page.evaluate(`document.querySelector('#object-sql').value = 'SELECT 1'; document.querySelector('#object-editor-submit').click()`);
  assert.ok(await waitFor(() => page.evaluate(`/did not start/i.test(document.querySelector('#object-editor-status').textContent)`)));
  assert.equal(fixtureApi.calls.find(c => c.url.pathname === '/api/object/replace').body.sql, 'SELECT 1');
  assert.equal(fixtureApi.calls.filter(c => ['/api/routine/drop', '/api/query', '/api/object/create'].includes(c.url.pathname)).length, 0);
  assert.match(await page.evaluate(`document.querySelector('#object-original').value`), /CREATE PROCEDURE/);
});

test('large fetched original stays copyable while replacement is disabled', async (t) => {
  const definition = 'CREATE PROCEDURE add_total() SELECT "' + 'é'.repeat(400000) + '"';
  const original = { database: 'shop', name: 'add_total', kind: 'procedure', generation: 7, definition, truncated: false,
    metadata: { procedure: 'add_total', 'create procedure': definition, sql_mode: '', character_set_client: 'utf8mb4', collation_connection: 'utf8mb4_general_ci', 'database collation': 'utf8mb4_general_ci' } };
  const { page, fixtureApi } = await objectTestPage(t, c => c.url.pathname === '/api/object/definition' ? { body: original } : null);
  let confirms = 0;
  page.onDialog(() => { confirms++; return true; });
  await openFirstObjectEditor(page);
  assert.equal(await page.evaluate(`document.querySelector('#object-original').value.length`), definition.length);
  assert.equal(await page.evaluate(`document.querySelector('#object-editor-submit').disabled`), true);
  assert.match(await page.evaluate(`document.querySelector('#object-editor-status').textContent`), /1 MiB.*metadata/i);
  await page.evaluate(`document.querySelector('#object-sql').value = 'CREATE PROCEDURE add_total() SELECT 1'; document.querySelector('#object-sql').dispatchEvent(new Event('input', {bubbles:true})); document.querySelector('#object-editor-submit').disabled = false; document.querySelector('#object-editor-submit').click()`);
  assert.equal(confirms, 0);
  assert.equal(fixtureApi.calls.filter(c => c.url.pathname === '/api/object/replace').length, 0);
  await page.evaluate(`document.querySelector('#object-editor-cancel').click()`);
  assert.equal(await page.evaluate(`document.querySelector('#query-outcome-warnings textarea').value.length`), definition.length);
});

test('shortening edited SQL can bring an oversized replacement under the limit', async (t) => {
  const definition = 'CREATE PROCEDURE add_total() SELECT "' + 'x'.repeat(400000) + '"';
  const original = { database: 'shop', name: 'add_total', kind: 'procedure', generation: 7, definition, truncated: false,
    metadata: { procedure: 'add_total', 'create procedure': definition, sql_mode: '', character_set_client: 'utf8mb4', collation_connection: 'utf8mb4_general_ci', 'database collation': 'utf8mb4_general_ci' } };
  const { page, fixtureApi } = await objectTestPage(t, c => c.url.pathname === '/api/object/definition' ? { body: original } : null);
  await openFirstObjectEditor(page);
  assert.equal(await page.evaluate(`document.querySelector('#object-editor-submit').disabled`), true);
  await page.evaluate(`document.querySelector('#object-sql').value = 'CREATE PROCEDURE add_total() SELECT 1'; document.querySelector('#object-sql').dispatchEvent(new Event('input', {bubbles:true}))`);
  assert.equal(await page.evaluate(`document.querySelector('#object-editor-submit').disabled`), false);
  assert.equal(await page.evaluate(`document.querySelector('#object-original').value.length`), definition.length);
  assert.equal(fixtureApi.calls.filter(c => c.url.pathname === '/api/object/replace').length, 0);
});

test('oversized UTF-8 JSON replacement is rejected before confirmation', async (t) => {
  const { page, fixtureApi } = await objectTestPage(t);
  let confirms = 0;
  page.onDialog(() => { confirms++; return true; });
  await openFirstObjectEditor(page);
  await page.evaluate(`document.querySelector('#object-sql').value = 'é'.repeat(530000); document.querySelector('#object-editor-submit').click()`);
  assert.match(await page.evaluate(`document.querySelector('#object-editor-status').textContent`), /1 MiB JSON byte limit/i);
  assert.equal(confirms, 0);
  assert.equal(fixtureApi.calls.filter(c => c.url.pathname === '/api/object/replace').length, 0);
});

test('incomplete snapshot fields and generation mismatches cannot be submitted', async (t) => {
  const definition = 'CREATE PROCEDURE add_total() BEGIN SELECT 1; END';
  const valid = { database: 'shop', name: 'add_total', kind: 'procedure', generation: 7, definition, truncated: false,
    metadata: { procedure: 'add_total', 'create procedure': definition, sql_mode: '', character_set_client: 'utf8mb4', collation_connection: 'utf8mb4_general_ci', 'database collation': 'utf8mb4_general_ci' } };
  for (const bad of [{ ...valid, database: 'other' }, { ...valid, generation: 8 }, { ...valid, generation: null },
    { ...valid, metadata: { ...valid.metadata, sql_mode: undefined } },
    { ...valid, metadata: { ...valid.metadata, 'create procedure': 'changed' } }]) await t.test(JSON.stringify(bad), async (t) => {
    const { page, fixtureApi } = await objectTestPage(t, c => c.url.pathname === '/api/object/definition' ? { body: bad } : null);
    page.onDialog(() => true);
    await page.evaluate(`document.querySelector('#objects-result [data-action="edit"]').click()`);
    assert.ok(await waitFor(() => page.evaluate(`/not available/i.test(document.querySelector('#object-editor-status').textContent)`)));
    await page.evaluate(`document.querySelector('#object-editor-submit').disabled = false; document.querySelector('#object-editor-submit').click()`);
    assert.equal(fixtureApi.calls.filter(c => c.url.pathname === '/api/object/replace').length, 0);
  });
});

test('creation binds to generation returned by connect', async (t) => {
  const made = makeV2Api();
  const fixtureApi = await fixture({ handler: c => c.url.pathname === '/api/connect' ?
    { body: { ...V2_SESSION, generation: 29 } } : made.handler(c) });
  const page = await openPage(fixtureApi.url);
  t.after(async () => { await page.close(); await fixtureApi.close(); });
  page.onDialog(() => true);
  assert.ok(await waitFor(() => page.evaluate(`!document.querySelector('#connect-view').hidden`)));
  await page.evaluate(`document.querySelector('#user').value = 'fixture-user'; document.querySelector('#connect-form').requestSubmit()`);
  assert.ok(await waitFor(() => page.evaluate(`document.querySelector('.database-button') !== null`)));
  await page.evaluate(`document.querySelector('.database-button').click()`);
  assert.ok(await waitFor(() => page.evaluate(`!document.querySelector('#tab-objects').disabled`)));
  await page.evaluate(`document.querySelector('#tab-objects').click(); document.querySelector('#object-create-procedure').click(); document.querySelector('#object-editor-submit').click()`);
  assert.ok(await waitFor(() => fixtureApi.calls.some(c => c.url.pathname === '/api/object/create')));
  assert.equal(fixtureApi.calls.find(c => c.url.pathname === '/api/object/create').body.generation, 29);
});

test('creation refuses missing session generation before confirmation', async (t) => {
  const { page, fixtureApi } = await objectTestPage(t, () => null, { ...V2_SESSION, generation: undefined });
  let confirms = 0;
  page.onDialog(() => { confirms++; return true; });
  await page.evaluate(`document.querySelector('#object-create-procedure').click(); document.querySelector('#object-editor-submit').click()`);
  assert.match(await page.evaluate(`document.querySelector('#object-editor-status').textContent`), /generation is unavailable/i);
  assert.equal(confirms, 0);
  assert.equal(fixtureApi.calls.filter(c => c.url.pathname === '/api/object/create').length, 0);
});

test('malformed and lost replacement responses lock retries', async (t) => {
  for (const reply of [{ body: {} }, { body: { stage: 'replaced', error: 'contradiction' } }, { drop: true }, { status: 400, body: { error: 'bad' } }])
    await t.test(JSON.stringify(reply), async (t) => {
      const { page } = await objectTestPage(t, c => c.url.pathname === '/api/object/replace' ? reply : null);
      page.onDialog(() => true);
      await openFirstObjectEditor(page);
      await page.evaluate(`document.querySelector('#object-editor-submit').click()`);
      assert.ok(await waitFor(() => page.evaluate(`/outcome is unknown/i.test(document.querySelector('#object-editor-status').textContent)`)));
      assert.equal(await page.evaluate(`document.querySelector('#object-editor-submit').disabled`), true);
    });
});

test('background replacement success is announced without affecting another dialog', async (t) => {
  const { page, fixtureApi } = await objectTestPage(t, async c => {
    if (c.url.pathname === '/api/object/replace') { await delay(350); return { body: { stage: 'replaced' } }; }
  });
  page.onDialog(() => true);
  await openFirstObjectEditor(page);
  await page.evaluate(`document.querySelector('#object-editor-submit').click()`);
  assert.ok(await waitFor(() => fixtureApi.calls.some(c => c.url.pathname === '/api/object/replace')));
  await page.evaluate(`document.querySelector('#object-editor-cancel').click(); document.querySelector('#objects-result .object-item button').click()`);
  assert.ok(await waitFor(() => page.evaluate(`/Replacement completed successfully/i.test(document.querySelector('#query-outcome-warnings').textContent)`)));
  assert.equal(await page.evaluate(`document.querySelector('#definition-modal').hidden`), false);
});

test('background replacement failure preserves original and locks reopened editor', async (t) => {
  const { page, fixtureApi } = await objectTestPage(t, async c => {
    if (c.url.pathname === '/api/object/replace') { await delay(350); return { status: 502, body: { stage: 'create_unknown', error: 'lost' } }; }
  });
  page.onDialog(() => true);
  await openFirstObjectEditor(page);
  await page.evaluate(`document.querySelector('#object-editor-submit').click()`);
  assert.ok(await waitFor(() => fixtureApi.calls.some(c => c.url.pathname === '/api/object/replace')));
  await page.evaluate(`document.querySelector('#object-editor-cancel').click()`);
  assert.ok(await waitFor(() => page.evaluate(`/CREATE outcome is unknown/i.test(document.querySelector('#query-outcome-warnings').textContent)`)));
  assert.match(await page.evaluate(`document.querySelector('#query-outcome-warnings textarea').value`), /CREATE PROCEDURE/);
  await openFirstObjectEditor(page);
  assert.equal(await page.evaluate(`document.querySelector('#object-editor-submit').disabled`), true);
});

test('invalid definitions block editing and direct DROP', async (t) => {
  for (const invalid of [null, { name: 'wrong', kind: 'procedure', definition: 'sql', truncated: false },
    { name: 'add_total', kind: 'trigger', definition: 'sql', truncated: false },
    { name: 'add_total', kind: 'procedure', definition: ' ', truncated: false },
    { name: 'add_total', kind: 'procedure', definition: 'sql', truncated: true }]) {
    await t.test(JSON.stringify(invalid), async (t) => {
      const { page, fixtureApi } = await objectTestPage(t, (call) => call.url.pathname === '/api/object/definition' ? { body: invalid } : null);
      page.onDialog(() => true);
      await page.evaluate(`document.querySelector('#objects-result [data-action="edit"]').click()`);
      assert.ok(await waitFor(() => page.evaluate(`/not available/i.test(document.querySelector('#object-editor-status').textContent)`)));
      assert.equal(await page.evaluate(`document.querySelector('#object-editor-submit').disabled`), true);
      await page.evaluate(`document.querySelector('#object-editor-cancel').click(); document.querySelector('#objects-result .button-danger').click()`);
      assert.ok(await waitFor(() => page.evaluate(`/not available/i.test(document.querySelector('#objects-status').textContent)`)));
      assert.equal(fixtureApi.calls.filter((c) => c.url.pathname === '/api/routine/drop').length, 0);
    });
  }
});

test('unverified CREATE is retained and never treated as success or retried', async (t) => {
  for (const status of [200, 502]) await t.test(String(status), async (t) => {
    const { page, fixtureApi } = await objectTestPage(t, (call) => call.url.pathname === '/api/object/create' ?
      { status, body: { stage: 'created_unverified', affected_rows: 0, error: 'readback failed' } } : null);
    page.onDialog(() => true);
    await page.evaluate(`document.querySelector('#object-create-procedure').click(); document.querySelector('#object-editor-submit').click()`);
    assert.ok(await waitFor(() => page.evaluate(`/CREATE completed.*could not be verified/i.test(document.querySelector('#object-editor-status').textContent)`)));
    assert.equal(await page.evaluate(`document.querySelector('#object-editor-submit').disabled`), true);
    await page.evaluate(`document.querySelector('#object-editor-submit').click()`);
    assert.equal(fixtureApi.calls.filter(c => c.url.pathname === '/api/object/create').length, 1);
    await page.evaluate(`document.querySelector('#object-editor-cancel').click()`);
    assert.match(await page.evaluate(`document.querySelector('#query-outcome-warnings').textContent`), /CREATE completed.*could not be verified/i);
    await page.evaluate(`document.querySelector('#object-create-procedure').click(); document.querySelector('#object-editor-submit').click()`);
    assert.match(await page.evaluate(`document.querySelector('#object-editor-status').textContent`), /locked.*uncertain operation/i);
    assert.equal(fixtureApi.calls.filter(c => c.url.pathname === '/api/object/create').length, 1);
  });
});

test('create failures block retries and report unknown outcomes', async (t) => {
  for (const fail of ['error', 'invalid', 'lost']) {
    await t.test(fail, async (t) => {
      const { page, fixtureApi } = await objectTestPage(t, (call) => {
        if (call.url.pathname === '/api/object/create') {
          return fail === 'error' ? { status: 500, body: { error: 'fixture failure' } } : fail === 'lost' ? { drop: true } : { body: {} };
        }
      });
      page.onDialog(() => true);
      await page.evaluate(`document.querySelector('#object-create-procedure').click(); document.querySelector('#object-editor-submit').click(); document.querySelector('#object-editor-submit').click()`);
      assert.ok(await waitFor(() => page.evaluate(`/inspect the server/i.test(document.querySelector('#object-editor-status').textContent)`)));
      assert.equal(await page.evaluate(`document.querySelector('#object-editor-submit').disabled`), true);
      const calls = fixtureApi.calls.filter((c) => c.url.pathname === '/api/object/create');
      assert.ok(calls.length >= 1);
      // Chromium may retransmit a request on connection loss; every wire
      // attempt must retain the same server-enforced replay key.
      assert.equal(new Set(calls.map(c => c.headers['x-dbkid-request-id'])).size, 1);
    });
  }
});

test('closing a pending definition cannot overwrite another editor', async (t) => {
  const { page } = await objectTestPage(t, async (call) => {
    if (call.url.pathname === '/api/object/definition') { await delay(300); return { body: { name: 'add_total', kind: 'procedure', definition: 'late original', truncated: false } }; }
  });
  await page.evaluate(`document.querySelector('#objects-result [data-action="edit"]').click(); document.querySelector('#object-editor-cancel').click(); document.querySelector('#object-create-event').click()`);
  await delay(450);
  assert.equal(await page.evaluate(`document.querySelector('#object-editor-modal').hidden`), false);
  assert.match(await page.evaluate(`document.querySelector('#object-sql').value`), /CREATE EVENT/);
  assert.equal(await page.evaluate(`document.querySelector('#object-editor-title').textContent`), 'Create event');
});

test('trigger and event definitions load using their exact identities', async (t) => {
  const { page, fixtureApi } = await objectTestPage(t);
  for (const [index, name, kind] of [[2, 'items_before_insert', 'trigger'], [3, 'nightly_cleanup', 'event']]) {
    await page.evaluate(`document.querySelectorAll('#objects-result .object-item')[${index}].querySelector('[data-action="edit"]').click()`);
    assert.ok(await waitFor(() => fixtureApi.calls.some(c => c.url.pathname === '/api/object/definition' && c.url.searchParams.get('kind') === kind)));
    assert.ok(await waitFor(() => page.evaluate(`/Review and save the original/.test(document.querySelector('#object-editor-status').textContent)`)));
    assert.equal(await page.evaluate(`document.querySelector('#object-editor-submit').disabled`), false);
    const call = fixtureApi.calls.filter(c => c.url.pathname === '/api/object/definition').at(-1);
    assert.deepEqual(Object.fromEntries(call.url.searchParams), { database: 'shop', name, kind });
    await page.evaluate(`document.querySelector('#object-editor-cancel').click()`);
  }
});

test('an independent object list error does not hide other kinds', async (t) => {
  const made = makeV2Api();
  const fixtureApi = await fixture({ session: V2_SESSION, handler: (call) =>
    call.url.pathname === '/api/triggers' ? { status: 500, body: { error: 'trigger permission denied' } } : made.handler(call) });
  const page = await openTablePage(fixtureApi.url);
  t.after(async () => { await page.close(); await fixtureApi.close(); });
  await page.evaluate(`document.querySelector('#tab-objects').click()`);
  assert.ok(await waitFor(() => page.evaluate(`document.querySelectorAll('#objects-result .object-item').length === 3`)));
  assert.match(await page.evaluate(`document.querySelector('#objects-result').textContent`), /Triggers.*trigger permission denied/is);
  assert.match(await page.evaluate(`document.querySelector('#objects-result').textContent`), /nightly_cleanup/);
});

test('changing database while loading a definition never sends DROP', async (t) => {
  const { page, fixtureApi } = await objectTestPage(t, async (call) => {
    if (call.url.pathname === '/api/databases') return { body: { databases: ['shop', 'other'] } };
    if (call.url.pathname === '/api/object/definition') {
      await delay(350);
      return { body: { name: 'add_total', kind: 'procedure', definition: 'CREATE PROCEDURE add_total() BEGIN SELECT 1; END', truncated: false } };
    }
  });
  page.onDialog(() => true);
  await page.evaluate(`document.querySelector('#objects-result [data-action="edit"]').click()`);
  assert.ok(await waitFor(() => fixtureApi.calls.some(c => c.url.pathname === '/api/object/definition')));
  await page.evaluate(`Array.from(document.querySelectorAll('.database-button')).find(b => b.textContent.includes('other')).click()`);
  await delay(500);
  assert.equal(fixtureApi.calls.filter(c => c.url.pathname === '/api/routine/drop').length, 0);
  assert.equal(fixtureApi.calls.filter(c => c.url.pathname === '/api/query').length, 0);
});

test('a background CREATE success is announced', async (t) => {
  const { page, fixtureApi } = await objectTestPage(t, async (call) => {
    if (call.url.pathname === '/api/object/create') { await delay(350); return { body: { stage: 'created', affected_rows: 0 } }; }
  });
  page.onDialog(() => true);
  await page.evaluate(`document.querySelector('#object-create-procedure').click(); document.querySelector('#object-editor-submit').click()`);
  assert.ok(await waitFor(() => fixtureApi.calls.some(c => c.url.pathname === '/api/object/create')));
  await page.evaluate(`document.querySelector('#object-editor-cancel').click()`);
  assert.ok(await waitFor(() => page.evaluate(`/CREATE completed successfully/i.test(document.querySelector('#query-outcome-warnings').textContent)`)));
});

test('carriage returns disable editing and original download is lossless', async (t) => {
  const definition = 'CREATE PROCEDURE add_total() SELECT \'a\rb\'';
  const { page, fixtureApi } = await objectTestPage(t, (call) => call.url.pathname === '/api/object/definition' ?
    { body: { database: 'shop', name: 'add_total', kind: 'procedure', generation: 7, definition, truncated: false,
      metadata: { procedure: 'add_total', 'create procedure': definition, sql_mode: '', character_set_client: 'utf8mb4', collation_connection: 'utf8mb4_general_ci', 'database collation': 'utf8mb4_general_ci' } } } : null);
  page.onDialog(() => true);
  await openFirstObjectEditor(page);
  assert.match(await page.evaluate(`document.querySelector('#object-editor-status').textContent`), /carriage returns.*normalize/i);
  assert.equal(await page.evaluate(`document.querySelector('#object-sql').readOnly`), true);
  assert.equal(await page.evaluate(`document.querySelector('#object-editor-submit').disabled`), true);
  await page.evaluate(`window.__oldCreateObjectURL = URL.createObjectURL; URL.createObjectURL = blob => { window.__originalBlob = blob; return window.__oldCreateObjectURL(blob) }; document.querySelector('#object-original-download').click()`);
  assert.equal(await page.evaluate(`window.__originalBlob.text()`), definition);
  await page.evaluate(`document.querySelector('#object-editor-cancel').click()`);
  assert.match(await page.evaluate(`document.querySelector('#query-outcome-warnings').textContent`), /normalize carriage returns/);
  assert.equal(fixtureApi.calls.filter(c => c.url.pathname === '/api/object/replace').length, 0);
});

test('mobile object filter and editor fit viewport and remain keyboard accessible', async (t) => {
  const { page } = await objectTestPage(t);
  if (process.env.DBKID_LAYOUT_SCREENSHOTS) {
    await page.setViewport(1200, 800);
    await page.screenshot(path.join(process.env.DBKID_LAYOUT_SCREENSHOTS, 'objects-page-desktop.png'));
  }
  await page.setViewport(390, 844);
  await page.evaluate(`document.querySelector('#objects-filter').value = 'nightly'; document.querySelector('#objects-filter').dispatchEvent(new Event('input', {bubbles:true}))`);
  assert.equal(await page.evaluate(`document.querySelectorAll('#objects-result .object-item:not([hidden])').length`), 1);
  await page.evaluate(`document.querySelector('#object-create-event').click()`);
  if (process.env.DBKID_LAYOUT_SCREENSHOTS) {
    await page.screenshot(path.join(process.env.DBKID_LAYOUT_SCREENSHOTS, 'objects-editor-mobile.png'));
  }
  assert.equal(await page.evaluate(`document.querySelector('#object-editor-modal').hidden`), false);
  assert.equal(await page.evaluate(`document.documentElement.scrollWidth <= innerWidth`), true);
  assert.equal(await page.evaluate(`document.activeElement.id`), 'object-name');
  await page.evaluate(`document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }))`);
  assert.equal(await page.evaluate(`document.querySelector('#object-editor-modal').hidden`), true);
});

test('the objects tab lists routines, shows definitions, and drops objects', async (t) => {
  const { fixtureApi } = await v2Fixture({});
  const page = await openTablePage(fixtureApi.url);
  t.after(async () => { await page.close(); await fixtureApi.close(); });
  const confirms = [];
  page.onDialog((dialog) => { confirms.push(dialog.message); return true; });
  await page.evaluate('document.querySelector("#tab-objects").click()');
  assert.ok(await waitFor(() => fixtureApi.calls.some((call) => call.url.pathname === '/api/routines')));
  assert.ok(await waitFor(() => page.evaluate('document.querySelectorAll("#objects-result .object-item").length === 4')),
    'routines, triggers and events must all render');
  await page.evaluate(`Array.from(document.querySelectorAll('#objects-result .object-item button'))
    .find((button) => button.textContent === 'Definition').click()`);
  assert.ok(await waitFor(() => fixtureApi.calls.some((call) => call.url.pathname === '/api/routine')));
  const routineCall = fixtureApi.calls.find((call) => call.url.pathname === '/api/routine');
  assert.equal(routineCall.url.searchParams.get('database'), V2_DATABASE);
  assert.equal(routineCall.url.searchParams.get('name'), 'add_total');
  assert.equal(routineCall.url.searchParams.get('kind'), 'procedure');
  assert.ok(await waitFor(() => page.evaluate(`document.querySelector('#definition-code').textContent.includes('CREATE PROCEDURE add_total')`)),
    'the definition modal must show the routine body');
  await page.evaluate('document.querySelector("#definition-close").click()');
  assert.equal(await page.evaluate('document.querySelector("#definition-modal").hidden'), true);
  await page.evaluate(`Array.from(document.querySelectorAll('#objects-result .object-item button'))
    .find((button) => button.textContent === 'Drop').click()`);
  assert.ok(await waitFor(() => fixtureApi.calls.some((call) => call.url.pathname === '/api/routine/drop')));
  assert.ok(confirms[0].includes('procedure') && confirms[0].includes(JSON.stringify('add_total')) && confirms[0].includes(JSON.stringify(V2_DATABASE)),
    'the drop confirmation must name the kind, object and database');
  assert.match(confirms[0], /cannot be undone/i);
  const dropCall = fixtureApi.calls.find((call) => call.url.pathname === '/api/routine/drop');
  assert.deepEqual(dropCall.body, { database: V2_DATABASE, name: 'add_total', kind: 'procedure', confirmed: true });
});

test('search runs against the database and opens matches in Browse with a filter', async (t) => {
  const { fixtureApi } = await v2Fixture({});
  const page = await openTablePage(fixtureApi.url);
  t.after(async () => { await page.close(); await fixtureApi.close(); });
  page.onDialog(() => false);
  await page.evaluate('document.querySelector("#tab-search").click()');
  await page.evaluate(`(() => {
    document.querySelector('#search-input').value = 'wallet';
    document.querySelector('#search-form').requestSubmit();
  })()`);
  assert.ok(await waitFor(() => fixtureApi.calls.some((call) => call.url.pathname === '/api/search')));
  const searchCall = fixtureApi.calls.find((call) => call.url.pathname === '/api/search');
  assert.equal(searchCall.url.searchParams.get('database'), V2_DATABASE);
  assert.equal(searchCall.url.searchParams.get('q'), 'wallet');
  assert.equal(searchCall.url.searchParams.get('mode'), 'like');
  assert.ok(await waitFor(() => page.evaluate('document.querySelectorAll("#search-result .search-table-result").length === 1')));
  assert.ok(await page.evaluate('document.querySelector("#search-result").textContent.includes("wallet")'),
    'matched rows must render through text content');
  await page.evaluate(`Array.from(document.querySelectorAll('#search-result button'))
    .find((button) => button.textContent === 'Open in Browse').click()`);
  assert.ok(await waitFor(() => {
    const rowsCalls = fixtureApi.calls.filter((call) => call.url.pathname === '/api/rows');
    return rowsCalls.length && rowsCalls[rowsCalls.length - 1].url.searchParams.get('filter') !== null;
  }), 'Open in Browse must prefill a filter and reload rows');
  const lastRowsCall = fixtureApi.calls.filter((call) => call.url.pathname === '/api/rows').pop();
  assert.deepEqual(JSON.parse(lastRowsCall.url.searchParams.get('filter')),
    [{ column: 'name', op: 'like', value: 'wallet' }],
    'the prefilled filter must target the first string column');
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#browse-panel").hidden === false')),
    'Open in Browse must switch back to the Browse tab');
});

test('the status tab shows server state and kills processes after confirmation', async (t) => {
  const { fixtureApi } = await v2Fixture({});
  const page = await openTablePage(fixtureApi.url);
  t.after(async () => { await page.close(); await fixtureApi.close(); });
  const confirms = [];
  page.onDialog((dialog) => { confirms.push(dialog.message); return true; });
  await page.evaluate('document.querySelector("#tab-status").click()');
  assert.ok(await waitFor(() => fixtureApi.calls.some((call) => call.url.pathname === '/api/status')));
  assert.ok(await waitFor(() => page.evaluate('document.querySelectorAll("#processes-result tbody tr").length === 1')));
  assert.ok(await page.evaluate('document.querySelectorAll("#variables-result tbody tr").length === 1'));
  assert.ok(await page.evaluate('document.querySelectorAll("#server-status-result tbody tr").length === 1'));
  await page.evaluate(`Array.from(document.querySelectorAll('#processes-result tbody tr button'))[0].click()`);
  assert.ok(await waitFor(() => fixtureApi.calls.some((call) => call.url.pathname === '/api/process/kill')));
  assert.ok(confirms[0].includes('process 5'), 'the kill confirmation must name the process');
  assert.match(confirms[0], /cannot be undone/i);
  const killCall = fixtureApi.calls.find((call) => call.url.pathname === '/api/process/kill');
  assert.deepEqual(killCall.body, { id: 5, confirmed: true });
  assert.match(killCall.headers['x-dbkid-request-id'] || '', /^[A-Za-z0-9_-]{22,64}$/);
  assert.ok(await waitFor(() => fixtureApi.calls.filter((call) => call.url.pathname === '/api/status').length >= 2),
    'killing a process must refresh the status view');
});

// Optional smoke check of a separately started, disposable loopback DBKid binary.
// The new browser profile creates its own empty session; no database is contacted.
if (process.env.DBKID_LIVE_URL) {
  test('embedded Go backend serves the interface and accepts its disconnect request', async (t) => {
    const url = new URL(process.env.DBKID_LIVE_URL);
    assert.equal(url.protocol, 'http:');
    assert.equal(url.hostname, '127.0.0.1');
    const page = await openPage(url.href);
    t.after(() => page.close());
    assert.ok(await waitFor(() => page.evaluate('document.querySelector("#connect-view")?.hidden === false')),
      'the real embedded interface must bootstrap');
    const result = await page.evaluate(`(async () => {
      const sessionResponse = await fetch('/api/session', { credentials: 'same-origin', cache: 'no-store' });
      const session = await sessionResponse.json();
      if (session.connected) return { unexpectedlyConnected: true };
      const response = await fetch('/api/disconnect', {
        method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': session.csrf_token }, body: '{}'
      });
      return { sessionStatus: sessionResponse.status, connected: session.connected,
        disconnectStatus: response.status, disconnected: (await response.json()).connected,
        cookieExposed: document.cookie.includes('dbkid_session=') };
    })()`);
    assert.deepEqual(result, { sessionStatus: 200, connected: false, disconnectStatus: 200, disconnected: false, cookieExposed: false });
  });
}

test('the SQL editor highlights keywords, strings, identifiers and comments', async (t) => {
  const { fixtureApi } = await v2Fixture({});
  const page = await openTablePage(fixtureApi.url);
  t.after(async () => { await page.close(); await fixtureApi.close(); });
  await waitForRowActions(page);
  await page.evaluate(`(() => {
    const editor = document.querySelector('#sql-editor');
    editor.value = "SELECT \`odd_name\` FROM items -- tail comment\\nWHERE note = 'it''s' /* block\\ncomment */ AND n = 0x1F # sharp\\nLIMIT 1.5e2";
    editor.dispatchEvent(new Event('input', { bubbles: true }));
  })()`);
  const classes = await page.evaluate(`Array.from(document.querySelectorAll('#sql-highlight > *')).map((node) =>
    node.className + ':' + node.textContent)`);
  assert.ok(classes.some((entry) => entry === 'tok-keyword:SELECT'), 'SELECT must be a keyword');
  assert.ok(classes.some((entry) => entry === 'tok-ident:`odd_name`'), 'backtick identifiers must be typed');
  assert.ok(classes.some((entry) => entry === "tok-string:'it''s'"), 'doubled quotes stay inside the string');
  assert.ok(classes.some((entry) => entry.startsWith('tok-comment:-- tail comment')), 'line comments must be typed');
  assert.ok(classes.some((entry) => entry.startsWith('tok-comment:/* block') && entry.includes('comment */')), 'block comments must be typed');
  assert.ok(classes.some((entry) => entry === 'tok-number:0x1F'), 'hex numbers must be typed');
  assert.ok(classes.some((entry) => entry === 'tok-number:1.5e2'), 'decimal exponents must be typed');
  assert.ok(!classes.some((entry) => /tok-keyword/.test(entry) && /'it''s'/.test(entry)),
    'text inside strings must never be highlighted');
  // case-insensitivity
  await page.evaluate(`(() => {
    const editor = document.querySelector('#sql-editor');
    editor.value = 'select from where';
    editor.dispatchEvent(new Event('input', { bubbles: true }));
  })()`);
  assert.equal(await page.evaluate(`document.querySelectorAll('#sql-highlight .tok-keyword').length`), 3,
    'keywords must match case-insensitively');
  // The overlay must sit behind a transparent-text textarea.
  const styles = await page.evaluate(`(() => {
    const editor = document.querySelector('#sql-editor');
    const overlay = document.querySelector('#sql-highlight');
    const editorBox = editor.getBoundingClientRect();
    const overlayBox = overlay.getBoundingClientRect();
    return {
      transparent: getComputedStyle(editor).color,
      overlayBehind: getComputedStyle(overlay).pointerEvents,
      aligned: Math.abs(editorBox.left - overlayBox.left) < 1 && Math.abs(editorBox.top - overlayBox.top) < 1,
    };
  })()`);
  assert.equal(styles.transparent, 'rgba(0, 0, 0, 0)', 'the textarea text must be transparent over the overlay');
  assert.equal(styles.overlayBehind, 'none', 'the overlay must not take pointer events');
  assert.ok(styles.aligned, 'the overlay must align with the textarea');
});

test('history recall re-highlights the editor', async (t) => {
  const { fixtureApi } = await v2Fixture({ history: [{ sql: 'SELECT 1', ok: true, duration_ms: 1 }] });
  const page = await openTablePage(fixtureApi.url);
  t.after(async () => { await page.close(); await fixtureApi.close(); });
  await waitForRowActions(page);
  await page.evaluate('document.querySelector("#tab-sql").click()');
  assert.ok(await waitFor(() => page.evaluate('document.querySelectorAll("#history-list .history-item").length === 1')),
    'the history item must render');
  await page.evaluate(`document.querySelector('#history-list .history-item').click()`);
  assert.ok(await waitFor(() => page.evaluate(`document.querySelector('#sql-editor').value === 'SELECT 1' &&
    document.querySelector('#sql-highlight .tok-keyword') !== null`)),
    'loading history must re-highlight the editor');
});

test('the structure tab lists, adds and drops foreign keys', async (t) => {
  const { fixtureApi } = await v2Fixture({
    foreignKeys: [{ name: 'fk_items_parent', columns: ['parent_id'], referenced_database: V2_DATABASE,
      referenced_table: 'parents', referenced_columns: ['id'], on_delete: 'CASCADE', on_update: 'RESTRICT' }],
  });
  const page = await openTablePage(fixtureApi.url);
  t.after(async () => { await page.close(); await fixtureApi.close(); });
  await waitForRowActions(page);
  await page.evaluate('document.querySelector("#tab-structure").click()');
  assert.ok(await waitFor(() => page.evaluate('document.querySelectorAll("#foreign-keys-result .fk-item").length === 1')),
    'the foreign key list must render');
  assert.ok(await page.evaluate(`document.querySelector('#foreign-keys-result .fk-item').textContent.includes('parents')`),
    'the referenced table must be shown');
  const confirms = [];
  page.onDialog((dialog) => { confirms.push(dialog.message); return true; });
  // Add one through the form.
  await page.evaluate('document.querySelector("#fk-add-button").click()');
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#fk-form").hidden === false')),
    'the add form must open');
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#fk-ref-table-0 option") !== null')),
    'the referenced table select must be populated');
  await page.evaluate(`(() => {
    document.querySelector('#fk-name').value = 'fk_probe';
    document.querySelector('#fk-column-0').value = 'id';
    document.querySelector('#fk-ref-table-0').value = '${V2_TABLE}';
    document.querySelector('#fk-ref-table-0').dispatchEvent(new Event('change', { bubbles: true }));
  })()`);
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#fk-ref-column-0 option") !== null')),
    'choosing a table must fetch its columns');
  await page.evaluate(`(() => {
    document.querySelector('#fk-ref-column-0').value = 'id';
    document.querySelector('#fk-on-delete').value = 'SET NULL';
    document.querySelector('#fk-form-submit').click();
  })()`);
  assert.ok(await waitFor(() => fixtureApi.calls.some((call) => call.url.pathname === '/api/foreign-key/add')),
    'the add must be submitted');
  const addCall = fixtureApi.calls.find((call) => call.url.pathname === '/api/foreign-key/add');
  assert.deepEqual(addCall.body, {
    database: V2_DATABASE, table: V2_TABLE, name: 'fk_probe', columns: ['id'],
    referenced_database: V2_DATABASE, referenced_table: V2_TABLE, referenced_columns: ['id'],
    on_delete: 'SET NULL', on_update: '', confirmed: true,
  });
  assert.match(addCall.headers['x-dbkid-request-id'] || '', /^[A-Za-z0-9_-]{22,64}$/);
  assert.ok(confirms[0].includes(JSON.stringify(V2_TABLE)) && confirms[0].includes(JSON.stringify(V2_TABLE)),
    'the confirm must name the table and referenced table');
  assert.ok(await waitFor(() => page.evaluate('document.querySelectorAll("#foreign-keys-result .fk-item").length === 2')),
    'the list must refresh after adding');
  // Drop the original one.
  await page.evaluate(`Array.from(document.querySelectorAll('#foreign-keys-result .fk-item button'))
    .find((button) => button.textContent === 'Drop').click()`);
  assert.ok(await waitFor(() => fixtureApi.calls.some((call) => call.url.pathname === '/api/foreign-key/drop')),
    'the drop must be submitted');
  const dropCall = fixtureApi.calls.find((call) => call.url.pathname === '/api/foreign-key/drop');
  assert.deepEqual(dropCall.body, { database: V2_DATABASE, table: V2_TABLE, name: 'fk_items_parent', confirmed: true });
  assert.match(confirms[1] || '', /cannot be undone/);
  assert.match(dropCall.headers['x-dbkid-request-id'] || '', /^[A-Za-z0-9_-]{22,64}$/);
  assert.ok(await waitFor(() => page.evaluate('document.querySelectorAll("#foreign-keys-result .fk-item").length === 1')),
    'the list must refresh after dropping');
});

test('the users tab lists accounts and shows grants', async (t) => {
  const { fixtureApi } = await v2Fixture({});
  const page = await openTablePage(fixtureApi.url);
  t.after(async () => { await page.close(); await fixtureApi.close(); });
  await waitForRowActions(page);
  await page.evaluate('document.querySelector("#tab-users").click()');
  assert.ok(await waitFor(() => page.evaluate('document.querySelectorAll("#users-result .fk-item").length === 2')),
    'the accounts must render');
  await page.evaluate(`Array.from(document.querySelectorAll('#users-result .fk-item button'))
    .find((button) => button.textContent === 'Grants').click()`);
  assert.ok(await waitFor(() => page.evaluate(`document.querySelector('#user-grants-modal').hidden === false &&
    document.querySelectorAll('#user-grants-result .grant-line').length === 2`)),
    'the grants modal must show the SHOW GRANTS lines');
  const grantsCall = fixtureApi.calls.find((call) => call.url.pathname === '/api/grants');
  assert.equal(grantsCall.url.searchParams.get('user'), 'admin');
  assert.equal(grantsCall.url.searchParams.get('host'), 'localhost');
});

test('user creation, password change and privilege changes confirm and carry request IDs', async (t) => {
  const { fixtureApi } = await v2Fixture({});
  const page = await openTablePage(fixtureApi.url);
  t.after(async () => { await page.close(); await fixtureApi.close(); });
  await waitForRowActions(page);
  const confirms = [];
  page.onDialog((dialog) => { confirms.push(dialog.message); return true; });
  await page.evaluate('document.querySelector("#tab-users").click()');
  assert.ok(await waitFor(() => page.evaluate('document.querySelectorAll("#users-result .fk-item").length === 2')));
  // Create user.
  await page.evaluate('document.querySelector("#user-create-button").click()');
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#user-create-modal").hidden === false')));
  await page.evaluate(`(() => {
    document.querySelector('#user-create-name').value = 'probe';
    document.querySelector('#user-create-host').value = '127.0.0.1';
    document.querySelector('#user-create-password').value = 'pw-probe-1';
    document.querySelector('#user-create-submit').click();
  })()`);
  assert.ok(await waitFor(() => fixtureApi.calls.some((call) => call.url.pathname === '/api/user/create')),
    'create must be submitted');
  const createCall = fixtureApi.calls.find((call) => call.url.pathname === '/api/user/create');
  assert.deepEqual(createCall.body, { user: 'probe', host: '127.0.0.1', password: 'pw-probe-1', confirmed: true });
  assert.match(createCall.headers['x-dbkid-request-id'] || '', /^[A-Za-z0-9_-]{22,64}$/);
  assert.ok(!confirms[0].includes('pw-probe-1'), 'the create confirm must never show the password');
  assert.ok(await waitFor(() => page.evaluate('document.querySelectorAll("#users-result .fk-item").length === 3')),
    'the account list must refresh after creating');
  // Password change.
  await page.evaluate(`Array.from(document.querySelectorAll('#users-result .fk-item button'))
    .filter((button) => button.textContent === 'Password')[0].click()`);
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#user-password-modal").hidden === false')));
  await page.evaluate(`(() => {
    document.querySelector('#user-password-value').value = 'pw-probe-2';
    document.querySelector('#user-password-submit').click();
  })()`);
  assert.ok(await waitFor(() => fixtureApi.calls.some((call) => call.url.pathname === '/api/user/password')),
    'the password change must be submitted');
  const passwordCall = fixtureApi.calls.find((call) => call.url.pathname === '/api/user/password');
  assert.deepEqual(passwordCall.body, { user: 'admin', host: 'localhost', password: 'pw-probe-2', confirmed: true });
  assert.ok(!confirms[1].includes('pw-probe-2'), 'the password confirm must never show the password');
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#user-password-modal").hidden === true')),
    'the modal must close after a successful change');
  // Grant.
  await page.evaluate(`Array.from(document.querySelectorAll('#users-result .fk-item button'))
    .filter((button) => button.textContent === 'Grant')[0].click()`);
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#privilege-modal").hidden === false && document.querySelectorAll("#privilege-checkboxes label").length === 20')),
    'the privilege modal must show the whitelist');
  await page.evaluate(`(() => {
    const modal = document.querySelector('#privilege-modal');
    modal.querySelector('#privilege-scope-select').value = 'database';
    modal.querySelector('#privilege-scope-select').dispatchEvent(new Event('change', { bubbles: true }));
    modal.querySelector('#privilege-database').value = '${V2_DATABASE}';
    Array.from(modal.querySelectorAll('#privilege-checkboxes input')).find((box) => box.value === 'INSERT').checked = true;
    modal.querySelector('#privilege-grant-option').checked = true;
    modal.querySelector('#privilege-submit').click();
  })()`);
  assert.ok(await waitFor(() => fixtureApi.calls.some((call) => call.url.pathname === '/api/grant')),
    'the grant must be submitted');
  const grantCall = fixtureApi.calls.find((call) => call.url.pathname === '/api/grant');
  assert.deepEqual(grantCall.body, {
    user: 'admin', host: 'localhost', privileges: ['SELECT', 'INSERT'],
    database: V2_DATABASE, table: '', grant_option: true, confirmed: true,
  });
  assert.match(grantCall.headers['x-dbkid-request-id'] || '', /^[A-Za-z0-9_-]{22,64}$/);
  assert.ok(confirms[2].includes('SELECT, INSERT') && confirms[2].includes('WITH GRANT OPTION'),
    'the grant confirm must spell out the privileges');
});

test('dropping a user requires typing the exact account', async (t) => {
  const { fixtureApi } = await v2Fixture({});
  const page = await openTablePage(fixtureApi.url);
  t.after(async () => { await page.close(); await fixtureApi.close(); });
  await waitForRowActions(page);
  const confirms = [];
  page.onDialog((dialog) => { confirms.push(dialog.message); return true; });
  await page.evaluate('document.querySelector("#tab-users").click()');
  assert.ok(await waitFor(() => page.evaluate('document.querySelectorAll("#users-result .fk-item").length === 2')));
  await page.evaluate(`Array.from(document.querySelectorAll('#users-result .fk-item button'))
    .find((button) => button.textContent === 'Drop').click()`);
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#user-drop-modal").hidden === false')));
  assert.equal(await page.evaluate('document.querySelector("#user-drop-submit").disabled'), true,
    'the drop button must stay disabled until the account is typed');
  await page.evaluate(`document.querySelector('#user-drop-confirm').value = 'admin';
    document.querySelector('#user-drop-confirm').dispatchEvent(new Event('input', { bubbles: true }))`);
  assert.equal(await page.evaluate('document.querySelector("#user-drop-submit").disabled'), true,
    'a wrong account must not enable the drop button');
  await page.evaluate(`document.querySelector('#user-drop-confirm').value = 'admin@localhost';
    document.querySelector('#user-drop-confirm').dispatchEvent(new Event('input', { bubbles: true }))`);
  assert.equal(await page.evaluate('document.querySelector("#user-drop-submit").disabled'), false,
    'typing the exact account must enable the drop button');
  await page.evaluate('document.querySelector("#user-drop-submit").click()');
  assert.ok(await waitFor(() => fixtureApi.calls.some((call) => call.url.pathname === '/api/user/drop')),
    'the drop must be submitted');
  const dropCall = fixtureApi.calls.find((call) => call.url.pathname === '/api/user/drop');
  assert.deepEqual(dropCall.body, { user: 'admin', host: 'localhost', confirmed: true });
  assert.match(confirms[0], /cannot be undone/);
  assert.ok(await waitFor(() => page.evaluate('document.querySelectorAll("#users-result .fk-item").length === 1')),
    'the account list must refresh after dropping');
});

test('restricted accounts see a notice and their own grants only', async (t) => {
  const { fixtureApi } = await v2Fixture({ usersRestricted: true });
  const page = await openTablePage(fixtureApi.url);
  t.after(async () => { await page.close(); await fixtureApi.close(); });
  await waitForRowActions(page);
  await page.evaluate('document.querySelector("#tab-users").click()');
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#users-restricted").hidden === false')),
    'the restricted notice must appear');
  assert.ok(await waitFor(() => page.evaluate(`document.querySelector('#my-grants').hidden === false &&
    document.querySelectorAll('#my-grants-result .grant-line').length === 1`)),
    'the connected account grants must be shown');
  const selfGrants = fixtureApi.calls.filter((call) => call.url.pathname === '/api/grants');
  assert.ok(selfGrants.length === 1 && selfGrants[0].url.searchParams.get('user') === null,
    'self grants must be requested without account parameters');
});

test('the create-table form builds a validated request and selects the new table', async (t) => {
  const { fixtureApi } = await v2Fixture({});
  const page = await openTablePage(fixtureApi.url);
  t.after(async () => { await page.close(); await fixtureApi.close(); });
  await waitForRowActions(page);
  const confirms = [];
  page.onDialog((dialog) => { confirms.push(dialog.message); return true; });
  // Open the form from the sidebar tools.
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#new-table-button") !== null')),
    'the sidebar must offer creating a table');
  await page.evaluate('document.querySelector("#new-table-button").click()');
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#table-create-modal").hidden === false')),
    'the create-table modal must open');
  assert.equal(await page.evaluate('document.querySelector("#table-create-database").value'), V2_DATABASE,
    'the selected database must be pre-chosen');
  assert.equal(await page.evaluate('document.querySelectorAll("#table-create-rows .table-create-row").length'), 1,
    'the form must start with one column row');
  assert.equal(await page.evaluate('document.querySelector(".tc-remove").disabled'), true,
    'a single column row cannot be removable');
  // Validation: empty name.
  await page.evaluate('document.querySelector("#table-create-name").value = "probe_table"; document.querySelector("#table-create-submit").click()');
  assert.equal(await page.evaluate('document.querySelector("#table-create-status").textContent'), 'Column 1: enter a name.');
  assert.ok(!fixtureApi.calls.some((call) => call.url.pathname === '/api/table/create'),
    'an invalid form must not reach the server');
  // Validation: missing type.
  await page.evaluate(`(() => {
    document.querySelector(".tc-name").value = "id";
    document.querySelector("#table-create-submit").click();
  })()`);
  assert.equal(await page.evaluate('document.querySelector("#table-create-status").textContent'), 'Column 1: choose a type.');
  // Validation: varchar without length.
  await page.evaluate(`(() => {
    document.querySelector(".tc-type").value = "varchar";
    document.querySelector("#table-create-submit").click();
  })()`);
  assert.equal(await page.evaluate('document.querySelector("#table-create-status").textContent'),
    'Column 1: this type needs a length (for example 30).');
  assert.ok(!fixtureApi.calls.some((call) => call.url.pathname === '/api/table/create'));
  // Fill both columns and submit for real.
  await page.evaluate(`(() => {
    document.querySelector(".tc-type").value = "int";
    document.querySelector(".tc-length").value = "12";
    document.querySelector(".tc-auto").checked = true;
    document.querySelector(".tc-primary").checked = true;
    document.querySelector("#table-create-add-column").click();
  })()`);
  assert.equal(await page.evaluate('document.querySelectorAll("#table-create-rows .table-create-row").length'), 2,
    'the + Column button must add a row');
  await page.evaluate(`(() => {
    const rows = document.querySelectorAll("#table-create-rows .table-create-row");
    const second = rows[rows.length - 1];
    second.querySelector(".tc-name").value = "label";
    second.querySelector(".tc-type").value = "varchar";
    second.querySelector(".tc-length").value = "30";
    second.querySelector(".tc-nullable").checked = true;
    document.querySelector("#table-create-engine").value = "innodb";
    document.querySelector("#table-create-submit").click();
  })()`);
  assert.ok(await waitFor(() => fixtureApi.calls.some((call) => call.url.pathname === '/api/table/create')),
    'the create must be submitted');
  const createCall = fixtureApi.calls.find((call) => call.url.pathname === '/api/table/create');
  assert.deepEqual(createCall.body, {
    database: V2_DATABASE, name: 'probe_table', engine: 'innodb', confirmed: true,
    columns: [
      { name: 'id', type: 'int', length: '12', decimals: '', nullable: false,
        auto_increment: true, primary: true, unique: false, default: '', default_null: false },
      { name: 'label', type: 'varchar', length: '30', decimals: '', nullable: true,
        auto_increment: false, primary: false, unique: false, default: '', default_null: false },
    ],
  });
  assert.match(createCall.headers['x-dbkid-request-id'] || '', /^[A-Za-z0-9_-]{22,64}$/);
  assert.equal(confirms[0], `Create table "probe_table" in database ${JSON.stringify(V2_DATABASE)}? This modifies the database structure.`);
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#table-create-modal").hidden === true')),
    'the modal must close after a successful create');
  assert.ok(await waitFor(() => page.evaluate(
    `Array.from(document.querySelectorAll(".table-button")).some((button) => button.dataset.table === "probe_table")`)),
    'the new table must appear in the sidebar');
});

test('the create-table form fits its dialog at desktop and mobile widths', async (t) => {
  const { fixtureApi } = await v2Fixture({});
  const page = await openTablePage(fixtureApi.url);
  t.after(async () => { await page.close(); await fixtureApi.close(); });
  await waitForRowActions(page);
  await page.evaluate('document.querySelector("#new-table-button").click()');
  await page.evaluate('document.querySelector("#table-create-add-column").click()');
  await page.evaluate(`(() => {
    document.querySelector('#table-create-name').value = 'orders';
    const rows = document.querySelectorAll('.table-create-row');
    rows[0].querySelector('.tc-name').value = 'id';
    rows[0].querySelector('.tc-type').value = 'int';
    rows[0].querySelector('.tc-primary').checked = true;
    rows[1].querySelector('.tc-name').value = 'description';
    rows[1].querySelector('.tc-type').value = 'varchar';
    rows[1].querySelector('.tc-length').value = '255';
  })()`);
  for (const width of [1200, 390, 320]) {
    await page.setViewport(width, 800);
    if (process.env.DBKID_LAYOUT_SCREENSHOTS) {
      await page.screenshot(path.join(process.env.DBKID_LAYOUT_SCREENSHOTS, `table-layout-${width}.png`));
    }
    const metrics = await page.evaluate(`(() => {
      const modal = document.querySelector('#table-create-modal .modal');
      const row = document.querySelector('.table-create-row');
      const checkbox = row.querySelector('.tc-nullable');
      return { scrollWidth: modal.scrollWidth, clientWidth: modal.clientWidth,
        rowWidth: row.getBoundingClientRect().width,
        rowScrollWidth: row.scrollWidth,
        checkboxWidth: checkbox.getBoundingClientRect().width };
    })()`);
    assert.ok(metrics.scrollWidth <= metrics.clientWidth + 1, `${width}px: dialog must not overflow horizontally: ${JSON.stringify(metrics)}`);
    assert.ok(metrics.rowScrollWidth <= metrics.rowWidth + 1, `${width}px: column controls must fit: ${JSON.stringify(metrics)}`);
    assert.ok(metrics.checkboxWidth >= 14, `${width}px: checkboxes must remain usable: ${JSON.stringify(metrics)}`);
  }
});

test('the create-table form enforces its row and column limits locally', async (t) => {
  const { fixtureApi } = await v2Fixture({});
  const page = await openTablePage(fixtureApi.url);
  t.after(async () => { await page.close(); await fixtureApi.close(); });
  await waitForRowActions(page);
  await page.evaluate('document.querySelector("#new-table-button").click()');
  assert.ok(await waitFor(() => page.evaluate('document.querySelector("#table-create-modal").hidden === false')));
  // A row can be removed once a second one exists.
  await page.evaluate('document.querySelector("#table-create-add-column").click()');
  const removeStates = await page.evaluate(
    'Array.from(document.querySelectorAll(".tc-remove")).map((button) => button.disabled)');
  assert.deepEqual(removeStates, [false, false], 'with two rows both remove buttons must work');
  await page.evaluate('document.querySelector(".tc-remove").click()');
  assert.equal(await page.evaluate('document.querySelectorAll("#table-create-rows .table-create-row").length'), 1,
    'removing a row must leave one');
  assert.equal(await page.evaluate('document.querySelector(".tc-remove").disabled'), true,
    'the last row cannot be removable');
  assert.ok(!fixtureApi.calls.some((call) => call.url.pathname === '/api/table/create'));
});
