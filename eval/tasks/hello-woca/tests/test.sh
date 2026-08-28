#!/bin/bash
set -euo pipefail
test "$(cat /app/greeting.txt)" = "woca-was-here"
