@echo off
chcp 65001 >nul
cd /d "%~dp0"
if not exist .venv\Scripts\python.exe (
  echo Creating Python environment - first run only...
  py -3 -m venv .venv || python -m venv .venv
)
if not exist .venv\Scripts\python.exe (
  echo.
  echo Python was not found. Install it from https://www.python.org/downloads/
  echo and tick "Add python.exe to PATH", then run this file again.
  pause
  exit /b 1
)
echo Checking packages...
.venv\Scripts\python -m pip install -q --disable-pip-version-check -r requirements.txt || (pause & exit /b 1)
start "" http://localhost:5000
.venv\Scripts\python app.py
pause
