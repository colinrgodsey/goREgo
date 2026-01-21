#!/bin/bash

set -e

CONTAINER_NAME=ghcr.io/colinrgodsey/base-worker:latest

docker build -t $CONTAINER_NAME .

docker push $CONTAINER_NAME