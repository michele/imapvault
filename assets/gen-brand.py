"""Generate imapvault logo, banners and social preview as self-contained SVGs
(text converted to paths, so GitHub renders them identically everywhere)."""
import sys, pathlib
from fontTools.ttLib import TTFont
from fontTools.pens.svgPathPen import SVGPathPen
from fontTools.pens.transformPen import TransformPen

FONTDIR = pathlib.Path(sys.argv[1])
OUT = pathlib.Path(sys.argv[2]); OUT.mkdir(parents=True, exist_ok=True)
FONTS = {w: TTFont(FONTDIR / f"jetbrains-mono-latin-{w}-normal.woff2") for w in (400, 700, 800)}

NAVY, NAVY2, NAVY3 = "#0B1D3A", "#11284D", "#1C3966"
AMBER = "#F5A524"
INK_LIGHT, MUTED_DARK = "#F3F5F9", "#9FB3D1"
PAPER, MUTED_LIGHT = "#F6F8FB", "#4A5B78"


def text_path(s, x, y, size, weight, fill):
    """Return (svg <path>, advance width) for string s with baseline at y."""
    f = FONTS[weight]
    upm = f["head"].unitsPerEm
    gs, cmap, hmtx = f.getGlyphSet(), f.getBestCmap(), f["hmtx"]
    k = size / upm
    pen = SVGPathPen(gs)
    cx = 0
    for ch in s:
        g = cmap[ord(ch)]
        tp = TransformPen(pen, (k, 0, 0, -k, x + cx * k, y))
        gs[g].draw(tp)
        cx += hmtx[g][0]
    return f'<path fill="{fill}" d="{pen.getCommands()}"/>', cx * k


def vault(x, y, s):
    """Vault-door-with-envelope glyph, drawn in a 512 box, placed at (x,y) scaled to size s."""
    import math
    bolts = "".join(
        f'<circle cx="{256 + 152 * math.cos(a):.1f}" cy="{256 + 152 * math.sin(a):.1f}" r="9" fill="{AMBER}"/>'
        for a in [i * math.pi / 6 + math.pi / 12 for i in range(12)])
    return f'''<g transform="translate({x},{y}) scale({s / 512})">
  <rect width="512" height="512" rx="104" fill="{NAVY}"/>
  <rect x="6" y="6" width="500" height="500" rx="98" fill="none" stroke="{NAVY3}" stroke-width="4"/>
  <rect x="58" y="176" width="34" height="46" rx="8" fill="{AMBER}"/>
  <rect x="58" y="290" width="34" height="46" rx="8" fill="{AMBER}"/>
  <circle cx="256" cy="256" r="182" fill="{NAVY2}" stroke="{AMBER}" stroke-width="16"/>
  <circle cx="256" cy="256" r="128" fill="none" stroke="{NAVY3}" stroke-width="6"/>
  {bolts}
  <rect x="170" y="200" width="172" height="112" rx="12" fill="{NAVY}" stroke="{AMBER}" stroke-width="12" stroke-linejoin="round"/>
  <polyline points="176,206 256,266 336,206" fill="none" stroke="{AMBER}" stroke-width="12" stroke-linecap="round" stroke-linejoin="round"/>
</g>'''


def svg(w, h, body, title):
    title = title.replace("&", "&amp;")
    return (f'<svg xmlns="http://www.w3.org/2000/svg" width="{w}" height="{h}" viewBox="0 0 {w} {h}" '
            f'role="img" aria-label="{title}"><title>{title}</title>\n{body}\n</svg>\n')


def dotgrid(w, h, color, op):
    return (f'<defs><pattern id="g" width="24" height="24" patternUnits="userSpaceOnUse">'
            f'<circle cx="2" cy="2" r="1.4" fill="{color}"/></pattern></defs>'
            f'<rect width="{w}" height="{h}" fill="url(#g)" opacity="{op}"/>')


TAGLINE = "snapshots & point-in-time restore for IMAP mail"


def banner(dark):
    W, H = 1280, 320
    bg, title, muted = (NAVY, INK_LIGHT, MUTED_DARK) if dark else (PAPER, NAVY, MUTED_LIGHT)
    parts = [f'<rect width="{W}" height="{H}" rx="28" fill="{bg}"/>',
             dotgrid(W, H, NAVY3 if dark else "#C9D3E3", 0.55),
             vault(72, 64, 192)]
    t, _ = text_path("imapvault", 312, 168, 92, 800, title); parts.append(t)
    p, pw = text_path("$ ", 316, 232, 30, 700, AMBER); parts.append(p)
    l, lw = text_path(TAGLINE, 316 + pw, 232, 30, 400, muted); parts.append(l)
    parts.append(f'<rect x="{316 + pw + lw + 8:.1f}" y="206" width="16" height="32" fill="{AMBER}"/>')
    return svg(W, H, "\n".join(parts), "imapvault: " + TAGLINE)


def social():
    W, H = 1280, 640
    parts = [f'<rect width="{W}" height="{H}" fill="{NAVY}"/>', dotgrid(W, H, NAVY3, 0.6),
             vault(96, 150, 300)]
    t, _ = text_path("imapvault", 460, 268, 104, 800, INK_LIGHT); parts.append(t)
    for i, line in enumerate(["snapshots & point-in-time restore", "for IMAP mail"]):
        x, _ = text_path(line, 464, 330 + i * 44, 34, 400, MUTED_DARK); parts.append(x)
    # terminal line
    parts.append(f'<rect x="460" y="430" width="720" height="64" rx="12" fill="{NAVY2}" stroke="{NAVY3}" stroke-width="2"/>')
    p, pw = text_path("$ ", 484, 472, 28, 700, AMBER); parts.append(p)
    c, cw = text_path("imapvault restore --deleted-since 7d", 484 + pw, 472, 28, 400, INK_LIGHT); parts.append(c)
    parts.append(f'<rect x="{484 + pw + cw + 6:.1f}" y="450" width="14" height="28" fill="{AMBER}"/>')
    return svg(W, H, "\n".join(parts), "imapvault: " + TAGLINE)


(OUT / "logo.svg").write_text(svg(512, 512, vault(0, 0, 512), "imapvault logo"))
(OUT / "banner-dark.svg").write_text(banner(True))
(OUT / "banner-light.svg").write_text(banner(False))
(OUT / "social-preview.svg").write_text(social())
print("ok")
