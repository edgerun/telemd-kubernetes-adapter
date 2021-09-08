#!/usr/bin/env bash

image=edgerun/telemd-kubernetes-adapter

if [[ $1 ]]; then
	version="$1"
else
	version=$(git rev-parse --short HEAD)
fi

basetag="${image}:${version}"

# change into project root
BASE="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
PROJECT_ROOT=$(realpath "${BASE}/../")
cd $PROJECT_ROOT

# build all the images
docker build -t ${basetag}-amd64 -f build/package/telemd-kubernetes-adapter/Dockerfile.amd64 .

# # push em all
docker push ${basetag}-amd64
