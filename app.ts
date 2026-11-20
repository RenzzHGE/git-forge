// The integrated server: git smart HTTP, REST v3, GraphQL v4 and MCP over one
// shared store, served by the Go binary (forge-go) vendored beside this file.
//
// main.ts is the entry <-> env lifecycle and stays as it ships; this file is
// the server. It keeps main.ts's contract exactly: createApp() returns an
// object with .listen(0, cb), .address() -> {port}, .close() and
// .once('error', handler).
//
// The Go binary binds its own ephemeral port and prints `PORT <n>` as its
// first stdout line; the shim reports that port as the server's address, so
// main.ts registers the Go port directly and all traffic goes straight to Go
// (no Node proxy hop). The binary's stdin is a pipe the shim holds open:
// when the parent exits the pipe closes and the binary shuts itself down.
// close() ends stdin and SIGTERMs the child.

import { spawn, type ChildProcess } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

/** The minimal server interface main.ts uses. */
export interface ForgeServer {
  listen(port: number, cb: () => void): void;
  address(): { port: number } | string | null;
  close(): void;
  once(event: 'error', handler: (err: Error) => void): void;
}

/** Build the server. It binds nothing until the caller listens; the entry listens on an ephemeral port. */
export function createApp(): ForgeServer {
  let child: ChildProcess | null = null;
  let boundPort = 0;
  let errorHandler: ((err: Error) => void) | null = null;
  let settled = false;
  let stdoutBuf = '';

  const fail = (err: Error): void => {
    if (settled) return;
    settled = true;
    if (errorHandler !== null) errorHandler(err);
    else process.stderr.write(`[app] forge-go failed: ${err.message}\n`);
  };

  const server: ForgeServer = {
    once(event, handler): void {
      if (event === 'error') errorHandler = handler;
    },

    listen(_port, cb): void {
      const bin = join(dirname(fileURLToPath(import.meta.url)), 'forge-go');
      child = spawn(bin, [], { stdio: ['pipe', 'pipe', 'inherit'] });
      child.on('error', (err: Error) => fail(err));
      child.on('exit', (code: number | null, signal: string | null) => {
        if (!settled) fail(new Error(`forge-go exited before binding (code ${code}, signal ${signal})`));
      });
      child.stdout?.on('data', (chunk: Buffer) => {
        stdoutBuf += chunk.toString('utf8');
        const nl = stdoutBuf.indexOf('\n');
        if (nl === -1) return;
        const line = stdoutBuf.slice(0, nl).trim();
        stdoutBuf = stdoutBuf.slice(nl + 1);
        const m = /^PORT (\d+)$/.exec(line);
        if (m === null) {
          fail(new Error(`forge-go printed an unexpected first line: ${JSON.stringify(line)}`));
          return;
        }
        boundPort = Number(m[1]);
        settled = true;
        cb();
      });
    },

    address(): { port: number } | null {
      return boundPort === 0 ? null : { port: boundPort };
    },

    close(): void {
      if (child !== null) {
        try {
          child.stdin?.end();
        } catch {
          // stdin already closed; the child is going away regardless.
        }
        child.kill('SIGTERM');
        child = null;
      }
    },
  };
  return server;
}
