#!/usr/bin/env python3
"""Show the semantic difference between a seccomp profile and a baseline.

Docker applies a seccomp profile as a whole-policy replacement, so every
custom profile is a full copy of cmgr's embedded default with a small edit.
Reading one top to bottom therefore tells you almost nothing, and one
unwanted rule in the middle of 800 lines is easy to miss in review. This
tool prints only what actually differs from the baseline.

Usage:
    python3 profile-diff.py PROFILE [--baseline BASELINE]

The baseline defaults to cmgr's embedded policy (cmgr/seccomp.json),
auto-located when run inside a cmgr checkout. Outside a checkout, pass
--baseline explicitly.

Exit codes: 0 = no semantic difference, 1 = differences found, 2 = error.
"""

import argparse
import json
import os
import sys


def die(message):
    print(f"error: {message}", file=sys.stderr)
    sys.exit(2)


def load(path):
    try:
        with open(path, encoding="utf-8") as handle:
            return json.load(handle)
    except OSError as err:
        die(f"cannot read {path}: {err}")
    except json.JSONDecodeError as err:
        die(f"{path} is not valid JSON: {err}")


def canonical_args(args):
    """Order-independent, comment-free canonical form of a rule's args."""
    canon = []
    for arg in args or []:
        canon.append(
            json.dumps(
                {key: arg[key] for key in sorted(arg) if key != "comment"},
                sort_keys=True,
            )
        )
    return sorted(canon)


def rule_context(rule):
    """Everything that decides when a rule applies and what it does.

    Names are handled separately and comments are cosmetic, so neither is
    part of the context.
    """
    return json.dumps(
        {
            "action": rule.get("action"),
            "errnoRet": rule.get("errnoRet"),
            "args": canonical_args(rule.get("args")),
            "includes": rule.get("includes") or {},
            "excludes": rule.get("excludes") or {},
        },
        sort_keys=True,
    )


def syscall_map(profile):
    """Map each syscall name to the set of contexts it appears under."""
    result = {}
    for rule in profile.get("syscalls") or []:
        names = rule.get("names") or ([rule["name"]] if rule.get("name") else [])
        context = rule_context(rule)
        for name in names:
            result.setdefault(name, set()).add(context)
    return result


def describe_gate(prefix, gate):
    parts = []
    if gate.get("caps"):
        parts.append("caps " + "+".join(gate["caps"]))
    if gate.get("minKernel"):
        parts.append("kernel>=" + str(gate["minKernel"]))
    if gate.get("arches"):
        parts.append("arch " + ",".join(gate["arches"]))
    return f"{prefix}({'; '.join(parts)})" if parts else ""


def describe(context_json):
    context = json.loads(context_json)
    parts = [context.get("action") or "(no action)"]
    if context.get("errnoRet") is not None:
        parts.append(f"errnoRet={context['errnoRet']}")
    for arg_json in context["args"]:
        arg = json.loads(arg_json)
        index = arg.get("index", 0)
        op = arg.get("op", "?")
        value = arg.get("value", 0)
        value_two = arg.get("valueTwo", 0)
        if op == "SCMP_CMP_MASKED_EQ" and value_two == 0:
            permitted = ~value & 0xFFFFFFFFFFFFFFFF
            parts.append(
                f"arg{index} & {value:#x} == 0 (only bits {permitted:#x} permitted)"
            )
        else:
            rendered = f"arg{index} {op} {value:#x}"
            if value_two:
                rendered += f",{value_two:#x}"
            parts.append(rendered)
    for prefix, key in (("if", "includes"), ("unless", "excludes")):
        gate = describe_gate(prefix, context.get(key) or {})
        if gate:
            parts.append(gate)
    return " ".join(parts)


def find_default_baseline():
    # The embedded policy from a cmgr checkout is authoritative; default.json
    # beside this script is its test-enforced identical copy, shipped with the
    # examples so the tool also works outside a checkout.
    script_dir = os.path.dirname(os.path.abspath(__file__))
    candidates = [
        os.path.join(script_dir, "..", "..", "cmgr", "seccomp.json"),
        os.path.join(script_dir, "default.json"),
        os.path.join(os.getcwd(), "cmgr", "seccomp.json"),
    ]
    for candidate in candidates:
        if os.path.isfile(candidate):
            return os.path.normpath(candidate)
    return None


def main():
    parser = argparse.ArgumentParser(
        description="semantic diff of a seccomp profile against a baseline"
    )
    parser.add_argument("profile", help="profile JSON to inspect")
    parser.add_argument(
        "--baseline",
        help="baseline profile JSON (default: cmgr/seccomp.json in a cmgr checkout)",
    )
    options = parser.parse_args()

    baseline_path = options.baseline or find_default_baseline()
    if baseline_path is None:
        die(
            "could not locate cmgr/seccomp.json automatically; "
            "pass --baseline <path to the embedded default policy>"
        )

    baseline = load(baseline_path)
    profile = load(options.profile)

    print(f"baseline: {baseline_path}")
    print(f"profile:  {options.profile}")
    print()

    differences = 0

    for field in ("defaultAction", "defaultErrnoRet"):
        if baseline.get(field) != profile.get(field):
            differences += 1
            print(f"~ {field}: {baseline.get(field)!r} -> {profile.get(field)!r}")

    base_arch = json.dumps(baseline.get("archMap"), sort_keys=True)
    prof_arch = json.dumps(profile.get("archMap"), sort_keys=True)
    if base_arch != prof_arch:
        differences += 1
        print("~ archMap differs (inspect manually)")

    base_map = syscall_map(baseline)
    prof_map = syscall_map(profile)

    added = sorted(set(prof_map) - set(base_map))
    removed = sorted(set(base_map) - set(prof_map))
    changed = sorted(
        name
        for name in set(base_map) & set(prof_map)
        if base_map[name] != prof_map[name]
    )

    # Added rules first: new attack surface is the dangerous direction.
    for name in added:
        print(f"+ {name}")
        for context in sorted(prof_map[name]):
            print(f"    + {describe(context)}")
    for name in changed:
        print(f"~ {name}")
        for context in sorted(base_map[name] - prof_map[name]):
            print(f"    - {describe(context)}")
        for context in sorted(prof_map[name] - base_map[name]):
            print(f"    + {describe(context)}")
    for name in removed:
        print(f"- {name}")
        for context in sorted(base_map[name]):
            print(f"    was: {describe(context)}")

    differences += len(added) + len(changed) + len(removed)
    if differences == 0:
        print("no semantic differences")
        return 0
    print()
    print(
        f"{len(added)} syscall(s) added, {len(changed)} changed, "
        f"{len(removed)} removed"
    )
    return 1


if __name__ == "__main__":
    sys.exit(main())
