#!/bin/sh
# Запуск служб стенда и прогон тестов.
set -eu

log() { printf '\033[36m%s\033[0m\n' "$*"; }
ok()  { printf '\033[32m%s\033[0m\n' "$*"; }
bad() { printf '\033[31m%s\033[0m\n' "$*"; }

wait_for() {
    what=$1; shift
    i=0
    while [ $i -lt 60 ]; do
        if "$@" >/dev/null 2>&1; then
            ok "  $what готов"
            return 0
        fi
        i=$((i + 1))
        sleep 0.5
    done
    bad "  $what не поднялся за 30 с"
    return 1
}

log "Запуск служб..."

# MariaDB. mariadbd-safe уходит в фон сам; ждём отклика через сокет.
mkdir -p /run/mysqld && chown mysql:mysql /run/mysqld
mariadbd-safe --skip-syslog >/var/log/mariadb-start.log 2>&1 &
wait_for MariaDB mariadb -e "SELECT 1"

# PostgreSQL: кластер создаётся при установке пакета, надо только запустить.
pg_ctlcluster 15 main start >/dev/null 2>&1 || service postgresql start >/dev/null 2>&1 || true
wait_for PostgreSQL su -s /bin/sh postgres -c "psql -Atc 'SELECT 1'"

# php-fpm и nginx нужны не для работы, а чтобы их конфигурации и сокеты
# существовали: сборщик configs забирает именно их.
(service php8.2-fpm start || service php-fpm start) >/dev/null 2>&1 || true
nginx -t >/dev/null 2>&1 && service nginx start >/dev/null 2>&1 || true

service ssh start >/dev/null 2>&1 || /usr/sbin/sshd
wait_for sshd sh -c "ss -ltn 2>/dev/null | grep -q ':22 ' || netstat -ltn 2>/dev/null | grep -q ':22 '"

# Docker внутри контейнера поднимается только с --privileged. Без него
# тесты docker-сборщика просто пропустятся - это отражено в отчёте.
if command -v dockerd >/dev/null 2>&1; then
    if [ -w /sys/fs/cgroup ] || [ "${PRIVILEGED:-}" = "1" ]; then
        dockerd >/var/log/dockerd.log 2>&1 &
        if wait_for Docker docker info; then :; else
            bad "  docker не поднялся - тесты тома будут пропущены"
        fi
    else
        log "  docker пропущен: контейнер запущен без --privileged"
    fi
fi

log "Наполнение данными..."
/usr/local/bin/seed.sh --data

case "${1:-test}" in
    test)
        shift 2>/dev/null || true
        log ""
        log "════════ Интеграционные тесты ════════"
        # -test.count=1 отключает кэш: результаты должны отражать
        # текущее состояние стенда, а не прошлый запуск.
        exec /usr/local/bin/integration.test -test.v -test.count=1 \
             -test.timeout=20m "$@"
        ;;
    shell)
        exec /bin/bash
        ;;
    *)
        exec "$@"
        ;;
esac
