@echo off
rem ============================================================
rem  AutoBak: Windows Scheduled Task helper
rem
rem    autobak-task.cmd install [hours]  create the task (default: hourly)
rem    autobak-task.cmd uninstall        remove it
rem    autobak-task.cmd disable          turn it off temporarily
rem    autobak-task.cmd enable           turn it back on
rem    autobak-task.cmd status           show current state
rem    autobak-task.cmd run              run once, right now
rem
rem  The task fires hourly and lets the program decide which servers are
rem  due: per-server schedules live in the app, not here.
rem  No administrator rights needed - the task runs as your own account.
rem
rem  This file is deliberately plain ASCII. A .cmd is read by the console
rem  in the OEM codepage (866 on Russian Windows) but by editors as UTF-8,
rem  so any non-ASCII text is guaranteed to look broken in one of them.
rem ============================================================
setlocal EnableExtensions

set "TASK=AutoBak"
set "HERE=%~dp0"
set "EXE=%HERE%autobak-cli.exe"
set "LOGDIR=%LOCALAPPDATA%\autobak"
set "LOG=%LOGDIR%\schedule.log"

rem Fall back to the windowed build if the console one is not next to us.
if not exist "%EXE%" set "EXE=%HERE%autobak.exe"

if "%~1"=="" goto :usage
if /i "%~1"=="install"   goto :install
if /i "%~1"=="uninstall" goto :uninstall
if /i "%~1"=="disable"   goto :disable
if /i "%~1"=="enable"    goto :enable
if /i "%~1"=="status"    goto :status
if /i "%~1"=="run"       goto :run
goto :usage

:usage
echo.
echo   autobak-task.cmd install [hours]  create the scheduled task
echo   autobak-task.cmd uninstall        remove it
echo   autobak-task.cmd disable          turn it off
echo   autobak-task.cmd enable           turn it on
echo   autobak-task.cmd status           show state
echo   autobak-task.cmd run              run once now
echo.
exit /b 2

:install
if not exist "%EXE%" (
    echo [error] autobak-cli.exe not found next to this script.
    echo         Put autobak-task.cmd in the same folder as the program.
    exit /b 1
)
set "EVERY=%~2"
if "%EVERY%"=="" set "EVERY=1"
if not exist "%LOGDIR%" mkdir "%LOGDIR%" >nul 2>&1

rem The task invokes this same file with "run" so that output lands in a
rem log file instead of vanishing with the scheduler's invisible window.
schtasks /create /tn "%TASK%" /tr "\"%~f0\" run" /sc hourly /mo %EVERY% /f >nul
if errorlevel 1 (
    echo [error] Could not create the task.
    exit /b 1
)
echo Task "%TASK%" created: runs every %EVERY% hour(s).
echo Log file: %LOG%
echo.
echo The task runs only while you are logged in. In "this computer pulls"
echo mode no backup happens while the machine is off - switch the server
echo to "server pushes on its own" for round-the-clock operation.
goto :show

:uninstall
schtasks /delete /tn "%TASK%" /f >nul 2>&1
if errorlevel 1 (
    echo Task "%TASK%" not found, nothing to remove.
    exit /b 0
)
echo Task "%TASK%" removed. Settings and backups are untouched.
exit /b 0

:disable
schtasks /change /tn "%TASK%" /disable >nul 2>&1
if errorlevel 1 (
    echo [error] Task "%TASK%" not found.
    exit /b 1
)
echo Task disabled. Scheduled backups will not run.
exit /b 0

:enable
schtasks /change /tn "%TASK%" /enable >nul 2>&1
if errorlevel 1 (
    echo [error] Task "%TASK%" not found. Create it: autobak-task.cmd install
    exit /b 1
)
echo Task enabled.
goto :show

:status
schtasks /query /tn "%TASK%" >nul 2>&1
if errorlevel 1 (
    echo Task "%TASK%" is not installed.
    echo Install it: autobak-task.cmd install
    exit /b 1
)
goto :show

:show
echo.
schtasks /query /tn "%TASK%" /fo list
exit /b 0

:run
if not exist "%LOGDIR%" mkdir "%LOGDIR%" >nul 2>&1
echo.>> "%LOG%"
echo ==== %date% %time% ====>> "%LOG%"
"%EXE%" schedule run >> "%LOG%" 2>&1
set "CODE=%ERRORLEVEL%"
if not "%CODE%"=="0" echo [exit code %CODE%]>> "%LOG%"
exit /b %CODE%
