# Executable Heap

- Namespace: cmgr/examples
- Type: remote-make
- Category: Binary Exploitation
- Points: 50

## Description

This challenge is an older 32-bit binary exploitation service whose intended
solve executes shellcode from the heap. That needs the `READ_IMPLIES_EXEC`
process personality, which the challenge requests at startup. cmgr's default
seccomp policy blocks the `personality` syscall, so the challenge selects a
seccomp profile that permits it.

You can connect to the problem at `nc {{server}} {{port}}`.

## Details

This example exists to show the `seccomp` challenge option below, and the full
wiring an author needs when a challenge requires a syscall the default policy
denies. `execstack.c` is written to be read: its comments record two facts that
are easy to get wrong.

- `READ_IMPLIES_EXEC` is applied by the ELF loader at `execve`, not when the
  syscall returns. Calling `personality()` from inside a running process is too
  late — the heap was already mapped. The program therefore sets the
  personality and re-`execve`s itself once; this is exactly what `setarch -X`
  does from the outside.
- It only works for **32-bit** binaries. `set_personality_64bit()` clears
  `READ_IMPLIES_EXEC` on every `execve` of a 64-bit ELF, so a 64-bit process
  cannot get an executable heap this way. (An executable *stack* is a different
  thing and needs only `-z execstack`, on either bitness.)

The wiring itself:

- `execstack.json` sits directly beside `problem.md`. It is cmgr's default
  policy with the `personality` rule widened to also allow `READ_IMPLIES_EXEC`.
- The `seccomp` option below selects it. Because the profile lives in the
  challenge directory it is part of the challenge's source checksum, so editing
  it causes `cmgr update` to rebuild.

The service reports whether its heap is executable and then serves the flag.
Without the profile it degrades rather than becoming a dead socket: it prints
`heap executable: no` and a warning explaining that the sandbox blocked
`personality`. With the profile it prints `heap executable: yes`. The solver
requires `yes`, so a missing profile fails the solve legibly instead of quietly
passing on a challenge whose exploit path was closed.

## Challenge Options

```yaml
seccomp:
    profile: execstack.json
```

## Solution Overview

The service needs `READ_IMPLIES_EXEC`, which requires the `personality` syscall
that cmgr's default policy denies. Selecting the bundled `execstack.json`
profile permits it; the program re-execs itself so the loader applies the
personality, the heap becomes executable, and the flag is served. The solver
connects, confirms the heap is executable, and reads the flag.

## Learning Objective

By the end of this challenge, authors should understand how to supply a custom
per-challenge seccomp profile for challenges that need a syscall the default
policy denies, and the 32-bit constraint on an executable heap via
`READ_IMPLIES_EXEC`.

## Tags

- example
- seccomp
- binex

## Attributes

- author: cmgr examples
- organization: picoCTF
