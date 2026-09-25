#!/usr/bin/env python3
"""A stand-in for the CloudWatch agent's embedded-metric-format socket.

The real agent, with logs.metrics_collected.emf set, listens on UDP
127.0.0.1:25888 and forwards each datagram to CloudWatch Logs as one log
event. This binds the same address and appends each datagram to a file, which
is the same shape. corkd is pointed at it with CORK_EMF_ENDPOINT exactly as
production points at the agent, so the path under test is the real one: the
encoder, the socket, and the datagram.

It asserts nothing. The scenario does that against the capture, so what is
checked lives beside the rest of the assertions rather than in here. The one
thing it records itself is framing, because that cannot be recovered from the
file afterwards: each datagram is written verbatim, so a record that arrives
without its terminating newline runs into the next one rather than quietly
being fixed up here. An earlier version appended the newline itself and would
have hidden a cork that sent none -- which is the defect most worth catching,
since the agent takes each event as a line and a record it never finds the end
of is discarded with nothing said on either side.

It exits when the stop file appears, which is how the scenario takes the agent
away mid-run to show that losing it costs launches nothing. Polling for it on
the socket timeout rather than a signal keeps the whole thing to one process
with no handler to get wrong.
"""

import os
import socket
import sys

CAPTURE = sys.argv[1]
STOP = CAPTURE + ".stop"
UNFRAMED = CAPTURE + ".unframed"
ADDR = os.environ.get("E2E_EMF_ADDR", "127.0.0.1:25888")

host, _, port = ADDR.rpartition(":")

sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
# A burst of launches arrives faster than this can write. The default receive
# buffer holds a few hundred records, which is enough for the bursts the
# scenario runs, but asking for a megabyte costs nothing and means a dropped
# datagram here is never mistaken for one cork failed to send.
sock.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, 1 << 20)
sock.settimeout(1.0)
sock.bind((host, int(port)))

sys.stderr.write("emf stub: listening on %s, capturing to %s\n" % (ADDR, CAPTURE))
sys.stderr.flush()

unframed = 0
# Line buffered: the scenario reads this file while the stub is still running.
with open(CAPTURE, "a", buffering=1) as out:
    while not os.path.exists(STOP):
        try:
            data, _ = sock.recvfrom(65535)
        except socket.timeout:
            continue
        text = data.decode("utf-8", "replace")
        if not text.endswith("\n"):
            unframed += 1
            with open(UNFRAMED, "a") as bad:
                bad.write(text[:200] + "\n")
        out.write(text)

sock.close()
sys.stderr.write("emf stub: stop file present, exiting (%d unframed)\n" % unframed)
