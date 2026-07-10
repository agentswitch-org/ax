#!/usr/bin/env bash
# check-services.sh - stub deterministic health check.
# Set FIXTURE=outage to simulate an outage.
# In production, replace this script with your real check:
#   curl endpoints, parse monitoring API responses, run cli health tools, etc.
set -euo pipefail

FIXTURE="${FIXTURE:-no_issues}"
TS=$(date -u +%Y-%m-%dT%H:%M:%SZ)

case "$FIXTURE" in
  outage)
    cat <<EOF
HEALTH CHECK REPORT
timestamp: ${TS}
OUTAGE_DETECTED

service: api-gateway
status: DOWN
last_seen: ${TS}
error: connection_refused port 8080

service: database
status: DEGRADED
response_time_ms: 4823
threshold_ms: 500

service: cache
status: UP
response_time_ms: 2

service: worker-queue
status: DOWN
last_seen: ${TS}
error: health_endpoint_timeout
EOF
    ;;
  *)
    cat <<EOF
HEALTH CHECK REPORT
timestamp: ${TS}
NO_ISSUES

service: api-gateway
status: UP
response_time_ms: 12

service: database
status: UP
response_time_ms: 45

service: cache
status: UP
response_time_ms: 2

service: worker-queue
status: UP
response_time_ms: 8
EOF
    ;;
esac
