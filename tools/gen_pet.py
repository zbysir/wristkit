"""手绘像素猫的动作帧，输出到 Agent 猫的 server/apps/agent-cat/assets/<皮肤>/<状态>_<序号>.png。
插件宿主按图片内容哈希推到手环，换了图会自动推新的，不用管手环缓存。

猫本身是一张字符格子图（BASE），每个字符一个像素：
  K 轮廓  F 毛色  S 条纹  D 暗部  C 浅色（口鼻、围兜、爪子）  P 粉色  E 眼睛  W 眼睛高光  . 透明
表情是在格子图上替换几个格子；尾巴、牌子、Z、彩纸、笔记本用代码画（尾巴先画，压在身体后面）。
画布 40×40 格，按最近邻放大 SCALE 倍。三种猫共用格子图，只换配色。
怎么画、踩过哪些坑见 docs/pixel-cat.md。

    .venv/bin/python tools/gen_pet.py [预览图.png]
    PET_OUT=/tmp/pet .venv/bin/python tools/gen_pet.py   # 先输出到别的目录预览
"""

import io
import os
import random
import shutil
import sys

from PIL import Image, ImageDraw

GRID = 40
SCALE = 4
OUT = os.environ.get("PET_OUT") or os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "server", "apps", "agent-cat", "assets")
# App 自己的一套（菜单栏图标、连接向导里的猫），和插件的分开放，只要用到的几种动作
APP_OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "server", "app", "frontend", "public", "pet")
APP_STATES = ("sleep", "work", "celebrate")
APP_SKINS = ("orange", "gray", "white")  # Mac App 菜单栏、连接向导用的猫；Clawd 只在手环上

# 坐着的猫，正面（28 列 × 28 行），左右对称（第 c 列 ↔ 第 24-c 列）。尾巴不在格子图里，见 draw_tail。
# 第 0~17 行是头，21 格宽（第 2~22 列）：小耳朵，耳朵内侧斜线直接接到平的头顶（中间不要拱起）；
# 两侧上下都往里收一点（圆）；额头三道纹；大白口鼻。
# 第 18~27 行是身体，最宽 19 格（第 3~21 列），比头窄：身体和头一样宽就显胖。
BASE = """
....K...............K.......
....KK.............KK.......
....KFK...........KFK.......
....KFFKKKKKKKKKKKFFK.......
....KFFFFFSFSFSFFFFFK.......
...KFFFFFFSFSFSFFFFFFK......
...KFFFFFFSFSFSFFFFFFK......
..KFFFFFFFSFFFSFFFFFFFK.....
..KFFFFFFFFFFFFFFFFFFFK.....
..KFFFFFFFFFFFFFFFFFFFK.....
..KSSFFFFFFFFFFFFFFFSSK.....
..KFFFFFFFCPPPCFFFFFFFK.....
..KSSPFFFCCCCCCCFFFPSSK.....
..KFFFFFCCCCCCCCCFFFFFK.....
...KFFFFCCCCCCCCCFFFFK......
....KFFFFCCCCCCCFFFFK.......
.....KKFFFCCCCCFFFKK........
.....KKKKKKKKKKKKKKK........
.....KFFFFFFFFFFFFFK........
.....KFFFCCCCCCCFFFK........
....KFFFCCCCCCCCCFFFK.......
....KFFFFCCCCCCCFFFFK.......
...KSFFFFCCCCCCCFFFFSK......
...KFFFFFFCCCCCFFFFFFK......
...KSDFFFFFCCCFFFFFDSK......
...KDDFFKKKKFKKKKFFDDK......
...KDDFKCCCCKCCCCKFDDK......
....KKKKKKKKKKKKKKKKK.......
""".strip("\n").split("\n")

# 眼睛（左眼占第 6~8 列，右眼第 16~18 列；第 7~10 行）。每种表情给出左眼 3 列 × 4 行，右眼照抄（高光都在左上）
EYES = {
    "open": ["FFF", "WEE", "EEE", "EEE"],
    "wide": ["WEE", "WEE", "EEE", "EEE"],   # 更高的眼睛：吃惊
    "focus": ["FFF", "FFF", "EEE", "EEE"],  # 往下看屏幕
    "happy": ["FFF", "FFF", "FEF", "EFE"],  # ^ ^
    "closed": ["FFF", "FFF", "EEE", "FFF"],  # 闭眼的一道线
}
# 嘴（第 12~14 行，第 11~13 列，在白色口鼻部里）
MOUTHS = {
    "smile": ["CKC", "KCK", "CCC"],  # ω
    "open": ["CCC", "KKK", "KKK"],
}

# 道具和配色
C = {
    "laptop": (120, 128, 140),
    "laptop_dark": (70, 76, 86),
    "screen": (46, 160, 255),
    "screen_dim": (24, 90, 160),
    "sign": (255, 159, 28),
    "sign_text": (255, 255, 255),
    "stick": (150, 100, 60),
    "z": (140, 170, 255),
}
SKINS = {
    "orange": {"K": (59, 35, 20), "F": (255, 159, 67), "S": (214, 110, 32), "D": (230, 135, 50),
               "C": (255, 236, 210), "P": (255, 130, 168), "E": (30, 22, 18), "W": (255, 255, 255)},
    "gray": {"K": (36, 38, 44), "F": (150, 152, 160), "S": (92, 94, 104), "D": (128, 130, 138),
             "C": (228, 228, 232), "P": (255, 130, 168), "E": (24, 24, 28), "W": (255, 255, 255)},
    "white": {"K": (52, 54, 62), "F": (236, 237, 241), "S": (88, 90, 100), "D": (210, 212, 220),
              "C": (255, 255, 255), "P": (255, 130, 168), "E": (24, 24, 28), "W": (255, 255, 255)},
}
CONFETTI = [(255, 99, 132), (61, 220, 132), (46, 160, 255), (255, 206, 86), (186, 104, 255)]

# 猫在 40×40 画布上的位置（格子图左上角）
X0, Y0 = 8, 10


def cat_grid(eyes="open", mouth="smile"):
    g = [list(r) for r in BASE]

    def put(r, c, ch):
        if 0 <= r < len(g):
            while c >= len(g[r]):
                g[r].append(".")
            if c >= 0:
                g[r][c] = ch

    for i, row in enumerate(EYES[eyes]):
        for j, ch in enumerate(row):
            put(7 + i, 6 + j, ch)
            put(7 + i, 16 + j, ch)
    for i, row in enumerate(MOUTHS[mouth]):
        for j, ch in enumerate(row):
            put(12 + i, 11 + j, ch)
    return g


def paste(img, g, dx=0, dy=0, pal=None):
    for r, row in enumerate(g):
        for c, ch in enumerate(row):
            if ch in pal:
                x, y = X0 + c + dx, Y0 + r + dy
                if 0 <= x < GRID and 0 <= y < GRID:
                    img.putpixel((x, y), pal[ch] + (255,))


def rect(d, x0, y0, x1, y1, color):
    d.rectangle((x0, y0, x1, y1), fill=color)


def draw_tail(img, dy, wag, pal):
    # 尾巴：5 个 2×2 的方块从身体右下角斜着往上翘，毛色和条纹交替，不描边；
    # 先画，身体盖住尾巴根。wag（-1/0/1）让上半截左右摆
    d = ImageDraw.Draw(img)
    for i, (x, y) in enumerate([(29, 32), (30, 31), (31, 30), (31 + wag, 29), (31 + wag, 28)]):
        rect(d, x, y + dy, x + 1, y + dy + 1, pal["S"] if i % 2 else pal["F"])


def draw_laptop(d, glow):
    rect(d, 11, 33, 29, 34, C["laptop_dark"])
    rect(d, 12, 35, 28, 37, C["laptop"])
    rect(d, 13, 35, 27, 36, C["screen"] if glow else C["screen_dim"])
    rect(d, 9, 38, 31, 39, C["laptop_dark"])


def draw_typing_paws(img, left_up, pal):
    # 敲键盘的两只爪子，搭在笔记本上沿
    for x, up in ((14, left_up), (23, not left_up)):
        y = 31 - (1 if up else 0)
        for dx in range(4):
            img.putpixel((x + dx, y), pal["K"] + (255,))
            img.putpixel((x + dx, y + 1), pal["C"] + (255,))
        img.putpixel((x - 1, y + 1), pal["K"] + (255,))
        img.putpixel((x + 4, y + 1), pal["K"] + (255,))


def draw_sign(d, dy, wobble):
    x = 32 + wobble
    y = 3 + dy  # 跳到最高（dy=-3）时牌子上沿刚好在画布顶部
    rect(d, x + 2, y + 8, x + 3, y + 20, C["stick"])
    d.rounded_rectangle((x - 2, y, x + 7, y + 9), radius=2, fill=(40, 30, 20))
    d.rounded_rectangle((x - 1, y + 1, x + 6, y + 8), radius=1, fill=C["sign"])
    rect(d, x + 2, y + 2, x + 3, y + 5, C["sign_text"])
    rect(d, x + 2, y + 7, x + 3, y + 7, C["sign_text"])


def draw_z(d, x, y, size):
    for i in range(size):
        d.point((x + i, y), fill=C["z"])
        d.point((x + i, y + size - 1), fill=C["z"])
        d.point((x + size - 1 - i, y + i), fill=C["z"])


def new_frame():
    return Image.new("RGBA", (GRID, GRID), (0, 0, 0, 0))


def finish(img):
    return img.resize((GRID * SCALE, GRID * SCALE), Image.NEAREST)


def work_frames(pal):
    out = []
    for i in range(6):
        img = new_frame()
        dy = 1 if i in (2, 3) else 0
        draw_tail(img, dy, i % 2, pal)
        paste(img, cat_grid(eyes="focus"), dy=dy, pal=pal)
        draw_laptop(ImageDraw.Draw(img), glow=i % 3 != 2)
        draw_typing_paws(img, i % 2 == 0, pal)
        out.append(finish(img))
    return out


def alert_frames(pal):
    out = []
    for i, dy in enumerate([0, -2, -3, -2, 0, 0]):
        img = new_frame()
        draw_tail(img, dy, 1 if i % 2 else -1, pal)
        paste(img, cat_grid(eyes="wide", mouth="open"), dy=dy, pal=pal)
        draw_sign(ImageDraw.Draw(img), dy, wobble=1 if i % 2 else 0)
        out.append(finish(img))
    return out


def celebrate_frames(pal):
    rnd = random.Random(7)
    out = []
    # 不举手：跳起来 + ^ ^ + 张嘴 + 彩纸。手往外伸在这么小的画布上怎么画都突兀
    for i, dy in enumerate([0, -1, -2, -1, 0, 0]):
        bits = [(rnd.randrange(1, 39), rnd.randrange(0, 6), rnd.choice(CONFETTI)) for _ in range(14)]
        img = new_frame()
        draw_tail(img, dy, 1, pal)
        paste(img, cat_grid(eyes="happy", mouth="open"), dy=dy, pal=pal)
        d = ImageDraw.Draw(img)
        for x, y, c in bits:
            fy = y + (i * 2) % 6  # 彩纸往下飘，只在头顶上方
            rect(d, x, fy, x + 1, fy, c)
        out.append(finish(img))
    return out


def sleep_frames(pal):
    out = []
    for i in range(8):
        img = new_frame()
        dy = 1 if i % 4 in (1, 2) else 0
        draw_tail(img, dy, 0, pal)
        paste(img, cat_grid(eyes="closed"), dy=dy, pal=pal)
        d = ImageDraw.Draw(img)
        k = i % 4
        draw_z(d, 31, 10 - k * 2, 3)
        if k >= 2:
            draw_z(d, 34, 4 - (k - 2) * 2, 4)
        out.append(finish(img))
    return out


# ---------- Clawd：Claude Code 的吉祥物 ----------
# 不是猫，单独画：一块扁方的橙色身子，两只竖条眼睛，左右两只小短手，底下四条小短腿，不描边。
# 照着 Claude Code 欢迎界面里的字符画（▐▛███▜▌ / ▝▜█████▛▘ / ▘▘ ▝▝）放大：身子 24×16，手 4×4，腿 2×4。
# 左右对称，中线在 x=19.5。没有尾巴、没有嘴；表情只换眼睛，动作靠挪手和整只上下跳。
CLAWD = {"F": (217, 119, 87), "E": (24, 18, 16)}
CLAWD_EYES = {  # 相对左眼左上角 (12, 20) 的点，右眼平移 14 格
    "open": [(x, y) for x in (0, 1) for y in range(4)],
    "wide": [(x, y) for x in (0, 1) for y in range(-1, 4)],  # 更高：吃惊
    "focus": [(x, y) for x in (0, 1) for y in (2, 3)],  # 往下看屏幕
    "happy": [(0, 1), (1, 1), (-1, 2), (2, 2)],  # ^ ^
    "closed": [(x, 3) for x in range(-1, 3)],  # 一道线
}


def draw_clawd(img, dy=0, eyes="open", arms=(0, 0)):
    # arms：左右手相对平常位置往上挪几格（负数往下）
    d = ImageDraw.Draw(img)
    rect(d, 8, 16 + dy, 31, 31 + dy, CLAWD["F"])
    for x in (10, 14, 24, 28):
        rect(d, x, 32 + dy, x + 1, 35 + dy, CLAWD["F"])
    for x, up in ((4, arms[0]), (32, arms[1])):
        rect(d, x, 24 + dy - up, x + 3, 27 + dy - up, CLAWD["F"])
    for ex in (12, 26):
        for x, y in CLAWD_EYES[eyes]:
            img.putpixel((ex + x, 20 + dy + y), CLAWD["E"] + (255,))


def draw_laptop_back(d, glow):
    # 笔记本从背面看：Clawd 坐在后面，屏幕朝着它，挡住下半身和腿。猫用的 draw_laptop 屏幕朝外、压在身子下沿，
    # 套在 Clawd 上腿从后面露出来，手也够不着
    rect(d, 9, 27, 30, 35, C["laptop_dark"])
    rect(d, 10, 28, 29, 34, C["laptop"])
    rect(d, 19, 30, 20, 31, (255, 255, 255) if glow else (180, 186, 196))  # 背面的小灯
    rect(d, 7, 36, 32, 37, C["laptop_dark"])


def clawd_work_frames():
    out = []
    for i in range(6):
        img = new_frame()
        dy = 1 if i in (2, 3) else 0
        draw_clawd(img, dy, "focus", (2, 0) if i % 2 == 0 else (0, 2))  # 两只手轮流敲
        draw_laptop_back(ImageDraw.Draw(img), glow=i % 3 != 2)
        out.append(finish(img))
    return out


def clawd_alert_frames():
    out = []
    for i, dy in enumerate([0, -2, -3, -2, 0, 0]):
        img = new_frame()
        draw_sign(ImageDraw.Draw(img), dy, wobble=0)  # 牌子杆正好立在右手上，像举着
        draw_clawd(img, dy, "wide", (1 if i % 2 else 0, 0))
        out.append(finish(img))
    return out


def clawd_celebrate_frames():
    rnd = random.Random(7)
    out = []
    for i, dy in enumerate([0, -1, -2, -1, 0, 0]):
        bits = [(rnd.randrange(1, 39), rnd.randrange(0, 6), rnd.choice(CONFETTI)) for _ in range(14)]
        img = new_frame()
        up = 4 if i % 2 else 2  # 两只手一起往上挥
        draw_clawd(img, dy, "happy", (up, up))
        d = ImageDraw.Draw(img)
        for x, y, c in bits:
            fy = y + (i * 2) % 6
            rect(d, x, fy, x + 1, fy, c)
        out.append(finish(img))
    return out


def clawd_sleep_frames():
    out = []
    for i in range(8):
        img = new_frame()
        dy = 1 if i % 4 in (1, 2) else 0
        draw_clawd(img, dy, "closed", (-1, -1))  # 手垂下来一点
        d = ImageDraw.Draw(img)
        k = i % 4
        draw_z(d, 31, 10 - k * 2, 3)
        if k >= 2:
            draw_z(d, 34, 4 - (k - 2) * 2, 4)
        out.append(finish(img))
    return out


STATES = {"work": work_frames, "alert": alert_frames, "celebrate": celebrate_frames, "sleep": sleep_frames}


CLAWD_STATES = {"work": clawd_work_frames, "alert": clawd_alert_frames, "celebrate": clawd_celebrate_frames, "sleep": clawd_sleep_frames}


def main():
    rows, files = [], {}
    for skin in list(SKINS) + ["clawd"]:
        row = []
        for state in STATES:
            frames = CLAWD_STATES[state]() if skin == "clawd" else STATES[state](SKINS[skin])
            for i, img in enumerate(frames):
                buf = io.BytesIO()
                img.save(buf, format="PNG", optimize=True)
                files[f"{skin}/{state}_{i}.png"] = buf.getvalue()
            row.append(frames)
        rows.append(row)

    for out, skins, keep in ((OUT, None, None), (APP_OUT, APP_SKINS, APP_STATES)):
        for skin in skins or list(SKINS) + ["clawd"]:  # 删掉旧帧（比如帧数变少时多出来的）
            shutil.rmtree(os.path.join(out, skin), ignore_errors=True)
        for name, data in files.items():
            skin, file = name.split("/")
            if (skins and skin not in skins) or (keep and file.split("_")[0] not in keep):
                continue
            path = os.path.join(out, name)
            os.makedirs(os.path.dirname(path), exist_ok=True)
            with open(path, "wb") as f:
                f.write(data)
        print("帧目录:", out)

    if len(sys.argv) > 1:
        # 预览：每行一种猫，依次是每个状态的所有帧
        frames = [[f for st in row for f in st] for row in rows]
        w = max(len(r) for r in frames) * GRID * SCALE
        sheet = Image.new("RGBA", (w, len(frames) * GRID * SCALE), (12, 14, 20, 255))
        for r, fs in enumerate(frames):
            for c, f in enumerate(fs):
                sheet.alpha_composite(f, (c * GRID * SCALE, r * GRID * SCALE))
        sheet.save(sys.argv[1])


if __name__ == "__main__":
    main()
