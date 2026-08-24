# AutoBak

**Русский** · [English](README.en.md)

Резервное копирование серверов: сайты, базы, Docker, конфиги. Один файл на
сервере, окно на компьютере или веб-интерфейс. Данные шифруются до отправки,
одинаковые куски хранятся один раз - по сети едет только новое.

```
первый бэкап                     11.7 МБ данных, 11.7 МБ по сети
повторный без изменений          0 байт по сети
после правки одного файла из 12  38 байт по сети
```

## Установка

Для Windows есть готовые сборки, Go для них не нужен:
[последний релиз](https://github.com/iamtime/autobak/releases/latest) - `autobak.exe` (окно) и
`autobak-cli.exe` (командная строка).

Всё остальное собирается из исходников. Нужен Go 1.26. Если ещё не стоит:

```powershell
# Windows
winget install GoLang.Go        # или: choco install golang
```

```sh
# macOS
brew install go

# Debian / Ubuntu
sudo apt install golang-go       # если в репозитории версия старее 1.26 - с go.dev/dl

# Fedora / RHEL
sudo dnf install golang
```

Дальше сборка:

```sh
git clone https://github.com/iamtime/autobak && cd autobak
make            # Linux/macOS → ./dist
# или на Windows:  .\build.ps1
```

Поставить агента на сервер:

```sh
scp dist/autobak-agent-linux-amd64 deploy/install.sh root@server:/tmp/
ssh root@server 'sh /tmp/install.sh /tmp/autobak-agent-linux-amd64'
```

Дальше выбери, откуда управлять.

## Окно на своём компьютере

```sh
./dist/autobak            # Linux/macOS
.\dist\autobak.exe        # Windows
```

Подключить хранилище (диск или S3), добавить сервер, нажать **Обследовать** -
агент сам найдёт сайты, базы и конфиги. Отметить нужное, нажать **Бэкап**.

## Веб-интерфейс на выделенной машине

Купил сервер под бэкапы, поставил docker:

```sh
cd deploy/web
echo "AUTOBAK_PASSWORD=длинный-пароль" > .env
docker compose up -d
docker compose logs        # тут публичный ключ SSH - разложи по серверам
```

Открыть `http://127.0.0.1:8080`. Наружу - только за TLS-прокси
(`AUTOBAK_BEHIND_TLS_PROXY=1`): через веб доступно восстановление на любой
сервер.

## Командная строка

```sh
autobak repo add --name main --kind s3 --endpoint s3.example.com --bucket backups
autobak server add --name prod --host server.example.com --repo main
autobak server plan prod --apply
autobak backup prod
autobak restore prod <снимок>       # по умолчанию сухой прогон
autobak verify main                 # данные целы?
autobak drill prod                  # а поднимается из них то же самое?
```

## По расписанию

```sh
# Windows
deploy\autobak-task.cmd install

# Linux (cron)
0 * * * * /usr/local/bin/autobak schedule run
```

В веб-контейнере расписание уже работает само.

## Безопасность в двух словах

- Агент запускается по SSH, портов не слушает. Ключ ограничен
  `command="... serve --backup-only --allow=/var/www",restrict` - не даёт ни
  shell, ни чтения вне разрешённых каталогов.
- Шифрование `XChaCha20-Poly1305` до отправки, свой ключ на каждый сервер.
- Восстановление и удаление снимков требуют набрать имя сервера.
- **Для push-режима включи на S3-бакете версионирование и Object Lock** -
  только это защищает прошлые бэкапы от шифровальщика.

## Что дальше

- Пошаговая установка Windows + Linux-сервер - [QUICKSTART.md](QUICKSTART.md)
- Полное устройство, все команды и режимы - [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)
- Сборка под конкретную ОС, автозапуск, push-режим, Kubernetes, git-история
  конфигов, второе хранилище - там же.
- Сообщить об уязвимости - [SECURITY.md](SECURITY.md)

Лицензия - [MIT](LICENSE).
