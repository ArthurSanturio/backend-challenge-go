#!/usr/bin/env bash
set -euo pipefail
# Mantém o comando antigo e usa a preparação completa das integrações atuais.
exec "$(dirname -- "${BASH_SOURCE[0]}")/test-system.sh" "$@"
