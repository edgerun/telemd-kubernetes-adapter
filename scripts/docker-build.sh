#!/usr/bin/env bash

# script to build x86 docker images for local usage.
# we're assuming that you are using a an x86 machine.

BASE="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
PROJECT_ROOT=$(realpath "${BASE}/../")

docker build -t edgerun/telemd-kubernetes-adapter -f build/package/telemd-kubernetes-adapter/Dockerfile.amd64 .
