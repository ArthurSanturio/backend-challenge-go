#!/usr/bin/env bash
set -euo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.."

test_project="wallet-system-tests-$$-${RANDOM}"
compose=(docker compose -p "$test_project" -f compose.integration.yaml)
test_directory=$(mktemp -d)
cleanup() {
  "${compose[@]}" unpause postgres >/dev/null 2>&1 || true
  "${compose[@]}" down --volumes --remove-orphans || true
  rm -rf -- "$test_directory"
}
trap cleanup EXIT

go build -race -o "$test_directory/api" ./cmd/api
"${compose[@]}" up -d --wait --wait-timeout 180
postgres_address=$("${compose[@]}" port postgres 5432)
keycloak_address=$("${compose[@]}" port keycloak 8080)
sqs_address=$("${compose[@]}" port ministack 4566)
export POSTGRES_TEST_URL="postgres://challenge:integration_local_only@${postgres_address}/http_integration?sslmode=disable"
export POSTGRES_TEST_CONTAINER
POSTGRES_TEST_CONTAINER=$("${compose[@]}" ps -q postgres)
export OIDC_TEST_ISSUER_URL="http://${keycloak_address}/realms/wallet"
export OIDC_TEST_JWKS_URL="${OIDC_TEST_ISSUER_URL}/protocol/openid-connect/certs"
export SQS_TEST_ENDPOINT_URL="http://${sqs_address}"
# The bootstrap identity is emulator-only; do not inherit a host AWS session.
unset AWS_SESSION_TOKEN AWS_PROFILE AWS_SHARED_CREDENTIALS_FILE AWS_CONFIG_FILE
export AWS_ACCESS_KEY_ID=test
export AWS_SECRET_ACCESS_KEY=test
export AWS_EC2_METADATA_DISABLED=true
export API_TEST_BINARY="$test_directory/api"

go test -race -count=1 -tags=integration ./... -timeout=300s "$@"
go vet -tags=integration ./...
