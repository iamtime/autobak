#!/bin/sh
# Установка агента autobak на сервер.
#
# Скрипт намеренно на /bin/sh, без bash-измов: он должен отработать
# на Debian, Ubuntu, CentOS, Alpine и на урезанном образе, где bash
# не установлен вовсе.
#
# Использование:
#   ./install.sh /path/to/autobak-agent          установить агента
#   ./install.sh --key "ssh-ed25519 AAAA..."     добавить ключ только для бэкапов
#   ./install.sh --key-restore "ssh-ed25519 ..." добавить ключ с правом восстановления
#   ./install.sh --push                          настроить бэкап по таймеру
#   ./install.sh --uninstall                     удалить всё
set -eu

BIN=/usr/local/bin/autobak-agent
CONF=/etc/autobak
SERVICE=/etc/systemd/system/autobak.service
TIMER=/etc/systemd/system/autobak.timer

die() { echo "autobak: $*" >&2; exit 1; }
info() { echo "  $*"; }

[ "$(id -u)" = 0 ] || die "запустите от root"

install_binary() {
    src=$1
    [ -f "$src" ] || die "файл $src не найден"
    # Пишем во временный файл и переименовываем: если в этот момент
    # выполняется бэкап, он не должен получить полупустой бинарь.
    tmp=$(mktemp)
    cat "$src" > "$tmp"
    chmod 0755 "$tmp"
    mv "$tmp" "$BIN"
    info "агент установлен: $("$BIN" version)"

    mkdir -p "$CONF"
    # 0700: внутри лежат ключ репозитория и доступы к хранилищу.
    chmod 0700 "$CONF"
    info "каталог настроек: $CONF (доступ только root)"
}

# add_key прописывает ключ, который не даёт ничего, кроме вызова агента.
#
# restrict отключает проброс портов, агент, X11 и pty. Даже если ключ
# утечёт, через него нельзя ни получить shell, ни пробросить туннель
# внутрь сети - только выполнить команды из белого списка агента.
add_key() {
    key=$1
    mode=$2 # "backup" (по умолчанию) или "restore"

    # Санитизация: ключ вставляется в authorized_keys, и перевод строки в
    # нём дописал бы вторую строку - уже без command=/restrict, то есть
    # полноценный shell по подставленному ключу. Отсекаем всё, начиная с
    # первого перевода строки или возврата каретки.
    key=$(printf '%s' "$key" | tr -d '\r' | sed -n '1p')
    case "$key" in
        ssh-ed25519\ *|ssh-rsa\ *|ecdsa-sha2-*\ *|sk-ssh-*\ *|sk-ecdsa-*\ *) : ;;
        *) die "это не похоже на публичный ключ SSH: $key" ;;
    esac

    serve="serve --backup-only"
    if [ "$mode" = restore ]; then
        # Ключ с правом восстановления пишет произвольные файлы от root, то
        # есть равносилен root-доступу. Выдаётся сознательно и отдельной
        # командой, не по умолчанию.
        serve="serve"
        info "ВНИМАНИЕ: ключ с правом восстановления равносилен root на этом сервере."
    fi

    home=$(getent passwd root | cut -d: -f6)
    dir="$home/.ssh"
    mkdir -p "$dir"
    chmod 0700 "$dir"
    line="command=\"$BIN $serve\",restrict $key"
    touch "$dir/authorized_keys"
    chmod 0600 "$dir/authorized_keys"
    if grep -qF "$key" "$dir/authorized_keys" 2>/dev/null; then
        info "такой ключ уже прописан - пропускаю"
        return
    fi
    printf '%s\n' "$line" >> "$dir/authorized_keys"
    info "ключ добавлен с ограничением command=\"$BIN $serve\",restrict"
}

setup_push() {
    [ -x "$BIN" ] || die "сначала установите агента"
    [ -f "$CONF/config.json" ] || die "нет $CONF/config.json - создайте его в программе на компьютере"
    [ -f "$CONF/key" ] || die "нет $CONF/key - файл с ключом репозитория"
    chmod 0600 "$CONF/config.json" "$CONF/key"

    cat > "$SERVICE" <<EOF
[Unit]
Description=AutoBak: резервное копирование
# Сеть нужна для выгрузки в хранилище; без неё запуск бессмысленен.
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=$BIN backup --config $CONF/config.json
# Бэкап не должен мешать сайтам: минимальный приоритет и по процессору,
# и по диску. Второе важнее - чтение сотен тысяч файлов забивает очередь
# диска, и время ответа сайта растёт даже при простаивающем процессоре.
Nice=10
IOSchedulingClass=idle
CPUSchedulingPolicy=idle
# Ограничения: агенту нужно читать всё и запускать mysqldump, но не нужно
# ничего из перечисленного ниже.
NoNewPrivileges=yes
PrivateTmp=yes
ProtectControlGroups=yes
ProtectKernelModules=yes
ProtectKernelTunables=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
TimeoutStartSec=12h

[Install]
WantedBy=multi-user.target
EOF

    cat > "$TIMER" <<EOF
[Unit]
Description=AutoBak: запуск по расписанию

[Timer]
OnCalendar=*-*-* 04:00:00
# Разброс до получаса: если серверов несколько и все пишут в одно
# хранилище, одновременный старт упрётся в лимиты запросов.
RandomizedDelaySec=30m
# Пропущенный из-за выключенного сервера запуск выполняется при старте.
Persistent=true

[Install]
WantedBy=timers.target
EOF

    systemctl daemon-reload
    systemctl enable --now autobak.timer
    info "таймер включён:"
    systemctl list-timers autobak.timer --no-pager | sed -n 2p
    info "проверить настройки: $BIN backup --config $CONF/config.json --dry-run"
}

uninstall() {
    if [ -f "$TIMER" ]; then
        systemctl disable --now autobak.timer 2>/dev/null || true
        rm -f "$TIMER" "$SERVICE"
        systemctl daemon-reload
        info "таймер удалён"
    fi
    rm -f "$BIN"
    info "агент удалён"
    # Настройки и ключ не трогаем намеренно: в файле ключа может быть
    # единственная копия доступа к репозиторию, и стереть её походя нельзя.
    if [ -d "$CONF" ]; then
        info "каталог $CONF оставлен - в нём ключ репозитория."
        info "Удалить вручную: rm -rf $CONF"
    fi
}

case "${1:-}" in
    --key)         [ $# -ge 2 ] || die "укажите публичный ключ"; add_key "$2" backup ;;
    --key-restore) [ $# -ge 2 ] || die "укажите публичный ключ"; add_key "$2" restore ;;
    --push)    setup_push ;;
    --uninstall) uninstall ;;
    --check)   "$BIN" selftest ;;
    "")        die "укажите путь к бинарю агента или ключ: см. комментарий в начале файла" ;;
    *)         install_binary "$1"; "$BIN" selftest || true ;;
esac
