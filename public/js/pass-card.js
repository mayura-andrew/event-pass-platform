// =============================================================================
// Printable delegate pass: composes the 70 x 110 mm Colombo template with the
// delegate's QR code (Colombo mark in the centre), membership ID and team.
// =============================================================================
// Output: a PNG for phones and a print-ready PDF at exact physical size. Built
// on a canvas so it also works offline once the assets are cached by sw.js.
(() => {
  'use strict';

  // Template geometry in millimetres, measured from public/assets/qr-template.pdf.
  // The background JPEG is that PDF rendered at 600 dpi, so 1 mm = 600/25.4 px.
  const CARD_MM = { w: 70, h: 110 };
  const QR_BOX_MM = { x: 20.12, y: 40.05, size: 29.84 };            // white 30 x 30 mm square
  const ID_BOX_MM = { x: 8.37, y: 77.40, w: 53.26, h: 7.03 };        // first white strip
  const TEAM_BOX_MM = { x: 8.37, y: 90.78, w: 53.26, h: 7.03 };      // second white strip

  const ASSETS = {
    background: '/assets/pass-template.jpg',
    mark: '/assets/colombo-mark.png',
  };

  // QR layout. Quiet zone sits inside the white square, which is surrounded by
  // the dark background, so it must be generous enough for scanners.
  const QUIET_MODULES = 3;
  // Share of the QR width covered by the logo plate. Level H restores up to
  // 30% damaged codewords; ~23% of the width is ~5% of the area.
  const LOGO_FRACTION = 0.23;

  const INK = '#0D173E';        // template navy, readable on the white strips
  const LABEL_INK = '#5A6488';
  const FONT = '"Inter", "Atkinson Hyperlegible", system-ui, sans-serif';

  // ---------------------------------------------------------------------------
  // Asset loading
  // ---------------------------------------------------------------------------

  const imageCache = new Map();

  function loadImage(src) {
    if (!imageCache.has(src)) {
      imageCache.set(src, new Promise((resolve, reject) => {
        const img = new Image();
        img.onload = () => resolve(img);
        img.onerror = () => {
          imageCache.delete(src); // allow a retry once back online
          reject(new Error(`Could not load ${src}`));
        };
        img.src = src;
      }));
    }
    return imageCache.get(src);
  }

  async function loadFonts() {
    // Canvas text uses whatever font is ready; wait so the first render isn't
    // drawn in the fallback face. Offline without a cached font, fall back.
    if (!document.fonts?.load) return;
    try {
      await Promise.race([
        Promise.all([
          document.fonts.load(`800 40px ${FONT}`),
          document.fonts.load(`600 40px ${FONT}`),
        ]),
        new Promise((r) => setTimeout(r, 2500)),
      ]);
    } catch { /* fallback font is fine */ }
  }

  // ---------------------------------------------------------------------------
  // QR matrix
  // ---------------------------------------------------------------------------

  /**
   * Builds the QR module matrix with the vendored node-qrcode 1.5.4 encoder
   * (vendor/qrcode-gen-1.5.4.min.js). Not qrcodejs 1.0.0, which the on-screen
   * pass uses: at level H it picks version 11 for this payload and the result
   * fails ZXing (the decoder html5-qrcode uses) at several sizes, while
   * node-qrcode's version 10 code decodes reliably.
   */
  function qrMatrix(text) {
    const { modules } = QRMatrix.create(text, { errorCorrectionLevel: 'H' }); // H: room for the centre logo
    const n = modules.size;
    const rows = [];
    for (let r = 0; r < n; r++) {
      const row = new Array(n);
      for (let c = 0; c < n; c++) row[c] = Boolean(modules.get(r, c));
      rows.push(row);
    }
    return rows;
  }

  // ---------------------------------------------------------------------------
  // Drawing
  // ---------------------------------------------------------------------------

  function roundRect(ctx, x, y, w, h, r) {
    ctx.beginPath();
    ctx.moveTo(x + r, y);
    ctx.arcTo(x + w, y, x + w, y + h, r);
    ctx.arcTo(x + w, y + h, x, y + h, r);
    ctx.arcTo(x, y + h, x, y, r);
    ctx.arcTo(x, y, x + w, y, r);
    ctx.closePath();
  }

  function drawQR(ctx, matrix, box, mark) {
    const n = matrix.length;
    // Whole-pixel modules keep every edge sharp; centre what's left over.
    const module = Math.floor(box.size / (n + QUIET_MODULES * 2));
    const size = module * n;
    const x0 = Math.round(box.x + (box.size - size) / 2);
    const y0 = Math.round(box.y + (box.size - size) / 2);

    ctx.fillStyle = '#FFFFFF';
    ctx.fillRect(x0 - module * QUIET_MODULES, y0 - module * QUIET_MODULES,
      size + module * QUIET_MODULES * 2, size + module * QUIET_MODULES * 2);

    ctx.fillStyle = '#000000';
    for (let r = 0; r < n; r++) {
      for (let c = 0; c < n; c++) {
        if (matrix[r][c]) ctx.fillRect(x0 + c * module, y0 + r * module, module, module);
      }
    }

    // Logo plate: a whole number of modules, odd so it sits dead centre.
    let plateModules = Math.round(n * LOGO_FRACTION);
    if (plateModules % 2 === 0) plateModules += 1;
    const plate = plateModules * module;
    const px = x0 + ((n - plateModules) / 2) * module;
    const py = y0 + ((n - plateModules) / 2) * module;
    ctx.fillStyle = '#FFFFFF';
    roundRect(ctx, px, py, plate, plate, module * 1.5);
    ctx.fill();

    const pad = module * 1.2;
    const inner = plate - pad * 2;
    const scale = Math.min(inner / mark.width, inner / mark.height);
    const mw = mark.width * scale;
    const mh = mark.height * scale;
    ctx.imageSmoothingEnabled = true;
    ctx.imageSmoothingQuality = 'high';
    ctx.drawImage(mark, px + (plate - mw) / 2, py + (plate - mh) / 2, mw, mh);
  }

  /**
   * Draws "LABEL  value" centred in a strip, shrinking the value to fit and
   * ellipsizing only if it still doesn't fit at the minimum size.
   */
  function drawField(ctx, box, label, value, pxPerMm) {
    const padX = 2.5 * pxPerMm;
    const maxW = box.w - padX * 2;
    const gap = 1.8 * pxPerMm;
    const labelSize = 2.5 * pxPerMm;
    let valueSize = 4.3 * pxPerMm;          // ~3.1 mm cap height
    const minValueSize = 2.8 * pxPerMm;

    ctx.textBaseline = 'alphabetic';
    ctx.font = `600 ${labelSize}px ${FONT}`;
    const labelW = ctx.measureText(label).width;

    let text = value;
    const fits = () => {
      ctx.font = `800 ${valueSize}px ${FONT}`;
      return labelW + gap + ctx.measureText(text).width <= maxW;
    };
    while (!fits() && valueSize > minValueSize) valueSize -= 0.1 * pxPerMm;
    if (!fits()) {
      while (text.length > 1 && !fits()) text = text.slice(0, -1);
      text = `${text.slice(0, -1).trimEnd()}…`;
    }

    ctx.font = `800 ${valueSize}px ${FONT}`;
    const valueW = ctx.measureText(text).width;
    const startX = box.x + (box.w - (labelW + gap + valueW)) / 2;
    // Optical centre: baseline sits ~0.36 em below the strip's midline.
    const baseline = box.y + box.h / 2 + valueSize * 0.36;

    ctx.fillStyle = LABEL_INK;
    ctx.font = `600 ${labelSize}px ${FONT}`;
    ctx.fillText(label, startX, baseline);

    ctx.fillStyle = INK;
    ctx.font = `800 ${valueSize}px ${FONT}`;
    ctx.fillText(text, startX + labelW + gap, baseline);
  }

  /**
   * Renders the card for one delegate.
   * @param {{id:string, qr_signature:string, membership_id:string, team_name?:string}} data
   * @returns {Promise<HTMLCanvasElement>}
   */
  async function render(data) {
    const [bg, mark] = await Promise.all([
      loadImage(ASSETS.background),
      loadImage(ASSETS.mark),
      loadFonts(),
    ]);

    const canvas = document.createElement('canvas');
    canvas.width = bg.naturalWidth;
    canvas.height = bg.naturalHeight;
    const pxPerMm = canvas.width / CARD_MM.w;
    const mm = (b) => Object.fromEntries(Object.entries(b).map(([k, v]) => [k, v * pxPerMm]));

    const ctx = canvas.getContext('2d');
    ctx.drawImage(bg, 0, 0);

    // Same universal payload as the on-screen QR and the scanner expects.
    const payload = JSON.stringify({ id: data.id, sig: data.qr_signature });
    ctx.imageSmoothingEnabled = false;
    drawQR(ctx, qrMatrix(payload), mm(QR_BOX_MM), mark);

    drawField(ctx, mm(ID_BOX_MM), 'ID', data.membership_id, pxPerMm);
    drawField(ctx, mm(TEAM_BOX_MM), 'TEAM', data.team_name || 'No team', pxPerMm);
    return canvas;
  }

  // ---------------------------------------------------------------------------
  // Export
  // ---------------------------------------------------------------------------

  const toBlob = (canvas, type, quality) =>
    new Promise((resolve, reject) => canvas.toBlob((b) => (b ? resolve(b) : reject(new Error('Export failed'))), type, quality));

  const toPNG = (canvas) => toBlob(canvas, 'image/png');

  /**
   * One-page PDF at exactly 70 x 110 mm containing the card as a JPEG. Written
   * by hand: a single DCT image needs no PDF library.
   */
  async function toPDF(canvas) {
    const jpeg = new Uint8Array(await (await toBlob(canvas, 'image/jpeg', 0.95)).arrayBuffer());
    const wPt = (CARD_MM.w / 25.4 * 72).toFixed(3);
    const hPt = (CARD_MM.h / 25.4 * 72).toFixed(3);
    const enc = new TextEncoder();
    const content = `q ${wPt} 0 0 ${hPt} 0 0 cm /Im0 Do Q\n`;

    const parts = [];
    const offsets = [];
    let length = 0;
    const push = (chunk) => {
      const bytes = typeof chunk === 'string' ? enc.encode(chunk) : chunk;
      parts.push(bytes);
      length += bytes.length;
    };
    const obj = (num, body) => { offsets[num] = length; push(`${num} 0 obj\n${body}\nendobj\n`); };

    // The binary comment line tells tools the file contains binary data.
    push('%PDF-1.4\n%\xE2\xE3\xCF\xD3\n');
    obj(1, '<< /Type /Catalog /Pages 2 0 R >>');
    obj(2, '<< /Type /Pages /Kids [3 0 R] /Count 1 >>');
    obj(3, `<< /Type /Page /Parent 2 0 R /MediaBox [0 0 ${wPt} ${hPt}] /TrimBox [0 0 ${wPt} ${hPt}] ` +
      '/Resources << /XObject << /Im0 4 0 R >> >> /Contents 5 0 R >>');
    offsets[4] = length;
    push(`4 0 obj\n<< /Type /XObject /Subtype /Image /Width ${canvas.width} /Height ${canvas.height} ` +
      `/ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /DCTDecode /Length ${jpeg.length} >>\nstream\n`);
    push(jpeg);
    push('\nendstream\nendobj\n');
    obj(5, `<< /Length ${enc.encode(content).length} >>\nstream\n${content}endstream`);
    obj(6, '<< /Title (NASA Space Apps Colombo delegate pass) /Producer (nsac-qr) >>');

    const xref = length;
    let table = 'xref\n0 7\n0000000000 65535 f \n';
    for (let i = 1; i <= 6; i++) table += `${String(offsets[i]).padStart(10, '0')} 00000 n \n`;
    push(`${table}trailer\n<< /Size 7 /Root 1 0 R /Info 6 0 R >>\nstartxref\n${xref}\n%%EOF\n`);

    return new Blob(parts, { type: 'application/pdf' });
  }

  /** Triggers a download of blob as filename. */
  function download(blob, filename) {
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = filename;
    document.body.append(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 30_000);
  }

  window.NSACPassCard = { render, toPNG, toPDF, download };
})();
