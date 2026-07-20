@echo off
REM FleetChat launcher for Windows -- just double-click this file.
REM Builds daemon.exe on first run if you have Go installed (https://go.dev/dl/), then launches it.
echo.
echo   Starting FleetChat...
echo.
cd /d "%~dp0daemon"
if not exist daemon.exe (
  where go >nul 2>nul
  if errorlevel 1 (
    echo   Go is required to build the daemon the first time ^(https://go.dev/dl/^).
    echo   Install it, then re-run this file.
    pause
    exit /b 1
  )
  echo   First run: building daemon.exe...
  go build -o daemon.exe .
)
daemon.exe %*
echo.
echo   FleetChat has stopped.
