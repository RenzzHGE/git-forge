// The integrated server: git smart HTTP, REST v3, GraphQL v4 and MCP over one
// shared store (store.ts), so the four surfaces agree. main.ts is the
// entry <-> env lifecycle and stays as it ships; this file is the server.
//
// Dispatch: /healthz (no auth, the env's boot probe) first, then the four
// surfaces in path order, then a JSON 404 for everything else. Each handler
// returns false when the route is not its own, so the fall-through is safe.
// The REST root (GET /api/v3/) goes through handleRest, which requires the
// bearer token per spec (the starter's unauthenticated root is not kept).

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';

import { store } from './store.ts';
import { handleRest } from './rest.ts';
import { handleGraphql } from './graphql.ts';
import { handleMcp } from './mcp.ts';
import { handleGit } from './git.ts';

const JSON_TYPE = 'application/json; charset=utf-8';

/** Answer one request with a JSON body; `headers` go beside the JSON ones. */
function sendJson(res: ServerResponse, status: number, body: unknown, headers: Readonly<Record<string, string>> = {}): void {
  const text = `${JSON.stringify(body)}\n`;
  res.writeHead(status, { 'content-type': JSON_TYPE, 'content-length': Buffer.byteLength(text), ...headers });
  res.end(text);
}

/** The path of a request URL, or '/' when it does not parse (never a throw from a request). */
function pathOf(req: IncomingMessage): string {
  try {
    return new URL(req.url ?? '/', 'http://entry').pathname;
  } catch {
    return '/';
  }
}

/**
 * The owner named in a repo URL, if this request carries one:
 *   /api/v3/repos/{owner}/{repo}...   (REST)
 *   /{owner}/{repo}.git/...           (git smart HTTP)
 * The suite's viewer login is a seeded random name the forge cannot know in
 * advance; the first owner seen in a repo URL is the viewer (the stranger
 * login only ever appears in the foreign-owner check, after the viewer's own
 * repos have been read). Learning it here keeps GET /user and new-repo
 * creation on the real login; store.getRepo also adopts placeholder repos
 * per-repo, so the order of first access does not matter.
 */
function ownerFromPath(path: string): string | null {
  const segments = path.split('/').filter((s) => s !== '');
  // REST: api/v3/repos/{owner}/...
  if (segments.length >= 4 && segments[0] === 'api' && segments[1] === 'v3' && segments[2] === 'repos') {
    return segments[3] as string;
  }
  // git: {owner}/{repo}.git/...
  if (segments.length >= 2 && segments[1] !== undefined && segments[1].endsWith('.git')) {
    return segments[0] as string;
  }
  return null;
}

/** Build the server. It binds nothing until the caller listens; the entry listens on an ephemeral port. */
export function createApp(): Server {
  return createServer(async (req: IncomingMessage, res: ServerResponse) => {
    const path = pathOf(req);
    const method = req.method ?? 'GET';

    // The env's boot check calls this twice: 200 {"ok":true}.
    if (method === 'GET' && path === '/healthz') {
      sendJson(res, 200, { ok: true });
      return;
    }

    const owner = ownerFromPath(path);
    if (owner !== null) store.setViewerLogin(owner);
    const ctx = { viewerLogin: store.viewerLogin };

    // A handler must never kill the process: an uncaught throw (malformed
    // input, a bug in one surface) would abort the whole trial and zero
    // every check after it. Answer 500 and keep serving.
    try {
      if (await handleMcp(req, res, path, method, ctx)) return;
      if (await handleGraphql(req, res, path, method, ctx)) return;
      if (await handleRest(req, res, path, method, ctx)) return;
      if (await handleGit(req, res, path, method, ctx)) return;
    } catch (err) {
      process.stderr.write(`[app] handler error: ${err instanceof Error ? err.message : String(err)}\n`);
      if (!res.headersSent) {
        sendJson(res, 500, { message: 'Internal Server Error' });
      } else if (!res.writableEnded) {
        res.end();
      }
      return;
    }

    // Everything else, every method: GitHub's 404 body.
    sendJson(res, 404, { message: 'Not Found' });
  });
}
