#!/bin/sh
# Запуск веб-интерфейса AutoBak в контейнере.
#
# Задача точки входа - подготовить то, что нельзя зашить в образ:
# каталог состояния, права на него и ключ SSH, которым интерфейс ходит
# на серверы. Дальше работает сам сервер.
set -eu

DATA="${AUTOBAK_DATA:-/var/lib/autobak/config}"
SSH_DIR="${HOME:-/var/lib/autobak}/.ssh"

mkdir -p "$DATA" "$SSH_DIR"
# 0700 обязательны: в каталоге лежат ключи от репозиториев, а ssh просто
# откажется работать с домашним каталогом, открытым для чтения другим.
chmod 0700 "$DATA" "$SSH_DIR" 2>/dev/null || true

# Ключ SSH создаётся один раз и остаётся в томе. Публичную часть
# печатаем в журнал при первом запуске: её нужно разложить по серверам,
# и искать её потом в томе неудобно.
if [ ! -f "$SSH_DIR/id_ed25519" ]; then
    ssh-keygen -t ed25519 -N "" -C "autobak@$(hostname)" -f "$SSH_DIR/id_ed25519" >/dev/null
    echo "autobak: создан ключ SSH. Публичная часть:"
    echo
    cat "$SSH_DIR/id_ed25519.pub"
    echo
    echo "autobak: добавьте её в ~/.ssh/authorized_keys на серверах,"
    echo "autobak: которые будете бэкапить (интерфейс подскажет, с какими ограничениями)."
fi

if [ -n "${AUTOBAK_PASSWORD:-}" ] && [ "${#AUTOBAK_PASSWORD}" -lt 8 ]; then
    echo "autobak: AUTOBAK_PASSWORD короче 8 символов - через этот интерфейс"
    echo "autobak: доступно восстановление на любой сервер, так нельзя." >&2
    exit 1
fi

exec /usr/local/bin/autobak-web "$@"
