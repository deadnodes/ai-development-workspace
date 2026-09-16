// One-way DB -> file projection. Git commit/push belongs to the caller's workflow.
import { mkdir, writeFile, rename, unlink } from 'node:fs/promises';
import { dirname, resolve } from 'node:path';
const [productID, destination] = process.argv.slice(2);
if (!productID || !destination) {
  console.error('Usage: node scripts/export-configuration.mjs PRODUCT_ID OUTPUT.json');
  process.exit(1);
}
const base = process.env.RC_URL || 'http://127.0.0.1:8090';
const headers = {};
if (process.env.RC_TOKEN) headers.Authorization = `Bearer ${process.env.RC_TOKEN}`;
const response = await fetch(`${base}/api/products/${encodeURIComponent(productID)}/configuration`, { headers, signal: AbortSignal.timeout(30000) });
if (!response.ok) throw new Error(`Configuration export failed: HTTP ${response.status}`);
const snapshot = await response.json();
if (snapshot.authority !== 'control-plane-database' || snapshot.product_id !== productID || snapshot.schema_version !== 1) throw new Error('Unexpected configuration response');
const target = resolve(destination);
await mkdir(dirname(target), { recursive: true });
const temporary = `${target}.${process.pid}.tmp`;
try {
  await writeFile(temporary, JSON.stringify(snapshot, null, 2) + '\n', { flag: 'wx', mode: 0o600 });
  await rename(temporary, target);
} finally {
  await unlink(temporary).catch(error => { if (error.code !== 'ENOENT') throw error; });
}
console.log(`${snapshot.revision} -> ${target}`);
