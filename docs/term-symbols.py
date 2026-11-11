#!/usr/bin/env python3
"""The terminal's symbols face: the seven transport symbols U+23F4-23FA.

Claude Code draws its mode row with them ("⏵⏵ auto mode on", "⏸ plan mode
on") and, on a Mac, its bullet (⏺). Few monospace faces hold them, and
where no installed font does the browser draws a box for each, or a
colour emoji that ignores the row's colour. The desk carries this face as
the last name of the terminal's font stack (TERM_FONT in
internal/server/ui/index.html), so any installed face that has a symbol
still draws it and this one draws the rest.

The shapes sit on the metrics of the stack's first face (Meslo, which is
DejaVu Sans Mono's: 2048 units to the em, a 1233 advance) and centre on
y=532 like its ▶ ▸ ● •, between ▸ and ▶ in size as "medium" says.

    python3 docs/term-symbols.py          writes the face into index.html
    python3 docs/term-symbols.py out.ttf  writes a TrueType file to look at

Needs fontTools and brotli (pip install fonttools brotli).
"""
import base64
import io
import math
import re
import sys
from pathlib import Path

from fontTools.fontBuilder import FontBuilder
from fontTools.misc.timeTools import timestampFromString
from fontTools.pens.ttGlyphPen import TTGlyphPen

UPM, ADV = 2048, 1233
ASCENT, DESCENT = 1901, -483
CX, CY = 616.5, 532          # the cell's centre line, the shapes' middle
LONG, SHORT = 960, 860       # a triangle's base, and its height
FAMILY = "exe Terminal Symbols"
INDEX = Path(__file__).resolve().parent.parent / "internal/server/ui/index.html"


def poly(pen, pts):
    pen.moveTo(pts[0])
    for p in pts[1:]:
        pen.lineTo(p)
    pen.closePath()


def rect(pen, x0, y0, x1, y1):
    # clockwise, as TrueType fills its outer contours
    poly(pen, [(x0, y0), (x0, y1), (x1, y1), (x1, y0)])


def right(pen):
    x0, x1 = CX - SHORT / 2, CX + SHORT / 2
    poly(pen, [(x0, CY - LONG / 2), (x0, CY + LONG / 2), (x1, CY)])


def left(pen):
    x0, x1 = CX - SHORT / 2, CX + SHORT / 2
    poly(pen, [(x1, CY - LONG / 2), (x0, CY), (x1, CY + LONG / 2)])


def up(pen):
    y0, y1 = CY - SHORT / 2, CY + SHORT / 2
    poly(pen, [(CX - LONG / 2, y0), (CX, y1), (CX + LONG / 2, y0)])


def down(pen):
    y0, y1 = CY - SHORT / 2, CY + SHORT / 2
    poly(pen, [(CX - LONG / 2, y1), (CX + LONG / 2, y1), (CX, y0)])


def pause(pen):
    bar, x0, x1 = 300, CX - SHORT / 2, CX + SHORT / 2
    rect(pen, x0, CY - LONG / 2, x0 + bar, CY + LONG / 2)
    rect(pen, x1 - bar, CY - LONG / 2, x1, CY + LONG / 2)


def stop(pen):
    rect(pen, CX - SHORT / 2, CY - SHORT / 2, CX + SHORT / 2, CY + SHORT / 2)


def record(pen):
    # a circle as eight quadratic arcs, clockwise from the top
    r, n = LONG / 2, 8
    on = lambda k: (CX + r * math.sin(2 * math.pi * k / n), CY + r * math.cos(2 * math.pi * k / n))
    ro = r / math.cos(math.pi / n)
    off = lambda k: (CX + ro * math.sin(2 * math.pi * (k + .5) / n), CY + ro * math.cos(2 * math.pi * (k + .5) / n))
    pen.moveTo(on(0))
    for k in range(n):
        pen.qCurveTo(off(k), on(k + 1))
    pen.closePath()


GLYPHS = [
    (0x23F4, "uni23F4", left),
    (0x23F5, "uni23F5", right),
    (0x23F6, "uni23F6", up),
    (0x23F7, "uni23F7", down),
    (0x23F8, "uni23F8", pause),
    (0x23F9, "uni23F9", stop),
    (0x23FA, "uni23FA", record),
]


class Rounded(TTGlyphPen):
    """TrueType points are whole units."""

    def _r(self, p):
        return (round(p[0]), round(p[1]))

    def moveTo(self, p):
        super().moveTo(self._r(p))

    def lineTo(self, p):
        super().lineTo(self._r(p))

    def qCurveTo(self, *pts):
        super().qCurveTo(*[self._r(p) for p in pts])


def build():
    fb = FontBuilder(UPM, isTTF=True)
    order = [".notdef"] + [name for _, name, _ in GLYPHS]
    fb.setupGlyphOrder(order)
    fb.setupCharacterMap({cp: name for cp, name, _ in GLYPHS})
    glyphs = {".notdef": TTGlyphPen(None).glyph()}
    for _, name, draw in GLYPHS:
        pen = Rounded(None)
        draw(pen)
        glyphs[name] = pen.glyph()
    fb.setupGlyf(glyphs)
    fb.setupHorizontalMetrics({n: (ADV, fb.font["glyf"][n].xMin if n != ".notdef" else 0) for n in order})
    fb.setupHorizontalHeader(ascent=ASCENT, descent=DESCENT)
    fb.setupNameTable({"familyName": FAMILY, "styleName": "Regular"})
    fb.setupOS2(sTypoAscender=ASCENT, sTypoDescender=DESCENT, sTypoLineGap=0,
                usWinAscent=ASCENT, usWinDescent=-DESCENT, sxHeight=1120, sCapHeight=1493)
    fb.setupPost(isFixedPitch=1)
    # the same bytes from the same source, whenever it is built
    fb.font["head"].created = fb.font["head"].modified = timestampFromString("Sun Nov  1 00:00:00 2026")
    fb.font.recalcTimestamp = False
    return fb.font


def main():
    font = build()
    if len(sys.argv) > 1:
        font.save(sys.argv[1])
        return
    font.flavor = "woff2"
    buf = io.BytesIO()
    font.save(buf)
    uri = "data:font/woff2;base64," + base64.b64encode(buf.getvalue()).decode()
    html = INDEX.read_text(encoding="utf-8")
    pat = re.compile(r'(/\* term-symbols \*/\s*src: url\()[^)]*(\))')
    if not pat.search(html):
        sys.exit("no /* term-symbols */ src: url(…) in " + str(INDEX))
    INDEX.write_text(pat.sub(lambda m: m.group(1) + uri + m.group(2), html), encoding="utf-8")
    print(f"{len(buf.getvalue())} bytes of WOFF2 into {INDEX}")


if __name__ == "__main__":
    main()
