const $ = (sel) => document.querySelector(sel);

const state = { part: null };

function toast(msg, isError = false) {
  const t = $("#toast");
  t.textContent = msg;
  t.className = "toast" + (isError ? " error" : "");
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => t.classList.add("hidden"), 3500);
}

function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);
}

async function api(path, opts = {}) {
  const res = await fetch(path, {
    headers: { "Content-Type": "application/json" },
    ...opts,
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  const data = await res.json().catch(() => ({ ok: false, error: `שגיאת שרת (${res.status})` }));
  return data;
}

// ---------- tabs ----------
document.querySelectorAll(".tab").forEach((btn) =>
  btn.addEventListener("click", () => {
    document.querySelectorAll(".tab").forEach((b) => b.classList.toggle("active", b === btn));
    ["add", "history", "help"].forEach((t) => $(`#tab-${t}`).classList.toggle("hidden", t !== btn.dataset.tab));
    if (btn.dataset.tab === "history") loadHistory();
    if (btn.dataset.tab === "help") loadBookmarklet();
  })
);

// ---------- lookup ----------
function packHint(unit) {
  const m = (unit || "").match(/(?:pack|package|box|set) of\s*([\d,]+)/i);
  if (m) return `(מספר חבילות של ${m[1]})`;
  if (/^each$/i.test(unit || "")) return "(יחידות)";
  if (/ft|foot/i.test(unit || "")) return "(רגל)";
  return "";
}

function showItemForm(part) {
  state.part = part;
  $("#f-pn").textContent = part.part_number;
  $("#f-link").href = part.url || `https://www.mcmaster.com/${part.part_number}/`;
  $("#f-name").value = part.name || "";
  $("#name-options").innerHTML = (part.name_options || []).map((n) => `<option value="${esc(n)}">`).join("");
  $("#f-unit").value = part.unit || "";
  $("#f-price").value = part.price || "";
  $("#f-qty").value = 1;
  $("#f-qty-hint").textContent = packHint(part.unit);
  const img = $("#f-img");
  if (part.image) {
    img.src = part.image;
    img.style.display = "";
    $("#f-noimg").style.display = "none";
  } else {
    img.removeAttribute("src");
    img.style.display = "none";
    $("#f-noimg").style.display = "";
  }
  $("#f-requester").value = localStorage.getItem("requester") || "";
  $("#f-note").value = "";
  $("#item-form").classList.remove("hidden");
  (part.name ? $("#f-qty") : $("#f-name")).focus();
  if (part.name) $("#f-qty").select();
}

async function lookup(refresh = false) {
  const pn = $("#pn").value.trim();
  if (!pn) return;
  const status = $("#lookup-status");
  status.className = "status";
  status.textContent = refresh ? "מרענן מ-McMaster…" : "מחפש… (בדיקה ראשונה של מק״ט לוקחת כ-10–20 שניות)";
  $("#lookup-btn").disabled = true;
  try {
    const data = await api(`/api/lookup?pn=${encodeURIComponent(pn)}${refresh ? "&refresh=1" : ""}`);
    if (data.ok) {
      status.textContent = data.part.cached ? "נמצא בזיכרון (נבדק בעבר)." : "נמצא.";
      showItemForm(data.part);
    } else {
      status.className = "status error";
      status.innerHTML =
        `${esc(data.error)}` +
        (data.url ? ` <a href="${esc(data.url)}" target="_blank" rel="noopener">פתח ב-McMaster</a> והשתמש ב<a href="#" data-goto="help">כפתור המהיר</a>, או מלא ידנית למטה.` : "");
      if (data.url) {
        showItemForm({ part_number: pn.replace(/[^0-9a-z]/gi, "").toUpperCase(), url: data.url, name: "", unit: "", price: "", image: "" });
      }
    }
  } catch (e) {
    status.className = "status error";
    status.textContent = "השרת לא זמין.";
  } finally {
    $("#lookup-btn").disabled = false;
  }
}

$("#lookup-form").addEventListener("submit", (e) => {
  e.preventDefault();
  lookup();
});
$("#refresh-btn").addEventListener("click", () => {
  $("#pn").value = state.part?.part_number || $("#pn").value;
  lookup(true);
});
$("#f-unit").addEventListener("input", () => ($("#f-qty-hint").textContent = packHint($("#f-unit").value)));
$("#cancel-btn").addEventListener("click", () => {
  $("#item-form").classList.add("hidden");
  $("#pn").select();
});
document.addEventListener("click", (e) => {
  const goto = e.target.closest("[data-goto]");
  if (goto) {
    e.preventDefault();
    document.querySelector(`.tab[data-tab="${goto.dataset.goto}"]`).click();
  }
});

// ---------- add ----------
$("#item-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const requester = $("#f-requester").value.trim();
  localStorage.setItem("requester", requester);
  const data = await api("/api/items", {
    method: "POST",
    body: {
      part_number: state.part.part_number,
      name: $("#f-name").value.trim(),
      unit: $("#f-unit").value.trim(),
      price: $("#f-price").value.trim(),
      image: state.part.image || "",
      quantity: $("#f-qty").value,
      requester,
      note: $("#f-note").value.trim(),
      source: state.part.source,
    },
  });
  if (!data.ok) return toast(data.error, true);
  toast("נוסף לרשימה.");
  $("#item-form").classList.add("hidden");
  $("#lookup-status").textContent = "";
  $("#pn").value = "";
  $("#pn").focus();
  loadOpen();
});

// ---------- open list ----------
async function loadOpen() {
  const { items } = await api("/api/items?status=open");
  state.open = items;
  $("#open-count").textContent = items.length;
  $("#open-empty").style.display = items.length ? "none" : "";
  $("#open-table").style.display = items.length ? "" : "none";
  $("#open-table tbody").innerHTML = items
    .map(
      (it) => `<tr data-id="${it.id}">
        <td class="img">${it.image ? `<img src="${esc(it.image)}" alt="">` : ""}</td>
        <td dir="ltr"><a href="https://www.mcmaster.com/${esc(it.part_number)}/" target="_blank" rel="noopener">${esc(it.part_number)}</a></td>
        <td dir="ltr" class="name">${esc(it.name)}</td>
        <td dir="ltr">${esc(it.unit)}</td>
        <td><input class="qty" type="number" min="1" value="${it.quantity}"></td>
        <td>${esc(it.requester)}</td>
        <td>${esc(it.note)}</td>
        <td><button class="link del" title="מחק">✕</button></td>
      </tr>`
    )
    .join("");
}

$("#open-table").addEventListener("change", async (e) => {
  if (!e.target.classList.contains("qty")) return;
  const id = e.target.closest("tr").dataset.id;
  const data = await api(`/api/items/${id}`, { method: "PATCH", body: { quantity: e.target.value } });
  if (!data.ok) toast(data.error, true);
  loadOpen();
});

$("#open-table").addEventListener("click", async (e) => {
  if (!e.target.classList.contains("del")) return;
  const tr = e.target.closest("tr");
  if (!confirm(`למחוק את ${tr.children[1].innerText} מהרשימה?`)) return;
  await api(`/api/items/${tr.dataset.id}`, { method: "DELETE" });
  loadOpen();
});

$("#copy-btn").addEventListener("click", async () => {
  const text = (state.open || []).map((it) => `${it.part_number}\t${it.quantity}`).join("\n");
  if (!text) return toast("הרשימה ריקה.", true);
  try {
    await navigator.clipboard.writeText(text);
    toast("הועתק: מק״ט + כמות בכל שורה.");
  } catch {
    prompt("העתק ידנית:", text);
  }
});

$("#ordered-btn").addEventListener("click", async () => {
  const n = (state.open || []).length;
  if (!n) return toast("הרשימה ריקה.", true);
  if (!confirm(`לסמן ${n} פריטים כ"הוזמן" ולנקות את הרשימה?`)) return;
  const ids = state.open.map((it) => it.id); // only what was on screen, not items added meanwhile
  const data = await api("/api/items/mark-ordered", { method: "POST", body: { ids } });
  toast(`${data.count} פריטים הועברו להזמנות קודמות.`);
  loadOpen();
});

// ---------- history ----------
async function loadHistory() {
  const { items } = await api("/api/items?status=ordered");
  $("#history-table tbody").innerHTML = items
    .map(
      (it) => `<tr>
        <td>${esc((it.ordered_at || "").slice(0, 10))}</td>
        <td dir="ltr">${esc(it.part_number)}</td>
        <td dir="ltr" class="name">${esc(it.name)}</td>
        <td dir="ltr">${esc(it.unit)}</td>
        <td>${it.quantity}</td>
        <td>${esc(it.requester)}</td>
        <td>${esc(it.note)}</td>
      </tr>`
    )
    .join("") || `<tr><td colspan="7" class="empty">אין עדיין.</td></tr>`;
}

// ---------- bookmarklet ----------
async function loadBookmarklet() {
  const { href } = await api("/api/bookmarklet");
  $("#bookmarklet").href = href;
}
$("#bookmarklet").addEventListener("click", (e) => {
  e.preventDefault();
  toast("אל תלחץ כאן – גרור את הכפתור לסרגל הסימניות.");
});

// ---------- prefill from bookmarklet (/add?pn=...) ----------
(function prefill() {
  const q = new URLSearchParams(location.search);
  if (location.pathname !== "/add" || !q.get("pn")) return;
  const pn = q.get("pn").replace(/[^0-9a-z]/gi, "").toUpperCase();
  $("#pn").value = pn;
  showItemForm({
    part_number: pn,
    name: q.get("name") || "",
    name_options: [q.get("name") || ""],
    unit: q.get("unit") || "",
    price: q.get("price") || "",
    image: q.get("image") || "",
    url: `https://www.mcmaster.com/${pn}/`,
    source: q.get("src") || "bookmarklet",
  });
  history.replaceState(null, "", "/");
})();

loadOpen();
