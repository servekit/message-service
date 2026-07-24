#!/bin/sh
set -e

echo "[entrypoint] running migrations..."
./migrate

echo "[entrypoint] starting server..."
exec ./server
