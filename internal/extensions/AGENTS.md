# Extension DOX

## Purpose

- Own trusted executable hook discovery, invocation, and Git-backed extension installation.

## Local Contracts

- Treat installed extensions as trusted code but validate manifests, paths, event names, and repository state before execution or installation.
- Accept only the currently emitted `message.received`, `harness.before`, and `harness.after` hooks; reject unknown hook names rather than silently retaining dead configuration.
- Invoke hooks through `Runner.Run` with no automatically supplied stable ID, no durable per-extension receipt, and no runner retry; an invocation can repeat when its caller retries, so consumers must make visible effects idempotent with their own keys when needed.
- Preserve local repositories and configuration on installation failure; never embed credentials or arbitrary shell interpolation.

## Child DOX Index

No child DOX files.
