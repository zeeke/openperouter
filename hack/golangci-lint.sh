#!/bin/bash
set -o errexit
set -x

CONTAINER_ENGINE=${CONTAINER_ENGINE:-docker}
GOLANGCI_LINT_CACHE=${GOLANGCI_LINT_CACHE:-$HOME/.cache/golangci-lint}
GOLANGCI_LINT_VERSION="${GOLANGCI_LINT_VERSION:-2.9.0}"

TIMEOUT="10m0s"
ENV="${ENV:-container}"

function _run() {
	if [ "$ENV" == "container" ]; then
	     local -r local_build_cache_dir="/.cache/go-build"
	     local -r local_mod_cache_dir="/.cache/mod"
	     local -r local_lint_cache_dir="/.cache/golangci-lint"
	     $CONTAINER_ENGINE run --rm \
			-u "$(id -u)":"$(id -g)" \
			-e GOCACHE="$local_build_cache_dir" \
			-e GOMODCACHE="$local_mod_cache_dir" \
			-e GOLANGCI_LINT_CACHE="$local_lint_cache_dir" \
			-v "$(go env GOCACHE)":"$local_build_cache_dir":Z \
			-v "$(go env GOMODCACHE)":"$local_mod_cache_dir":Z \
			-v "$GOLANGCI_LINT_CACHE":"$local_lint_cache_dir":Z \
			-v "$(git rev-parse --show-toplevel)":/app:Z \
			-w /app \
			docker.io/golangci/golangci-lint:v"$GOLANGCI_LINT_VERSION" \
			"$@"
	else
		curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(go env GOPATH)/bin v"$GOLANGCI_LINT_VERSION"
		$@
	fi
}

function build() {
	_run golangci-lint custom
}

function run() {
	_run bin/golangci-lint-custom run --timeout $TIMEOUT ./...
}

$@
