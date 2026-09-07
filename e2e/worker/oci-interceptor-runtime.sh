#!/bin/sh
# Invoked by dockerd as the OCI runtime for challenge containers, mirroring the
# shape the multihost_docker ansible role deploys (its
# templates/oci_interceptor_runtime.sh.j2). Keep the two in step: the point of
# running the interceptor here at all is that the fleet exercises the runtime
# chain production runs, and the chain is what the leak lived in.
#
# The flags live HERE rather than in daemon.json's runtimeArgs. Docker
# generates its own wrapper for any runtime with a non-empty runtimeArgs, and
# that generated wrapper does not exec:
#
#     #!/bin/sh
#     /usr/local/bin/oci-interceptor --flags $@
#
# so it stays in the process tree between the containerd shim and the real
# runtime. containerd invokes the runtime through go-runc, which builds
# commands with Go's exec.CommandContext; a cancelled call SIGKILLs the direct
# child only -- that shell -- orphaning runc beneath it. For a `runc delete`
# the task is then never deleted and its containerd-shim-runc-v2 never shuts
# down, leaking ~5 MiB per occurrence until reboot.
#
# Two things not to change:
#   - Do not move the flag into daemon.json runtimeArgs.
#   - Keep "$@" quoted. Docker's generated wrapper uses a bare $@, which
#     word-splits and glob-expands its arguments.
#
# The flag itself is an accepted no-op as of oci-interceptor v0.3.0, which
# applies the read-only networking mounts unconditionally. It is passed anyway
# because production passes it: what is under test is the chain, and a flag is
# what puts a wrapper in the chain at all.
exec /usr/local/bin/oci-interceptor --oi-readonly-networking-mounts "$@"
