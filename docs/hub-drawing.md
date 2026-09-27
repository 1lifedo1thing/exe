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

A picture tag replays a drawing when it loads, and has no way to be told to
play again. WebKit replays only a fresh address: a second picture of an
address already played shows the finished pad. So Replay from Start, the small
button under a drawing in the Hub app's feed (`drawReplay`), gives the same
picture tag a blob of the same bytes for its address, and lets the blob go once
the picture has loaded it; both engines start again, press after press. The
desktop's viewer loads a picture as a blob of its own, which is a fresh address
too.

The file must reach the hub as it is. A re-encode through a canvas (the
composer's `prepareImage`, for photographs) keeps the finished picture and
drops both the replay and the record.

## Test

`~/tools/playwright/exe-hub-draw-test.js`: `DPR=1.5 node exe-hub-draw-test.js`,
`PHONE=1 node exe-hub-draw-test.js`. It reads the file back chunk by chunk.
