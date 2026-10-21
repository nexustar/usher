#!/usr/bin/env python3
"""Generate all icon variants from icon-raw.svg (1280x1280 canvas, content in center 1024x1024).

Source: icon-raw.svg (1280x1280, coordinates offset +128)
Outputs:
  static/icons/icon.svg              — Apple corners, 0-1024 coordinates (shifted -128)
  static/icons/icon-192.png          — 192x192 RGBA, Apple corners + padding
  static/icons/icon-512.png          — 512x512 RGBA, Apple corners + padding
  static/icons/icon-maskable-512.png — 512x512 RGB full bleed
  static/icons/apple-touch-icon.png  — 180x180 RGB, no clip (iOS masks)
"""

import os, io, re
import cairosvg
from PIL import Image

SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
RAW_SVG = os.path.join(SCRIPT_DIR, "icon-raw.svg")
ICONS_DIR = os.path.join(SCRIPT_DIR, "..", "static", "icons")

CANVAS = 1280
CONTENT = 1024
OFFSET = (CANVAS - CONTENT) // 2  # 128

C1, C2, C3, C4 = 1.52866498, 1.08849296, 0.86840694, 0.63149379
C5, C6, C7 = 0.37282383, 0.16905956, 0.07491139


def apple_rect_path(x, y, w, h, r):
    r = min(r, min(w, h) / 2 / C1)
    ext = r * C1
    p = []
    p.append(f"M{x+ext:.2f} {y:.2f}")
    p.append(f"L{x+w-ext:.2f} {y:.2f}")
    ox, oy = x + w, y
    p.append(f"C{ox-r*C2:.2f} {oy:.2f} {ox-r*C3:.2f} {oy:.2f} {ox-r*C4:.2f} {oy+r*C7:.2f}")
    p.append(f"C{ox-r*C5:.2f} {oy+r*C6:.2f} {ox-r*C6:.2f} {oy+r*C5:.2f} {ox-r*C7:.2f} {oy+r*C4:.2f}")
    p.append(f"C{ox:.2f} {oy+r*C3:.2f} {ox:.2f} {oy+r*C2:.2f} {ox:.2f} {oy+r*C1:.2f}")
    p.append(f"L{x+w:.2f} {y+h-ext:.2f}")
    ox, oy = x + w, y + h
    p.append(f"C{ox:.2f} {oy-r*C2:.2f} {ox:.2f} {oy-r*C3:.2f} {ox-r*C7:.2f} {oy-r*C4:.2f}")
    p.append(f"C{ox-r*C6:.2f} {oy-r*C5:.2f} {ox-r*C5:.2f} {oy-r*C6:.2f} {ox-r*C4:.2f} {oy-r*C7:.2f}")
    p.append(f"C{ox-r*C3:.2f} {oy:.2f} {ox-r*C2:.2f} {oy:.2f} {ox-r*C1:.2f} {oy:.2f}")
    p.append(f"L{x+ext:.2f} {y+h:.2f}")
    ox, oy = x, y + h
    p.append(f"C{ox+r*C2:.2f} {oy:.2f} {ox+r*C3:.2f} {oy:.2f} {ox+r*C4:.2f} {oy-r*C7:.2f}")
    p.append(f"C{ox+r*C5:.2f} {oy-r*C6:.2f} {ox+r*C6:.2f} {oy-r*C5:.2f} {ox+r*C7:.2f} {oy-r*C4:.2f}")
    p.append(f"C{ox:.2f} {oy-r*C3:.2f} {ox:.2f} {oy-r*C2:.2f} {ox:.2f} {oy-r*C1:.2f}")
    p.append(f"L{x:.2f} {y+ext:.2f}")
    ox, oy = x, y
    p.append(f"C{ox:.2f} {oy+r*C2:.2f} {ox:.2f} {oy+r*C3:.2f} {ox+r*C7:.2f} {oy+r*C4:.2f}")
    p.append(f"C{ox+r*C6:.2f} {oy+r*C5:.2f} {ox+r*C5:.2f} {oy+r*C6:.2f} {ox+r*C4:.2f} {oy+r*C7:.2f}")
    p.append(f"C{ox+r*C3:.2f} {oy:.2f} {ox+r*C2:.2f} {oy:.2f} {ox+r*C1:.2f} {oy:.2f}")
    p.append("Z")
    return " ".join(p)


def shift_path_d(d, dx):
    """Shift all absolute coordinates in a path d string by dx, preserving original spacing."""
    def shift_num(m):
        val = m.group(0)
        v = float(val) + dx
        return f"{v:.2f}" if "." in val else str(int(v))

    return re.sub(r"[-+]?\d*\.?\d+", shift_num, d)


def shift_svg(svg_text, dx):
    """Shift coordinate attributes and path d values by dx. Leaves r/width/height/stroke-width/id unchanged."""
    coord_attrs = re.compile(r'\b(cx|cy|x1|y1|x2|y2)="([-+]?\d*\.?\d+)"')

    def shift_attr(m):
        name, val = m.group(1), m.group(2)
        new_val = float(val) + dx
        fmt = f"{new_val:.2f}" if "." in val else str(int(new_val))
        return f'{name}="{fmt}"'

    result = coord_attrs.sub(shift_attr, svg_text)

    # Shift bare x= and y= but not inside other attr names (cx, cy, rx, etc.)
    bare_xy = re.compile(r'(?<![a-zA-Z])(x|y)="([-+]?\d*\.?\d+)"')
    result = bare_xy.sub(shift_attr, result)

    result = re.sub(r' d="([^"]+)"', lambda m: f' d="{shift_path_d(m.group(1), dx)}"', result)
    return result


def build_shifted_body(raw_svg_text):
    defs_match = re.search(r"<defs>(.*?)</defs>", raw_svg_text, re.DOTALL)
    defs_inner = shift_svg(defs_match.group(1), -OFFSET) if defs_match else ""

    body_match = re.search(r"</defs>\s*(.*?)\s*</svg>", raw_svg_text, re.DOTALL)
    body = shift_svg(body_match.group(1), -OFFSET) if body_match else ""

    body = re.sub(
        r'<rect x="[^"]*" y="[^"]*" width="[^"]*" height="[^"]*" fill="#1F1F20"/>',
        '<rect x="0" y="0" width="1024" height="1024" fill="#1F1F20"/>',
        body,
    )
    return defs_inner, body


def build_icon_svg(raw_svg_text):
    clip_path = apple_rect_path(0, 0, CONTENT, CONTENT, CONTENT * 0.225)
    defs_inner, body = build_shifted_body(raw_svg_text)

    clip_block = f'  <clipPath id="shape">\n    <path d="{clip_path}"/>\n  </clipPath>'

    return (
        f'<?xml version="1.0" encoding="UTF-8"?>\n'
        f'<svg xmlns="http://www.w3.org/2000/svg" width="{CONTENT}" height="{CONTENT}" viewBox="0 0 {CONTENT} {CONTENT}">\n'
        f'<defs>{defs_inner}{clip_block}\n'
        f'</defs>\n'
        f'<g clip-path="url(#shape)">\n'
        f'{body.strip()}\n'
        f'</g>\n'
        f'</svg>\n'
    )


def build_fullbleed_svg(raw_svg_text):
    """1024x1024 content, no clip path — for apple-touch-icon (iOS applies its own mask)."""
    defs_inner, body = build_shifted_body(raw_svg_text)

    return (
        f'<?xml version="1.0" encoding="UTF-8"?>\n'
        f'<svg xmlns="http://www.w3.org/2000/svg" width="{CONTENT}" height="{CONTENT}" viewBox="0 0 {CONTENT} {CONTENT}">\n'
        f'<defs>{defs_inner}\n'
        f'</defs>\n'
        f'{body.strip()}\n'
        f'</svg>\n'
    )


def render_png(svg_bytes, size, mode="RGBA"):
    png = cairosvg.svg2png(bytestring=svg_bytes, output_width=size, output_height=size)
    return Image.open(io.BytesIO(png)).convert(mode)


def main():
    with open(RAW_SVG) as f:
        raw_svg = f.read()
    raw_bytes = raw_svg.encode()

    icon_svg = build_icon_svg(raw_svg)
    icon_bytes = icon_svg.encode()
    fullbleed_bytes = build_fullbleed_svg(raw_svg).encode()

    with open(os.path.join(ICONS_DIR, "icon.svg"), "w") as f:
        f.write(icon_svg)
    print("  icon.svg")

    for size in [192, 512]:
        name = f"icon-{size}.png"
        inner = int(size * CONTENT / CANVAS)  # 80% of target
        pad = (size - inner) // 2
        icon_img = render_png(icon_bytes, inner, "RGBA")
        canvas = Image.new("RGBA", (size, size), (0, 0, 0, 0))
        canvas.paste(icon_img, (pad, pad))
        canvas.save(os.path.join(ICONS_DIR, name))
        print(f"  {name}")

    render_png(raw_bytes, 512, "RGB").save(os.path.join(ICONS_DIR, "icon-maskable-512.png"))
    print("  icon-maskable-512.png")

    render_png(fullbleed_bytes, 180, "RGB").save(os.path.join(ICONS_DIR, "apple-touch-icon.png"))
    print("  apple-touch-icon.png")

    print("done")


if __name__ == "__main__":
    main()
