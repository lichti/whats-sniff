#!/bin/bash

# -e Exit the script if any statement returns a non-true return value
# -o pipefail Exit the script if any uninitialised variable is used
set -eo pipefail

DEFAULT_APP_ARGS=${DEFAULT_APP_ARGS:-""}


/app/pocketbase $DEFAULT_APP_ARGS migrate 
/app/pocketbase $DEFAULT_APP_ARGS $@