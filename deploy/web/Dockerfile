# Веб-интерфейс AutoBak.
#
# Это вариант для отдельной машины, которая занимается только бэкапами:
# развернул контейнер, зашёл браузером, добавил серверы. Ядро то же
# самое, что в оконной версии, поэтому и возможности те же.
#
# Собирается из исходников целиком, чтобы разворачивалось одной командой
# без подготовки бинарей на стороне.

FROM golang:1.26-bookworm AS build
WORKDIR /src

# Зависимости отдельным слоем: они меняются куда реже кода, и при правке
# исходников их не приходится выкачивать заново.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG TARGETARCH=amd64
# CGO выключен: иначе бинарь окажется привязан к glibc сборочного образа
# и не запустится в лёгком финальном.
RUN set -eux; \
    CGO_ENABLED=0 GOOS=linux GOARCH="${TARGETARCH}" \
      go build -trimpath -ldflags "-s -w -X main.Version=${VERSION}" \
      -o /out/autobak-web ./cmd/autobak-web; \
    CGO_ENABLED=0 GOOS=linux GOARCH="${TARGETARCH}" \
      go build -trimpath -ldflags "-s -w -X main.Version=${VERSION}" \
      -o /out/autobak-agent ./cmd/autobak-agent

FROM debian:bookworm-slim

# openssh-client - единственная обязательная зависимость: подключение к
# серверам идёт через настоящий ssh, а не через свою реализацию протокола.
# Так работают ~/.ssh/config, агент ключей и сертификаты хоста.
#
# Клиенты баз нужны для восстановления напрямую с этой машины: снимает
# дамп агент на своей стороне, но залить его обратно можно и отсюда.
ARG PG_MAJOR=17
ENV DEBIAN_FRONTEND=noninteractive
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        ca-certificates openssh-client tzdata mariadb-client curl gnupg; \
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

COPY --from=build /out/autobak-web /usr/local/bin/autobak-web
# Агент для Linux лежит рядом намеренно: при добавлении сервера его нужно
# залить на ту машину, и брать его удобнее отсюда же - версия гарантированно
# совпадает с версией интерфейса.
COPY --from=build /out/autobak-agent /usr/local/share/autobak/autobak-agent-linux
COPY deploy/web/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod 0755 /usr/local/bin/autobak-web /usr/local/bin/entrypoint.sh

# Работает не от root. Через этот интерфейс идёт восстановление на чужие
# серверы, и лишние права на своей машине ему ни к чему.
RUN useradd --system --create-home --home-dir /var/lib/autobak --shell /usr/sbin/nologin autobak
WORKDIR /var/lib/autobak

# Настройки, ключи от репозиториев и SSH-ключи. Всё состояние здесь:
# потеряете этот том - потеряете доступ к бэкапам.
ENV AUTOBAK_DATA=/var/lib/autobak/config
VOLUME ["/var/lib/autobak"]

# По умолчанию слушаем на всех адресах: внутри контейнера иначе никак,
# наружу его выставляет уже проброс порта в compose.
ENV AUTOBAK_ADDR=0.0.0.0:8080
EXPOSE 8080

# Проверка живости - попытка подключиться к своему же порту. Запускать
# для этого что-то содержательное нельзя: любая настоящая операция здесь
# ходит по SSH на чужие серверы.
HEALTHCHECK --interval=1m --timeout=5s --start-period=10s \
    CMD bash -c 'exec 3<>/dev/tcp/127.0.0.1/${AUTOBAK_ADDR##*:}' || exit 1

USER autobak
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
