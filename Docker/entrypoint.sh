#!/bin/bash

# -e Exit the script if any statement returns a non-true return value
# -o pipefail Exit the script if any uninitialised variable is used
set -eo pipefail
# 
shopt -s nullglob

# Set default values for environment variables
DEFAULT_APP=${DEFAULT_APP:-"/bin/sh"}

# if command starts with an option, prepend whatsbot
if [ "${1:0:1}" = '-' ] || [ "$1" = "serve" ] || [ "$1" = "admin" ] || [ "$1" = "migrate" ] || [ "$1" = "update" ]; then
    echo "Prepending $DEFAULT_APP to the command..."
    set -- $DEFAULT_APP "$@"
fi

# Start in the foreground or run the command passed as arguments to the entrypoint script
if [ "$1" = '' ]; then
    echo "Starting with sleep infinity..."
    sleep infinity
else
    echo "Running command passed as arguments to the entrypoint script..."
    echo "Command: $@"
    exec "$@"
fi
