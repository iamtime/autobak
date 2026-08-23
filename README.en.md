# AutoBak

[Русский](README.md) · **English**

Server backups: websites, databases, Docker, configs. One binary on the
server, a window on your machine or a web UI. Data is encrypted before it
leaves, identical chunks are stored once — only new data crosses the network.

```
first backup                    11.7 MB of data, 11.7 MB over the wire
repeat, nothing changed         0 bytes over the wire
after editing 1 file out of 12  38 bytes over the wire
```

## Install

Go 1.26 required. If you don't have it yet:

```powershell
# Windows
winget install GoLang.Go        # or: choco install golang
```

```sh
# macOS
brew install go

# Debian / Ubuntu
sudo apt install golang-go       # if the repo ships older than 1.26, grab it from go.dev/dl

# Fedora / RHEL
sudo dnf install golang
```

Then build:

```sh
git clone https://github.com/iamtime/autobak && cd autobak
make            # Linux/macOS → ./dist
# or on Windows:  .\build.ps1
```

Put the agent on a server:

```sh
scp dist/autobak-agent-linux-amd64 deploy/install.sh root@server:/tmp/
ssh root@server 'sh /tmp/install.sh /tmp/autobak-agent-linux-amd64'
```

Then choose where to drive it from.

## Window on your own machine

```sh
./dist/autobak            # Linux/macOS
.\dist\autobak.exe        # Windows
```

Attach storage (disk or S3), add a server, hit **Discover** — the agent finds
sites, databases and configs itself. Tick what you need, hit **Backup**.

## Web UI on a dedicated box

Bought a server for backups, installed docker:

```sh
cd deploy/web
echo "AUTOBAK_PASSWORD=a-long-password" > .env
docker compose up -d
docker compose logs        # the SSH public key is here — deploy it to your servers
```

Open `http://127.0.0.1:8080`. Expose it only behind a TLS proxy
(`AUTOBAK_BEHIND_TLS_PROXY=1`): the web UI can restore to any server.

## Command line

```sh
autobak repo add --name main --kind s3 --endpoint s3.example.com --bucket backups
autobak server add --name prod --host 203.0.113.10 --repo main
autobak server plan prod --apply
autobak backup prod
autobak restore prod <snapshot>     # dry run by default
autobak verify main                 # is the data intact?
autobak drill prod                  # does the same thing come back out?
```

## On a schedule

```sh
# Windows
deploy\autobak-task.cmd install

# Linux (cron)
0 * * * * /usr/local/bin/autobak schedule run
```

The web container runs the schedule itself.

## Security in a nutshell

- The agent runs over SSH, listens on no ports. Its key is restricted to
  `command="... serve --backup-only --allow=/var/www",restrict` — no shell,
  no reads outside the allowed directories.
- `XChaCha20-Poly1305` encryption before upload, a separate key per server.
- Restore and snapshot deletion require typing the server name.
- **For push mode, enable versioning and Object Lock on the S3 bucket** —
  that alone protects past backups from ransomware.

## More

- Step-by-step setup for Windows + a Linux server — [QUICKSTART.md](QUICKSTART.md) (in Russian)
- Full design, every command and mode — [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)
- Per-OS builds, autostart, push mode, Kubernetes, git config history,
  a second repository — all there.
- Reporting a vulnerability — [SECURITY.md](SECURITY.md)

Licensed under [MIT](LICENSE).
