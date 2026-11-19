"""Render the real `sesh` binary's coloured output to GIFs with Pillow (no recorder needed).
usage: render.py <sesh-binary> <fixtures-dir> <out-dir>"""
import os, re, subprocess, sys, tempfile
from PIL import Image, ImageDraw, ImageFont

BIN, FIX, OUT = sys.argv[1:4]
COLS, ROWS = 112, 27
STEPS = [
    ("sesh list", ["list"]),
    ("sesh inspect claude:7c1e --tail 4 --abandoned 2", ["inspect", "claude:7c1e", "--tail", "4", "--abandoned", "2"]),
    ("sesh validate --all", ["validate", "--all"]),
    ("sesh convert codex:01b2 -o demo.osp.jsonl", ["convert", "codex:01b2", "-o", "demo.osp.jsonl"]),
    ("sesh validate demo.osp.jsonl", ["validate", "demo.osp.jsonl"]),
]
BG, FG = (26, 27, 38), (192, 202, 245)
ANSI = {30: (65, 72, 104), 31: (247, 118, 142), 32: (158, 206, 106), 33: (224, 175, 104), 34: (122, 162, 247),
        35: (187, 154, 247), 36: (125, 207, 255), 37: FG}
env = dict(os.environ, CLAUDE_CONFIG_DIR=f"{FIX}/claude", CODEX_HOME=f"{FIX}/codex", XDG_DATA_HOME=f"{FIX}/data", CLICOLOR_FORCE="1")
env.pop("NO_COLOR", None)
work = tempfile.mkdtemp()

def run(args):
    p = subprocess.run([BIN, *args], cwd=work, env=env, capture_output=True, text=True)
    return (p.stdout + p.stderr).rstrip("\n")

SGR = re.compile(r"\x1b\[([0-9;]*)m")
def parse(line):
    """-> list of (char, fg, bold, dim)"""
    out, fg, bold, dim, pos = [], FG, False, False, 0
    for m in SGR.finditer(line):
        out += [(c, fg, bold, dim) for c in line[pos:m.start()]]
        pos = m.end()
        for code in (int(x) for x in (m.group(1) or "0").split(";")):
            if code == 0: fg, bold, dim = FG, False, False
            elif code == 1: bold = True
            elif code == 2: dim = True
            elif code == 22: bold = dim = False
            elif code == 39: fg = FG
            elif code in ANSI: fg = ANSI[code]
    out += [(c, fg, bold, dim) for c in line[pos:]]
    return out

def render_all(width_px):
    fs = int(width_px / COLS / 0.6)
    reg = ImageFont.truetype("/System/Library/Fonts/Menlo.ttc", fs, index=0)
    bld = ImageFont.truetype("/System/Library/Fonts/Menlo.ttc", fs, index=1)
    cw = int(reg.getlength("M")); lh = int(fs * 1.45)
    pad, bar = 22, 38
    W, H = cw * COLS + pad * 2, lh * ROWS + pad * 2 + bar
    def frame(lines, cursor=True):
        im = Image.new("RGB", (W, H), BG); d = ImageDraw.Draw(im)
        d.rectangle([0, 0, W, bar], fill=(36, 40, 59))
        for i, col in enumerate([(255, 95, 86), (255, 189, 46), (39, 201, 63)]):
            d.ellipse([pad + i * 24, 12, pad + i * 24 + 14, 26], fill=col)
        d.text((W // 2, 19), "sesh - Open Session Protocol", font=reg, fill=(120, 130, 170), anchor="mm")
        for r, ln in enumerate(lines[-ROWS:]):
            y = bar + pad + r * lh
            for c, (ch, fg, b, dim) in enumerate(ln):
                col = tuple(int(v * .55 + BG[i] * .45) for i, v in enumerate(fg)) if dim else fg
                d.text((pad + c * cw, y), ch, font=bld if b else reg, fill=col)
        if cursor and lines:
            r = min(len(lines), ROWS) - 1
            x = pad + len(lines[-1]) * cw
            d.rectangle([x, bar + pad + r * lh + 3, x + cw - 2, bar + pad + r * lh + lh - 3], fill=(192, 202, 245))
        return im
    frames = []  # (image, ms)
    def add(lines, ms, cursor=True): frames.append((frame(lines, cursor), ms))
    PROMPT = parse("\x1b[32m❯\x1b[0m ")
    for title, args in STEPS:
        out = run(args).split("\n")
        screen = []
        typed = []
        for i in range(0, len(title) + 1, 2):
            add(screen + [PROMPT + [(c, FG, False, False) for c in title[:i]]], 70)
        screen = screen + [PROMPT + [(c, FG, False, False) for c in title]]
        add(screen, 250)
        for i in range(0, len(out), 3):
            screen = screen + [parse(l) for l in out[i:i + 3]]
            add(screen, 110)
        add(screen + [PROMPT], 2300 if len(out) > 3 else 1400)
        # new "page": keep context for short outputs, clear for long
        if len(out) > 8:
            screen = []
    return frames

def save(frames, path, colors, width):
    # shared palette from sampled frames, no dithering, merge consecutive duplicates
    step = max(1, len(frames) // 6)
    sample = [f for f, _ in frames[::step]][:6]
    mont = Image.new("RGB", (sample[0].width, sample[0].height * len(sample)))
    for i, f in enumerate(sample): mont.paste(f, (0, i * f.height))
    pal = mont.quantize(colors=colors, method=Image.Quantize.MEDIANCUT, dither=Image.Dither.NONE)
    imgs, durs = [], []
    for f, ms in frames:
        q = f.quantize(palette=pal, dither=Image.Dither.NONE)
        if imgs and list(q.getdata()) == list(imgs[-1].getdata()):
            durs[-1] += ms; continue
        imgs.append(q); durs.append(ms)
    h = int(imgs[0].height * width / imgs[0].width)
    imgs = [i.resize((width, h), Image.Resampling.LANCZOS) if i.width != width else i for i in imgs]
    imgs[0].save(path, save_all=True, append_images=imgs[1:], duration=durs, loop=0, optimize=True, disposal=1)
    print(path, os.path.getsize(path) // 1024, "KiB", len(imgs), "frames")

os.makedirs(OUT, exist_ok=True)
fr = render_all(1200)
save(fr, f"{OUT}/demo.gif", 64, 960)
save(fr, f"{OUT}/demo-linkedin.gif", 128, 1200)
