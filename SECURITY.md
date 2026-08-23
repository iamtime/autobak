# Security Policy / Безопасность

## Reporting a vulnerability

AutoBak handles backups, encryption keys and root-equivalent access to
servers, so security reports are taken seriously.

Please report vulnerabilities **privately**, not through public issues:

- open a [GitHub Security Advisory](https://github.com/iamtime/autobak/security/advisories/new), or
- email the maintainer (see the profile on GitHub).

Include what you found, how to reproduce it, and the impact. You will get an
acknowledgement, and a fix or mitigation will be worked out before any public
disclosure.

## Что уже заложено в модель

- Свой ключ шифрования на каждый сервер; данные шифруются до отправки в
  хранилище (`XChaCha20-Poly1305`), ключ данных на сервер не попадает.
- Ключ агента ограничен тремя уровнями: `restrict` (ни shell, ни туннелей),
  `--backup-only` (нельзя восстанавливать) и `--allow=...` (нельзя читать
  вне разрешённых каталогов).
- Неизменяемость прошлых бэкапов обеспечивается на стороне хранилища
  (Object Lock + версионирование) - см. [docs/DEPLOY.md](docs/DEPLOY.md).
- Секреты не попадают в `argv`, логи, git-зеркало и тексты ошибок.

Подробнее об устройстве - [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md),
раздел «Безопасность».
