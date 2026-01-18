#!/bin/sh
# fake_sandbox.sh - mocks linux-sandbox for testing in restricted environments

# Skip all arguments until '--' is found
while [ $# -gt 0 ]; do
  if [ "$1" = "--" ]; then
    shift
    exec "$@"
  fi
  
  # Handle working directory (-W path)
  if [ "$1" = "-W" ] && [ $# -gt 1 ]; then
    cd "$2" || { echo "Failed to cd to $2"; exit 1; }
    shift 2
    continue
  fi

  # Handle bind mounts (-M src -m dst)
  if [ "$1" = "-M" ] && [ $# -gt 3 ] && [ "$3" = "-m" ]; then
    src="$2"
    dst="$4"
    # Emulate bind mount with copy (to avoid modifying CAS source via symlink)
    # Ensure parent directory exists
    mkdir -p "$(dirname "$dst")"
    # Copy if it doesn't exist
    if [ ! -e "$dst" ]; then
      cp -r "$src" "$dst"
    fi
    shift 4
    continue
  fi

  shift
done

echo "Error: -- delimiter not found in fake_sandbox.sh" >&2
exit 1
