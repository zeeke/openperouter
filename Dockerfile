ARG FRR_IMAGE=quay.io/frrouting/frr:10.5.3

# Build grcli from source on Alpine (musl-compatible).
# grcli only needs libecoli, but meson setup evaluates the whole project
# including DPDK. We patch meson.build to skip daemon-only dependencies.
FROM ${FRR_IMAGE} AS grout-builder
ARG GROUT_VERSION=0.15.0
RUN apk add meson ninja gcc musl-dev libedit-dev libedit-static ncurses-dev \
    ncurses-static pkgconf \
    # FRR plugin build deps (autotools build of FRR subproject)
    autoconf automake libtool bison flex python3 libyang-dev json-c-dev \
    c-ares-dev protobuf-c-dev readline-dev linux-headers perl elfutils-dev \
    python3-dev libcap-dev pcre2-dev make
ADD https://github.com/DPDK/grout/archive/refs/tags/v${GROUT_VERSION}.tar.gz /tmp/grout.tar.gz
RUN tar -xzf /tmp/grout.tar.gz -C /tmp && mv /tmp/grout-${GROUT_VERSION} /grout && rm /tmp/grout.tar.gz
WORKDIR /grout

# Patch sources for musl compatibility:
# 1. Remove daemon-only deps (dpdk, libevent, libmnl, numa) from meson.build
# 2. Replace glibc-specific functions with portable equivalents
RUN sed -i \
    -e '/^dpdk_dep = dependency/,/^)/d' \
    -e '/^ev_core_dep/d' -e '/^ev_extra_dep/d' -e '/^ev_thread_dep/d' \
    -e '/^mnl_dep/d' -e '/^numa_dep/d' \
    -e '/^if dpdk_dep/,/^endif/d' \
    -e '/^grout_exe = executable/,/^)/d' \
    -e '/^executable.*fib_inject/,/^)/d' \
    -e '/^cmocka_dep/,/^endif/d' \
    meson.build \
    && sed -i 's/strerrordesc_np(errno)/strerror(errno)/;s/strerrorname_np(errno)/""/' \
    cli/exec.c \
    && sed -i '/^#include <printf.h>/i #ifdef __GLIBC__' api/printf.c \
    && echo '#endif /* __GLIBC__ */' >> api/printf.c \
    && meson setup build -Ddocs=disabled -Dfrr=enabled -Dtests=disabled \
    && ninja -C build

# Build the manager binary
FROM golang:1.25.7 AS builder

ARG GIT_COMMIT=dev
ARG GIT_BRANCH=dev
ARG TARGETOS
ARG TARGETARCH

WORKDIR $GOPATH/openperouter
RUN --mount=type=cache,target=/go/pkg/mod/ \
  --mount=type=bind,source=go.sum,target=go.sum \
  --mount=type=bind,source=go.mod,target=go.mod \
  go mod download -x

COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/
COPY operator/ operator/

RUN --mount=type=cache,target=/root/.cache/go-build \
  --mount=type=cache,target=/go/pkg/mod \
  --mount=type=bind,source=go.sum,target=go.sum \
  --mount=type=bind,source=go.mod,target=go.mod \
  --mount=type=bind,source=internal,target=internal \
  --mount=type=bind,source=api,target=api \
  --mount=type=bind,source=cmd,target=cmd \
  --mount=type=bind,source=operator,target=operator \
  CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -v -o reloader ./cmd/reloader \
  && \
  CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -v -o controller ./cmd/hostcontroller \
  && \
  CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -v -o nodemarker ./cmd/nodemarker \
  && \
  CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -v -o hostbridge ./cmd/hostbridge \
  && \
  CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -v -o operatorbinary ./operator

FROM ${FRR_IMAGE}
WORKDIR /
COPY --from=builder /go/openperouter/reloader .
COPY --from=builder /go/openperouter/controller .
COPY --from=builder /go/openperouter/hostbridge .
COPY --from=builder /go/openperouter/nodemarker .
COPY --from=builder /go/openperouter/operatorbinary ./operator
COPY operator/bindata bindata
# Copy FRR startup configuration to the default location
COPY systemdmode/frrconfig/daemons /etc/frr/daemons
COPY systemdmode/frrconfig/vtysh.conf /etc/frr/vtysh.conf
COPY systemdmode/frrconfig/frr.conf /etc/frr/frr.conf

COPY --from=grout-builder /grout/build/grcli /usr/bin/grcli
COPY --from=grout-builder /grout/build/frr/dplane_grout.so /usr/lib/frr/modules/dplane_grout.so

ENTRYPOINT ["/controller"]
