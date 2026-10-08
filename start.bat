@echo off
rem Switch the console to UTF-8 so non-ASCII output from artex is not mangled on a legacy code page.
chcp 65001 >nul 2>&1
rem ARTEX supervisor start script (Windows)
rem
rem Usage:
rem   start.bat                  run in the foreground (Ctrl-C to stop)
rem   start.bat -addr :9000      extra flags are passed through to artex verbatim
rem
rem It does exactly one thing: start artex.exe, and when the process exits, decide from the exit code whether to start it again.
rem
rem   0      stopped normally by the user  -> leave the loop
rem   75     the program asked to restart  -> rerun immediately ("one-click update" or "roll back" was clicked in the UI)
rem   other  crash                         -> rerun after a backoff (1->2->4... capped at 60 seconds)
rem
rem Downloading, SHA256 verification and swapping the binary are not done here; artex does all of it
rem itself at startup (the selfupdate package). This script stays dumb -- see the notes at the top of start.sh.

setlocal enabledelayedexpansion
cd /d "%~dp0"

set "BIN=artex.exe"
if not exist "%BIN%" (
	echo [artex] executable not found: %BIN% 1>&2
	exit /b 1
)

set "RESTART_CODE=75"
set "MAX_DELAY=60"
set /a delay=1

:loop
"%BIN%" %*
set "code=!ERRORLEVEL!"

if "!code!"=="0" (
	echo [artex] exited normally
	exit /b 0
)

if "!code!"=="%RESTART_CODE%" (
	rem An update/rollback is staged: on the next run artex completes the swap at startup.
	echo [artex] restart requested ^(applying the new version^)...
	set /a delay=1
	goto loop
)

echo [artex] abnormal exit ^(code=!code!^), restarting in !delay!s 1>&2
rem timeout fails in a redirected console, so ping is used as a fallback (an N-second delay needs N+1 pings).
set /a pings=!delay!+1
ping -n !pings! 127.0.0.1 >nul 2>&1
set /a delay=!delay!*2
if !delay! gtr %MAX_DELAY% set /a delay=%MAX_DELAY%
goto loop
