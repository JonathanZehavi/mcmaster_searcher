import sqlite3
import threading
from contextlib import contextmanager
from datetime import datetime
from pathlib import Path

SCHEMA = """
CREATE TABLE IF NOT EXISTS parts (
    part_number TEXT PRIMARY KEY,
    name        TEXT,
    unit        TEXT,
    price       TEXT,
    image_url   TEXT,
    image_file  TEXT,
    source      TEXT,
    fetched_at  TEXT
);
CREATE TABLE IF NOT EXISTS items (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    part_number TEXT NOT NULL,
    name        TEXT NOT NULL,
    unit        TEXT,
    price       TEXT,
    image       TEXT,
    quantity    INTEGER NOT NULL DEFAULT 1,
    requester   TEXT,
    note        TEXT,
    status      TEXT NOT NULL DEFAULT 'open',
    created_at  TEXT NOT NULL,
    ordered_at  TEXT
);
"""

ITEM_FIELDS = ("part_number", "name", "unit", "price", "image", "quantity", "requester", "note")


def now():
    return datetime.now().isoformat(timespec="seconds")


class Database:
    def __init__(self, path):
        self.path = str(path)
        Path(self.path).parent.mkdir(parents=True, exist_ok=True)
        self._lock = threading.Lock()
        with self.conn() as c:
            c.executescript(SCHEMA)

    @contextmanager
    def conn(self):
        with self._lock:
            c = sqlite3.connect(self.path)
            c.row_factory = sqlite3.Row
            try:
                yield c
                c.commit()
            finally:
                c.close()

    # --- part cache ---
    def get_part(self, pn):
        with self.conn() as c:
            row = c.execute("SELECT * FROM parts WHERE part_number = ?", (pn,)).fetchone()
            return dict(row) if row else None

    def save_part(self, pn, name, unit, price, image_url, image_file, source):
        with self.conn() as c:
            c.execute(
                """INSERT INTO parts (part_number, name, unit, price, image_url, image_file, source, fetched_at)
                   VALUES (?, ?, ?, ?, ?, ?, ?, ?)
                   ON CONFLICT(part_number) DO UPDATE SET
                     name=excluded.name, unit=excluded.unit, price=excluded.price,
                     image_url=excluded.image_url, image_file=excluded.image_file,
                     source=excluded.source, fetched_at=excluded.fetched_at""",
                (pn, name, unit, price, image_url, image_file, source, now()),
            )

    # --- order list ---
    def list_items(self, status="open"):
        with self.conn() as c:
            if status == "ordered":
                rows = c.execute(
                    "SELECT * FROM items WHERE status = 'ordered' ORDER BY ordered_at DESC, id DESC LIMIT 500"
                ).fetchall()
            else:
                rows = c.execute("SELECT * FROM items WHERE status = ? ORDER BY id", (status,)).fetchall()
            return [dict(r) for r in rows]

    def add_item(self, data):
        values = {k: data.get(k) for k in ITEM_FIELDS}
        with self.conn() as c:
            cur = c.execute(
                f"INSERT INTO items ({', '.join(ITEM_FIELDS)}, created_at) "
                f"VALUES ({', '.join('?' for _ in ITEM_FIELDS)}, ?)",
                (*values.values(), now()),
            )
            return cur.lastrowid

    def update_item(self, item_id, data):
        fields = {k: data[k] for k in ("quantity", "name", "unit", "requester", "note") if k in data}
        if not fields:
            return
        with self.conn() as c:
            c.execute(
                f"UPDATE items SET {', '.join(f'{k} = ?' for k in fields)} WHERE id = ? AND status = 'open'",
                (*fields.values(), item_id),
            )

    def delete_item(self, item_id):
        with self.conn() as c:
            c.execute("DELETE FROM items WHERE id = ? AND status = 'open'", (item_id,))

    def mark_ordered(self, ids=None):
        with self.conn() as c:
            if ids:
                q = f"UPDATE items SET status='ordered', ordered_at=? WHERE status='open' AND id IN ({','.join('?' for _ in ids)})"
                return c.execute(q, (now(), *ids)).rowcount
            return c.execute("UPDATE items SET status='ordered', ordered_at=? WHERE status='open'", (now(),)).rowcount
