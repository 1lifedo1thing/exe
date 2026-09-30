#!/usr/bin/env python3
"""The homepage's 88x31 badge, drawn pixel by pixel.

    python3 badge.py                  writes badge.gif beside this file
    python3 badge.py sheet.png        also writes every frame, 8x, to look at

The button the web linked with in 1998: a Platinum bevel, the desktop's
own icon (ui/icon.svg, redrawn to fit 31 rows), "exe" in the icon's
screen green, and a terminal under it that types what exe is. The first
frame is the whole badge, since that is what a reader who stops
animations sees. Needs Pillow; nothing else.
"""
import os
import sys

from PIL import Image

W, H = 88, 31

PAL = {
    'K': (0x26, 0x26, 0x26),  # the Platinum black: the frame
    'k': (0x00, 0x00, 0x00),  # the icon's outline, and the terminal
    'W': (0xff, 0xff, 0xff),
    'F': (0xdd, 0xdd, 0xdd),  # the face
    'S': (0x99, 0x99, 0x99),  # its shadow bevel
    'B': (0xda, 0xd5, 0xca),  # the icon's case
    'b': (0xc8, 0xc3, 0xb8),  # its stand
    'h': (0xf2, 0xef, 0xe8),  # the case catching the light
    'd': (0xa8, 0xa2, 0x96),  # and turned from it
    'T': (0x55, 0x52, 0x4b),  # the drive slot
    'G': (0x7f, 0xd6, 0x7f),  # the icon's screen green
    'g': (0x2f, 0x7a, 0x2f),  # what is written on that screen
    'L': (0xc8, 0xf5, 0xc8),  # a glint on the green
}
ORDER = list(PAL)

# the desktop's icon at badge size: the case 18 by 22, its screen, its
# slot and its stand, as ui/icon.svg draws them at 32
COMPUTER = [
    "kkkkkkkkkkkkkkkkkk",
    "khhhhhhhhhhhhhhhhk",
    "khBBBBBBBBBBBBBBdk",
    "khBkkkkkkkkkkkkBdk",
    "khBkGGGGGGGGLLkBdk",
    "khBkGGGGGGGGGLkBdk",
    "khBkGGGGGGGGGGkBdk",
    "khBkGGGGGGGGGGkBdk",
    "khBkGGGGGGGGGGkBdk",
    "khBkGGGGGGGGGGkBdk",
    "khBkGGGGGGGGGGkBdk",
    "khBkkkkkkkkkkkkBdk",
    "khBBBBBBBBBBBBBBdk",
    "khBBBBBBBBBBBBBBdk",
    "khBBBBBBBBBBBBBBdk",
    "khBBBBTTTTTTTTBBdk",
    "khBBBBTTTTTTTTBBdk",
    "khBBBBBBBBBBBBBBdk",
    "khBBBBBBBBBBBBBBdk",
    "khBBBBBBBBBBBBBBdk",
    "khdddddddddddddddk",
    "kkkkkkkkkkkkkkkkkk",
    "..khbbbbbbbbbbdk..",
    "..kdddddddddddddk.",
    "..kkkkkkkkkkkkkkk.",
]
CX, CY = 4, 3  # where it stands
SCREEN = (CX + 4, CY + 4)  # the green's top left: 10 by 7

E = [
    "..######..",
    ".########.",
    "###....###",
    "###....###",
    "##########",
    "##########",
    "###.......",
    "###....###",
    ".########.",
    "..######..",
]
X = [
    "###....###",
    "###....###",
    ".###..###.",
    "..######..",
    "...####...",
    "...####...",
    "..######..",
    ".###..###.",
    "###....###",
    "###....###",
]
WX, WY = 37, 4  # the word's top left, its outline one pixel out

# the terminal's type: five rows, as narrow as each letter allows
FONT = {
    'A': [".##.", "#..#", "####", "#..#", "#..#"],
    'B': ["###.", "#..#", "###.", "#..#", "###."],
    'C': [".###", "#...", "#...", "#...", ".###"],
    'D': ["###.", "#..#", "#..#", "#..#", "###."],
    'E': ["###", "#..", "##.", "#..", "###"],
    'G': [".###", "#...", "#.##", "#..#", ".###"],
    'I': ["###", ".#.", ".#.", ".#.", "###"],
    'L': ["#..", "#..", "#..", "#..", "###"],
    'M': ["#...#", "##.##", "#.#.#", "#...#", "#...#"],
    'N': ["#..#", "##.#", "#.##", "#..#", "#..#"],
    'O': [".##.", "#..#", "#..#", "#..#", ".##."],
    'R': ["###.", "#..#", "###.", "#.#.", "#..#"],
    'T': ["###", ".#.", ".#.", ".#.", ".#."],
    'U': ["#..#", "#..#", "#..#", "#..#", ".##."],
    'V': ["#...#", "#...#", "#...#", ".#.#.", "..#.."],
    'W': ["#...#", "#...#", "#.#.#", "##.##", "#...#"],
    'Y': ["#.#", "#.#", ".#.", ".#.", ".#."],
    '!': ["#", "#", "#", ".", "#"],
    ' ': ["..", "..", "..", "..", ".."],
}
CURSOR = ["###"] * 5
TERM = (24, 17, 83, 27)  # the sunken strip, bevel included
TX, TY = 27, 20  # where its line begins

PHRASES = ["VM CLOUD", "ONE BINARY", "GET IT NOW!"]
LINES = [6, 7, 5]  # how long each phrase's line is on the little screen


def face():
    img = [['F'] * W for _ in range(H)]
    for x in range(W):
        img[0][x] = img[H - 1][x] = 'K'
        if 0 < x < W - 1:
            img[1][x], img[H - 2][x] = 'W', 'S'
    for y in range(H):
        img[y][0] = img[y][W - 1] = 'K'
        if 0 < y < H - 1:
            img[y][1], img[y][W - 2] = 'W', 'S'
    img[H - 2][1] = img[1][W - 2] = 'F'
    for y, row in enumerate(COMPUTER):
        for x, c in enumerate(row):
            if c != '.':
                img[CY + y][CX + x] = c
    x0, y0, x1, y1 = TERM
    for y in range(y0, y1 + 1):
        for x in range(x0, x1 + 1):
            edge_lt = x == x0 or y == y0
            edge_rb = x == x1 or y == y1
            img[y][x] = 'S' if edge_lt and not (x == x1 or y == y1) else 'W' if edge_rb else 'k'
    return img


def word_mask():
    rows = []
    for r in range(len(E)):
        rows.append(E[r] + '..' + X[r] + '..' + E[r])
    return rows


MASK = word_mask()


def draw_word(img, glint=None):
    """The word in green with a black outline; glint is where a diagonal
    shine crosses it, in x + y, or None."""
    hgt, wid = len(MASK), len(MASK[0])

    def on(x, y):
        return 0 <= y < hgt and 0 <= x < wid and MASK[y][x] == '#'

    for y in range(-1, hgt + 1):
        for x in range(-1, wid + 1):
            if on(x, y):
                c = 'G'
                if glint is not None:
                    d = (x + y) - glint
                    if d in (0, 1):
                        c = 'W'
                    elif d in (-1, 2):
                        c = 'L'
                img[WY + y][WX + x] = c
            elif any(on(x + dx, y + dy) for dx in (-1, 0, 1) for dy in (-1, 0, 1)):
                img[WY + y][WX + x] = 'k'


def draw_text(img, s, cursor):
    x = TX
    for ch in s:
        g = FONT[ch]
        for y, row in enumerate(g):
            for i, c in enumerate(row):
                if c == '#':
                    img[TY + y][x + i] = 'G'
        x += len(g[0]) + 1
    if cursor:
        for y, row in enumerate(CURSOR):
            for i, c in enumerate(row):
                img[TY + y][x + i] = 'G'


def draw_screen(img, lines, cursor):
    """The little computer's screen: a line of writing for each phrase
    typed so far, and its own cursor after the last."""
    sx, sy = SCREEN
    end = None
    for i, n in enumerate(lines):
        for x in range(n):
            img[sy + 1 + 2 * i][sx + 1 + x] = 'g'
        end = (sx + 1 + n + (1 if n else 0), sy + 1 + 2 * i)
    if end is None:
        end = (sx + 1, sy + 1)
    if cursor:
        img[end[1]][end[0]] = 'g'


def frame(text, cursor, lines, glint=None):
    img = face()
    draw_word(img, glint)
    draw_text(img, text, cursor)
    draw_screen(img, lines, cursor)
    return img


def timeline():
    """(frame, centiseconds) for one loop. It opens on the first phrase
    whole, types the other two, and ends typing the first again, so the
    loop closes on its own first frame."""
    out = []
    add = lambda f, cs: out.append((f, cs))
    beat = [9, 7, 11, 8, 6, 10, 8, 12, 7, 9, 8]  # a hand on the keys

    # the first phrase, held while a shine crosses the word
    shown = [LINES[0]]
    add(frame(PHRASES[0], True, shown), 60)
    for gl in range(-3, len(MASK[0]) + len(MASK) + 3, 3):
        add(frame(PHRASES[0], True, shown, glint=gl), 4)
    for on, cs in [(False, 40), (True, 45), (False, 40)]:
        add(frame(PHRASES[0], on, shown), cs)

    def backspace(p, lines):
        for n in range(len(p) - 1, -1, -1):
            add(frame(p[:n], True, lines), 3)

    def type_in(i, before):
        p = PHRASES[i]
        for n in range(1, len(p) + 1):
            lines = before + [round(LINES[i] * n / len(p))]
            add(frame(p[:n], True, lines), beat[n % len(beat)])
        return before + [LINES[i]]

    backspace(PHRASES[0], shown)
    add(frame("", False, shown), 25)
    shown = type_in(1, shown)
    for on, cs in [(False, 40), (True, 45), (False, 40)]:
        add(frame(PHRASES[1], on, shown), cs)
    backspace(PHRASES[1], shown)
    add(frame("", False, shown), 25)
    shown = type_in(2, shown)
    # the 1998 part: NOW! blinks
    add(frame(PHRASES[2], False, shown), 30)
    for _ in range(3):
        add(frame("GET IT", False, shown), 25)
        add(frame(PHRASES[2], False, shown), 35)
    add(frame(PHRASES[2], True, shown), 45)
    backspace(PHRASES[2], shown)
    # a clear screen, and the first phrase again
    add(frame("", False, []), 15)
    add(frame("", True, []), 30)
    add(frame("", False, []), 20)
    type_in(0, [])
    return out


def to_image(f):
    im = Image.new('P', (W, H))
    flat = []
    for c in ORDER:
        flat.extend(PAL[c])
    im.putpalette(flat)
    im.putdata([ORDER.index(c) for row in f for c in row])
    return im


def main():
    here = os.path.dirname(os.path.abspath(__file__))
    frames = timeline()
    ims = [to_image(f) for f, _ in frames]
    ims[0].save(os.path.join(here, 'badge.gif'), save_all=True, append_images=ims[1:],
                duration=[cs * 10 for _, cs in frames], loop=0, disposal=1, optimize=False)
    if len(sys.argv) > 1:
        scale, cols = 4, 4
        rows = (len(ims) + cols - 1) // cols
        sheet = Image.new('RGB', (cols * (W * scale + 8), rows * (H * scale + 8)), (0x55, 0x55, 0x55))
        for i, im in enumerate(ims):
            big = im.convert('RGB').resize((W * scale, H * scale), Image.NEAREST)
            sheet.paste(big, ((i % cols) * (W * scale + 8), (i // cols) * (H * scale + 8)))
        sheet.save(sys.argv[1])
    total = sum(cs for _, cs in frames)
    print(f"{len(frames)} frames, {total / 100:.1f} s a loop, "
          f"{os.path.getsize(os.path.join(here, 'badge.gif'))} bytes")


if __name__ == '__main__':
    main()
