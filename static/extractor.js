// Runs inside a McMaster-Carr product page (by Playwright or by the bookmarklet)
// and returns whatever product details it can find.
//
// McMaster's markup is not public and changes over time, so everything here is
// heuristic: several candidates per field, best guess first. If lookups start
// coming back empty, this is the file to tune (see README, "debug" folder).
() => {
  const clean = (s) => (s || "").replace(/\s+/g, " ").trim();
  const bodyText = clean(document.body ? document.body.innerText : "");

  // --- part number: from the URL path (/91251A540/) ---
  const pathMatch = location.pathname.match(/\/([0-9]{1,6}[A-Za-z][0-9]{1,6})(?:[\/?#]|$)/);
  const partNumber = pathMatch ? pathMatch[1].toUpperCase() : "";

  // --- name candidates ---
  const names = [];
  // McMaster's site-wide marketing text sits in every page's metadata from the
  // first moment; it is never a product name.
  const boilerplate = /complete source for your plant|ship from stock|deliver same or next day|^mcmaster-carr$|^welcome\b|^sign in\b|^log in\b/i;
  const push = (s) => {
    s = clean(s).replace(/\s*\|\s*McMaster-Carr\s*$/i, "").replace(/^McMaster-Carr\s*[-|:]?\s*/i, "");
    if (s && s.length > 3 && s.length < 300 && !boilerplate.test(s) && !names.includes(s)) names.push(s);
  };
  const meta = (sel) => {
    const el = document.querySelector(sel);
    return el ? el.getAttribute("content") : "";
  };
  // Product-detail headings first, then generic headings, then metadata.
  document.querySelectorAll(
    '[class*="ProductDetail"] h1, [class*="ProductDetail"] h2, [class*="prodDtl"] h1, [class*="prodDtl"] h2, [class*="Header"] h1'
  ).forEach((el) => push(el.innerText));
  document.querySelectorAll("h1, h2, h3").forEach((el) => push(el.innerText));
  // How many names come from the page itself (rendered), not from metadata:
  // only those mean the product has actually appeared.
  const headings = names.length;
  push(meta('meta[property="og:title"]'));
  push(meta('meta[name="description"]'));
  push(document.title);

  // --- sale unit + price ("$12.34 per pack of 100", "$1.23 Each", "$4.56 per ft.") ---
  const unitRe =
    /\$\s?([\d,]+\.\d{2})\s*(?:per\s+)?(each|pack of\s+[\d,]+|package of\s+[\d,]+|box of\s+[\d,]+|set of\s+[\d,]+|pair|ft\.?|foot|yd\.?|yard|lb\.?|pound|roll|kit)\b/i;
  let unit = "";
  let unitLoose = false; // found somewhere on the page, not next to the price
  let price = "";
  const m = bodyText.match(unitRe);
  if (m) {
    price = m[1];
    unit = m[2];
  } else {
    const u = bodyText.match(/\b(pack of\s+[\d,]+|package of\s+[\d,]+|box of\s+[\d,]+)\b/i) || bodyText.match(/\b(each)\b/i);
    if (u) {
      unit = u[1];
      unitLoose = true;
    }
  }
  // --- quantity price tiers ("1-11 pairs $28.46", "12 or more $25.93") ---
  const found = [];
  let tierUnit = "";
  const num = (s) => +String(s).replace(/,/g, "");
  const rangeRe = /\b(\d[\d,]*)\s*[-–]\s*(\d[\d,]*)\s*([A-Za-z]+\.?)?\s*\$\s?([\d,]+\.\d{2})/g;
  const openRe = /\b(\d[\d,]*)\s*(?:or more|and (?:over|up)|[-–]\s*over|\+)\s*([A-Za-z]+\.?)?\s*\$\s?([\d,]+\.\d{2})/gi;
  for (const t of bodyText.matchAll(rangeRe)) {
    found.push({ min: num(t[1]), max: num(t[2]), price: num(t[4]) });
    if (t[3] && !tierUnit) tierUnit = t[3];
  }
  for (const t of bodyText.matchAll(openRe)) {
    found.push({ min: num(t[1]), max: 0, price: num(t[3]) });
    if (t[2] && !tierUnit) tierUnit = t[2];
  }
  // Keep only a consistent ladder starting at 1 (1-11, 12-23, 24+); anything
  // else is probably a thread size or dimension that happened to precede a price.
  let tiers = [];
  for (let next = 1; ; ) {
    const t = found.find((x) => x.min === next && x.price > 0 && (x.max === 0 || x.max >= x.min));
    if (!t) break;
    tiers.push(t);
    if (t.max === 0) break;
    next = t.max + 1;
  }
  if (tiers.length) tiers[tiers.length - 1] = { ...tiers[tiers.length - 1], max: 0 }; // last tier has no ceiling
  if (!tiers.length && price) tiers = [{ min: 1, max: 0, price: num(price) }];
  if (!price && tiers.length) price = tiers[0].price.toFixed(2);
  // A price ladder labels its own rows ("1-11 pairs"); trust that over loose text.
  if (tierUnit && (!unit || unitLoose || tiers.length > 1)) unit = tierUnit.replace(/s\.?$/i, "").replace(/\.$/, "");

  unit = clean(unit).replace(/^(\w)/, (c) => c.toUpperCase());

  // --- image: the biggest product-looking picture on the page ---
  // Scored by on-screen size, with bonuses for URLs that look like product
  // images or contain the part number, and for sitting in the product area.
  // The chosen element is tagged data-mcm-img so the app can photograph it if
  // the file itself cannot be downloaded.
  const notPicture = /logo|sprite|icon|blank|spacer|pixel|tracking|badge|flag|arrow|loading/i;
  let image = "";
  let imageReady = false;
  let bestEl = null;
  let bestScore = 0;
  document.querySelectorAll("[data-mcm-img]").forEach((el) => el.removeAttribute("data-mcm-img"));
  document.querySelectorAll("img").forEach((img) => {
    const srcset = (img.getAttribute("srcset") || img.getAttribute("data-srcset") || "").trim().split(/\s+/)[0];
    const src = img.currentSrc || img.getAttribute("src") || img.getAttribute("data-src") || srcset || "";
    if (!src || src.startsWith("data:") || notPicture.test(src) || notPicture.test(img.alt || "")) return;
    const box = img.getBoundingClientRect();
    const w = Math.max(img.naturalWidth || 0, box.width, +img.getAttribute("width") || 0);
    const h = Math.max(img.naturalHeight || 0, box.height, +img.getAttribute("height") || 0);
    if (Math.max(w, h) < 60 || Math.min(w, h) < 30) return;
    let score = w * h;
    if (partNumber && src.toUpperCase().includes(partNumber)) score *= 8;
    if (/ImageCache|WebParts|Contents\/gfx|\/mv\d|product|catalog/i.test(src)) score *= 4;
    if (img.closest('[class*="ProductDetail"], [class*="prodDtl"], [class*="product" i], [id*="product" i], main')) score *= 2;
    if (score > bestScore) {
      bestScore = score;
      bestEl = img;
      image = src;
    }
  });
  if (bestEl) {
    bestEl.setAttribute("data-mcm-img", "1");
    imageReady = bestEl.complete && bestEl.naturalWidth > 0;
  } else {
    // Metadata image only as a last resort (often a site logo).
    const og = meta('meta[property="og:image"]') || "";
    if (og && !notPicture.test(og)) image = og;
  }
  if (image) {
    try {
      image = new URL(image, location.href).href;
    } catch (e) {}
  }

  // blockText: the page says it blocked us. blocked also covers a near-empty
  // page (kept for the bookmarklet, where that means "nothing to read").
  const blockText = /access denied|unusual traffic|are you a robot|captcha|request blocked/i.test(bodyText);
  const blocked = blockText || bodyText.length < 40;
  const notFound = /no (?:products|results) (?:were )?found|not a valid part number|we couldn.t find/i.test(bodyText);

  return { partNumber, names, headings, unit, price, tiers, image, imageReady, url: location.href, blocked, blockText, notFound, textLength: bodyText.length };
}
