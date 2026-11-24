@echo off
setlocal EnableExtensions DisableDelayedExpansion
wsl.exe --distribution Ubuntu --cd "%~dp0." --exec bash ./scripts/redeploy-wsl.sh %*
set "RESULT=%ERRORLEVEL%"
if not "%RESULT%"=="0" echo Deployment command failed with exit code %RESULT%.
if "%~1"=="" pause
exit /b %RESULT%
