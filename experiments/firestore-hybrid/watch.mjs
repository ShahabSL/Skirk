// Test-only owner-authorized fixture. All Google I/O remains in the Go bridge.
import { readFile, writeFile, stat, access } from 'node:fs/promises';
import { dirname, join } from 'node:path';
import { startObserver } from '../firestore-listen/observer.mjs';

export async function startHybridWatch(config, blob, fetch = globalThis.fetch) {
  if (config.hybrid_research !== true || !Number.isSafeInteger(config.duration_ms) || config.duration_ms < 1000 || config.duration_ms > 300000 || !Number.isSafeInteger(config.request_limit) || config.request_limit < 1 || config.request_limit > 256 || !Number.isSafeInteger(config.event_limit) || config.event_limit < 1 || config.event_limit > 512) throw new Error('explicit bounded hybrid watch required');
  return startObserver(config, blob, fetch);
}

async function main() {
  if (process.env.SKIRK_FIRESTORE_HYBRID_WATCH !== '1') throw new Error('explicit hybrid observer opt-in required');
  const path = process.argv[2];
  const info = await stat(path);
  if (!info.isFile() || (info.mode & 0o077) !== 0 || info.size > 32768) throw new Error('private hybrid observer configuration required');
  const config = JSON.parse(await readFile(path, 'utf8'));
  const observer = await startHybridWatch(config, await readFile(config.webchannel_blob));
  const stopped = () => access(join(dirname(path), 'stop-hybrid-watch.json')).then(() => true, () => false);
  const start = Date.now();
  try {
    while (!observer.state().stopped && !observer.state().error && Date.now() - start < config.duration_ms) {
      if (await stopped()) break;
      await new Promise(resolve => setTimeout(resolve, 25));
    }
  } finally {
    await observer.close();
    await writeFile(join(dirname(path), 'hybrid-watch-result.json'), JSON.stringify(observer.state()) + '\n', { mode: 0o600, flag: 'wx' });
  }
  if (observer.state().error) process.exitCode = 1;
}

if (process.argv[1] && new URL(import.meta.url).pathname === process.argv[1]) main().catch(() => {
  process.stderr.write('bounded Firestore hybrid watch failed\n');
  process.exitCode = 1;
});
