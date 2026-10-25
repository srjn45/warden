#!/usr/bin/env python3
"""Size-cap the web captures written by shoot-web.mjs.

    python3 -I site/scripts/optimize-web-media.py DIR [--cap 300000]

* web-*.png over the cap are palette-quantized (lossless layout, fewer colours).
* DIR/frames/NNN-<hold ms>.png are assembled into DIR/web-monitor.gif, downscaled and
  colour-reduced until the file is under the cap.
"""
import glob, os, sys
from PIL import Image

args = [a for a in sys.argv[1:] if not a.startswith("--")]
d = args[0]
cap = int(sys.argv[sys.argv.index("--cap") + 1]) if "--cap" in sys.argv else 300_000
if "--cap" in sys.argv:
    args = [a for a in args if a != str(cap)]

for p in sorted(glob.glob(os.path.join(d, "web-*.png"))):
    if os.path.getsize(p) > cap:
        Image.open(p).convert("RGB").quantize(colors=128, method=Image.Quantize.MEDIANCUT).save(p, optimize=True)
    print(f"{os.path.basename(p)} {os.path.getsize(p)}")

frames = sorted(glob.glob(os.path.join(d, "frames", "*.png")))
if frames:
    out = os.path.join(d, "web-monitor.gif")
    holds = [int(os.path.basename(f)[:-4].split("-")[1]) for f in frames]   # NNN-<ms>.png
    for width, colors in [(1100, 128), (1000, 96), (960, 80), (880, 64), (800, 64), (800, 48)]:
        ims = []
        for f in frames:
            im = Image.open(f).convert("RGB")
            im = im.resize((width, round(im.height * width / im.width)), Image.LANCZOS)
            ims.append(im.quantize(colors=colors, method=Image.Quantize.FASTOCTREE, dither=Image.Dither.NONE))
        ims[0].save(out, save_all=True, append_images=ims[1:], duration=holds, loop=0, optimize=True, disposal=1)
        size = os.path.getsize(out)
        print(f"web-monitor.gif width={width} colors={colors} frames={len(ims)} -> {size}")
        if size <= cap:
            break
