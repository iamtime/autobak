# Быстрый старт: Windows PC + Linux-сервер

Пошаговая установка для типового случая: управляешь бэкапами с **Windows**,
бэкапишь **Linux-сервер**.

Роли:

- **Windows** - собирает всё и управляет бэкапами (окно или командная строка).
- **Linux-сервер** - получает только готовый бинарь агента. **Go на сервере не нужен.**

---

## Часть 1. Windows: поставить Go и собрать

> Не хочешь ставить Go - возьми готовые `autobak.exe` и `autobak-cli.exe`
> из [последнего релиза](https://github.com/iamtime/autobak/releases/latest), положи их в каталог `dist\` и переходи
> сразу к части 2. Агент для сервера собирается только из исходников.

```powershell
winget install GoLang.Go
# закрой и открой терминал заново, чтобы go попал в PATH
git clone https://github.com/iamtime/autobak
cd autobak
.\build.ps1
```

В каталоге `dist\` появятся:

| Файл | Что это |
|---|---|
| `autobak.exe` | окно управления |
| `autobak-cli.exe` | то же самое командной строкой |
| `autobak-agent-linux-amd64` | агент для сервера |
| `install.sh` | установщик агента на сервере |
| `autobak-task.cmd` | автозапуск по расписанию |

---

## Часть 2. SSH-доступ к серверу (один раз)

```powershell
# если ключа ещё нет
ssh-keygen -t ed25519          # Enter на все вопросы

# положить свой публичный ключ на сервер
type $env:USERPROFILE\.ssh\id_ed25519.pub | ssh root@SERVER_IP "mkdir -p ~/.ssh && cat >> ~/.ssh/authorized_keys"

# проверить вход без пароля
ssh root@SERVER_IP echo ok
```

Замени `SERVER_IP` на адрес своего сервера.

---

## Часть 3. Настроить бэкап

Все команды - в каталоге `autobak` на Windows.

### 3.1 Хранилище - куда складывать бэкапы

Локальный диск:

```powershell
.\dist\autobak-cli.exe repo add --name main --kind local --path D:\backups
```

или S3-совместимое облако:

```powershell
.\dist\autobak-cli.exe repo add --name main --kind s3 --endpoint s3.example.com --region us-east-1 --bucket mybackups --access-key AKIA...
```

Программа спросит секретный ключ S3 (если S3) и **пароль репозитория** - им
шифруются данные - и покажет **recovery-код**.

> ⚠️ **Запиши recovery-код на бумагу.** Это единственный способ открыть
> бэкапы, если забудешь пароль. Храни отдельно от компьютера.

### 3.2 Сервер

```powershell
.\dist\autobak-cli.exe server add --name prod --host SERVER_IP --user root --key $env:USERPROFILE\.ssh\id_ed25519 --repo main
```

### 3.3 Залить агента на сервер

```powershell
.\dist\autobak-cli.exe server install prod --binary .\dist\autobak-agent-linux-amd64
```

### 3.4 Обследовать сервер и сохранить план

```powershell
.\dist\autobak-cli.exe server discover prod
.\dist\autobak-cli.exe server plan prod --apply
```

`discover` покажет, что нашлось (сайты, базы, конфиги, docker), `plan --apply`
сохранит план бэкапа по найденному.

### 3.5 Первый бэкап

```powershell
.\dist\autobak-cli.exe backup prod
```

### 3.6 Убедиться, что из бэкапа поднимается

```powershell
.\dist\autobak-cli.exe verify main      # данные целы?
.\dist\autobak-cli.exe drill prod       # а поднимается из них то же самое?
```

`drill` восстанавливает выборку во временный каталог и сверяет - сделай это
хотя бы раз, пока бэкап не понадобился по-настоящему.

---

## Часть 4. По желанию

### Мышью вместо команд

Всё то же самое в окне:

```powershell
.\dist\autobak.exe
```

### Автозапуск по расписанию

Раз в час, программа сама решает, чьё время подошло (расписание каждого
сервера задаётся в его настройках):

```powershell
.\dist\autobak-task.cmd install
```

Права администратора не нужны. Вывод - в `%LOCALAPPDATA%\autobak\schedule.log`.

### Ограничить ключ на сервере

Чтобы украденный ключ не мог прочитать лишнее, ограничь его в
`~/.ssh/authorized_keys` на сервере строкой вида:

```
command="/usr/local/bin/autobak-agent serve --backup-only --allow=/home,/var/www",restrict ssh-ed25519 AAAA...
```

Готовую строку с уже подставленным `--allow` по твоему плану показывает окно
`autobak.exe` в разделе сервера - просто скопируй её. `--backup-only`
запрещает восстановление этим ключом, `--allow` ограничивает чтение только
перечисленными каталогами.

---

## Что легко забыть

- **Recovery-код** из шага 3.1 - единственный ключ к бэкапам при утере
  пароля. Храни отдельно от машины.
- Если хранилище **S3 и push-режим** (сервер пишет сам, без включённого PC) -
  включи на бакете **версионирование и Object Lock**: только это защищает
  прошлые бэкапы от перезаписи шифровальщиком.
- Первый настоящий **`drill`** - до того, как бэкап понадобится в аварии.

Подробнее об устройстве и остальных режимах (push, Kubernetes, второе
хранилище, git-история конфигов) - [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).
