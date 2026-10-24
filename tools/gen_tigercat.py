"""橘黑猫（tiger-cat app）的动作帧，输出到 server/apps/tiger-cat/assets/<状态>_<序号>.png。

复用 gen_pet.py 的坐姿格子图和画法（见 docs/pixel-cat.md），换成橘底粗黑条纹（像警示桩），
再加每个状态的道具：书和字母气泡、电视、毛线球、碗和鱼、爱心、领带。

    .venv/bin/python tools/gen_tigercat.py [预览图.png] [预览.gif]
    PET_OUT=/tmp/tiger .venv/bin/python tools/gen_tigercat.py   # 先输出到别的目录预览
"""

import io
import os
import shutil
import sys

from PIL import Image, ImageDraw

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from gen_pet import (BASE, EYES, MOUTHS, GRID, SCALE, X0, Y0, rect, new_frame, finish,  # noqa: E402
                     draw_laptop, draw_typing_paws, draw_z)

OUT = os.environ.get("PET_OUT") or os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "server", "apps", "tiger-cat", "assets")

# 毛色亮橘，条纹深灰：用黑色的话条纹碰到深褐轮廓连成一片，身体看着像断了
PAL = {"K": (72, 34, 12), "F": (255, 140, 26), "S": (70, 70, 78), "D": (225, 112, 14),
       "C": (255, 238, 214), "P": (255, 130, 168), "E": (22, 20, 20), "W": (255, 255, 255),
       "R": (220, 40, 50), "r": (160, 24, 34)}  # R/r 领带


def _arc(steps):
    # 一道绕着身体的弯条纹：steps 是 [(离中线最远几列, 这一段占哪几行)]，中间高、两边往下弯
    cells = set()
    for c in range(25):
        for maxd, rows in steps:
            if abs(c - 12) <= maxd:
                cells |= {(r, c) for r in rows}
                break
    return cells


# 身体上两道粗条纹。头上只留额头三道细纹（BASE 里的），脸颊、身体两侧的纹去掉，不然像老虎；
# 也不要白围兜，围兜会把条纹截成左右两块，看着像斑
_STRIPES = _arc([(2, (19, 20)), (5, (20, 21)), (99, (21, 22))]) | _arc([(3, (23, 24)), (99, (24, 25))])

# 耳朵耷拉（不开心）：替换第 0~3 行
DROOP_EARS = [
    "............................",
    "............................",
    "...KKK.............KKK......",
    "...KFFKKKKKKKKKKKKKFFK......",
]

EYES = dict(EYES)
EYES["left"] = ["FFF", "EEF", "EEF", "EEF"]   # 往左看（看电视、看毛线球）
EYES["right"] = ["FFF", "FEE", "FEE", "FEE"]  # 往右看
EYES["down"] = ["FFF", "FFF", "FFF", "EEE"]   # 往下看（看书、看碗）
EYES["sad"] = ["FFF", "FFF", "EEE", "WEE"]    # 无精打采，眼睛半闭、泛泪光
MOUTHS = dict(MOUTHS)
MOUTHS["frown"] = ["CCC", "CKC", "KCK"]
MOUTHS["chew"] = ["CCC", "CKC", "CCC"]
MOUTHS["tongue"] = ["CKC", "KPK", "CPC"]  # 舔嘴


def grid(eyes="open", mouth="smile", ears="up", tie=False):
    g = [list(r) for r in BASE]
    for r in range(8, len(g)):
        g[r] = ["F" if ch == "S" or (18 <= r <= 24 and ch == "C") else ch for ch in g[r]]
    for r, c in _STRIPES:
        if g[r][c] in "FD":
            g[r][c] = "S"
    if ears == "droop":
        for r, row in enumerate(DROOP_EARS):
            g[r] = list(row)
    for i, row in enumerate(EYES[eyes]):
        for j, ch in enumerate(row):
            g[7 + i][6 + j] = ch
            g[7 + i][16 + j] = ch
    for i, row in enumerate(MOUTHS[mouth]):
        for j, ch in enumerate(row):
            g[12 + i][11 + j] = ch
    if tie:  # 胸前一条红领带
        for r, cols in ((18, "rRr"), (19, ".R."), (20, "RRR"), (21, "RRR"), (22, "RRR"), (23, ".R.")):
            for j, ch in enumerate(cols):
                if ch != ".":
                    g[r][11 + j] = "r" if ch == "r" else "R"
    return g


def paste(img, g, dx=0, dy=0):
    for r, row in enumerate(g):
        for c, ch in enumerate(row):
            if ch in PAL:
                x, y = X0 + c + dx, Y0 + r + dy
                if 0 <= x < GRID and 0 <= y < GRID:
                    img.putpixel((x, y), PAL[ch] + (255,))


def tail(img, dx, dy, wag):
    # 同 gen_pet.draw_tail，橘黑交替；多一个 dx（猫整体挪位置时跟着挪）
    d = ImageDraw.Draw(img)
    for i, (x, y) in enumerate([(29, 32), (30, 31), (31, 30), (31 + wag, 29), (31 + wag, 28)]):
        rect(d, x + dx, y + dy, x + dx + 1, y + dy + 1, PAL["S"] if i % 2 else PAL["F"])


def cat(img, dx=0, dy=0, wag=0, **kw):
    tail(img, dx, dy, wag)
    paste(img, grid(**kw), dx, dy)


def sprite(img, x, y, rows, colors):
    for j, row in enumerate(rows):
        for i, ch in enumerate(row):
            if ch in colors:
                px, py = x + i, y + j
                if 0 <= px < GRID and 0 <= py < GRID:
                    img.putpixel((px, py), colors[ch] + (255,))


INK = (40, 30, 24)
FONT = {  # 3×5 像素字
    "A": [".X.", "X.X", "XXX", "X.X", "X.X"],
    "B": ["XX.", "X.X", "XX.", "X.X", "XX."],
    "C": [".XX", "X..", "X..", "X..", ".XX"],
    "H": ["X.X", "X.X", "XXX", "X.X", "X.X"],
    "i": [".X.", "...", ".X.", ".X.", ".X."],
}
HEART = [".X.X.", "XXXXX", "XXXXX", ".XXX.", "..X.."]


def bubble(img, text):
    d = ImageDraw.Draw(img)
    w = len(text) * 4 + 3
    x0, y0 = 39 - w, 0
    d.rounded_rectangle((x0, y0, 39, 8), radius=2, fill=INK)
    d.rounded_rectangle((x0 + 1, y0 + 1, 38, 7), radius=1, fill=(255, 255, 255))
    rect(d, x0 + 2, 9, x0 + 3, 9, INK)  # 气泡的小尾巴，指向猫头
    rect(d, x0 + 1, 10, x0 + 1, 10, INK)
    for k, ch in enumerate(text):
        sprite(img, x0 + 2 + k * 4, 2, FONT[ch], {"X": (46, 120, 230)})


def book(img, flip):
    # 摊开的书，压在猫前面；flip 时右页翻起来一点
    d = ImageDraw.Draw(img)
    rect(d, 10, 33, 30, 38, INK)
    rect(d, 11, 33, 19, 37, (255, 252, 240))
    rect(d, 21, 33 - (2 if flip else 0), 29, 37, (255, 252, 240))
    if flip:
        rect(d, 20, 31, 20, 32, INK)
        rect(d, 21, 30, 29, 30, INK)
        rect(d, 30, 31, 30, 32, INK)
    rect(d, 20, 33, 20, 38, (200, 60, 60))  # 书脊
    for y in (34, 36):
        rect(d, 12, y, 18, y, (170, 170, 180))
        rect(d, 22, y, 28, y, (170, 170, 180))
    rect(d, 10, 38, 30, 39, (200, 60, 60))  # 封面


TV_SCREENS = [(80, 200, 255), (120, 230, 140), (255, 210, 90), (255, 120, 160)]


def tv(img, k):
    # 电视放在猫左边，猫往右挪了 4 格；k 换屏幕颜色
    d = ImageDraw.Draw(img)
    rect(d, 2, 19, 2, 21, (130, 130, 140))  # 天线
    rect(d, 9, 19, 9, 21, (130, 130, 140))
    rect(d, 0, 22, 12, 33, (60, 64, 76))
    rect(d, 1, 23, 10, 31, TV_SCREENS[k % 4])
    rect(d, 1 + (k * 3) % 8, 26, 2 + (k * 3) % 8, 28, (255, 255, 255))  # 屏幕里的小人
    rect(d, 11, 24, 11, 24, (255, 90, 80))
    rect(d, 2, 34, 3, 37, (60, 64, 76))
    rect(d, 9, 34, 10, 37, (60, 64, 76))


YARN = ["..XXX..", ".XLXXX.", "XXXLXXX", "XLXXLXX", "XXLXXXX", ".XXXLX.", "..XXX.."]


def yarn(img, x, y):
    sprite(img, x, y, YARN, {"X": (90, 170, 255), "L": (200, 230, 255)})


FISH = ["..XX....X", ".XXXX.XX.", "XWXXXXX..", ".XXXX.XX.", "..XX....X"]


def bowl(img, full):
    d = ImageDraw.Draw(img)
    if full:
        sprite(img, 15, 29, FISH, {"X": (60, 120, 200), "W": (255, 255, 255)})
    rect(d, 11, 34, 29, 35, (220, 60, 70))
    rect(d, 12, 36, 28, 37, (200, 45, 55))
    rect(d, 14, 38, 26, 39, (160, 30, 40))
    rect(d, 13, 35, 27, 35, (255, 255, 255))  # 碗口一道白边


def frames_sleep():
    out = []
    for i in range(8):
        img = new_frame()
        dy = 1 if i % 4 in (1, 2) else 0
        cat(img, dy=dy, eyes="closed")
        k = i % 4
        d = ImageDraw.Draw(img)
        draw_z(d, 31, 10 - k * 2, 3)
        if k >= 2:
            draw_z(d, 34, 4 - (k - 2) * 2, 4)
        out.append(finish(img))
    return out


def frames_work():
    out = []
    for i in range(6):
        img = new_frame()
        dy = 1 if i in (2, 3) else 0
        cat(img, dy=dy, wag=i % 2, eyes="focus", tie=True)
        draw_laptop(ImageDraw.Draw(img), glow=i % 3 != 2)
        draw_typing_paws(img, i % 2 == 0, PAL)
        out.append(finish(img))
    return out


def frames_english():
    out = []
    words = ["A", "A", "B", "B", "C", "C", "Hi", "Hi"]
    for i in range(8):
        img = new_frame()
        dy = 1 if i % 2 else 0  # 跟着念，一点一点头
        cat(img, dy=dy, eyes="down", mouth="open" if i % 2 == 0 else "smile")
        book(img, flip=i in (6, 7))
        bubble(img, words[i])
        out.append(finish(img))
    return out


def frames_tv():
    out = []
    for i in range(8):
        img = new_frame()
        cat(img, dx=4, wag=0, eyes="left", mouth="open" if i in (4, 5) else "smile")  # 看到好笑的张嘴
        tv(img, i // 2)
        out.append(finish(img))
    return out


def frames_toy():
    # 毛线球在猫左边上下弹，猫的眼睛跟着；尾巴摇得快
    out = []
    path = [(0, 31), (0, 25), (1, 21), (1, 25), (0, 31), (0, 31)]
    eyes = ["left", "left", "wide", "left", "down", "left"]
    for i, (x, y) in enumerate(path):
        img = new_frame()
        cat(img, dx=3, dy=-1 if i == 2 else 0, wag=1 if i % 2 else -1, eyes=eyes[i], mouth="open" if i == 2 else "smile")
        yarn(img, x, y)
        d = ImageDraw.Draw(img)
        rect(d, x + 6, y + 5, x + 9, y + 5, (90, 170, 255))  # 线头
        out.append(finish(img))
    return out


def frames_happy():
    out = []
    spots = [(2, 14), (32, 12), (5, 4), (30, 2)]
    for i, dy in enumerate([0, -1, -2, -1, 0, 0]):
        img = new_frame()
        cat(img, dy=dy, wag=1 if i % 2 else -1, eyes="happy", mouth="open")
        for k, (x, y) in enumerate(spots):  # 爱心往上飘，错开
            fy = y - ((i + k * 2) % 6)
            sprite(img, x, fy, HEART, {"X": (255, 90, 130)})
        out.append(finish(img))
    return out


def frames_eat():
    out = []
    for i in range(6):
        img = new_frame()
        dy = 1 if i % 2 else 0  # 低头一口一口
        cat(img, dy=dy, eyes="happy" if i % 2 else "down", mouth="chew" if i % 2 else "open")
        bowl(img, full=True)
        out.append(finish(img))
    return out


def frames_hungry():
    # 碗空了，看看碗、舔舔嘴
    out = []
    for i in range(6):
        img = new_frame()
        cat(img, eyes="down" if i < 3 else "open", mouth="tongue" if i in (3, 4) else "smile")
        bowl(img, full=False)
        out.append(finish(img))
    return out


def frames_sad():
    out = []
    for i in range(6):
        img = new_frame()
        dy = 1 if i in (2, 3, 4) else 0  # 叹气
        cat(img, dy=dy, eyes="sad", mouth="frown", ears="droop")
        d = ImageDraw.Draw(img)
        # 头顶一小团乌云
        rect(d, 30, 2, 37, 5, (120, 126, 140))
        rect(d, 32, 1, 35, 1, (120, 126, 140))
        for k, x in enumerate((31, 34, 37)):
            y = 7 + (i + k) % 3 * 2
            rect(d, x, y, x, y + 1, (90, 160, 255))
        out.append(finish(img))
    return out


STATES = {
    "sleep": frames_sleep, "work": frames_work, "english": frames_english, "tv": frames_tv,
    "toy": frames_toy, "happy": frames_happy, "eat": frames_eat, "hungry": frames_hungry, "sad": frames_sad,
}


def main():
    rows, files = [], {}
    for state, fn in STATES.items():
        frames = fn()
        rows.append(frames)
        for i, img in enumerate(frames):
            buf = io.BytesIO()
            img.save(buf, format="PNG", optimize=True)
            files[f"{state}_{i}.png"] = buf.getvalue()

    for f in os.listdir(OUT) if os.path.isdir(OUT) else []:  # 删掉旧帧
        if f.endswith(".png") and f.split("_")[0] in STATES:
            os.remove(os.path.join(OUT, f))
    os.makedirs(OUT, exist_ok=True)
    for name, data in files.items():
        with open(os.path.join(OUT, name), "wb") as f:
            f.write(data)
    print("帧目录:", OUT)

    # 手环上的图标（96×96）：猫头，开心的表情
    icon = new_frame()
    paste(icon, grid(eyes="happy"))
    head = icon.crop((X0, Y0 - 3, X0 + 24, Y0 + 21))  # 头 21 格宽、18 格高，四周留一点
    head.resize((96, 96), Image.NEAREST).save(os.path.join(OUT, "..", "icon.png"))

    size = GRID * SCALE
    if len(sys.argv) > 1:  # 预览：每行一个状态
        w = max(len(r) for r in rows) * size
        sheet = Image.new("RGBA", (w, len(rows) * size), (12, 14, 20, 255))
        for r, fs in enumerate(rows):
            for c, f in enumerate(fs):
                sheet.alpha_composite(f, (c * size, r * size))
        sheet.save(sys.argv[1])
    if len(sys.argv) > 2:  # GIF：所有状态排成一格一格同时动
        cols = 3
        n = max(len(r) for r in rows) * 2
        gif = []
        for t in range(n):
            sheet = Image.new("RGBA", (cols * size, -(-len(rows) // cols) * size), (12, 14, 20, 255))
            for k, fs in enumerate(rows):
                sheet.alpha_composite(fs[t % len(fs)], ((k % cols) * size, (k // cols) * size))
            gif.append(sheet.convert("RGB"))
        gif[0].save(sys.argv[2], save_all=True, append_images=gif[1:], duration=300, loop=0)


if __name__ == "__main__":
    main()
