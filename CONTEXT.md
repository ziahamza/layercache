# Layer Cache

Layer Cache describes how engineers reuse build artifacts across machines and trust boundaries.

## Language

**Local Cache**:
A private cache shared by builds on one engineer's machine.
_Avoid_: Local Store, local tier

**Team**:
A group of engineers sharing project access through administrator, writer, and reader roles.
_Avoid_: Organization, tenant

**Project**:
A team's repository-backed boundary for cache artifacts and build activity. The same repository can belong to distinct projects in different teams without sharing their artifacts.
_Avoid_: Workspace, cache namespace

**Team Cache**:
A private cache shared by engineers and CI within one team.
_Avoid_: Project Cache, team store, team tier

**Public Cache**:
A globally readable cache containing artifacts produced only by Public Builds.
_Avoid_: Global Cache, global tier, public store

**Public Build**:
A Layer Cache-controlled build whose outputs may be published to the Public Cache. Engineers may request a Public Build but cannot publish its artifacts themselves.
_Avoid_: Global Build, public build service

**Local Build**:
A build executed on an engineer's machine.
_Avoid_: Local run

**Workspace**:
A short-lived repository checkout and execution environment, including a Git worktree or disposable VM. A Workspace may disappear while its reusable artifacts remain cached.
_Avoid_: Agent box, runner when it is not specifically a CI runner

**Transparent Cache**:
An integration that redirects an existing tool's cache protocol to Layer Cache without changing the workload definition.
_Avoid_: Automatic cache, invisible cache
