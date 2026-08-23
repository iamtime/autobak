#!/bin/sh
# Наполнение стенда: сайт, данные панели, конфигурации, базы.
#
# --files выполняется при сборке образа (службы ещё не запущены),
# --data   - при старте контейнера, когда MariaDB и PostgreSQL готовы.
set -eu

seed_files() {
    # Сайт как у HestiaCP: /home/<юзер>/web/<домен>/public_html
    id -u admin >/dev/null 2>&1 || useradd -m -s /bin/bash admin
    root=/home/admin/web/shop.ru
    mkdir -p "$root/public_html/wp-content/uploads" \
             "$root/public_html/wp-content/cache" \
             "$root/private" "$root/logs"

    cat > "$root/public_html/index.php" <<'PHP'
<?php
require __DIR__ . '/wp-config.php';
echo 'магазин работает';
PHP
    cat > "$root/public_html/wp-config.php" <<'PHP'
<?php
define('DB_NAME', 'admin_shop');
define('DB_USER', 'admin_shop');
define('DB_PASSWORD', 'пароль-от-базы');
PHP
    # Файлы разного размера: мелкие текстовые и покрупнее двоичные -
    # чтобы чанкер работал в обоих режимах.
    for i in 1 2 3 4 5 6 7 8; do
        head -c 300000 /dev/urandom > "$root/public_html/wp-content/uploads/img$i.bin"
    done
    # То, что обязано быть исключено по умолчанию.
    head -c 2000000 /dev/urandom > "$root/public_html/wp-content/cache/junk.tmp"
    echo "куча бесполезных строк" > "$root/logs/access.log"
    ln -sf public_html "$root/current"

    chown -R admin:admin "$root"
    chmod 0750 "$root/private"
    echo "секрет" > "$root/private/notes.txt"
    chmod 0600 "$root/private/notes.txt"

    # Данные панели в том формате, который читает автообнаружение.
    mkdir -p /usr/local/hestia/conf /usr/local/hestia/data/users/admin /usr/local/hestia/ssl
    cat > /usr/local/hestia/conf/hestia.conf <<'CONF'
VERSION='1.8.11'
WEB_SYSTEM='nginx'
DB_SYSTEM='mysql'
CONF
    cat > /usr/local/hestia/data/users/admin/user.conf <<'CONF'
NAME='admin' PACKAGE='default' WEB_DOMAINS='1' DATABASES='1' SUSPENDED='no'
CONF
    cat > /usr/local/hestia/data/users/admin/web.conf <<'CONF'
DOMAIN='shop.ru' IP='203.0.113.10' ALIAS='www.shop.ru' SSL='yes' BACKEND_TEMPLATE='PHP-8_2' SUSPENDED='no'
CONF
    cat > /usr/local/hestia/data/users/admin/db.conf <<'CONF'
DB='admin_shop' DBUSER='admin_shop' HOST='localhost' TYPE='mysql' CHARSET='utf8mb4'
CONF

    # Конфигурация nginx для того же сайта: автообнаружение без панели
    # опирается на неё, а модуль configs забирает её целиком.
    cat > /etc/nginx/sites-available/shop.ru <<'CONF'
server {
    listen 80;
    server_name shop.ru www.shop.ru;
    root /home/admin/web/shop.ru/public_html;
    index index.php;
    location ~ \.php$ {
        fastcgi_pass unix:/run/php/php-fpm.sock;
    }
}
CONF
    mkdir -p /etc/nginx/sites-enabled
    ln -sf /etc/nginx/sites-available/shop.ru /etc/nginx/sites-enabled/shop.ru

    # Ключ, ограниченный вызовом только агента, - как на боевом сервере.
    if [ ! -f /root/.ssh/autobak_test ]; then
        ssh-keygen -t ed25519 -N '' -C autobak-test -f /root/.ssh/autobak_test >/dev/null
        pub=$(cat /root/.ssh/autobak_test.pub)
        printf 'command="/usr/local/bin/autobak-agent serve",restrict %s\n' "$pub" \
            >> /root/.ssh/authorized_keys
        chmod 0600 /root/.ssh/authorized_keys /root/.ssh/autobak_test
    fi
    echo "  файлы, панель и ключи готовы"
}

seed_data() {
    # Боевая база сайта: её найдёт автообнаружение и она попадёт в план.
    mariadb -e "CREATE DATABASE IF NOT EXISTS admin_shop CHARACTER SET utf8mb4"
    mariadb admin_shop -e "
        CREATE TABLE IF NOT EXISTS posts (
            id INT AUTO_INCREMENT PRIMARY KEY,
            title VARCHAR(200),
            body TEXT
        ) ENGINE=InnoDB;
        INSERT INTO posts (title, body)
            SELECT CONCAT('запись ', n), REPEAT('содержимое статьи ', 20)
            FROM (SELECT 1 n UNION SELECT 2 UNION SELECT 3 UNION SELECT 4 UNION SELECT 5) t;"
    mariadb -e "
        CREATE USER IF NOT EXISTS 'admin_shop'@'localhost' IDENTIFIED BY 'пароль-от-базы';
        GRANT ALL ON admin_shop.* TO 'admin_shop'@'localhost';
        FLUSH PRIVILEGES;"
    echo "  база admin_shop наполнена"
}

case "${1:---all}" in
    --files) seed_files ;;
    --data)  seed_data ;;
    *)       seed_files; seed_data ;;
esac
