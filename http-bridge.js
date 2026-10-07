const http = require('http');
const crypto = require('crypto');
const { URL } = require('url');
const { spawn } = require('child_process');

const ISSUER = process.env.MCP_ISSUER || 'https://mcp.snaptape.in';
const PORT = process.env.PORT || 8765;

const APP_DIR = process.env.APP_DIR;
if (!APP_DIR) {
  console.error('APP_DIR env var is required');
  process.exit(1);
}
const MCP_PASSCODE = process.env.MCP_PASSCODE;
if (!MCP_PASSCODE) {
  console.error('MCP_PASSCODE env var is required');
  process.exit(1);
}
const MCP_PASSCODE_HASH = crypto.createHash('sha256').update(MCP_PASSCODE).digest();

// in-memory stores (VM restart = re-auth, fine for single-user personal use)
const clients = new Map();   // client_id -> {client_secret, redirect_uris}
const pendingAuth = new Map(); // req_id -> {client_id, redirect_uri, state, code_challenge, code_challenge_method, expires}
const codes = new Map();     // code -> {client_id, redirect_uri, code_challenge, code_challenge_method, expires}
const tokens = new Map();    // access_token -> {client_id, expires}
const refreshTokens = new Map(); // refresh_token -> {client_id}
const loginAttempts = new Map(); // ip -> {failCount, lockUntil}

const CODE_TTL_MS = 60 * 1000;
const PENDING_AUTH_TTL_MS = 5 * 60 * 1000;
const RESUBMIT_GRACE_MS = 60 * 1000;
const TOKEN_TTL_S = 3600;
const MAX_LOGIN_FAILURES = 5;
const LOCKOUT_MS = 15 * 60 * 1000;

function randToken(bytes = 32) {
  return crypto.randomBytes(bytes).toString('base64url');
}

function sendJson(res, code, obj) {
  res.writeHead(code, { 'Content-Type': 'application/json' });
  res.end(JSON.stringify(obj));
}

function sendHtml(res, code, html) {
  res.writeHead(code, { 'Content-Type': 'text/html; charset=utf-8' });
  res.end(html);
}

function corsHeaders(res) {
  res.setHeader('Access-Control-Allow-Origin', '*');
  res.setHeader('Access-Control-Allow-Methods', 'GET, POST, OPTIONS');
  res.setHeader('Access-Control-Allow-Headers', 'Content-Type, Authorization');
  res.setHeader('Access-Control-Max-Age', '86400');
}

function readBody(req) {
  return new Promise((resolve) => {
    let body = '';
    req.on('data', (d) => (body += d));
    req.on('end', () => resolve(body));
  });
}

function parseBody(req, body) {
  const ct = req.headers['content-type'] || '';
  if (ct.includes('application/json')) {
    try { return JSON.parse(body || '{}'); } catch { return {}; }
  }
  // application/x-www-form-urlencoded
  const params = new URLSearchParams(body || '');
  return Object.fromEntries(params.entries());
}

function clientIp(req) {
  return req.headers['x-real-ip'] || req.socket.remoteAddress || 'unknown';
}

function isLockedOut(ip) {
  const entry = loginAttempts.get(ip);
  return !!(entry && entry.lockUntil && entry.lockUntil > Date.now());
}

function recordLoginFailure(ip) {
  const entry = loginAttempts.get(ip) || { failCount: 0, lockUntil: 0 };
  entry.failCount += 1;
  if (entry.failCount >= MAX_LOGIN_FAILURES) {
    entry.lockUntil = Date.now() + LOCKOUT_MS;
    entry.failCount = 0;
  }
  loginAttempts.set(ip, entry);
}

function recordLoginSuccess(ip) {
  loginAttempts.delete(ip);
}

function passcodeMatches(candidate) {
  const candidateHash = crypto.createHash('sha256').update(candidate || '').digest();
  return crypto.timingSafeEqual(candidateHash, MCP_PASSCODE_HASH);
}

function renderLoginPage({ reqId, error, locked }) {
  const message = locked
    ? '<p class="err">Too many attempts. Try again later.</p>'
    : error ? '<p class="err">Incorrect passcode.</p>' : '';
  return `<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>MCP Login</title>
<style>
body{font-family:-apple-system,BlinkMacSystemFont,sans-serif;background:#0b0b0c;color:#eee;
  display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0}
form{background:#17171a;padding:2rem;border-radius:12px;width:280px;box-shadow:0 4px 24px rgba(0,0,0,.4)}
h1{font-size:1.1rem;margin:0 0 1rem}
input[type=password]{width:100%;box-sizing:border-box;padding:.6rem;border-radius:8px;
  border:1px solid #333;background:#0b0b0c;color:#eee;font-size:1rem;margin-bottom:1rem}
button{width:100%;padding:.6rem;border-radius:8px;border:none;background:#5b8cff;
  color:#fff;font-size:1rem;cursor:pointer}
.err{color:#ff6b6b;font-size:.85rem;margin:0 0 1rem}
</style></head>
<body>
<form method="POST" action="/authorize">
<h1>Enter passcode</h1>
${message}
<input type="hidden" name="req_id" value="${reqId}">
<input type="password" name="passcode" autofocus ${locked ? 'disabled' : ''}>
<button type="submit" ${locked ? 'disabled' : ''}>Continue</button>
</form>
</body></html>`;
}

function runMcp(req, res) {
  const mcp = spawn('bash', ['-c',
    `git config --global --add safe.directory '${APP_DIR}'; ` +
    `export APP_DIR='${APP_DIR}'; ` +
    '/opt/mcp/mcp-server'
  ]);

  let output = '';
  let stderrOutput = '';
  mcp.stdout.on('data', (d) => (output += d));
  mcp.stderr.on('data', (d) => (stderrOutput += d));
  mcp.on('error', (err) => console.error('[mcp spawn error]', err));
  mcp.on('close', (code) => {
    if (stderrOutput) console.error('[mcp stderr]', stderrOutput);
    if (code !== 0) console.error('[mcp exit code]', code);
    if (!res.writableEnded) {
      if (output) res.end(output);
      else res.end();
    }
  });

  if (req.method === 'POST') {
    readBody(req).then((body) => {
      if (body) mcp.stdin.write(body + '\n');
      mcp.stdin.end();
    });
  } else {
    mcp.stdin.end();
  }

  setTimeout(() => {
    if (!res.writableEnded) {
      res.end('{"error":"timeout"}');
      mcp.kill();
    }
  }, Number(process.env.MCP_CALL_TIMEOUT_MS) || 130000);
}

function issueCodeAndRedirect(res, pending) {
  const code = randToken(24);
  codes.set(code, {
    client_id: pending.client_id,
    redirect_uri: pending.redirect_uri,
    code_challenge: pending.code_challenge,
    code_challenge_method: pending.code_challenge_method,
    expires: Date.now() + CODE_TTL_MS,
  });

  const redirect = new URL(pending.redirect_uri);
  redirect.searchParams.set('code', code);
  if (pending.state) redirect.searchParams.set('state', pending.state);
  res.writeHead(302, { Location: redirect.toString() });
  res.end();
}

const server = http.createServer(async (req, res) => {
  corsHeaders(res);
  console.error(`${req.method} ${req.url} accept=${req.headers['accept']}`);
  if (req.method === 'OPTIONS') { res.writeHead(200); res.end(); return; }

  const url = new URL(req.url, ISSUER);

  // OAuth 2.0 Authorization Server Metadata (RFC 8414)
  if (url.pathname === '/.well-known/oauth-authorization-server') {
    return sendJson(res, 200, {
      issuer: ISSUER,
      authorization_endpoint: `${ISSUER}/authorize`,
      token_endpoint: `${ISSUER}/token`,
      registration_endpoint: `${ISSUER}/register`,
      response_types_supported: ['code'],
      grant_types_supported: ['authorization_code', 'refresh_token'],
      code_challenge_methods_supported: ['S256'],
      token_endpoint_auth_methods_supported: ['none', 'client_secret_post'],
    });
  }

  // MCP protected-resource metadata (points clients at the auth server)
  if (url.pathname === '/.well-known/oauth-protected-resource') {
    return sendJson(res, 200, {
      resource: `${ISSUER}/mcp`,
      authorization_servers: [ISSUER],
    });
  }

  // Dynamic Client Registration (RFC 7591)
  if (url.pathname === '/register' && req.method === 'POST') {
    const body = parseBody(req, await readBody(req));
    const client_id = randToken(16);
    const client_secret = randToken(24);
    const redirect_uris = body.redirect_uris || [];
    clients.set(client_id, { client_secret, redirect_uris });
    return sendJson(res, 201, {
      client_id,
      client_secret,
      redirect_uris,
      token_endpoint_auth_method: 'client_secret_post',
      grant_types: ['authorization_code', 'refresh_token'],
      response_types: ['code'],
    });
  }

  // Authorization endpoint — gated behind a passcode login page
  if (url.pathname === '/authorize' && req.method === 'GET') {
    const client_id = url.searchParams.get('client_id');
    const redirect_uri = url.searchParams.get('redirect_uri');
    const state = url.searchParams.get('state');
    const code_challenge = url.searchParams.get('code_challenge');
    const code_challenge_method = url.searchParams.get('code_challenge_method') || 'plain';

    const client = clients.get(client_id);
    if (!client) return sendJson(res, 400, { error: 'invalid_client' });
    if (!redirect_uri) return sendJson(res, 400, { error: 'invalid_request' });

    const reqId = randToken(24);
    pendingAuth.set(reqId, {
      client_id, redirect_uri, state, code_challenge, code_challenge_method,
      expires: Date.now() + PENDING_AUTH_TTL_MS,
    });

    return sendHtml(res, 200, renderLoginPage({ reqId, locked: isLockedOut(clientIp(req)) }));
  }

  // Authorization endpoint — passcode submission
  if (url.pathname === '/authorize' && req.method === 'POST') {
    const ip = clientIp(req);
    const body = parseBody(req, await readBody(req));
    const pending = pendingAuth.get(body.req_id);

    if (!pending || pending.expires < Date.now()) {
      return sendHtml(res, 400, '<p>Login request expired. Close this and reconnect from the app.</p>');
    }

    // A duplicate submit of an already-accepted req_id (double-tap, or the
    // in-app browser re-posting on redirect/close) re-issues a fresh code
    // instead of erroring — the passcode was already verified once.
    if (pending.consumedAt && Date.now() - pending.consumedAt < RESUBMIT_GRACE_MS) {
      return issueCodeAndRedirect(res, pending);
    }

    if (isLockedOut(ip)) {
      return sendHtml(res, 429, renderLoginPage({ reqId: body.req_id, locked: true }));
    }

    if (!passcodeMatches(body.passcode)) {
      recordLoginFailure(ip);
      return sendHtml(res, 401, renderLoginPage({
        reqId: body.req_id, error: true, locked: isLockedOut(ip),
      }));
    }

    recordLoginSuccess(ip);
    pending.consumedAt = Date.now();
    return issueCodeAndRedirect(res, pending);
  }

  // Token endpoint
  if (url.pathname === '/token' && req.method === 'POST') {
    const body = parseBody(req, await readBody(req));

    if (body.grant_type === 'authorization_code') {
      const entry = codes.get(body.code);
      if (!entry || entry.expires < Date.now()) {
        return sendJson(res, 400, { error: 'invalid_grant' });
      }
      codes.delete(body.code);

      if (entry.code_challenge) {
        const verifier = body.code_verifier || '';
        const hash = crypto.createHash('sha256').update(verifier).digest('base64url');
        const expected = entry.code_challenge_method === 'S256' ? hash : verifier;
        if (expected !== entry.code_challenge) {
          return sendJson(res, 400, { error: 'invalid_grant', error_description: 'PKCE verification failed' });
        }
      }

      const access_token = randToken(32);
      const refresh_token = randToken(32);
      tokens.set(access_token, { client_id: entry.client_id, expires: Date.now() + TOKEN_TTL_S * 1000 });
      refreshTokens.set(refresh_token, { client_id: entry.client_id });

      return sendJson(res, 200, {
        access_token, token_type: 'Bearer', expires_in: TOKEN_TTL_S, refresh_token,
      });
    }

    if (body.grant_type === 'refresh_token') {
      const entry = refreshTokens.get(body.refresh_token);
      if (!entry) return sendJson(res, 400, { error: 'invalid_grant' });

      const access_token = randToken(32);
      tokens.set(access_token, { client_id: entry.client_id, expires: Date.now() + TOKEN_TTL_S * 1000 });
      return sendJson(res, 200, { access_token, token_type: 'Bearer', expires_in: TOKEN_TTL_S });
    }

    return sendJson(res, 400, { error: 'unsupported_grant_type' });
  }

  // MCP endpoint — requires bearer token. The connector's base URL is the
  // literal transport endpoint Claude clients POST/GET against (they don't
  // redirect based on the `resource` field in the OAuth metadata), so this
  // has to work at both "/" and "/mcp" regardless of which one was entered
  // as the connector URL.
  if (url.pathname === '/mcp' || url.pathname === '/') {
    const auth = req.headers['authorization'] || '';
    const token = auth.startsWith('Bearer ') ? auth.slice(7) : null;
    const entry = token && tokens.get(token);

    if (!entry || entry.expires < Date.now()) {
      res.setHeader('WWW-Authenticate', `Bearer resource_metadata="${ISSUER}/.well-known/oauth-protected-resource"`);
      return sendJson(res, 401, { error: 'invalid_token' });
    }

    if (req.method === 'GET') {
      // Optional server-initiated SSE stream, not implemented — per the MCP
      // Streamable HTTP spec, 405 tells the client not to expect it rather
      // than leaving the request hanging.
      res.writeHead(405, { 'Allow': 'POST' });
      return res.end();
    }

    res.setHeader('Content-Type', 'application/json');
    return runMcp(req, res);
  }

  sendJson(res, 404, { error: 'not_found' });
});

server.listen(PORT, 'localhost', () => console.log(`MCP OAuth Bridge (APP_DIR=${APP_DIR}) on http://localhost:${PORT} (behind nginx TLS)`));
