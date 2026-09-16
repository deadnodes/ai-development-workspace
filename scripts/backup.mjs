// Transport binary gzip directly; never send private archive bytes through agent text.
import { readFile, writeFile, mkdir } from 'node:fs/promises';
import { dirname, resolve } from 'node:path';
import { createHash } from 'node:crypto';
const [mode, filename] = process.argv.slice(2);
if (!['export', 'restore'].includes(mode) || !filename) {
  console.error('Usage: RC_URL=... RC_ACTOR=agent/name node scripts/backup.mjs export|restore ARCHIVE.rcp.json.gz');
  process.exit(1);
}
const endpoint = process.env.RC_URL || 'http://127.0.0.1:8090';
const headers = { 'X-RCP-Actor': process.env.RC_ACTOR || 'agent/backup' };
if (process.env.RC_TOKEN) headers.Authorization = `Bearer ${process.env.RC_TOKEN}`;
const target = resolve(filename);
const hash = bytes => createHash('sha256').update(bytes).digest('hex');
if (mode === 'export') {
  const response = await fetch(`${endpoint}/api/backups/export`, { method: 'POST', headers, signal: AbortSignal.timeout(120000) });
  if (!response.ok) throw new Error(`Export failed: HTTP ${response.status}`);
  const archive = Buffer.from(await response.arrayBuffer());
  const digest = response.headers.get('X-Backup-SHA256');
  if (archive.length > 8 * 1024 * 1024 || hash(archive) !== digest) throw new Error('Invalid backup size or checksum');
  await mkdir(dirname(target), { recursive: true });
  // New filenames only: existing backups must not be overwritten accidentally.
  await writeFile(target, archive, { flag: 'wx', mode: 0o600 });
  await writeFile(`${target}.sha256`, `${digest}\n`, { flag: 'wx', mode: 0o600 });
  console.log(`Saved ${archive.length} compressed bytes; SHA-256 ${digest}`);
} else {
  const archive = await readFile(target);
  const expected = (await readFile(`${target}.sha256`, 'utf8')).trim();
  if (archive.length > 8 * 1024 * 1024 || hash(archive) !== expected) throw new Error('Backup checksum/size mismatch');
  const response = await fetch(`${endpoint}/api/backups/restore`, {
    method: 'POST', headers: { ...headers, 'Content-Type': 'application/gzip', 'X-Backup-SHA256': expected },
    body: archive, signal: AbortSignal.timeout(120000),
  });
  const result = await response.json();
  if (!response.ok) throw new Error(`Restore failed (${response.status}): ${result.error}`);
  console.log(JSON.stringify(result));
}
