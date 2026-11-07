# The candidate image of dispatch://INFRA-1443: Playwright's own browser host image, with the
# few tools the Dispatch end-to-end harness and this repository's CI actions need added at
# image-build time, so that no CI job installs a browser host package from Ubuntu's apt mirror
# pool again.
#
# This file is a measurement spike. It is built by scripts/e2e/infra-1443-browser-image.sh,
# which .github/workflows/e2e.yaml's `workflow_dispatch` runs on the branch
# spike/infra-1443-browser-image, and by the `image` job of
# .github/docker/spike-infra-1443.workflow.yaml (which belongs at
# .github/workflows/spike-infra-1443.yaml; its own header says why it is not there).
# Nothing on main reads it.
#
# The base bakes the browsers and every host package they need under /ms-playwright, with
# PLAYWRIGHT_BROWSERS_PATH pointing at it, and the Playwright version is the one bun.lock pins
# (1.64.1, bun.lock:741,1967,1969). The digest is the index digest the issue names; its amd64
# child is sha256:ee15712262f017b8157421b212b12099146645f44b1bc5c0514cbbe6a229b1d0,
# 990,220,611 bytes compressed in 7 layers.
# The file store the Dispatch browser suite uploads through, which CI runs today as a
# `docker run` of chrislusf/seaweedfs:3.97 published on 127.0.0.1:18333
# (.github/workflows/ci.yaml). A job inside a container cannot reach a container the job
# started, so the binary is baked into the image below and the job starts it on that same
# address. The source is this repository's own mirror of that image, row seaweedfs-3.97-bb05d66d
# of .github/ci-images.yaml; `weed` lives at /usr/bin/weed there and is a static ELF, so it
# needs nothing else copied with it.
#
# A named stage, and never `COPY --from=<a registry reference>`:
# .github/scripts/check-ci-images.sh reads a FROM argument and does not read a COPY --from, so
# the second form would take this digest past the gate that holds every CI image to a row of
# .github/ci-images.yaml.
FROM ghcr.io/cybermonklabs/legion/ci@sha256:bb05d66d2963b1cc48073190781c3dd29e2c4c88a2ae2986bf58e38c86d89c6e AS weed

FROM mcr.microsoft.com/playwright:v1.64.1-noble@sha256:2f05f8a734509a7b3f26a912639488ee08f58a7b292d14d62271bcff9038768f

LABEL org.opencontainers.image.source=https://github.com/cybermonklabs/legion
LABEL org.opencontainers.image.description="INFRA-1443 spike: Playwright v1.64.1-noble with Postgres 18 + pgvector, weed, gh, jq, yq, python3 and flock"

# What the base image already carries, read from its own config history and then from the built
# image rather than guessed: node 26 and npm (NODE_VERSION=26, not 24), yarn, curl, wget, gpg,
# ca-certificates (so the openssl CLI too, which run-server.sh's `openssl genrsa` needs), git,
# openssh-client, zstd, `flock` (util-linux is an Ubuntu base package), a `pwuser` at
# **uid 1001, gid 1001** -- not 1000 -- and the browsers at /ms-playwright. Its apt sources
# point at azure.archive.ubuntu.com, which this build inherits.
ARG DEBIAN_FRONTEND=noninteractive

# PostgreSQL's own apt repository (apt.postgresql.org), because Ubuntu noble carries 16 and the
# harness's migration 0072 needs a `vector` extension built against the server it runs
# (packages/dispatch/e2e/harness-postgres.ts). noble-pgdg carries postgresql-18 18.6 and
# postgresql-18-pgvector 0.8.7.
ARG PG_MAJOR=18
ARG YQ_VERSION=v4.54.2
ARG GH_VERSION=2.103.3
# The uid GitHub's ubuntu-24.04 runner runs as, and the uid that owns $GITHUB_WORKSPACE and
# every bind mount a `container:` job gets. `initdb` refuses to run as root, so the suite runs
# the container as this uid -- and `initdb` refuses a uid with no passwd entry too. This base
# already answers for uid 1001: it is `pwuser`, created by the base image's own `adduser
# pwuser`. The block below therefore creates nothing and only records what holds the uid, so
# that a future base which numbers pwuser differently still leaves `--user 1001` resolvable.
ARG RUNNER_UID=1001
ARG RUNNER_GID=1001

# One layer for everything that is installed. The apt lists are removed in the same layer, so
# they never reach the pushed image.
RUN set -eux; \
    # postgresql-common's postinst runs `pg_createcluster` and so a whole `initdb` for a cluster
    # at /var/lib/postgresql/18/main that nothing here ever starts: the harness makes its own
    # under the run's temporary directory. Turning it off keeps both the build and the image
    # smaller.
    install -d /etc/postgresql-common; \
    echo 'create_main_cluster = false' > /etc/postgresql-common/createcluster.conf; \
    install -d /usr/share/postgresql-common/pgdg; \
    curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc \
      -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc; \
    echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt.postgresql.org/pub/repos/apt noble-pgdg main" \
      > /etc/apt/sources.list.d/pgdg.list; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      "postgresql-${PG_MAJOR}" \
      "postgresql-client-${PG_MAJOR}" \
      "postgresql-${PG_MAJOR}-pgvector" \
      python3 \
      util-linux \
      jq \
      # fc-list and fc-match, and the font the runner image resolves `sans-serif` to.
      # Measured: on ubuntu-24.04 `fc-match sans-serif` answers
      # `DejaVuSans.ttf: "DejaVu Sans" "Book"`, and in this base, which carries Playwright's
      # `tools` fonts and no DejaVu at all, it answers
      # `wqy-zenhei.ttc: "WenQuanYi Zen Hei" "Regular"` -- a CJK face with different metrics.
      # Every layout row of the suite measures text, and one of them
      # (agent-view.e2e.ts:922, the reasoning link's padded box) failed by 0.8 px in runs
      # 42338254048, 42339402235 and 42340825955 for exactly that reason. fonts-dejavu-core is
      # 1.1 MB and makes the two machines resolve the same family.
      fontconfig \
      fonts-dejavu-core \
      # oven-sh/setup-bun extracts Bun's release zip with @actions/tool-cache, whose
      # extractZip execs `unzip` on Linux (dist/setup/index.js, extractZipNix). Without it
      # ./.github/actions/setup-bun fails in a container before anything else runs.
      unzip \
      # gst-inspect-1.0, which Playwright's own dependency list does not install: it ships the
      # GStreamer libraries and plugins WebKit links and loads, not the command-line tools. The
      # registry assertion below is the thing that needs it.
      gstreamer1.0-tools; \
    # mikefarah's yq, not Debian's: ./.github/actions/ci-ok-report reads workflow YAML with it.
    curl -fsSL "https://github.com/mikefarah/yq/releases/download/${YQ_VERSION}/yq_linux_amd64" \
      -o /usr/local/bin/yq; \
    chmod 0755 /usr/local/bin/yq; \
    yq --version; \
    # GitHub's CLI, from its own release tarball: the Ubuntu archive carries none and GitHub's
    # apt repository would be a third apt source for one static binary.
    curl -fsSL "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_amd64.tar.gz" \
      -o /tmp/gh.tar.gz; \
    tar -xzf /tmp/gh.tar.gz -C /usr/local --strip-components=1 \
      "gh_${GH_VERSION}_linux_amd64/bin/gh"; \
    rm -f /tmp/gh.tar.gz; \
    gh --version; \
    # The runner's uid, with a home of its own, for the reason the ARG above gives. Neither
    # `groupadd` nor `useradd` is unconditional: gid 1001 is already taken in this base (the
    # build below prints by what), and a second `groupadd -g 1001` fails the build with
    # `GID '1001' already exists`. What matters is that uid 1001 resolves to a passwd entry with
    # a writable home, not that this file owns the group.
    echo "gid 1001 is held by: $(getent group "${RUNNER_GID}" || echo '<nothing>')"; \
    echo "uid 1001 is held by: $(getent passwd "${RUNNER_UID}" || echo '<nothing>')"; \
    getent group "${RUNNER_GID}" > /dev/null || groupadd -g "${RUNNER_GID}" runner; \
    getent passwd "${RUNNER_UID}" > /dev/null \
      || useradd -m -u "${RUNNER_UID}" -g "${RUNNER_GID}" -s /bin/bash runner; \
    getent passwd "${RUNNER_UID}"; \
    apt-get clean; \
    rm -rf /var/lib/apt/lists/*

# The `weed` of the stage above, in the one place a PATH lookup finds it.
COPY --from=weed /usr/bin/weed /usr/local/bin/weed

# initdb and pg_ctl live only in the version directory (the DISPATCH_E2E_PG_BIN the suite
# names); psql, pg_config and the rest are also reachable through postgresql-common's wrappers
# in /usr/bin. Appended, so nothing here shadows a runner tool of the same name.
ENV PATH="${PATH}:/usr/lib/postgresql/18/bin"

# The five GStreamer decoders WebKit plays media through, asserted in the registry rather than
# in a CI step: the image is where that belongs now. Each is `--exists`, which answers from the
# registry this very command builds, so a missing plugin fails the build with the element's own
# name. All five are already in the base: matroskademux and vp8dec from gstreamer1.0-plugins-good,
# h264parse from gstreamer1.0-plugins-bad, avdec_h264 from gstreamer1.0-libav and vorbisdec from
# gstreamer1.0-plugins-base, every one of them installed by the base image's own
# `playwright-core install-deps`. Only gst-inspect-1.0 itself had to be added
# (gstreamer1.0-tools, above).
RUN set -eux; \
    for element in matroskademux vp8dec h264parse avdec_h264 vorbisdec; do \
      gst-inspect-1.0 --exists "$element" \
        || { echo "::error::the GStreamer registry holds no $element"; exit 1; }; \
      echo "gst-inspect-1.0 --exists $element: present"; \
    done; \
    # Every tool the Dispatch end-to-end harness, run-server.sh and ./.github/actions/ci-ok-report
    # reach for, proven here so a missing one fails the build rather than a job.
    for tool in psql initdb pg_ctl pg_config python3 flock jq yq gh weed git openssl unzip tar curl node; do \
      command -v "$tool" >/dev/null || { echo "::error::the image holds no $tool"; exit 1; }; \
      printf '%-10s %s\n' "$tool" "$(command -v "$tool")"; \
    done; \
    pg_config --version; \
    python3 --version; \
    weed version
