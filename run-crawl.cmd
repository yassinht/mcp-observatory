@echo off
rem Runs one census and appends its console output to data\crawl.log.
rem Used by the Windows scheduled task; the Linux deployment uses a systemd
rem unit instead, but the shape is the same: one command, output to a log.
cd /d "%~dp0"
echo. >> data\crawl.log
echo ===== %DATE% %TIME% ===== >> data\crawl.log
"%~dp0crawler.exe" %* >> data\crawl.log 2>&1
echo exit code %ERRORLEVEL% >> data\crawl.log
