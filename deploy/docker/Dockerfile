# Агент AutoBak в контейнере.
#
# Смысл - развернуть бэкапы на сервере, где не хотят ставить ничего
# в систему: положил compose-файл, docker compose up -d, и всё.
#
# Клиенты баз и docker CLI нужны потому, что сборщики вызывают именно их:
# mysqldump снимает консистентный срез, pg_dump знает про роли, docker
# рассказывает про тома и compose-стеки.

# Клиент docker берётся из официального образа, а не из пакета docker.io.
# Пакет тянет весь движок - containerd, dockerd, runc, shim'ы - это
# четверть гигабайта того, что в контейнере никогда не запустится.
# Нужен ровно один файл.
FROM docker:27-cli AS dockercli

FROM debian:bookworm-slim

ARG TARGETARCH=amd64
ARG WITH_DOCKER=1
# Клиент PostgreSQL ставится из репозитория PGDG, а не из Debian.
# В bookworm лежит версия 15, а pg_dump отказывается выгружать сервер
# новее себя: с ним базы PostgreSQL 16 и 17 просто не попали бы в бэкап.
# Клиент 17 умеет выгружать всё начиная с 9.2.
ARG PG_MAJOR=17
ENV DEBIAN_FRONTEND=noninteractive

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        ca-certificates curl gnupg tzdata mariadb-client; \
    install -d /usr/share/postgresql-common/pgdg; \
    curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc \
        -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc; \
    echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] \
https://apt.postgresql.org/pub/repos/apt bookworm-pgdg main" \
        > /etc/apt/sources.list.d/pgdg.list; \
    apt-get update; \
    apt-get install -y --no-install-recommends "postgresql-client-${PG_MAJOR}"; \
    apt-get purge -y curl gnupg; \
    apt-get autoremove -y; \
    rm -rf /var/lib/apt/lists/*

# Один файл вместо пакета с движком. Собран статически, зависимостей нет.
COPY --from=dockercli /usr/local/bin/docker /usr/local/bin/docker
RUN if [ "$WITH_DOCKER" != "1" ]; then rm -f /usr/local/bin/docker; fi

# Бинарь собирается на хосте кросс-компиляцией и кладётся рядом с этим
# файлом. Компилятор Go в образ не тянем: он весит больше, чем всё
# остальное вместе взятое, а нужен ровно один раз.
COPY autobak-agent-linux-${TARGETARCH} /usr/local/bin/autobak-agent
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod 0755 /usr/local/bin/autobak-agent /usr/local/bin/entrypoint.sh

# Каталог с конфигурацией и ключом репозитория. Монтируется снаружи;
# внутри образа его быть не должно.
VOLUME ["/etc/autobak"]

# Проверка живости через сухой прогон: он открывает хранилище и читает
# индекс, то есть подтверждает, что доступы и ключ на месте. Настоящий
# бэкап для проверки живости запускать нельзя.
HEALTHCHECK --interval=30m --timeout=60s --start-period=30s \
    CMD /usr/local/bin/autobak-agent backup --config /etc/autobak/config.json --dry-run || exit 1

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
