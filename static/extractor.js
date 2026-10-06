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
  const push = (s) => {
    s = clean(s).replace(/\s*\|\s*McMaster-Carr\s*$/i, "").replace(/^McMaster-Carr\s*[-|:]?\s*/i, "");
    if (s && s.length > 3 && s.length < 300 && !/^mcmaster-carr$/i.test(s) && !names.includes(s)) names.push(s);
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
  push(meta('meta[property="og:title"]'));
  push(meta('meta[name="description"]'));
  push(document.title);

  // --- sale unit + price ("$12.34 per pack of 100", "$1.23 Each", "$4.56 per ft.") ---
  const unitRe =
    /\$\s?([\d,]+\.\d{2})\s*(?:per\s+)?(each|pack of\s+[\d,]+|package of\s+[\d,]+|box of\s+[\d,]+|set of\s+[\d,]+|pair|ft\.?|foot|yd\.?|yard|lb\.?|pound|roll|kit)\b/i;
  let unit = "";
  let price = "";
  const m = bodyText.match(unitRe);
  if (m) {
    price = m[1];
    unit = m[2];
  } else {
    const u = bodyText.match(/\b(pack of\s+[\d,]+|package of\s+[\d,]+|box of\s+[\d,]+)\b/i) || bodyText.match(/\b(each)\b/i);
    if (u) unit = u[1];
  }
  unit = clean(unit).replace(/^(\w)/, (c) => c.toUpperCase());

  // --- image: og:image, else the largest product-looking image ---
  let image = meta('meta[property="og:image"]') || "";
  if (!image) {
    let best = null;
    let bestArea = 0;
    document.querySelectorAll("img").forEach((img) => {
      const src = img.currentSrc || img.src || "";
      if (!src || src.startsWith("data:") || /logo|sprite|icon/i.test(src)) return;
      const area = (img.naturalWidth || img.width || 0) * (img.naturalHeight || img.height || 0);
      const bonus = /ImageCache|\/mv\d|product/i.test(src) ? 4 : 1;
      if (area * bonus > bestArea) {
        bestArea = area * bonus;
        best = src;
      }
    });
    if (best) image = best;
  }
  if (image) {
    try {
      image = new URL(image, location.href).href;
    } catch (e) {}
  }

  const blocked =
    /access denied|unusual traffic|are you a robot|captcha|request blocked/i.test(bodyText) || bodyText.length < 40;
  const notFound = /no (?:products|results) (?:were )?found|not a valid part number|we couldn.t find/i.test(bodyText);

  return { partNumber, names, unit, price, image, url: location.href, blocked, notFound, textLength: bodyText.length };
}
