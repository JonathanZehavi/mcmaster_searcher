import base64
import http.server
import os
import threading
from pathlib import Path

import pytest

from app import create_app
from db import Database
from scraper import LookupError_, Scraper

FIX = Path(__file__).parent / "fixtures"
PNG = base64.b64decode(
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
)


class FakeMcMaster(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path.startswith("/img/"):
            body, ctype = PNG, "image/png"
        elif self.path.startswith("/91251A540"):
            body, ctype = (FIX / "product.html").read_bytes(), "text/html"
        else:
            body, ctype = (FIX / "blocked.html").read_bytes(), "text/html"
        self.send_response(200)
        self.send_header("Content-Type", ctype)
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *a):
        pass


@pytest.fixture(scope="module")
def fake_site():
    srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), FakeMcMaster)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    yield f"http://127.0.0.1:{srv.server_port}"
    srv.shutdown()


@pytest.fixture
def scraper(tmp_path, fake_site, monkeypatch):
    monkeypatch.setenv("MCM_BASE_URL", fake_site)
    monkeypatch.setenv("MCM_MIN_INTERVAL", "0")
    monkeypatch.setenv("MCM_TIMEOUT", "8")
    if os.path.exists("/opt/pw-browsers/chromium"):
        monkeypatch.setenv("MCM_BROWSER_PATH", "/opt/pw-browsers/chromium")
    return Scraper(tmp_path)


@pytest.fixture
def client(tmp_path, scraper):
    app = create_app(db=Database(tmp_path / "t.db"), scraper=scraper)
    return app.test_client()


def test_scraper_extracts_product(scraper):
    r = scraper.lookup("91251A540")
    assert r["name"] == "Black-Oxide Alloy Steel Socket Head Screw"
    assert "M3 x 0.5 mm Thread, 8 mm Long" in r["name_options"]
    assert r["unit"] == "Pack of 100"
    assert r["price"] == "12.34"
    assert r["image_url"].endswith("/img/part.png")
    assert (scraper.images_dir / r["image_file"]).read_bytes() == PNG


def test_scraper_reports_block_and_saves_debug(scraper):
    with pytest.raises(LookupError_, match="חסם"):
        scraper.lookup("1234K56")
    assert any(scraper.debug_dir.glob("1234K56-*.html"))


def test_lookup_caches(client, scraper, monkeypatch):
    first = client.get("/api/lookup?pn=91251a540").get_json()
    assert first["ok"] and not first["part"]["cached"]
    assert first["part"]["image"].startswith("/images/")
    assert client.get(first["part"]["image"]).status_code == 200
    monkeypatch.setattr(scraper, "lookup", lambda pn: pytest.fail("should hit cache"))
    second = client.get("/api/lookup?pn=91251A540").get_json()
    assert second["part"]["cached"] and second["part"]["unit"] == "Pack of 100"


def test_lookup_rejects_garbage(client):
    r = client.get("/api/lookup?pn=hello")
    assert r.status_code == 400


def test_list_flow_and_export(client):
    item = {"part_number": "91251A540", "name": "Screw", "unit": "Pack of 100", "quantity": 2, "requester": "דני"}
    assert client.post("/api/items", json=item).get_json()["ok"]
    assert client.post("/api/items", json={**item, "quantity": 0}).status_code == 400
    items = client.get("/api/items").get_json()["items"]
    assert len(items) == 1 and items[0]["quantity"] == 2
    iid = items[0]["id"]
    client.patch(f"/api/items/{iid}", json={"quantity": 5})
    assert client.get("/api/items").get_json()["items"][0]["quantity"] == 5

    x = client.get("/api/export.xlsx")
    assert x.status_code == 200 and x.data[:2] == b"PK"

    # manual entry is remembered for the next lookup
    assert client.get("/api/lookup?pn=91251A540").get_json()["part"]["name"] == "Screw"

    assert client.post("/api/items/mark-ordered", json={"ids": [iid]}).get_json()["count"] == 1
    assert client.get("/api/items").get_json()["items"] == []
    assert len(client.get("/api/items?status=ordered").get_json()["items"]) == 1


def test_bookmarklet_href(client):
    href = client.get("/api/bookmarklet").get_json()["href"]
    assert href.startswith("javascript:") and "/add?" in href
