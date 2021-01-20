#!/bin/bash

OPERATOR_VERSION=$(./bin/manager --version)
SEMVER=$(echo "${OPERATOR_VERSION}" | cut -d ' ' -f4)

if [ "$SEMVER" != "$IMG_TAG" ]; then
    echo "Operator binary reports incorrect version, expected: [$IMG_TAG] reported: [$SEMVER]"
    exit 1
fi