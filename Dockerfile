# Grout + FRR build.
# CentOS Stream 9 is ABI-compatible with RHEL 9 / UBI9 and provides the full
# set of -devel packages needed by grout that are absent from UBI repos.
FROM quay.io/centos/centos:stream9 AS grout-builder

ARG GROUT_VERSION=main

# Enable CRB repo for -devel packages and install build dependencies.
# Use pip for meson to ensure a recent enough version (grout requires gcc 13+ and recent meson).
RUN dnf install -y --nodocs 'dnf-command(config-manager)' && \
    dnf config-manager --set-enabled crb && \
    dnf install -y --nodocs \
      gcc-toolset-13-gcc \
      gcc-toolset-13-gcc-c++ \
      git \
      make \
      ninja-build \
      pkgconf \
      python3-pip \
      python3-pyelftools \
      libarchive-devel \
      libedit-devel \
      libevent-devel \
      libmnl-devel \
      libsmartcols-devel \
      numactl-devel \
      rdma-core-devel \
      autoconf \
      automake \
      libtool \
      texinfo \
      bison \
      flex \
      file \
      json-c-devel \
      libyang-devel \
      readline-devel \
      c-ares-devel \
      pcre2-devel \
      protobuf-c-devel \
      elfutils-libelf-devel \
      libcap-devel \
      python3-devel \
      python3-sphinx \
    && pip3 install --no-cache-dir meson \
    && dnf clean all

RUN git clone --depth 1 --branch ${GROUT_VERSION} https://github.com/DPDK/grout.git /build/grout

WORKDIR /build/grout

# Build grout with FRR enabled and DPDK statically linked.
# Grout's meson build compiles FRR via autotools and installs it to
# build/frr_install. We run meson install for grout itself, then merge the
# FRR install tree into the same staging directory.
RUN source /opt/rh/gcc-toolset-13/enable && \
    meson setup build \
      --buildtype=release \
      --prefix=/usr \
      --sysconfdir=/etc \
      -Ddpdk:platform=generic \
      -Dfrr=enabled \
      -Ddpdk_static=true && \
    ninja -C build && \
    DESTDIR=/build/install meson install -C build && \
    mkdir -p /build/install/usr/sbin /build/install/usr/lib64/frr/modules && \
    cp -a build/frr_install/sbin/* /build/install/usr/sbin/ && \
    cp -a build/frr_install/bin/*  /build/install/usr/bin/ && \
    cp -a build/frr_install/lib/lib*.so* /build/install/usr/lib64/ && \
    cp -a build/frr_install/lib64/frr/modules/* /build/install/usr/lib64/frr/modules/ && \
    cp -a build/frr_install/etc/* /build/install/etc/ && \
    ldconfig -n /build/install/usr/lib64

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

FROM registry.access.redhat.com/ubi9-minimal:9.4

# Install runtime dependencies needed by grout and FRR.
RUN microdnf install -y --nodocs \
      libevent \
      libmnl \
      numactl-libs \
      libedit \
      json-c \
      readline \
      libcap \
      protobuf-c \
    && microdnf clean all

WORKDIR /

# Copy grout and FRR binaries from the grout build
COPY --from=grout-builder /build/install/ /

# Copy runtime libraries not available in UBI9-minimal repos
COPY --from=grout-builder /usr/lib64/libyang*.so* /usr/lib64/
COPY --from=grout-builder /usr/lib64/libcares*.so* /usr/lib64/
COPY --from=grout-builder /usr/lib64/libibverbs*.so* /usr/lib64/
COPY --from=grout-builder /usr/lib64/libmlx5*.so* /usr/lib64/
COPY --from=grout-builder /usr/lib64/libnl-route-3*.so* /usr/lib64/
COPY --from=grout-builder /usr/lib64/libnl-3*.so* /usr/lib64/
RUN ldconfig

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

ENTRYPOINT ["/controller"]
