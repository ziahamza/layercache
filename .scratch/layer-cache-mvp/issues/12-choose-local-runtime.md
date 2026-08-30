# Choose the local runtime and cache lifecycle

Type: grilling
Status: open
Blocked by: 04, 05, 06, 07

## Question

Should Layer Cache run as an on-demand CLI, a long-lived per-user process, a system service, protocol-specific helper processes, or a combination? Decide discovery, ports and sockets, concurrent clients, storage ownership outside disposable worktrees, access from short-lived VMs, crash recovery, offline behavior, startup, shutdown, and uninstallation on Linux and macOS.
