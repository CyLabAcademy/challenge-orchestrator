# Example seccomp profiles

Starting points for challenges that need a seccomp policy other than cmgr's
default. Copy one next to your `problem.md`, edit if needed, and reference it:

```yaml
seccomp:
    profile: execstack.json
```

A top-level `seccomp` setting applies to every runtime container. Placing it
under `overrides.<host>.seccomp` applies it to that container only.

This directory contains no `problem.md`, so it is not itself a challenge and is
skipped by challenge discovery and by `cmgr test`.

## What the default policy already allows

Every Linux container gets cmgr's embedded policy (`cmgr/seccomp.json`) unless a
challenge selects otherwise. It is a broad allowlist, not a jail:

- `defaultAction` is `SCMP_ACT_ERRNO` returning `EPERM`
- 346 syscalls are allowed unconditionally, including `execve`, `open`,
  `openat`, `read`, `write`, `mmap`, and `mprotect`
- 61 more are conditional. Some are gated on a capability the container lacks,
  so `unshare`, `setns`, `mount`, `bpf`, and `perf_event_open` are **denied**
  (they require `CAP_SYS_ADMIN`, or `CAP_BPF` / `CAP_PERFMON`). Others are
  allowed but argument- or kernel-filtered: `clone` is permitted for thread and
  process creation while its namespace-creation flags are gated on
  `CAP_SYS_ADMIN`, and `ptrace` with `process_vm_readv`/`process_vm_writev` is
  allowed on kernels ≥ 4.8 (effectively always) or with `CAP_SYS_PTRACE`.
- `pivot_root`, `keyctl`, and `userfaultfd` are denied outright
- `personality` is allowed only for `UNAME26`, `ADDR_NO_RANDOMIZE`, and
  `PER_LINUX32`; other bits, notably `READ_IMPLIES_EXEC`, are denied
- `clone3` returns `ENOSYS` rather than `EPERM`, deliberately, so that glibc
  falls back to `clone` instead of failing

Most challenges need no profile at all. Reach for one only after confirming that
a syscall you need is actually denied.

## Memory-corruption challenges: what already works

This is the category that most often prompts a profile, and usually does not
need one. All of the following were verified under the default policy with no
profile applied:

| Need | Status by default |
|---|---|
| Disable ASLR — `personality(ADDR_NO_RANDOMIZE)` / `setarch -R` | works |
| Allocate RWX memory — `mmap`/`mprotect` with `PROT_EXEC` | works |
| Debug inside the container — `ptrace`, gdb | works |
| Read/write another process — `process_vm_readv`, `process_vm_writev` | works |
| Anonymous executable files — `memfd_create` | works |
| Implicitly executable pages — `personality(READ_IMPLIES_EXEC)` / `setarch -X` | **denied**, needs `execstack.json` |

Note that the first and last rows are the same syscall with different bits, and
they behave differently. The default policy permits `personality` only for
`ADDR_NO_RANDOMIZE` (`0x0040000`), `UNAME26`, and `PER_LINUX32`.
`READ_IMPLIES_EXEC` (`0x0400000`) is outside that mask and returns `EPERM`, as
does `setarch -R -X`, which combines the two into `0x0440000`.

So a challenge that shellcodes into a buffer it `mprotect`s itself, or that
wants deterministic addresses under `setarch -R`, needs no profile.

`execstack.json` is for the narrower case of a 32-bit challenge whose exploit
runs code from the heap, which requires `READ_IMPLIES_EXEC`. If the challenge can
call `mprotect` itself, or if it only needs an executable stack (`-z execstack`),
it does not need the profile. See the `execstack.json` section below for the
details, and [../execstack/](../execstack/) for a worked challenge.

## What these examples will and will not do

A profile **replaces** the default; there is no "add one rule" form. Each file
here is a full copy of `cmgr/seccomp.json` with a small delta, which is why they
are large. When cmgr's embedded policy changes, these do not follow
automatically.

The examples deliberately stay inside one boundary: they change what the
challenge process may do **to itself and within its own container**. None of
them relax the isolation between the container and the host. Widening that
second boundary is possible, and the recipes below show how, but a profile that
does it is not something to copy without understanding it — see
[Relaxing container isolation](#relaxing-container-isolation).

## Reviewing a profile

A consequence of the full-copy format: reading a profile top to bottom tells
you almost nothing, and one unwanted rule in the middle of 800 lines is easy
to miss in review. Review the *delta* instead:

```console
$ python3 profile-diff.py execstack.json
baseline: .../cmgr/seccomp.json
profile:  execstack.json

~ personality
    - SCMP_ACT_ALLOW arg0 & 0xfffffffffff9fff7 == 0 (only bits 0x60008 permitted)
    + SCMP_ACT_ALLOW arg0 & 0xffffffffffb9fff7 == 0 (only bits 0x460008 permitted)

0 syscall(s) added, 1 changed, 0 removed
```

[`profile-diff.py`](profile-diff.py) compares a profile against cmgr's
embedded default (auto-located in a cmgr checkout; pass `--baseline`
otherwise) and prints only what changed, ignoring comments and rule order.
Every `+` line is new attack surface: a clean review is a delta that matches
what the challenge's `problem.md` says it needs and nothing else. A buried
`{"names": ["unshare", "setns"], "action": "SCMP_ACT_ALLOW"}` that would be
invisible in the raw file shows up as two `+ SCMP_ACT_ALLOW` lines.

Exit codes follow `diff`: 0 means semantically identical to the baseline, 1
means differences were found, 2 means error — so it can also gate CI.

## Profiles must let the container start

runc installs the seccomp filter *before* it executes the container's command,
so a profile that denies what startup needs produces a container that never
runs. Two real failure modes:

Removing `execve` from the allowlist:

```
docker: Error response from daemon: failed to create task for container:
  ... exec /bin/true: operation not permitted
```

A minimal "only read/write/open/exit" allowlist:

```
docker: Error response from daemon: failed to create task for container:
  ... error during container init: error closing exec fds:
  get handle to /proc/thread-self/fd: fstatfs fsmount:fscontext:proc:
  operation not permitted
```

**This means the classic seccomp-jail challenge cannot be built with a container
profile.** If the puzzle is "you have RCE but no `execve`, so read the flag with
open/read/write", that filter belongs inside the challenge binary, installed at
runtime with libseccomp or `prctl(PR_SET_SECCOMP)` after the process has already
started. Use a container profile to adjust the policy a challenge runs under,
and the binary's own filter to build the puzzle.

## The profiles

Each was verified by running a container under it and probing the syscall
directly. "Before" is the embedded default.

For a complete challenge wired up with this profile end to end -- problem.md,
profile, Makefile, and solver -- see [../execstack/](../execstack/).

### `execstack.json` — widen

Widens the `personality` rule to permit `READ_IMPLIES_EXEC` alongside the bits
the default already allows. The mask is the complement of `0x460008`, so it
accepts any combination of `UNAME26`, `ADDR_NO_RANDOMIZE`, `PER_LINUX32`, and
`READ_IMPLIES_EXEC` — which covers `setarch -R -X`. An exact-match rule on
`READ_IMPLIES_EXEC` alone would not.

For older pwn challenges whose intended solve executes shellcode from the heap.
A complete worked challenge built around this profile — problem.md, profile,
Makefile, and solver — is in [../execstack/](../execstack/). Three facts about
it are easy to get wrong:

- **It is about the heap.** An executable *stack* needs only `-z execstack` at
  link time and no profile at all. `READ_IMPLIES_EXEC` is what makes the heap
  (and other readable mappings) executable.
- **32-bit only.** `set_personality_64bit()` clears `READ_IMPLIES_EXEC` on every
  `execve` of a 64-bit ELF, so a 64-bit process cannot obtain an executable heap
  this way. Design 64-bit challenges around a path that executes no
  attacker-supplied bytes.
- **The syscall alone is not enough.** `READ_IMPLIES_EXEC` is applied by the ELF
  loader at `execve`, so a process must set the personality and then re-`execve`
  (what `setarch -X` does); calling `personality()` after the heap is already
  mapped has no effect on it.

`personality(READ_IMPLIES_EXEC)`: `EPERM` before, permitted after; on a 32-bit
binary the heap then maps `rwxp`.

This defeats NX for the challenge process, which is usually the entire point of
such a challenge — but scope the profile to the challenges that need it.

### `no-ptrace.json` — narrow

Removes `ptrace`, `process_vm_readv`, `process_vm_writev`, and `kcmp` from the
allowlist.

Why narrow at all, when the default already denies the dangerous capability-gated
syscalls? Two reasons, plus one that does *not* hold up:

- **Defense in depth for an intentionally vulnerable service.** A pwn challenge
  is remote code execution by design. Once a player has code running in the
  container, the syscalls still reachable are the kernel attack surface they use
  to try to escape it. Subtracting the ones the challenge does not need shrinks
  that surface — `ptrace` and `process_vm_*` are common building blocks in
  local privilege-escalation and sandbox-escape chains. This is the broadly
  useful case.
- **Isolating players who share one container.** If a challenge is served with
  more than one user per instance, `ptrace`/`process_vm_readv` let one player
  read or hijack another player's process in the same container — their session,
  their in-progress exploit, or a per-user secret. Removing them restores that
  isolation. When every user gets their own instance, which is the common cmgr
  configuration, this does not apply.

What narrowing does **not** do: it is not a boundary between the player and the
flag. Seccomp filters every process in the container the same way, so a player
who already has code execution runs under the same policy as the challenge and,
sharing its uid, can read whatever the challenge can — including the flag file —
without any filtered syscall. Narrow to make escape harder, not to keep a secret
from the challenge's own user.

The challenge still starts after this edit, which makes it a template for
hardening generally: confirm startup does not need a syscall, then subtract it
the same way.

`ptrace(PTRACE_TRACEME)`: succeeds before, `EPERM` after.

## Relaxing container isolation

Some challenge categories need syscalls that the default denies specifically
because they widen what a player can reach beyond the container:

- **Namespace and container-escape challenges** need `unshare`, `setns`,
  `mount`, `umount2`, and `pivot_root`, which the default gates behind
  `CAP_SYS_ADMIN`. Adding an unconditional `SCMP_ACT_ALLOW` rule for them makes
  `unshare(CLONE_NEWUSER)` succeed. User namespaces are a long-standing local
  privilege-escalation surface, so this hands a player who achieves code
  execution a substantially larger kernel attack surface.
- **Side-channel and timing challenges** need `perf_event_open`, gated behind
  `CAP_SYS_ADMIN` or `CAP_PERFMON`. The host's
  `/proc/sys/kernel/perf_event_paranoid` still applies on top, so at the common
  default of `2` most measurement stays restricted regardless of the profile.
- **Heap-grooming and race challenges** sometimes want `userfaultfd`, which the
  default denies outright. Note that seccomp is only one of two gates here: with
  `/proc/sys/vm/unprivileged_userfaultfd` at `0`, which is a common default, the
  call still returns `EPERM` even after a profile permits it. Enabling it means
  changing the host sysctl as well, so a profile alone will look correct and
  silently not work.

Both are one appended rule, following the same shape as the deltas above:

```json
{
  "names": ["unshare", "setns", "mount", "umount2", "pivot_root"],
  "action": "SCMP_ACT_ALLOW"
}
```

These are documented rather than shipped as ready-to-paste files on purpose.
They should be a deliberate decision by whoever operates the challenge host, not
a default picked up by copying the nearest example. If you do need one, prefer
narrowing it with argument filters (`SCMP_CMP_MASKED_EQ` on the flags argument)
so only the specific operation the challenge requires is permitted, and prefer
placing it under `overrides.<host>` so it reaches one container rather than
every container in the challenge.
