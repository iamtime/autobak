#!/bin/sh
# Сборка стенда и прогон интеграционных тестов в Linux-контейнере.
# То же самое, что integration.ps1, но для macOS и Linux.
#
#   ./test/integration.sh                  собрать и прогнать
#   ./test/integration.sh -run TestMySQL   только совпадающие тесты
#   ./test/integration.sh shell            оболочка внутри стенда
#
# Нужен работающий docker. Тесты проверяют то, чего не проверить на
# машине разработчика: настоящие MariaDB и PostgreSQL, sshd с ключом,
# ограниченным command=, владельцев файлов и xattr.
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
ctx="$root/test/docker"
image=autobak-testbed
with_docker=${WITH_DOCKER:-1}

step() { printf '\n=== %s\n' "$1"; }

step "Проверка docker"
docker version --format '{{.Server.Version}}'

step "Кросс-компиляция под Linux"
# Бинари собираются здесь и копируются в образ готовыми: компилятор Go в
# контейнере не нужен, а проверяются ровно те бинари, которые поедут на
# настоящий сервер.
cd "$root"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags "-s -w -X main.Version=test" \
    -o "$ctx/autobak-agent" ./cmd/autobak-agent
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go test -c -tags integration -o "$ctx/integration.test" ./test/integration

step "Сборка образа"
docker build -t "$image" --build-arg "WITH_DOCKER=$with_docker" "$ctx"

step "Запуск стенда"
# Привилегии нужны dockerd внутри контейнера; без них тесты томов
# пропускаются, а не падают.
if [ "${1:-}" = "shell" ]; then
    exec docker run --rm -it -e PRIVILEGED=1 --privileged "$image" shell
fi
docker run --rm -e PRIVILEGED=1 --privileged "$image" test "$@"
