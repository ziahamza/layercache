// Linux host provisioning, Node 24. Never formats an existing image.
import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, openSync, closeSync, ftruncateSync, writeFileSync, chmodSync } from 'node:fs';
import { resolve } from 'node:path';

if (process.getuid?.() !== 0) throw new Error('Run as root');
const root = resolve(process.argv[2] ?? '');
if (!process.argv[2] || !root.endsWith('/layercache-pool') || root.split('/').length < 4) throw new Error('Supply a dedicated absolute path ending in /layercache-pool');
const image = `${root}.ext4`;
if (existsSync(root) || existsSync(image)) throw new Error('Target already exists; inspect it rather than reformatting');
const unit = execFileSync('systemd-escape', ['--path', '--suffix=mount', root], { encoding: 'utf8' }).trim();
if (existsSync(`/etc/systemd/system/${unit}`)) throw new Error('Mount unit already exists');
mkdirSync(root, { mode: 0o755 });
const fd = openSync(image, 'wx', 0o600);
try { ftruncateSync(fd, 48 * 1024 ** 3); } finally { closeSync(fd); }
execFileSync('mkfs.ext4', ['-q', '-m', '0', '-E', 'lazy_itable_init=0,lazy_journal_init=0', image], { stdio: 'inherit' });
writeFileSync(`/etc/systemd/system/${unit}`, `[Unit]\nDescription=LayerCache bounded 48 GiB filesystem\nBefore=local-fs.target\n\n[Mount]\nWhat=${image}\nWhere=${root}\nType=ext4\nOptions=loop,nodev,nosuid,noexec,discard\nTimeoutSec=60\n\n[Install]\nWantedBy=local-fs.target\n`, { flag: 'wx', mode: 0o644 });
execFileSync('systemctl', ['daemon-reload']);
execFileSync('systemctl', ['enable', '--now', unit], { stdio: 'inherit' });
writeFileSync(`${root}/.layercache-pool`, 'layercache-pool-v1\n', { flag: 'wx', mode: 0o444 });
chmodSync(root, 0o755);
console.log(`Prepared ${root}. Sparse backing file is bounded at 48 GiB; discard releases deleted extents.`);
