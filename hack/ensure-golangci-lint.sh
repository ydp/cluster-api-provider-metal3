#!/usr/bin/env bash

# Copyright 2023 The Metal3 Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# download an url and verify the downloaded object has the same sha as
# supplied in the function call

set -eux

CAPM3_DIR="$(dirname "$(readlink -f "$0")")/.."

wget_and_verify()
{
    local url="${1:?url missing}"
    local sha256="${2:?sha256 missing}"
    local target="${3:?target missing}"
    local checksum

    declare -a args=(
        --no-verbose
        -O "${target}"
        "${url}"
    )

    wget "${args[@]}"

    checksum="$(sha256sum "${target}" | awk '{print $1;}')"
    if [[ "${checksum}" != "${sha256}" ]]; then
        if [[ "${INSECURE_SKIP_DOWNLOAD_VERIFICATION}" == "true" ]]; then
            echo >&2 "warning: ${url} binary checksum '${checksum}' differs from expected checksum '${sha256}'"
        else
            echo >&2 "fatal: ${url} binary checksum '${checksum}' differs from expected checksum '${sha256}'"
            return 1
        fi
    fi

    return 0
}

download_and_install_golangci_lint()
{
    local tmp_dir
    local bin_dir="${1:?Binary path missing}"

    tmp_dir="$(mktemp -d)"
    pushd "${tmp_dir}" || return 1

    KERNEL_OS="$(uname | tr '[:upper:]' '[:lower:]')"
    ARCH="$(uname -m | sed -e 's/x86_64/amd64/' -e 's/\(arm\)\(64\)\?.*/\1\2/' -e 's/aarch64$/arm64/')"
    GOLANGCI_LINT="golangci-lint"
    GOLANGCI_VERSION="1.54.2"
    case "${KERNEL_OS}" in
        darwin) GOLANGCI_SHA256="7b33fb1be2f26b7e3d1f3c10ce9b2b5ce6d13bb1d8468a4b2ba794f05b4445e1" ;;
        linux) GOLANGCI_SHA256="a9f14b33473c65fcfbf411ec054b53a87dbb849f4e09ee438f1ee76dbf3f3d4e" ;;
      *) 
        echo >&2 "error:${KERNEL_OS} not supported. Please obtain the binary and calculate sha256 manually."
        exit 1
        ;;
    esac
    GOLANGCI_URL="https://github.com/golangci/golangci-lint/releases/download/v${GOLANGCI_VERSION}/${GOLANGCI_LINT}-${GOLANGCI_VERSION}-${KERNEL_OS}-${ARCH}.tar.gz"
    wget_and_verify "${GOLANGCI_URL}" "${GOLANGCI_SHA256}" "${GOLANGCI_LINT}".tar.gz
    tar zxvf "${GOLANGCI_LINT}".tar.gz
    rm -f "${GOLANGCI_LINT}".tar.gz
    mkdir -p "${CAPM3_DIR}/${bin_dir}"
    mv "${GOLANGCI_LINT}-${GOLANGCI_VERSION}-${KERNEL_OS}-${ARCH}/${GOLANGCI_LINT}" "${CAPM3_DIR}/${bin_dir}/"
}

download_and_install_golangci_lint "$1"
