import io
import os
from datetime import datetime
from pathlib import Path
from urllib.parse import quote

from flask import Flask, jsonify, request, send_file, send_from_directory
from openpyxl import Workbook
from openpyxl.styles import Font

from db import Database
from scraper import LookupError_, Scraper, looks_like_part_number, normalize_part_number

ROOT = Path(__file__).parent
DATA_DIR = Path(os.environ.get("MCM_DATA_DIR", ROOT / "data"))


def part_payload(part, cached):
    image = f"/images/{part['image_file']}" if part.get("image_file") else (part.get("image_url") or "")
    return {
        "part_number": part["part_number"],
        "name": part.get("name") or "",
        "name_options": part.get("name_options") or [part.get("name") or ""],
        "unit": part.get("unit") or "",
        "price": part.get("price") or "",
        "image": image,
        "url": f"https://www.mcmaster.com/{part['part_number']}/",
        "cached": cached,
    }


def create_app(db=None, scraper=None):
    app = Flask(__name__, static_folder=None)
    db = db or Database(DATA_DIR / "orders.db")
    scraper = scraper or Scraper(DATA_DIR)

    @app.get("/")
    @app.get("/add")
    def index():
        return send_from_directory(ROOT / "static", "index.html")

    @app.get("/static/<path:name>")
    def static_files(name):
        return send_from_directory(ROOT / "static", name)

    @app.get("/images/<path:name>")
    def images(name):
        return send_from_directory(scraper.images_dir, name)

    @app.get("/api/lookup")
    def lookup():
        pn = normalize_part_number(request.args.get("pn"))
        if not pn:
            return jsonify(ok=False, error="הכנס מק״ט."), 400
        manual_url = f"https://www.mcmaster.com/{pn}/"
        if not looks_like_part_number(pn):
            return jsonify(ok=False, error=f"'{pn}' לא נראה כמו מק״ט של McMaster (למשל 91251A540).", url=manual_url), 400

        if request.args.get("refresh") != "1":
            cached = db.get_part(pn)
            if cached and cached.get("name"):
                return jsonify(ok=True, part=part_payload(cached, cached=True))

        try:
            found = scraper.lookup(pn)
        except LookupError_ as e:
            return jsonify(ok=False, error=str(e), url=manual_url), 502
        db.save_part(pn, found["name"], found["unit"], found["price"], found["image_url"], found["image_file"], "auto")
        return jsonify(ok=True, part=part_payload(found, cached=False))

    @app.get("/api/items")
    def list_items():
        return jsonify(items=db.list_items(request.args.get("status", "open")))

    @app.post("/api/items")
    def add_item():
        data = request.get_json(force=True) or {}
        pn = normalize_part_number(data.get("part_number"))
        name = (data.get("name") or "").strip()
        try:
            qty = int(data.get("quantity", 1))
        except (TypeError, ValueError):
            qty = 0
        if not pn or not name or qty < 1:
            return jsonify(ok=False, error="חסר מק״ט, שם או כמות תקינה."), 400
        item = {
            "part_number": pn,
            "name": name,
            "unit": (data.get("unit") or "").strip(),
            "price": (data.get("price") or "").strip(),
            "image": (data.get("image") or "").strip(),
            "quantity": qty,
            "requester": (data.get("requester") or "").strip(),
            "note": (data.get("note") or "").strip(),
        }
        # Remember manual / bookmarklet details so the next lookup of this part is instant.
        existing = db.get_part(pn)
        if not existing or existing.get("name") != name or existing.get("unit") != item["unit"]:
            local = item["image"].startswith("/images/")
            db.save_part(
                pn, name, item["unit"], item["price"],
                None if local else item["image"],
                item["image"][len("/images/"):] if local else (existing or {}).get("image_file"),
                data.get("source") or "manual",
            )
        return jsonify(ok=True, id=db.add_item(item))

    @app.patch("/api/items/<int:item_id>")
    def update_item(item_id):
        data = request.get_json(force=True) or {}
        if "quantity" in data:
            try:
                data["quantity"] = int(data["quantity"])
            except (TypeError, ValueError):
                return jsonify(ok=False, error="כמות לא תקינה."), 400
            if data["quantity"] < 1:
                return jsonify(ok=False, error="כמות לא תקינה."), 400
        db.update_item(item_id, data)
        return jsonify(ok=True)

    @app.delete("/api/items/<int:item_id>")
    def delete_item(item_id):
        db.delete_item(item_id)
        return jsonify(ok=True)

    @app.post("/api/items/mark-ordered")
    def mark_ordered():
        ids = (request.get_json(silent=True) or {}).get("ids")
        return jsonify(ok=True, count=db.mark_ordered(ids))

    @app.get("/api/export.xlsx")
    def export():
        status = request.args.get("status", "open")
        items = db.list_items(status)
        wb = Workbook()
        ws = wb.active
        ws.title = "הזמנה" if status == "open" else "הוזמן"
        ws.sheet_view.rightToLeft = True
        headers = ["מק״ט", "שם מוצר", "יחידת מכירה", "כמות", "מחיר ליחידה ($)", "מבקש", "הערה", "נוסף ב", "קישור"]
        ws.append(headers)
        for cell in ws[1]:
            cell.font = Font(bold=True)
        for it in items:
            ws.append([
                it["part_number"], it["name"], it["unit"], it["quantity"], it["price"],
                it["requester"], it["note"], it["created_at"].replace("T", " "),
                f"https://www.mcmaster.com/{it['part_number']}/",
            ])
        for col, width in zip("ABCDEFGHI", (14, 60, 16, 8, 14, 14, 30, 18, 40)):
            ws.column_dimensions[col].width = width
        buf = io.BytesIO()
        wb.save(buf)
        buf.seek(0)
        fname = f"mcmaster-order-{datetime.now():%Y-%m-%d}.xlsx"
        return send_file(buf, as_attachment=True, download_name=fname,
                         mimetype="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")

    @app.get("/api/bookmarklet")
    def bookmarklet():
        extractor = "\n".join(
            line for line in (ROOT / "static" / "extractor.js").read_text(encoding="utf-8").splitlines()
            if not line.strip().startswith("//")
        ).strip()
        target = request.host_url.rstrip("/") + "/add?"
        js = (
            "(()=>{const d=(" + extractor + ")();"
            "const pn=d.partNumber||prompt('Part number?');if(!pn)return;"
            "window.open(" + repr(target) + "+new URLSearchParams({pn:pn,name:d.names[0]||'',"
            "unit:d.unit||'',price:d.price||'',image:d.image||'',src:'bookmarklet'}).toString(),'_blank');})();"
        )
        return jsonify(href="javascript:" + quote(js, safe="(){}[];,:=+!'*/&|.?<>\\$-_~"))

    @app.errorhandler(404)
    def not_found(_):
        if request.path.startswith("/api/"):
            return jsonify(ok=False, error="not found"), 404
        return "Not found", 404

    return app


if __name__ == "__main__":
    host = os.environ.get("MCM_HOST", "127.0.0.1")
    port = int(os.environ.get("MCM_PORT", "5000"))
    print(f"\n  McMaster order list running at http://{'localhost' if host == '127.0.0.1' else host}:{port}\n")
    create_app().run(host=host, port=port, threaded=True)
