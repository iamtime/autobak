#!/bin/sh
# Планировщик агента в контейнере.
#
# Собственного демона в агенте нет намеренно: на обычном сервере
# расписанием занимается systemd, и дублировать его внутри программы
# незачем. Но в контейнере контейнер сам и есть демон, поэтому цикл
# ожидания живёт здесь - на виду, в пятнадцати строках, а не спрятанный
# в бинаре.
set -eu

CONFIG=${AUTOBAK_CONFIG:-/etc/autobak/config.json}
AT=${AUTOBAK_AT:-04:00}
JITTER=${AUTOBAK_JITTER:-1800}

log() { echo "$(date '+%Y-%m-%d %H:%M:%S') $*"; }

case "${1:-schedule}" in
    schedule) ;;
    *) exec /usr/local/bin/autobak-agent "$@" ;;
esac

if [ ! -f "$CONFIG" ]; then
    log "нет $CONFIG"
    log "положите файл конфигурации и ключ репозитория в примонтированный каталог."
    log "создать их можно в программе на компьютере: она покажет готовый config.json."
    exit 1
fi

# Docker Desktop на Windows и macOS не передаёт права POSIX через
# смонтированный каталог: файл приезжает как 0755, и chmod внутри
# контейнера на него не действует. Агент такие файлы читать отказывается,
# и правильно делает - в них ключи от хранилища.
#
# Этот переключатель переносит конфигурацию и ключ в приватный каталог
# внутри контейнера. Защита не исчезает, а меняет предмет: внутри
# контейнера файлы доступны только root. За права на хостовый каталог
# отвечаете вы, и chmod 700 на нём остаётся обязательным.
#
# На Linux-сервере это не нужно: там права передаются как есть.
if [ "${AUTOBAK_PRIVATE_COPY:-0}" = "1" ]; then
    priv=/run/autobak
    mkdir -p "$priv"
    chmod 700 "$priv"

    key=$(sed -n 's/.*"key_file"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$CONFIG" | head -1)
    if [ -n "$key" ] && [ -f "$key" ]; then
        cp "$key" "$priv/key"
        chmod 600 "$priv/key"
    fi
    sed 's#"key_file"[[:space:]]*:[[:space:]]*"[^"]*"#"key_file": "/run/autobak/key"#' \
        "$CONFIG" > "$priv/config.json"
    chmod 600 "$priv/config.json"
    CONFIG="$priv/config.json"

    log "конфигурация скопирована в $priv (AUTOBAK_PRIVATE_COPY=1)"
    log "права на смонтированном каталоге при этом не проверяются -"
    log "убедитесь, что на хосте стоит chmod 700"
fi

log "проверка настроек..."
/usr/local/bin/autobak-agent backup --config "$CONFIG" --dry-run

if [ "${AUTOBAK_RUN_NOW:-0}" = "1" ]; then
    log "первый бэкап сразу по требованию AUTOBAK_RUN_NOW"
    /usr/local/bin/autobak-agent backup --config "$CONFIG" || log "бэкап завершился с ошибкой"
fi

log "расписание: ежедневно в $AT (разброс до ${JITTER}с)"

while true; do
    now=$(date +%s)
    # Ближайшее наступление указанного времени.
    target=$(date -d "today $AT" +%s 2>/dev/null || echo "")
    if [ -z "$target" ] || [ "$target" -le "$now" ]; then
        target=$(date -d "tomorrow $AT" +%s)
    fi
    # Разброс: если серверов несколько и все пишут в одно хранилище,
    # одновременный старт упрётся в ограничения по числу запросов.
    if [ "$JITTER" -gt 0 ]; then
        target=$((target + $(od -An -N2 -tu2 < /dev/urandom | tr -d ' ') % JITTER))
    fi
    sleep_for=$((target - now))
    log "следующий запуск через $((sleep_for / 60)) мин"
    sleep "$sleep_for"

    log "бэкап начат"
    if /usr/local/bin/autobak-agent backup --config "$CONFIG"; then
        log "бэкап завершён"
    else
        # Контейнер не падает из-за одного неудачного бэкапа: завтра
        # попробуем снова, а о сбое сообщит сам агент кодом возврата
        # и записью в журнал, которую видно в docker logs.
        log "бэкап завершился с ошибкой (код $?)"
    fi
done
