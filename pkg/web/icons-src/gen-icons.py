#!/usr/bin/env python3
"""Generate the icons from icon-raw.svg (1280 canvas, dark plate, content in the center 1024).

  icon.svg              — no plate, see-through halo, cropped; LIGHT colors unless dark mode
  icon-192/512.png      — center 1024 on a rounded plate with a transparent margin
  icon-maskable-512.png — raw as-is
  apple-touch-icon.png  — center 1024, no clip (iOS masks)
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

PLATE = (0x1F, 0x1F, 0x20)  # raw background
CORE_END = 28               # orb offset (%) where the core ends
CROP = 840                  # icon.svg view size
FOCUS = (512, 512)          # icon.svg view center
RADIUS = 0.16               # icon-192/512 plate corner (continuous), of the plate side
MARGIN = 0.10               # icon-192/512 transparent margin, of the canvas
EDGE = (12, 0.08)           # icon-192/512 inner hairline: width, white opacity

# Raw colors are tuned for the dark plate; icon.svg swaps these in on light backgrounds.
LIGHT = {
    ("fill", "#8F8C87"): "#75716D",
    ("fill", "#8A8987"): "#716F6D",
    ("fill", "#A49786"): "#857969",
    ("fill", "#9B9286"): "#7E756B",
    ("stop-color", "#F2E0C0"): "#5E4C3A",
    ("stop-color", "#EADCCC"): "#3A342E",
    ("stop-color", "#E0E0DF"): "#2A2826",
    ("stroke", "#E0E0DF"): "#2A2826",
}


# Apple continuous-corner (squircle) bezier constants.
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


def hex_rgb(h):
    return tuple(int(h[i:i + 2], 16) for i in (1, 3, 5))


def see_through_halo(defs, body, view):
    """Turn halo stops into colors that composite to the same over PLATE,
    and squeeze the glow to fade out at the crop edge."""
    m = re.search(r'<circle cx="([\d.]+)" cy="([\d.]+)" r="([\d.]+)" fill="url\(#orb\)"/>', body)
    cx, cy, r0 = (float(v) for v in m.groups())
    x0, y0, size = view
    r1 = min(cx - x0, x0 + size - cx, cy - y0, y0 + size - cy)
    core = r0 * CORE_END / 100
    k = (r1 - core) / (r0 - core)

    def stop(sm):
        off, color, rest = float(sm.group(1)), sm.group(2), sm.group(3)
        px = r0 * off / 100
        px = px if px <= core else core + (px - core) * k
        attrs = f'stop-color="{color}"{rest}'
        if off > CORE_END and "stop-opacity" not in rest:
            c = hex_rgb(color)
            a = max((v - p) / (255 - p) for v, p in zip(c, PLATE))
            lit = tuple(min(255, round(p + (v - p) / a)) for v, p in zip(c, PLATE))
            attrs = f'stop-color="#{lit[0]:02X}{lit[1]:02X}{lit[2]:02X}" stop-opacity="{a:.3f}"'
        elif "stop-opacity" in rest:  # final fade-out
            attrs = None
        return f'<stop offset="{px / r1 * 100:.3g}%" ' + (attrs or "{LAST}") + "/>"

    grad = re.search(r'<radialGradient id="orb".*?</radialGradient>', defs, re.DOTALL).group(0)
    new = re.sub(r'<stop offset="([\d.]+)%" stop-color="(#[0-9A-Fa-f]{6})"([^/]*)/>', stop, grad)
    last = re.findall(r'stop-color="(#[0-9A-F]{6})" stop-opacity', new)[-1]
    new = new.replace("{LAST}", f'stop-color="{last}" stop-opacity="0"')
    defs = defs.replace(grad, new)
    body = body.replace(m.group(0), m.group(0).replace(f'r="{m.group(3)}"', f'r="{r1:g}"'))
    return defs, body


def light_palette(defs, body):
    """Use LIGHT colors, keeping the raw ones under prefers-color-scheme: dark."""
    rules = {}

    def swap(m):
        key = (m.group(1), m.group(2).upper())
        if key not in LIGHT:
            return m.group(0)
        cls = rules.setdefault(key, f"c{len(rules)}")
        return f'{key[0]}="{LIGHT[key]}" class="{cls}"'

    pat = r'\b(fill|stroke|stop-color)="(#[0-9A-Fa-f]{6})"'
    defs, body = re.sub(pat, swap, defs), re.sub(pat, swap, body)
    css = "".join(f".{c}{{{a}:{v}}}" for (a, v), c in rules.items())
    return defs + f"  <style>@media (prefers-color-scheme:dark){{{css}}}</style>\n", body


def build_icon_svg(raw_svg_text):
    defs_inner, body = build_shifted_body(raw_svg_text)
    body = body.replace('<rect x="0" y="0" width="1024" height="1024" fill="#1F1F20"/>', "")
    view = (FOCUS[0] - CROP / 2, FOCUS[1] - CROP / 2, CROP)
    defs_inner, body = see_through_halo(defs_inner, body, view)
    defs_inner, body = light_palette(defs_inner, body)

    return (
        f'<?xml version="1.0" encoding="UTF-8"?>\n'
        f'<svg xmlns="http://www.w3.org/2000/svg" width="{CONTENT}" height="{CONTENT}" viewBox="{view[0]:g} {view[1]:g} {CROP} {CROP}">\n'
        f'<defs>{defs_inner}</defs>\n'
        f'{body.strip()}\n'
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


def build_plate_svg(raw_svg_text):
    """Center 1024 clipped to a continuous-corner rect, inset by MARGIN — for icon-192/512."""
    defs_inner, body = build_shifted_body(raw_svg_text)
    inset, scale = CONTENT * MARGIN, 1 - 2 * MARGIN
    plate = apple_rect_path(0, 0, CONTENT, CONTENT, CONTENT * RADIUS)
    width, opacity = EDGE

    return (
        f'<?xml version="1.0" encoding="UTF-8"?>\n'
        f'<svg xmlns="http://www.w3.org/2000/svg" width="{CONTENT}" height="{CONTENT}" viewBox="0 0 {CONTENT} {CONTENT}">\n'
        f'<defs>{defs_inner}<clipPath id="plate"><path d="{plate}"/></clipPath>\n'
        f'</defs>\n'
        f'<g transform="translate({inset:g} {inset:g}) scale({scale:g})" clip-path="url(#plate)">\n'
        f'{body.strip()}\n'
        f'<path d="{plate}" fill="none" stroke="#FFFFFF" stroke-opacity="{opacity}" stroke-width="{width * 2}"/>\n'
        f'</g>\n'
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
    fullbleed_bytes = build_fullbleed_svg(raw_svg).encode()
    plate_bytes = build_plate_svg(raw_svg).encode()

    with open(os.path.join(ICONS_DIR, "icon.svg"), "w") as f:
        f.write(icon_svg)
    print("  icon.svg")

    for size in [192, 512]:
        name = f"icon-{size}.png"
        render_png(plate_bytes, size, "RGBA").save(os.path.join(ICONS_DIR, name))
        print(f"  {name}")

    render_png(raw_bytes, 512, "RGB").save(os.path.join(ICONS_DIR, "icon-maskable-512.png"))
    print("  icon-maskable-512.png")

    render_png(fullbleed_bytes, 180, "RGB").save(os.path.join(ICONS_DIR, "apple-touch-icon.png"))
    print("  apple-touch-icon.png")

    print("done")


if __name__ == "__main__":
    main()
