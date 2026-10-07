const http = require('http');
const crypto = require('crypto');
const { URL } = require('url');
const { spawn } = require('child_process');

const ISSUER = process.env.MCP_ISSUER || 'https://mcp.snaptape.in';
const APP_DIR = process.env.APP_DIR || '/opt/app';
const MCP_BINARY = process.env.MCP_BINARY_PATH || '/opt/mcp/mcp-server';
const BRIDGE_HOST = process.env.MCP_BRIDGE_HOST || 'localhost';
const BRIDGE_PORT = process.env.MCP_BRIDGE_PORT || 8765;

// in-memory stores (VM restart = re-auth, fine for single-user personal use)
const clients = new Map();   // client_id -> {client_secret, redirect_uris}
const codes = new Map();     // code -> {client_id, redirect_uri, code_challenge, code_challenge_method, expires}
const tokens = new Map();    // access_token -> {client_id, expires}
const refreshTokens = new Map(); // refresh_token -> {client_id}

const CODE_TTL_MS = 60 * 1000;
const TOKEN_TTL_S = 3600;

function randToken(bytes = 32) {
  return crypto.randomBytes(bytes).toString('base64url');
}

function sendJson(res, code, obj) {
  res.writeHead(code, { 'Content-Type': 'application/json' });
  res.end(JSON.stringify(obj));
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

function runMcp(req, res) {
  const mcp = spawn('bash', ['-c',
    `git config --global --add safe.directory ${APP_DIR}; ` +
    `export APP_DIR=${APP_DIR}; ` +
    MCP_BINARY
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
  }, 10000);
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

  // Authorization endpoint — single-user, auto-approve, no login screen
  if (url.pathname === '/authorize' && req.method === 'GET') {
    const client_id = url.searchParams.get('client_id');
    const redirect_uri = url.searchParams.get('redirect_uri');
    const state = url.searchParams.get('state');
    const code_challenge = url.searchParams.get('code_challenge');
    const code_challenge_method = url.searchParams.get('code_challenge_method') || 'plain';

    const client = clients.get(client_id);
    if (!client) return sendJson(res, 400, { error: 'invalid_client' });
    if (!redirect_uri) return sendJson(res, 400, { error: 'invalid_request' });

    const code = randToken(24);
    codes.set(code, {
      client_id, redirect_uri, code_challenge, code_challenge_method,
      expires: Date.now() + CODE_TTL_MS,
    });

    const redirect = new URL(redirect_uri);
    redirect.searchParams.set('code', code);
    if (state) redirect.searchParams.set('state', state);
    res.writeHead(302, { Location: redirect.toString() });
    res.end();
    return;
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

  // MCP endpoint — requires bearer token
  if (url.pathname === '/mcp') {
    const auth = req.headers['authorization'] || '';
    const token = auth.startsWith('Bearer ') ? auth.slice(7) : null;
    const entry = token && tokens.get(token);

    if (!entry || entry.expires < Date.now()) {
      res.setHeader('WWW-Authenticate', `Bearer resource_metadata="${ISSUER}/.well-known/oauth-protected-resource"`);
      return sendJson(res, 401, { error: 'invalid_token' });
    }

    res.setHeader('Content-Type', 'application/json');
    return runMcp(req, res);
  }

  sendJson(res, 404, { error: 'not_found' });
});

server.listen(BRIDGE_PORT, BRIDGE_HOST, () => console.log(`MCP OAuth Bridge on http://${BRIDGE_HOST}:${BRIDGE_PORT} (behind nginx TLS)`));
