@echo off
chcp 65001 >nul
cd /d "%~dp0"
if not exist .venv (
  echo Creating Python environment - first run only...
  py -3 -m venv .venv || python -m venv .venv
  .venv\Scripts\python -m pip install -q -r requirements.txt
)
start "" http://localhost:5000
.venv\Scripts\python app.py
pause
