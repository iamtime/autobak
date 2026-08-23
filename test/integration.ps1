# Сборка стенда и прогон интеграционных тестов в Linux-контейнере.
#
#   .\test\integration.ps1              собрать и прогнать
#   .\test\integration.ps1 -Shell       открыть оболочку внутри стенда
#   .\test\integration.ps1 -Run TestMySQL   только совпадающие тесты
#   .\test\integration.ps1 -NoDocker    без docker внутри (образ легче)

param(
    [switch]$Shell,
    [switch]$NoDocker,
    [switch]$Rebuild,
    [string]$Run = ""
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
$ctx = Join-Path $root "test\docker"
$image = "autobak-testbed"

function Step($text) { Write-Host "`n=== $text" -ForegroundColor Cyan }

# Native запускает внешнюю программу и проверяет только её код возврата.
#
# Обходит давнюю особенность Windows PowerShell 5.1: любая строка, которую
# внешняя программа написала в stderr, превращается в ErrorRecord, а при
# $ErrorActionPreference = "Stop" это останавливает скрипт целиком. docker
# пишет в stderr ход сборки, то есть успешная сборка выглядела бы как
# падение. Проверять надо код возврата, а не факт вывода.
function Native {
    param([string]$Exe, [string[]]$Arguments, [string]$What)
    $prev = $ErrorActionPreference
    $ErrorActionPreference = "Continue"
    try {
        & $Exe @Arguments
        $code = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $prev
    }
    if ($code -ne 0) { throw "$What - код возврата $code" }
}

Step "Проверка docker"
Native docker @("version", "--format", "{{.Server.Version}}") "docker недоступен"

Step "Кросс-компиляция под Linux"
$env:GOOS = "linux"; $env:GOARCH = "amd64"; $env:CGO_ENABLED = "0"
try {
    Native go @("build", "-trimpath", "-ldflags", "-s -w -X main.Version=test",
        "-o", (Join-Path $ctx "autobak-agent"), "./cmd/autobak-agent") "не собрался агент"
    Write-Host ("  агент: {0:N1} МБ" -f ((Get-Item (Join-Path $ctx "autobak-agent")).Length / 1MB))

    # Тестовый бинарь собирается здесь же и копируется готовым:
    # компилятор Go в образе не нужен, а проверяются ровно те бинари,
    # которые поедут на настоящий сервер.
    Native go @("test", "-c", "-tags", "integration",
        "-o", (Join-Path $ctx "integration.test"), "./test/integration") "не собрались тесты"
    Write-Host ("  тесты: {0:N1} МБ" -f ((Get-Item (Join-Path $ctx "integration.test")).Length / 1MB))
} finally {
    Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue
}

Step "Сборка образа"
$buildArgs = @("build", "-t", $image, "--build-arg", "WITH_DOCKER=$(if ($NoDocker) {'0'} else {'1'})")
if ($Rebuild) { $buildArgs += "--no-cache" }
$buildArgs += $ctx
Native docker $buildArgs "образ не собрался"

Step "Запуск стенда"
$runArgs = @("run", "--rm", "-e", "PRIVILEGED=1")
if (-not $NoDocker) {
    # dockerd внутри контейнера требует привилегий; без них тесты тома
    # пропускаются, а не падают.
    $runArgs += "--privileged"
}
$runArgs += $image
if ($Shell) {
    $runArgs += "shell"
    & docker @runArgs -it
    exit
}
$runArgs += "test"
if ($Run) { $runArgs += @("-test.run", $Run) }

$prev = $ErrorActionPreference
$ErrorActionPreference = "Continue"
& docker @runArgs
$code = $LASTEXITCODE
$ErrorActionPreference = $prev

Write-Host ""
if ($code -eq 0) {
    Write-Host "Все интеграционные тесты прошли." -ForegroundColor Green
} else {
    Write-Host "Есть падения - см. вывод выше." -ForegroundColor Red
}
exit $code
