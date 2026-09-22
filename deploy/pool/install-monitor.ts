// Prints its plan by default. --install installs only these two named units and
// the root-owned monitor snapshot; it never edits application or ingress units.
import fs from 'node:fs';
import { execFileSync } from 'node:child_process';
import { resolve } from 'node:path';

const args = process.argv.slice(2);
if (args.length < 1 || args.length > 2 || (args[1] !== undefined && args[1] !== '--install')) throw new Error('Usage: node install-monitor.ts /absolute/protected/config.json [--install]');
const config = args[0]!;
for (const path of [config, process.execPath]) {
  if (path !== resolve(path) || !/^\/[a-zA-Z0-9_./-]+$/.test(path)) throw new Error('Use absolute paths without whitespace or systemd substitutions');
}
const unit = 'layercache-monitor';
const executable = '/usr/local/lib/layercache-monitor/monitor.ts';
const service = `[Unit]
Description=LayerCache capacity, maintenance, TLS and cache outcome checks
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=${process.execPath} ${executable} --config ${config} --state /var/lib/layercache-monitor/status.json
StateDirectory=layercache-monitor
StateDirectoryMode=0700
UMask=0077
TimeoutStartSec=900
MemoryMax=256M
CPUQuota=25%
NoNewPrivileges=true
PrivateTmp=true
PrivateDevices=true
ProtectSystem=strict
ProtectHome=read-only
ProtectKernelTunables=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
`;
const timer = `[Unit]
Description=Check LayerCache every five minutes

[Timer]
OnBootSec=2min
OnUnitActiveSec=5min
RandomizedDelaySec=30s
Persistent=true

[Install]
WantedBy=timers.target
`;
if (args[1] !== '--install') {
  console.log(`Dry run: ${unit}.service\n${service}\n${unit}.timer\n${timer}`);
} else {
  if (process.getuid?.() !== 0) throw new Error('Run installation as root');
  const metadata = fs.statSync(config);
  if (!metadata.isFile() || metadata.uid !== 0 || (metadata.mode & 0o077) !== 0) throw new Error('Monitor config must be root-owned and private');
  fs.mkdirSync('/usr/local/lib/layercache-monitor', { recursive: true, mode: 0o755 });
  fs.copyFileSync(new URL('./monitor.ts', import.meta.url), executable);
  fs.chownSync(executable, 0, 0);
  fs.chmodSync(executable, 0o644);
  fs.writeFileSync(`/etc/systemd/system/${unit}.service`, service, { mode: 0o644 });
  fs.writeFileSync(`/etc/systemd/system/${unit}.timer`, timer, { mode: 0o644 });
  execFileSync('systemctl', ['daemon-reload'], { stdio: 'inherit' });
  execFileSync('systemctl', ['enable', '--now', `${unit}.timer`], { stdio: 'inherit' });
  console.log('Monitor timer installed. Outbound alert delivery is not configured; inspect journal and enable the independent public probe.');
}
