#!/usr/bin/python3
import argparse
import socket

parser = argparse.ArgumentParser(description="solve script for 'Executable Heap'")
parser.add_argument("--host", default="challenge", help="the host for the instance")
parser.add_argument("--port", type=int, default=5000, help="the port of the instance")
parser.add_argument(
    "--print",
    action="store_true",
    help="print flag to stdout rather than saving to file",
)
args = parser.parse_args()

c = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
c.connect((args.host, args.port))
c.settimeout(10)

data = b""
try:
    while b"flag:" not in data:
        chunk = c.recv(4096)
        if not chunk:
            break
        data += chunk
except socket.timeout:
    pass
text = data.decode(errors="replace")

fields = {}
for line in text.splitlines():
    if ":" in line:
        key, _, value = line.partition(":")
        fields[key.strip()] = value.strip()

# The service degrades gracefully when the seccomp profile is not applied: it
# still returns a flag, but reports a non-executable heap. Requiring "yes" here
# makes the solve fail fast and legibly if the profile is missing, instead of
# passing on a challenge whose intended exploit path was actually closed.
if fields.get("heap executable") != "yes":
    print(f"error: heap is not executable; the seccomp profile is not applied: {text!r}")
    exit(-1)

flag = fields.get("flag")
if not flag:
    print(f"error: challenge did not serve a flag: {text!r}")
    exit(-1)

if args.print:
    print(f"flag: {flag}")
else:
    with open("flag", "w") as f:
        f.write(flag)
