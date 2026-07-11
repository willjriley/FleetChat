@echo off
REM ===================================================================
REM  FleetChat launcher for Windows -- just double-click this file.
REM  A console window opens, the board starts, and your browser opens
REM  to it. KEEP THE CONSOLE WINDOW OPEN -- closing it stops the crew.
REM ===================================================================
cd /d "%~dp0"
echo.
echo   Starting FleetChat...
echo   Keep THIS window open (closing it stops the server).
echo   Your browser will open to the board in a moment.
echo.
python run.py %*
echo.
echo   FleetChat has stopped.
pause
