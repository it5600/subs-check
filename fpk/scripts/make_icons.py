#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""生成 Sub-Store 管理面板的应用图标（64x64 / 256x256）。

设计语言：与前端 Industrial 工业仪表盘一致 —— 深色底 (#0d1117) + 蓝色强调 (#58a6ff)。
图形：节点网络（三节点连线）+ 绿色脉冲点，寓意"订阅节点测活与管理"。

用法: python make_icons.py <输出目录>
"""
import os
import sys
from PIL import Image, ImageDraw

BG = (13, 17, 23)
BG_SURFACE = (28, 33, 40)
ACCENT = (88, 166, 255)
ACCENT_DIM = (88, 166, 255, 60)
SUCCESS = (63, 185, 80)
BORDER = (88, 166, 255, 70)

SS = 8  # 超采样倍数


def _rounded_rect(draw, box, radius, fill):
    draw.rounded_rectangle(box, radius=radius, fill=fill)


def build(size: int) -> Image.Image:
    """按目标像素尺寸绘制图标，内部以 SS 倍超采样后缩放，保证边缘平滑。"""
    n = size * SS
    img = Image.new("RGBA", (n, n), (0, 0, 0, 0))
    d = ImageDraw.Draw(img)

    # ---- 圆角方形底板 ----
    pad = int(n * 0.03)
    _rounded_rect(d, (pad, pad, n - pad, n - pad), radius=int(n * 0.22), fill=BG)

    # 顶部高光，模拟仪表盘玻璃质感
    _rounded_rect(
        d,
        (pad, pad, n - pad, int(n * 0.46)),
        radius=int(n * 0.22),
        fill=BG_SURFACE,
    )
    # 覆盖下半部分圆角，修正高光块的底角
    d.rectangle((pad, int(n * 0.30), n - pad, int(n * 0.46)), fill=BG_SURFACE)
    _rounded_rect(d, (pad, int(n * 0.28), n - pad, int(n * 0.50)), radius=int(n * 0.10), fill=BG_SURFACE)

    # ---- 边框 ----
    d.rounded_rectangle(
        (pad, pad, n - pad, n - pad),
        radius=int(n * 0.22),
        outline=BORDER,
        width=max(1, int(n * 0.012)),
    )

    # ---- 节点网络：三节点连线 ----
    cx, cy = n * 0.5, n * 0.435
    r_node = n * 0.088
    line_w = max(2, int(n * 0.028))

    pts = [
        (cx - n * 0.205, cy - n * 0.145),  # 左上
        (cx + n * 0.205, cy - n * 0.145),  # 右上
        (cx, cy + n * 0.195),              # 下方
    ]

    # 连线（先画，节点覆盖其端点）
    for a, b in ((0, 1), (0, 2), (1, 2)):
        d.line((pts[a], pts[b]), fill=ACCENT_DIM, width=line_w)

    # 节点圆
    for i, (px, py) in enumerate(pts):
        # 外发光
        glow = int(n * 0.035)
        d.ellipse(
            (px - r_node - glow, py - r_node - glow, px + r_node + glow, py + r_node + glow),
            fill=(88, 166, 255, 28),
        )
        d.ellipse((px - r_node, py - r_node, px + r_node, py + r_node), fill=ACCENT)
        # 节点内核
        inner = r_node * 0.42
        d.ellipse((px - inner, py - inner, px + inner, py + inner), fill=(13, 17, 23, 255))

    # ---- 绿色脉冲点（测活状态指示）----
    px, py = cx + n * 0.205, cy - n * 0.145
    pr = n * 0.048
    d.ellipse((px - pr * 1.7, py - pr * 1.7, px + pr * 1.7, py + pr * 1.7), fill=(63, 185, 80, 40))
    d.ellipse((px - pr, py - pr, px + pr, py + pr), fill=SUCCESS)

    # ---- 底部信号条（测活/流媒体检测）----
    bar_w = n * 0.048
    gap = n * 0.032
    base_y = n * 0.885
    heights = [0.075, 0.135, 0.100, 0.160]
    total = len(heights) * bar_w + (len(heights) - 1) * gap
    x0 = cx - total / 2
    for i, h in enumerate(heights):
        x = x0 + i * (bar_w + gap)
        d.rounded_rectangle(
            (x, base_y - n * h, x + bar_w, base_y),
            radius=bar_w * 0.4,
            fill=ACCENT if i % 2 == 0 else SUCCESS,
        )

    return img.resize((size, size), Image.LANCZOS)


def main():
    out_dir = sys.argv[1] if len(sys.argv) > 1 else "."
    os.makedirs(out_dir, exist_ok=True)
    for size in (64, 256):
        img = build(size)
        path = os.path.join(out_dir, f"icon_{size}.png")
        img.save(path, "PNG", optimize=True)
        print(f"generated {path} ({os.path.getsize(path)} bytes)")


if __name__ == "__main__":
    main()
