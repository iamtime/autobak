# Стенд для интеграционных тестов: Linux со всем, что бывает на реальном
# веб-сервере - MariaDB, PostgreSQL, nginx, php-fpm, sshd и данные панели.
#
# Компилятор Go сюда не ставится: и агент, и тестовый бинарь собираются
# на хосте кросс-компиляцией и копируются готовыми. Образ из-за этого
# собирается минуты, а не десятки минут, и проверяет ровно те бинари,
# которые поедут на настоящий сервер.
FROM debian:bookworm-slim

ARG WITH_DOCKER=1
ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
        mariadb-server mariadb-client \
        postgresql postgresql-client \
        nginx php-fpm php-cli \
        openssh-server openssh-client \
        sudo procps ca-certificates attr acl \
        coreutils findutils \
    && if [ "$WITH_DOCKER" = "1" ]; then \
        apt-get install -y --no-install-recommends docker.io; \
       fi \
    && rm -rf /var/lib/apt/lists/*

# Ключи хоста для sshd и разрешение входа по ключу root.
RUN mkdir -p /run/sshd /root/.ssh \
    && chmod 700 /root/.ssh \
    && ssh-keygen -A \
    && sed -i 's/^#*PermitRootLogin.*/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config \
    && sed -i 's/^#*PubkeyAuthentication.*/PubkeyAuthentication yes/' /etc/ssh/sshd_config

# Бинари, собранные на хосте.
COPY autobak-agent /usr/local/bin/autobak-agent
COPY integration.test /usr/local/bin/integration.test
RUN chmod 0755 /usr/local/bin/autobak-agent /usr/local/bin/integration.test

COPY seed.sh entrypoint.sh /usr/local/bin/
RUN chmod 0755 /usr/local/bin/seed.sh /usr/local/bin/entrypoint.sh

# Данные сервера готовятся на этапе сборки - так запуск тестов не тратит
# время на создание сайтов и наполнение баз при каждом прогоне.
RUN /usr/local/bin/seed.sh --files

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["test"]
