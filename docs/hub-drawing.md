# A drawing on the hub

What the Hub app's Draw… panel sends (`internal/server/sysapps/hub/index.html`,
"a drawing"). Asked by Livid 2026-09-27, after PictoChat, which replays a
message's strokes: one embed, a picture that replays, the strokes kept with it.
The hub knows nothing of it: a drawing is uploaded and posted as any picture.

## The file

One animated PNG (APNG), sniffed and served as `image/png`.

- `IHDR`: the pad, 256 × 256 or 256 × 128; 4 bits a pixel, indexed.
- `PLTE`: the palette, 2 to 16 colours, the paper first (index 0).
- `acTL`: the number of frames; `num_plays` 1, so it plays once and stops on
  the last frame.
- `zTXt`, keyword `exe-sketch`: the record (below), zlib-compressed JSON.
- `IDAT`: **the finished pad, the default image, and no frame**: no `fcTL`
  comes before it. A decoder that knows no animation (Go's `image/png`,
  which draws the hub's link previews) reads the finished drawing.
- `fcTL` + `fdAT` pairs, the replay: the first frame is the whole empty pad
  and stands 8 ticks; every later frame is the box of what changed since the
  frame before, 1 tick. A tick is 50 ms (`delay_den` 20). Dispose none, blend
  source.
- `IEND`.

Scanlines carry filter 0. The compression is the browser's
`CompressionStream("deflate")`; a browser without it writes stored blocks,
larger and as good to every reader.

## The record

```json
{"v": 1, "w": 256, "h": 128,
 "palette": ["#eef9f5", "#123f5f", "#1b7087"],
 "ops": [[1, 1, 10, 10, 11, 10, 12, 11], [2, 3, 100, 20, 101, 21], [-1]]}
```

- `v`: 1.
- `w`, `h`: the pad.
- `palette`: the colours themselves, never a palette's name, so a drawing
  keeps its colours whatever becomes of the list it was picked from.
- `ops`: every operation, in the order it happened.
  - A stroke is `[colour, size, x, y, x, y, …]`: an index into the palette,
    the side in pad pixels of the square the pen stamps, and one point or
    more, whole numbers on the pad. The eraser's stroke is a stroke in the
    paper, colour 0. The pencil is 1 (thin) or 3 (thick), the eraser 3 or 9.
  - An undo is `[-1]`: it takes back the latest stroke still standing.

Undo is an operation, not an edit of the record (Livid's rule): what was
drawn and taken back is replayed drawn and taken back, and a pad that ends
empty is a drawing.

## Drawing a record

Whole-number lines, as Paint draws them: the first point of a stroke stamps
a square of `size` pixels, its top left corner `size >> 1` up and left of the
point; each later point is a Bresenham line from the point before, the square
stamped at every step. No smoothing, so a record draws the same pixels
wherever it is read. After an undo the pad is what the strokes still standing
draw, in their order.

## Limits

A record holds 20,000 points, an undo counting as 8. A replay has 200 frames
at the most, ten seconds: a frame takes 8 points, more when the drawing is
long. While the count allows, an undo shows the stroke it takes and then the
pad without it, a frame each; past that it is paced like ink.

## In the post

The embed is `{cid, mime: "image/png", filename: "drawing.png", width, height}`,
`width` and `height` the pad's. That is how a reader knows a drawing: the Hub
app shows a picture that declares 256 × 256 or 256 × 128 at that size, a pad
pixel a whole number of device pixels (two at 150 percent), `image-rendering:
pixelated`, its box standing before the picture loads. Pictures attached the
ordinary way declare no size.

A picture tag plays a drawing once, as it loads, and has no way to be told to
play again. Nor does every browser play it again for the asking. WebKit keeps
a played picture as it ended for as long as it holds it, and the hub serves an
embed as immutable: a second picture of an address already played shows the
finished pad, and so does the same page after a reload (Livid's report from
Safari on iOS, 2026-09-27: the drawing "stayed at the last frame"). Chromium
plays it again both times.

So the Hub app never shows a drawing by its hub address (`drawReplay`). It
fetches the bytes, which the browser holds, and gives the picture a blob of
them for its address, a fresh one every time, letting the blob go once the
picture has loaded it:

- in the feed, when half of the drawing has come into view (`drawWatch`); until
  then its box stands empty at its final size, so a drawing further down has
  not played before it is reached;
- under it, Replay from Start does the same again, press after press;
- the desktop's viewer loads any picture as a blob of its own.

The hub address is the way back for a browser without IntersectionObserver, or
when the bytes cannot be fetched.

The hub's public pages do the same (exe-hub `internal/api/web.html`,
`drawings`, `drawReplay`; its PLAN.md, Drawings): the server marks the
embed and serves the box at the pad's size with Replay from Start under
it, in the page's language; the script plays it on view from a blob, and
the page's viewer replays it. There a pad pixel takes fewer device pixels
where the row is narrower than the pad, and a reader without script is
shown the picture by its hub address. The two scripts are copies: a
change to one is a change to both.

## Drawn there too

Since 2026-09-27 (exe-hub 1643ed5) the pages have the pad as well: Draw…
in the Post and Reply windows, for a wallet signed in with Solana. The
engine from `DRAW_KEY` down to `drawScale` is this app's text copied into
`internal/api/web.html`, so a drawing made there is the same file as one
made here — change both. What differs is only what a wallet imposes:

- **Two signatures.** The first authorizes the file (`POST /v1/upload`,
  the text a profile picture signs: the time and the file's SHA-256),
  the second is the post's. The panel says which is being asked for.
  Here the desk signs as the node and Send is one step.
- **A post declined keeps everything** — the panel, the drawing, the
  words, the reply target and the file the hub already holds — and Send
  then asks for the post alone.
- **A finger's release is a press.** A stroke that ends in a flick
  leaves Chromium a fling to stop, and the tap that stops it gets no
  click, so Undo after a quick stroke did nothing. The pad's buttons
  there hear the release, wait 80 ms for the click, and take whichever
  comes. This app's pad has no such guard yet.

The file must reach the hub as it is. A re-encode through a canvas (the
composer's `prepareImage`, for photographs) keeps the finished picture and
drops both the replay and the record.

## Test

`~/tools/playwright/exe-hub-draw-test.js`: `DPR=1.5 node exe-hub-draw-test.js`,
`PHONE=1 node exe-hub-draw-test.js`. It reads the file back chunk by chunk.

`~/tools/playwright/exe-hub-page-draw-test.js`: the public pages, on a
scratch hub of its own (`BIN`, `HUB_SCRATCH`, `DPR`, `PHONE`, `ENGINE`).

`~/tools/playwright/exe-hub-page-pad-test.js`: the pad on the public
pages, on a scratch hub with a mock Wallet Standard wallet — the two
signatures, a post declined and sent again, a drawing with no words.

`~/tools/playwright/exe-hub-draw-reload-test.js`: the reload, in Chromium and
WebKit, over a real server with the hub's cache headers (a routed request
hides the browser's cache, and with it the fault). The page runs the app's own
`drawReplay` and `drawWatch`, lifted out of the app as they stand.
