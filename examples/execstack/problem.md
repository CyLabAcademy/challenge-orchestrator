# Executable Heap

- Namespace: cmgr/examples
- Type: remote-make
- Category: Binary Exploitation
- Points: 50

## Description

This is a demonstration of the per-challenge `seccomp` option rather than a
full exploitation challenge. It is built around one older-style
memory-corruption need — running code from the heap — which requires the
`READ_IMPLIES_EXEC` process personality. cmgr's default seccomp policy permits
`personality` in its query form and for a few benign values, but denies the
`READ_IMPLIES_EXEC` bit specifically, so the challenge selects a profile that
widens that one rule. The observable result is the heap becoming executable,
after which the service serves the flag.

## Details

Connect with `nc {{server}} {{port}}`.

This example exists to show the `seccomp` challenge option below, and the full
wiring an author needs when a challenge requires a syscall value the default
policy denies. It is deliberately small: the service probes whether its heap is
executable and reports the result — it does not implement a heap-shellcode
exploit. Point an author here for the wiring, not for an exploit to study.
The source file {{url_for('execstack.c', 'execstack.c')}} is written to be
read: its comments record two facts that are easy to get wrong.

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
`heap executable: no` and a warning naming the denied `READ_IMPLIES_EXEC` bit.
With the profile it prints `heap executable: yes`. The solver requires `yes`,
so a missing profile fails the solve legibly instead of quietly passing.

## Challenge Options

```yaml
seccomp:
    profile: execstack.json
```

## Solution Overview

The heap needs to be executable, which requires the `READ_IMPLIES_EXEC`
personality bit that cmgr's default policy denies. Selecting the bundled
`execstack.json` profile permits it; the program re-execs itself so the loader
applies the personality, the heap becomes executable, and the flag is served.
The solver connects, confirms the heap is executable, and reads the flag.

## Learning Objective

By the end of this challenge, authors should understand how to supply a custom
per-challenge seccomp profile for challenges that need a syscall value the
default policy denies, and the 32-bit constraint on an executable heap via
`READ_IMPLIES_EXEC`.

## Tags

- example
- seccomp
- binex

## Attributes

- author: cmgr examples
- organization: picoCTF
