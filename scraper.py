"""Looks up a McMaster-Carr part number by opening its page in a real browser.

One lookup at a time, with a minimum gap between lookups, so traffic stays at
"one person clicking around" levels. Results are cached by the caller.
"""
import mimetypes
import os
import re
import threading
import time
from datetime import datetime
from pathlib import Path

from playwright.sync_api import Error as PlaywrightError
from playwright.sync_api import sync_playwright

ROOT = Path(__file__).parent
EXTRACTOR_JS = (ROOT / "static" / "extractor.js").read_text(encoding="utf-8")
PART_RE = re.compile(r"^[0-9]{1,6}[A-Z][0-9]{1,6}$")


class LookupError_(Exception):
    """Lookup failed; message is shown to the user."""


def normalize_part_number(raw):
    return re.sub(r"[^0-9A-Za-z]", "", raw or "").upper()


def looks_like_part_number(pn):
    return bool(PART_RE.match(pn))


class Scraper:
    def __init__(self, data_dir):
        self.base_url = os.environ.get("MCM_BASE_URL", "https://www.mcmaster.com").rstrip("/")
        self.channel = os.environ.get("MCM_BROWSER_CHANNEL", "chrome")
        self.executable = os.environ.get("MCM_BROWSER_PATH") or None
        self.headless = os.environ.get("MCM_HEADLESS", "1") != "0"
        self.min_interval = float(os.environ.get("MCM_MIN_INTERVAL", "4"))
        self.timeout_ms = int(float(os.environ.get("MCM_TIMEOUT", "25")) * 1000)
        self.profile_dir = Path(data_dir) / "browser-profile"
        self.images_dir = Path(data_dir) / "images"
        self.debug_dir = Path(data_dir) / "debug"
        for d in (self.profile_dir, self.images_dir, self.debug_dir):
            d.mkdir(parents=True, exist_ok=True)
        self._lock = threading.Lock()
        self._last = 0.0

    def _launch(self, p):
        kwargs = dict(
            user_data_dir=str(self.profile_dir),
            headless=self.headless,
            locale="en-US",
            viewport={"width": 1366, "height": 900},
        )
        if self.executable:
            return p.chromium.launch_persistent_context(executable_path=self.executable, **kwargs)
        try:
            # The office's installed Chrome looks like a normal visitor; prefer it.
            return p.chromium.launch_persistent_context(channel=self.channel, **kwargs)
        except PlaywrightError:
            return p.chromium.launch_persistent_context(**kwargs)

    def lookup(self, pn):
        with self._lock:
            wait = self.min_interval - (time.monotonic() - self._last)
            if wait > 0:
                time.sleep(wait)
            try:
                return self._lookup(pn)
            finally:
                self._last = time.monotonic()

    def _lookup(self, pn):
        url = f"{self.base_url}/{pn}/"
        with sync_playwright() as p:
            try:
                ctx = self._launch(p)
            except PlaywrightError as e:
                raise LookupError_(f"לא הצלחתי להפעיל דפדפן: {e.message.splitlines()[0]}")
            try:
                page = ctx.pages[0] if ctx.pages else ctx.new_page()
                try:
                    page.goto(url, wait_until="domcontentloaded", timeout=self.timeout_ms)
                    # The page renders client-side; wait until a price or a heading shows up.
                    page.wait_for_function(
                        "() => /\\$\\s?[\\d,]+\\.\\d{2}/.test(document.body.innerText) || document.querySelector('h1')",
                        timeout=self.timeout_ms,
                    )
                    page.wait_for_timeout(800)
                except PlaywrightError:
                    pass  # extract whatever loaded; the checks below decide
                data = page.evaluate(EXTRACTOR_JS)

                if data["blocked"] or data["notFound"] or not data["names"]:
                    debug = self._save_debug(page, pn)
                    if data["notFound"]:
                        raise LookupError_("McMaster לא מצא את המק״ט הזה.")
                    if data["blocked"]:
                        raise LookupError_(f"נראה ש-McMaster חסם את הבדיקה האוטומטית (נשמר דיבאג: {debug}).")
                    raise LookupError_(f"הדף נטען אבל לא זיהיתי שם מוצר (נשמר דיבאג: {debug}).")

                image_file = self._save_image(ctx, pn, data["image"]) if data["image"] else None
                return {
                    "part_number": pn,
                    "name": data["names"][0],
                    "name_options": data["names"][:6],
                    "unit": data["unit"],
                    "price": data["price"],
                    "image_url": data["image"],
                    "image_file": image_file,
                    "url": url,
                }
            finally:
                ctx.close()

    def _save_image(self, ctx, pn, image_url):
        try:
            resp = ctx.request.get(image_url, timeout=15000)
            if not resp.ok:
                return None
            ctype = resp.headers.get("content-type", "").split(";")[0]
            ext = mimetypes.guess_extension(ctype) or Path(image_url.split("?")[0]).suffix or ".img"
            if ext == ".jpe":
                ext = ".jpg"
            name = f"{pn}{ext}"
            (self.images_dir / name).write_bytes(resp.body())
            return name
        except PlaywrightError:
            return None

    def _save_debug(self, page, pn):
        stamp = datetime.now().strftime("%Y%m%d-%H%M%S")
        base = self.debug_dir / f"{pn}-{stamp}"
        try:
            base.with_suffix(".html").write_text(page.content(), encoding="utf-8")
            page.screenshot(path=str(base.with_suffix(".png")), full_page=True)
        except PlaywrightError:
            pass
        return base.name
