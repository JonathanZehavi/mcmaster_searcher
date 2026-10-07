const $ = (sel) => document.querySelector(sel);
const state = { me: null, projects: [], part: null, mine: [], all: [] };

// ---------- helpers ----------
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

const money = (n) => (n > 0 ? "$" + Number(n).toLocaleString("en-US", { minimumFractionDigits: 2, maximumFractionDigits: 2 }) : "—");
const day = (iso) => (iso ? iso.slice(0, 10).split("-").reverse().join("/") : "");

async function api(path, opts = {}) {
  const res = await fetch(path, {
    headers: { "Content-Type": "application/json" },
    ...opts,
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  if (res.status === 401) {
    showAuth();
    throw new Error("unauthorized");
  }
  return res.json().catch(() => ({ ok: false, error: `שגיאת שרת (${res.status})` }));
}

// Same rule as the server: the tier whose range contains qty, else the first.
function unitPriceFor(tiers, qty) {
  if (!tiers || !tiers.length) return 0;
  const t = tiers.find((t) => qty >= t.min && (t.max === 0 || qty <= t.max));
  return (t || tiers[0]).price;
}

function tierLabel(t) {
  return t.max === 0 ? `${t.min}+` : t.min === t.max ? `${t.min}` : `${t.min}–${t.max}`;
}

// ---------- who is this (once per browser) ----------
function showAuth(names = []) {
  $("#app").classList.add("hidden");
  $("#auth").classList.remove("hidden");
  $("#known-names").innerHTML = names.map((n) => `<option value="${esc(n)}">`).join("");
  $("#who-input").focus();
}

function showApp(data) {
  state.me = data.user;
  state.adminMode = !!data.admin_mode;
  state.hasAdmin = !!data.has_admin;
  state.projects = data.projects || [];
  $("#auth").classList.add("hidden");
  $("#app").classList.remove("hidden");
  $("#who-name").textContent = state.me.name;
  document.body.classList.toggle("admin-mode", state.adminMode);
  $("#admin-btn").textContent = state.adminMode ? "יציאה ממצב מנהל" : state.hasAdmin ? "כניסת מנהל" : "הגדרת מנהל";
  // A manager tab that is no longer allowed falls back to the order tab.
  const active = document.querySelector(".tab.active");
  if (!state.adminMode && active && active.classList.contains("admin-only")) document.querySelector('.tab[data-tab="order"]').click();
  fillProjects();
  loadMine();
  prefillFromBookmarklet();
}

async function boot() {
  const data = await api("/api/me");
  if (!data.user) return showAuth(data.names);
  showApp(data);
}

$("#who-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const data = await api("/api/identify", { method: "POST", body: { name: $("#who-input").value } });
  if (!data.ok) return ($("#who-err").textContent = data.error);
  $("#who-err").textContent = "";
  boot();
});

$("#notme-btn").addEventListener("click", async () => {
  if (!confirm(`המחשב ישכח שאתה ${state.me.name}. להמשיך?`)) return;
  await api("/api/forget", { method: "POST" });
  location.href = "/";
});

$("#admin-btn").addEventListener("click", async () => {
  if (state.adminMode) {
    await api("/api/admin/leave", { method: "POST" });
    return boot();
  }
  $("#admin-title").textContent = state.hasAdmin ? "כניסת מנהל" : "הגדרת מנהל ראשון";
  $("#admin-text").textContent = state.hasAdmin
    ? "צריך להיכנס פעם אחת בכל מחשב. המחשב יזכור."
    : `עדיין אין מנהל. בחר סיסמה, ו-${state.me.name} יהיה המנהל: יראה את הרשימה של כולם, את ההיסטוריה ואת הניהול.`;
  $("#admin-pass").value = "";
  $("#admin-dialog").showModal();
});
$("#admin-dialog").addEventListener("close", async () => {
  if ($("#admin-dialog").returnValue !== "ok") return;
  const data = await api("/api/admin/enter", { method: "POST", body: { password: $("#admin-pass").value } });
  if (!data.ok) return toast(data.error, true);
  toast("מצב מנהל פעיל.");
  boot();
});

$("#pw-btn").addEventListener("click", () => {
  $("#pw-old").value = $("#pw-new").value = "";
  $("#pw-dialog").showModal();
});
$("#pw-dialog").addEventListener("close", async () => {
  if ($("#pw-dialog").returnValue !== "ok") return;
  const data = await api("/api/me/password", { method: "POST", body: { old: $("#pw-old").value, new: $("#pw-new").value } });
  toast(data.ok ? "הסיסמה עודכנה." : data.error, !data.ok);
});

// ---------- tabs ----------
const loaders = { order: loadMine, last: loadLast, purchase: loadAll, history: loadOrders, admin: loadAdmin, help: loadBookmarklet };
document.querySelectorAll(".tab").forEach((btn) =>
  btn.addEventListener("click", () => {
    document.querySelectorAll(".tab").forEach((b) => b.classList.toggle("active", b === btn));
    Object.keys(loaders).forEach((t) => $(`#tab-${t}`).classList.toggle("hidden", t !== btn.dataset.tab));
    loaders[btn.dataset.tab]();
  })
);

// ---------- tables ----------
// Columns: key -> [header, cell renderer]. Editable cells carry data-edit.
const COLS = {
  img: ["", (it) => (it.image ? `<img src="${esc(it.image)}" alt="">` : "")],
  pn: ["מק״ט", (it) => `<a dir="ltr" href="https://www.mcmaster.com/${esc(it.part_number)}/" target="_blank" rel="noopener">${esc(it.part_number)}</a>`],
  name: ["תיאור", (it) => `<span dir="ltr">${esc(it.name)}</span>`],
  unit: ["UOM", (it) => `<span dir="ltr">${esc(it.unit)}</span>`],
  price: ["מחיר ליחידה", (it) => money(it.unit_price)],
  qty: ["כמות", (it) => it.quantity],
  qtyEdit: ["כמות", (it) => `<input class="qty" data-edit="quantity" type="number" min="1" value="${it.quantity}">`],
  requester: ["מזמין", (it) => esc(it.requester)],
  date: ["תאריך", (it) => day(it.created_at)],
  project: ["פרויקט", (it) => esc(it.project)],
  purpose: ["מטרה", (it) => esc(it.purpose)],
  total: ["סה״כ", (it) => `<b>${money(it.total)}</b>`],
  del: ["", () => `<button class="link del" title="מחק">✕</button>`],
};

function renderTable(table, items, cols, emptyText) {
  if (!items.length) {
    table.innerHTML = `<tbody><tr><td class="empty">${emptyText}</td></tr></tbody>`;
    return;
  }
  table.innerHTML =
    `<thead><tr>${cols.map((c) => `<th class="c-${c}">${COLS[c][0]}</th>`).join("")}</tr></thead>` +
    `<tbody>${items.map((it) => `<tr data-id="${it.id}">${cols.map((c) => `<td class="c-${c}">${COLS[c][1](it)}</td>`).join("")}</tr>`).join("")}</tbody>`;
}

// Inline edits and deletes on editable tables.
function wireEditable(table, reload) {
  table.addEventListener("change", async (e) => {
    const field = e.target.dataset.edit;
    if (!field) return;
    const id = e.target.closest("tr").dataset.id;
    const data = await api(`/api/items/${id}`, { method: "PATCH", body: { [field]: e.target.value } });
    if (!data.ok) toast(data.error, true);
    reload();
  });
  table.addEventListener("click", async (e) => {
    if (!e.target.classList.contains("del")) return;
    const tr = e.target.closest("tr");
    if (!confirm(`למחוק את ${tr.querySelector(".c-pn").innerText} מהרשימה?`)) return;
    const data = await api(`/api/items/${tr.dataset.id}`, { method: "DELETE" });
    if (!data.ok) toast(data.error, true);
    reload();
  });
}

// ---------- order tab ----------
function fillProjects() {
  const list = state.projects.length ? state.projects : ["כללי"];
  $("#f-project").innerHTML =
    `<option value="">בחר פרויקט…</option>` + list.map((p) => `<option>${esc(p)}</option>`).join("");
  const last = localStorage.getItem("project");
  if (last && list.includes(last)) $("#f-project").value = last;
}

function packHint(unit) {
  const m = (unit || "").match(/(?:pack|package|box|set) of\s*([\d,]+)/i);
  if (m) return `(חבילות של ${m[1]})`;
  if (/^each$/i.test(unit || "")) return "(יחידות)";
  if (/^pair/i.test(unit || "")) return "(זוגות)";
  return "";
}

function currentTiers() {
  const p = state.part;
  if (p && p.tiers && p.tiers.length) return p.tiers;
  const manual = parseFloat(String($("#f-price").value).replace(/[$,\s]/g, ""));
  return manual > 0 ? [{ min: 1, max: 0, price: manual }] : [];
}

function updatePricing() {
  const qty = Math.max(1, parseInt($("#f-qty").value, 10) || 1);
  const p = state.part;
  const hasTiers = p && p.tiers && p.tiers.length > 0;
  const unit = unitPriceFor(currentTiers(), qty);
  $("#f-price").readOnly = hasTiers;
  if (hasTiers) $("#f-price").value = unit.toFixed(2);
  $("#f-total").textContent = unit ? money(unit * qty) : "—";
  const box = $("#f-tiers");
  if (hasTiers && p.tiers.length > 1) {
    box.classList.remove("hidden");
    box.innerHTML =
      "מחיר לפי כמות: " +
      p.tiers
        .map((t) => {
          const active = qty >= t.min && (t.max === 0 || qty <= t.max);
          return `<span class="tier${active ? " active" : ""}" dir="ltr">${tierLabel(t)}: ${money(t.price)}</span>`;
        })
        .join("");
  } else {
    box.classList.add("hidden");
  }
}

function showItemForm(part) {
  state.part = part;
  $("#f-pn").textContent = part.part_number;
  $("#f-link").href = part.url || `https://www.mcmaster.com/${part.part_number}/`;
  $("#f-name").value = part.name || "";
  $("#name-options").innerHTML = (part.name_options || []).map((n) => `<option value="${esc(n)}">`).join("");
  $("#f-unit").value = part.unit || "";
  $("#f-qty").value = 1;
  $("#f-price").value = "";
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
  $("#f-requester").value = state.me.name;
  $("#f-date").textContent = day(new Date().toISOString());
  $("#f-purpose").value = "";
  updatePricing();
  $("#item-form").classList.remove("hidden");
  const qty = $("#f-qty");
  (part.name ? qty : $("#f-name")).focus();
  if (part.name) qty.select();
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
      status.textContent = data.part.cached ? "נמצא (נבדק בעבר)." : "נמצא.";
      showItemForm(data.part);
    } else {
      status.className = "status error";
      status.innerHTML =
        esc(data.error) +
        (data.url ? ` <a href="${esc(data.url)}" target="_blank" rel="noopener">פתח ב-McMaster</a>, השתמש ב<a href="#" data-goto="help">כפתור המהיר</a>, או מלא ידנית למטה.` : "");
      if (data.url) {
        const clean = pn.replace(/[^0-9a-z]/gi, "").toUpperCase();
        showItemForm({ part_number: clean, url: data.url, name: "", unit: "", tiers: [], image: "" });
      }
    }
  } catch (e) {
    if (e.message !== "unauthorized") {
      status.className = "status error";
      status.textContent = "השרת לא זמין.";
    }
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
$("#f-qty").addEventListener("input", updatePricing);
$("#f-price").addEventListener("input", updatePricing);
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

$("#item-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const project = $("#f-project").value;
  if (!project) return toast("בחר פרויקט.", true);
  localStorage.setItem("project", project);
  const tiers = currentTiers();
  const data = await api("/api/items", {
    method: "POST",
    body: {
      part_number: state.part.part_number,
      name: $("#f-name").value.trim(),
      unit: $("#f-unit").value.trim(),
      tiers,
      unit_price: $("#f-price").value,
      image: state.part.image || "",
      quantity: $("#f-qty").value,
      project,
      purpose: $("#f-purpose").value.trim(),
      source: state.part.source,
    },
  });
  if (!data.ok) return toast(data.error, true);
  toast("נוסף לרשימה.");
  $("#item-form").classList.add("hidden");
  $("#lookup-status").textContent = "";
  $("#pn").value = "";
  $("#pn").focus();
  loadMine();
});

async function loadMine() {
  const { items } = await api("/api/items");
  state.mine = items;
  $("#mine-count").textContent = items.length;
  renderTable($("#mine-table"), items, ["img", "pn", "name", "unit", "price", "qtyEdit", "requester", "date", "project", "purpose", "total", "del"], "עוד לא הוספת פריטים.");
}
wireEditable($("#mine-table"), loadMine);

// ---------- my last order ----------
async function loadLast() {
  const { order, items } = await api("/api/my-last-order");
  $("#last-title").textContent = order
    ? `ההזמנה האחרונה שלי – ${day(order.po_date)}${order.po_number ? ` · PO ${order.po_number}` : ""}`
    : "ההזמנה האחרונה שלי";
  renderTable($("#last-table"), items, ["img", "pn", "name", "unit", "price", "qty", "requester", "date", "project", "purpose", "total"], "עוד לא בוצעה הזמנה עם פריטים שלך.");
}

// ---------- purchasing (admin) ----------
async function loadAll() {
  const { items } = await api("/api/items?scope=all");
  state.all = items;
  $("#all-count").textContent = items.length;
  renderTable($("#all-table"), items, ["img", "pn", "name", "unit", "price", "qtyEdit", "requester", "date", "project", "purpose", "total", "del"], "הרשימה ריקה.");
}
wireEditable($("#all-table"), loadAll);

$("#copy-btn").addEventListener("click", async () => {
  const text = state.all.map((it) => `${it.part_number}\t${it.quantity}`).join("\n");
  if (!text) return toast("הרשימה ריקה.", true);
  try {
    await navigator.clipboard.writeText(text);
    toast("הועתק: מק״ט + כמות בכל שורה.");
  } catch {
    prompt("העתק ידנית:", text);
  }
});

$("#place-btn").addEventListener("click", () => {
  if (!state.all.length) return toast("הרשימה ריקה.", true);
  $("#place-summary").textContent = `${state.all.length} שורות יועברו להיסטוריה והרשימה תתאפס.`;
  $("#po-number").value = "";
  $("#po-date").value = new Date().toISOString().slice(0, 10);
  $("#place-dialog").showModal();
});

$("#place-dialog").addEventListener("close", async () => {
  if ($("#place-dialog").returnValue !== "ok") return;
  // Only what was on screen, not lines added meanwhile.
  const ids = state.all.map((it) => it.id);
  const data = await api("/api/orders", { method: "POST", body: { ids, po_number: $("#po-number").value, po_date: $("#po-date").value } });
  if (!data.ok) return toast(data.error, true);
  toast(`ההזמנה נסגרה: ${data.order.items} פריטים.`);
  loadAll();
});

// ---------- history (admin) ----------
async function loadOrders() {
  const { orders } = await api("/api/orders");
  const t = $("#orders-table");
  if (!orders.length) {
    t.innerHTML = `<tbody><tr><td class="empty">עוד לא בוצעו הזמנות.</td></tr></tbody>`;
    return;
  }
  t.innerHTML =
    `<thead><tr><th>תאריך</th><th>PO</th><th>שורות</th><th>בוצע ע״י</th><th></th></tr></thead><tbody>` +
    orders
      .map(
        (o) => `<tr data-order="${o.id}"><td>${day(o.po_date)}</td><td dir="ltr">${esc(o.po_number) || "—"}</td><td>${o.items}</td>
        <td>${esc(o.ordered_by)}</td>
        <td><button class="link view">הצג</button> <a class="link" href="/api/export.xlsx?order=${o.id}">אקסל</a></td></tr>`
      )
      .join("") +
    `</tbody>`;
}

$("#orders-table").addEventListener("click", async (e) => {
  if (!e.target.classList.contains("view")) return;
  const id = e.target.closest("tr").dataset.order;
  const { order, items } = await api(`/api/orders/${id}`);
  $("#order-title").textContent = `הזמנה מ-${day(order.po_date)}${order.po_number ? ` · PO ${order.po_number}` : ""}`;
  $("#order-export").href = `/api/export.xlsx?order=${order.id}`;
  renderTable($("#order-table"), items, ["img", "pn", "name", "unit", "price", "qty", "requester", "date", "project", "purpose", "total"], "אין פריטים.");
  $("#order-detail").classList.remove("hidden");
  $("#order-detail").scrollIntoView({ behavior: "smooth" });
});

// ---------- admin: users & projects ----------
async function loadAdmin() {
  const { users } = await api("/api/users");
  state.users = users;
  $("#users-table").innerHTML =
    `<thead><tr><th>שם</th><th>תפקיד</th><th>סטטוס</th><th></th></tr></thead><tbody>` +
    users
      .map(
        (u) => `<tr data-user="${u.id}" class="${u.active ? "" : "inactive"}">
        <td dir="ltr">${esc(u.name)}</td>
        <td><select class="role"><option value="user"${u.role === "user" ? " selected" : ""}>משתמש</option><option value="admin"${u.role === "admin" ? " selected" : ""}>מנהל / רכש</option></select></td>
        <td>${u.active ? "פעיל" : "מושבת"}</td>
        <td>${u.role === "admin" ? `<button class="link reset">קבע סיסמת מנהל</button>` : ""} <button class="link toggle">${u.active ? "השבת" : "הפעל"}</button></td></tr>`
      )
      .join("") +
    `</tbody>`;
  renderProjects();
}

async function patchUser(id, body) {
  const data = await api(`/api/users/${id}`, { method: "PATCH", body });
  if (!data.ok) toast(data.error, true);
  loadAdmin();
}

$("#users-table").addEventListener("change", (e) => {
  if (!e.target.classList.contains("role")) return;
  const id = e.target.closest("tr").dataset.user;
  const u = state.users.find((x) => String(x.id) === id);
  const body = { role: e.target.value };
  if (e.target.value === "admin" && !u.has_password) {
    const pw = prompt(`סיסמת מנהל ל-${u.name} (לפחות 4 תווים):`);
    if (!pw) return loadAdmin();
    body.password = pw;
  }
  patchUser(id, body);
});
$("#users-table").addEventListener("click", (e) => {
  const tr = e.target.closest("tr");
  if (e.target.classList.contains("toggle")) patchUser(tr.dataset.user, { active: tr.classList.contains("inactive") });
  if (e.target.classList.contains("reset")) {
    const pw = prompt("סיסמת מנהל חדשה (לפחות 4 תווים):");
    if (pw) patchUser(tr.dataset.user, { password: pw });
  }
});

function renderProjects() {
  $("#projects-list").innerHTML =
    state.projects.map((p) => `<span class="chip">${esc(p)} <button class="link x" data-p="${esc(p)}" title="הסר">✕</button></span>`).join("") ||
    `<span class="muted">אין עדיין פרויקטים.</span>`;
}

async function saveProjects(list) {
  const data = await api("/api/projects", { method: "PUT", body: { projects: list } });
  if (!data.ok) return toast(data.error, true);
  state.projects = data.projects;
  renderProjects();
  fillProjects();
}

$("#project-form").addEventListener("submit", (e) => {
  e.preventDefault();
  const name = $("#p-name").value.trim();
  if (!name) return;
  $("#p-name").value = "";
  saveProjects([...state.projects, name]);
});
$("#projects-list").addEventListener("click", (e) => {
  if (e.target.classList.contains("x") && confirm(`להסיר את הפרויקט "${e.target.dataset.p}"?`)) {
    saveProjects(state.projects.filter((p) => p !== e.target.dataset.p));
  }
});

// ---------- bookmarklet ----------
async function loadBookmarklet() {
  const { href } = await api("/api/bookmarklet");
  $("#bookmarklet").href = href;
}
$("#bookmarklet").addEventListener("click", (e) => {
  e.preventDefault();
  toast("אל תלחץ כאן – גרור את הכפתור לסרגל הסימניות.");
});

// /add?pn=...: opened by the bookmarklet from a McMaster page.
function prefillFromBookmarklet() {
  const q = new URLSearchParams(location.search);
  if (location.pathname !== "/add" || !q.get("pn")) return;
  const pn = q.get("pn").replace(/[^0-9a-z]/gi, "").toUpperCase();
  let tiers = [];
  try {
    tiers = JSON.parse(q.get("tiers") || "[]");
  } catch {}
  $("#pn").value = pn;
  showItemForm({
    part_number: pn,
    name: q.get("name") || "",
    name_options: [q.get("name") || ""],
    unit: q.get("unit") || "",
    tiers,
    image: q.get("image") || "",
    url: `https://www.mcmaster.com/${pn}/`,
    source: q.get("src") || "bookmarklet",
  });
  history.replaceState(null, "", "/");
}

boot();
