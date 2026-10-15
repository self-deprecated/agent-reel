#!/usr/bin/env python3
"""Generate AgentReelBox.ttf: DejaVu Sans Mono's box-drawing block, stretched
so vertical strokes span the terminal player's whole line box.

The asciinema player lays rows out at line-height 1.3333333333em, but DejaVu
Sans Mono's box-drawing glyphs (U+2500-U+257F) only span its 1.164em content
area, so a column of │ ┌ ├ ╭ ║ ... shows a gap between rows. This subset keeps
every glyph's horizontal geometry and moves only the points on a glyph's top
or bottom edge (the ends of its vertical strokes) out past the line box; the
player's rows (.ap-line, overflow: hidden) clip them to exactly the line box,
so strokes meet row to row even with the browser's whole-pixel ascent and
descent rounding. Everything between the edges (horizontal strokes, arcs,
dashes, double-line gaps) moves up by the few units that put the horizontal
strokes on the line box's centre. Vertical metrics are left identical to the
source font, so the glyphs sit on the same baseline as AgentReelMono.

The output is registered with unicode-range: U+2500-257F ahead of
AgentReelMono (app.css, the video export renderer), and pinned in
internal/reel/web/vendor/manifest.json by scripts/assets.mjs.

License: DejaVu Sans Mono is under the Bitstream Vera license with public
domain DejaVu changes (LICENSE-font.txt, kept verbatim in nameID 0/13/14). That
license permits modification provided the modified font is renamed without
"Bitstream" or "Vera"; this font is named "AgentReel Box" and does not use
the DejaVu name either.

Usage (reproducible; pinned input, deterministic output):

  nix shell --impure --expr \\
    '(import (builtins.getFlake "nixpkgs") {}).python3.withPackages (ps: [ps.fonttools])' \\
    -c python3 scripts/box-font.py
"""

import hashlib
import io
import sys
from pathlib import Path

from fontTools import subset
from fontTools.ttLib import TTFont

ROOT = Path(__file__).resolve().parent.parent
VENDOR = ROOT / "internal/reel/web/vendor"
SOURCE = VENDOR / "DejaVuSansMono.ttf"
OUTPUT = VENDOR / "AgentReelBox.ttf"
SOURCE_SHA256 = "9d9bfebceb1c3f6f4ad383ded568a6926086208f43f7f92f92f7e93a1383fa38"

FIRST, LAST = 0x2500, 0x257F
# The player's default terminalLineHeight (player.js lineHeight()).
LINE_HEIGHT_EM = 1.3333333333
# How far stretched stroke ends reach beyond the line box. The row clips them;
# the margin absorbs the browser rounding ascent/descent to whole pixels
# (up to about a pixel at the catalog's smallest snapshot size).
OVERSHOOT_EM = 0.125
# Source glyph edges: DejaVu's full-height box glyphs span exactly these.
SOURCE_TOP, SOURCE_BOTTOM = 1921, -512
EDGE_TOLERANCE = 2

FAMILY = "AgentReel Box"
POSTSCRIPT = "AgentReelBox-Regular"


def main() -> int:
    data = SOURCE.read_bytes()
    if hashlib.sha256(data).hexdigest() != SOURCE_SHA256:
        sys.exit(f"{SOURCE}: not the pinned DejaVu Sans Mono 2.37")

    options = subset.Options()
    options.hinting = False  # instructions refer to the original outlines
    options.layout_features = []
    options.drop_tables += ["GSUB", "GPOS", "GDEF", "gasp", "kern", "FFTM"]
    options.name_IDs = ["*"]
    options.name_languages = ["*"]
    options.glyph_names = True
    options.notdef_outline = True
    options.recalc_timestamp = False
    font = TTFont(io.BytesIO(data), recalcTimestamp=False)
    subsetter = subset.Subsetter(options)
    subsetter.populate(unicodes=range(FIRST, LAST + 1))
    subsetter.subset(font)

    upm = font["head"].unitsPerEm
    ascent, descent = font["hhea"].ascent, -font["hhea"].descent
    line = LINE_HEIGHT_EM * upm
    half_leading = (line - (ascent + descent)) / 2
    line_top, line_bottom = ascent + half_leading, -descent - half_leading
    over = OVERSHOOT_EM * upm
    top, bottom = round(line_top + over), round(line_bottom - over)
    # Put the horizontal strokes (centred between the source edges) on the
    # line box's centre, which the stretched verticals share.
    shift = round((line_top + line_bottom) / 2 - (SOURCE_TOP + SOURCE_BOTTOM) / 2)

    glyf = font["glyf"]
    cmap = font.getBestCmap()
    stretched = 0
    for codepoint in range(FIRST, LAST + 1):
        name = cmap.get(codepoint)
        if name is None:
            continue
        glyph = glyf[name]
        if glyph.isComposite() or glyph.numberOfContours <= 0:
            continue
        coordinates = glyph.coordinates
        moved = False
        for i, (x, y) in enumerate(coordinates):
            if y >= SOURCE_TOP - EDGE_TOLERANCE:
                coordinates[i] = (x, top)
                moved = True
            elif y <= SOURCE_BOTTOM + EDGE_TOLERANCE:
                coordinates[i] = (x, bottom)
                moved = True
            else:
                coordinates[i] = (x, y + shift)
        stretched += moved
        glyph.recalcBounds(glyf)

    names = font["name"]
    for record in list(names.names):
        # Keep copyright (0), license (13) and license URL (14) verbatim;
        # everything naming the font is replaced below.
        if record.nameID not in (0, 13, 14):
            names.removeNames(nameID=record.nameID)
    for name_id, text in {
        1: FAMILY,
        2: "Regular",
        3: POSTSCRIPT + " 2.37",
        4: FAMILY,
        5: "Version 2.37",
        6: POSTSCRIPT,
        10: "Derived from DejaVu Sans Mono 2.37: box drawing U+2500-U+257F stretched to a 1.3333em line box for the AgentReel terminal player; generated by scripts/box-font.py.",
    }.items():
        names.setName(text, name_id, 3, 1, 0x409)
        names.setName(text, name_id, 1, 0, 0)
    font["post"].formatType = 2.0
    if "OS/2" in font:
        font["OS/2"].achVendID = "NONE"

    out = io.BytesIO()
    font.save(out)
    OUTPUT.write_bytes(out.getvalue())
    digest = hashlib.sha256(out.getvalue()).hexdigest()
    print(f"{OUTPUT.relative_to(ROOT)}: {len(out.getvalue())} bytes, sha256 {digest}; "
          f"{stretched} glyphs stretched to [{bottom}, {top}] (line box [{line_bottom:.1f}, {line_top:.1f}]), others shifted {shift:+d}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
