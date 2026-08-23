# Сборка AutoBak.
#
#   .\build.ps1              собрать всё в .\dist
#   .\build.ps1 -Test        прогнать тесты перед сборкой
#   .\build.ps1 -Version 1.0.0
#
# Агент собирается статически (CGO_ENABLED=0) под amd64 и arm64: он должен
# запускаться на любом Linux без установленных библиотек, включая старые
# CentOS и Alpine с musl.

param(
    [string]$Version = "0.1.0",
    [switch]$Test,
    [switch]$Clean
)

$ErrorActionPreference = "Stop"
$root = $PSScriptRoot
$dist = Join-Path $root "dist"

if ($Clean -and (Test-Path $dist)) { Remove-Item -Recurse -Force $dist }
if (-not (Test-Path $dist)) { New-Item -ItemType Directory -Force $dist | Out-Null }

if ($Test) {
    Write-Host "Тесты..." -ForegroundColor Cyan
    & go test ./... -timeout 600s
    if ($LASTEXITCODE -ne 0) { throw "тесты не прошли" }
}

$ldAgent = "-s -w -X main.Version=$Version"
$ldApp = "-s -w -X main.Version=$Version"

# Агент: статический, без отладочной информации.
foreach ($arch in @("amd64", "arm64")) {
    $out = Join-Path $dist "autobak-agent-linux-$arch"
    Write-Host "Агент linux/$arch..." -ForegroundColor Cyan
    $env:GOOS = "linux"; $env:GOARCH = $arch; $env:CGO_ENABLED = "0"
    & go build -trimpath -ldflags $ldAgent -o $out ./cmd/autobak-agent
    if ($LASTEXITCODE -ne 0) { throw "сборка агента для $arch не удалась" }
    $size = [math]::Round((Get-Item $out).Length / 1MB, 1)
    Write-Host "  $out - $size МБ"
}

# Десктоп: оконное приложение без консоли.
Write-Host "Десктоп windows/amd64..." -ForegroundColor Cyan
$env:GOOS = "windows"; $env:GOARCH = "amd64"; $env:CGO_ENABLED = "0"
$outApp = Join-Path $dist "autobak.exe"
& go build -trimpath -tags "desktop,production" -ldflags "$ldApp -H windowsgui" -o $outApp ./cmd/autobak
if ($LASTEXITCODE -ne 0) { throw "сборка десктопа не удалась" }

# Консольная сборка того же приложения: нужна планировщику задач и для
# разбора проблем - у оконной версии вывод в терминал не попадает.
$outCli = Join-Path $dist "autobak-cli.exe"
& go build -trimpath -tags "desktop,production" -ldflags $ldApp -o $outCli ./cmd/autobak
if ($LASTEXITCODE -ne 0) { throw "сборка консольной версии не удалась" }

# Веб-интерфейс: то же ядро, доступ через браузер. Собирается под Linux -
# он живёт на машине для бэкапов, а не на этом компьютере. Статически,
# по той же причине, что и агент.
foreach ($arch in @("amd64", "arm64")) {
    $out = Join-Path $dist "autobak-web-linux-$arch"
    Write-Host "Веб linux/$arch..." -ForegroundColor Cyan
    $env:GOOS = "linux"; $env:GOARCH = $arch; $env:CGO_ENABLED = "0"
    & go build -trimpath -ldflags $ldApp -o $out ./cmd/autobak-web
    if ($LASTEXITCODE -ne 0) { throw "сборка веб-интерфейса для $arch не удалась" }
}

Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue

# Всё, что кладётся рядом с программой или уезжает на сервер.
Copy-Item (Join-Path $root "deploy\install.sh") $dist -Force
Copy-Item (Join-Path $root "deploy\autobak-task.cmd") $dist -Force

$dockerDir = Join-Path $dist "docker"
if (-not (Test-Path $dockerDir)) { New-Item -ItemType Directory -Force $dockerDir | Out-Null }
Copy-Item (Join-Path $root "deploy\docker\*") $dockerDir -Force
# Агент кладётся в каталог сборки образа: Dockerfile берёт его оттуда,
# чтобы компилятор Go в образе был не нужен.
Copy-Item (Join-Path $dist "autobak-agent-linux-amd64") $dockerDir -Force
Copy-Item (Join-Path $dist "autobak-agent-linux-arm64") $dockerDir -Force

Write-Host "`nГотово:" -ForegroundColor Green
Get-ChildItem $dist | ForEach-Object {
    "{0,-34} {1,8:N1} МБ" -f $_.Name, ($_.Length / 1MB)
}
Write-Host @"

Дальше, любым из трёх способов:

  Обычная установка на сервер
    1. Скопировать autobak-agent-linux-amd64 и install.sh
    2. sudo sh install.sh ./autobak-agent-linux-amd64
    3. Запустить autobak.exe и добавить сервер

  Сервер пишет сам, через docker
    1. Скопировать каталог docker\ целиком
    2. Положить config.json и key в ./autobak, chmod 600
    3. docker compose up -d

  Бэкапы по расписанию на этом компьютере
    autobak-task.cmd install

  Веб-интерфейс на выделенной машине
    docker: из корня репозитория - cd deploy/web && docker compose up -d
    без docker: AUTOBAK_PASSWORD=... ./autobak-web-linux-amd64 -addr 127.0.0.1:8080
"@
