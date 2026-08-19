# A container for the kernel.
#
# Two things here are load-bearing rather than boilerplate, and both are about
# shutdown:
#
#   STOPSIGNAL SIGTERM plus a non-shell ENTRYPOINT means the signal reaches PID 1
#   and PID 1 is the kernel. With a shell form entrypoint the shell is PID 1, it
#   does not forward signals, and every `docker stop` becomes a SIGKILL ten
#   seconds later — which would skip the store's shutdown ordering, seglog's
#   buffered writer and the SQLite writer's drain on every single deploy.
#
#   The data directory is a VOLUME because it is not a cache. It holds the node's
#   Ed25519 identity, the effect ledger and the credential broker's encrypted
#   secrets — and the broker's key is *derived* from the identity, so a container
#   that loses identity/ turns every stored secret into ciphertext nobody can
#   open. Losing this directory is losing the evidence, not losing a cache.

FROM golang:1.25-alpine AS build
WORKDIR /src

# Dependencies first, so a source change does not re-download the module cache.
COPY kernel/go.mod kernel/go.sum ./
RUN go mod download

COPY kernel/ ./
# CGO_ENABLED=0 is what makes the binary work on a distroless base at all: the
# SQLite driver is pure Go (modernc.org/sqlite) precisely so this is possible.
# -trimpath keeps build paths out of the binary; -s -w drop the symbol table.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/aura ./cmd/aura

# An empty directory to seed /data with. It exists only to carry a mode and an
# owner into a base image that has no shell to create one with.
RUN mkdir -p /emptydata


FROM gcr.io/distroless/static-debian12:nonroot

# Distroless because the attack surface of a node that seals evidence should not
# include a shell. There is nothing here to exec into, which also means: debug
# with `aura` subcommands and the logs, not by getting a prompt.
COPY --from=build /out/aura /usr/local/bin/aura

# The data directory has to exist in the image, owned by the user that runs, and
# it has to exist *before* VOLUME.
#
# This is the line the container was missing. `VOLUME /data` on its own creates
# the mount point as root; the process runs as uid 65532, so the very first
# write failed with `persist node id: open /data/node-id: permission denied` and
# the node restarted forever. Docker seeds a fresh named volume from the image's
# directory — including its ownership — so creating it here with the right owner
# is what makes an empty volume writable. There is no shell in distroless to
# `mkdir` with, hence the copy of an empty directory built in the stage above.
COPY --from=build --chown=65532:65532 /emptydata /data

# Declared as a volume so an operator who forgets `-v` gets an anonymous volume
# rather than silently writing the ledger into the container's writable layer,
# where the next `docker rm` destroys it.
VOLUME /data

# HOME decides where every *other* `aura` subcommand looks: the CLI defaults to
# $HOME/.aura, and the node is started with --data=/data/.aura so the two agree.
#
# They did not agree before, and it was a trap with no error message. The node
# ran with --data=/data while `docker compose exec aura aura token issue` wrote
# into $HOME/.aura — a different database. The token was minted, `aura token ls`
# listed it, and the running node rejected it as unknown, because it was reading
# somewhere else entirely. Keeping the volume at /data and the store one level
# inside it is what makes "the CLI default" and "the node's directory" the same
# path.
ENV HOME=/data

EXPOSE 9080/tcp 9080/udp

# Readiness rather than liveness: this is the probe that says "send it traffic".
#
# `aura ready` reads /readyz — open precisely so a probe needs no credential —
# and turns its status code into an exit code. It replaced `aura status`, which
# was wrong here twice over: that command asks for the skill catalogue, which
# needs a token this container has no reason to hold, and it exits 0 when the
# request is refused. The check passed on a node it had just failed to
# authenticate against, which is the worst kind of green.
#
# There is no shell and no curl in this image, so a HEALTHCHECK has to be an
# `aura` subcommand. Kubernetes should use /readyz for readiness and /healthz
# for liveness — they are different questions and this node answers them
# differently.
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
  CMD ["/usr/local/bin/aura", "ready", "--port", "9080", "--quiet"]

STOPSIGNAL SIGTERM

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/aura"]
# 0.0.0.0 rather than the loopback default: inside a container, loopback means
# "unreachable from anywhere", so the bind has to widen. That is a real exposure
# decision and it is why the token is on — publish the port only where you mean
# to, and read GUIDE.md#security-model before you point it at the internet.
CMD ["up", "--listen", "0.0.0.0", "--port", "9080", "--data", "/data/.aura"]
