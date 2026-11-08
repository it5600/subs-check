#!/usr/bin/env python3
"""FPK 包离线结构校验（无需 fnpack / 无需飞牛设备）。

依据 fnOS FPK 官方契约实测不变量：
  1. 外层为 gzip tar，含 manifest / app.tgz / cmd/ / config/ / wizard/ / ICON*
  2. 内层 app.tgz 解压后包含 native/ 与 ui/ 目录树
  3. manifest.checksum == app.tgz 内容字节的 MD5
  4. 图标尺寸：ICON.PNG 64x64，ICON_256.PNG 256x256，单文件 <= 1024KB
  5. wizard/* 内容必须是 JSON 数组（不是 shell 脚本）
  6. config/privilege 必须是 JSON 对象（含 defaults.run-as）
  7. config/resource 声明的 data-share 目录名应包含 appname 前缀
  8. ui/config 的 gatewayPrefix 必须等于 /app/<appname 去点转连字符>

用法：
    python3 scripts/verify_fpk.py [path/to/app.fpk]

不带参数时默认校验 dist/trim.substore-ui-go.fpk。
退出码 0 表示全部通过，1 表示存在错误。
"""

import hashlib
import io
import json
import os
import struct
import sys
import tarfile
import tempfile

EXPECTED_APPNAME = "trim.substore-ui-go"
# appname 中的点号在网关前缀中必须转为连字符（fnOS 要求前缀不含 ".")
EXPECTED_PREFIX = "/app/" + EXPECTED_APPNAME.replace(".", "-")

errors = []
warnings = []


def err(msg):
    errors.append(msg)


def warn(msg):
    warnings.append(msg)


def png_size(data):
    """从 PNG 字节流中读取 IHDR 的宽高，失败返回 None。"""
    if len(data) < 24 or data[:8] != b"\x89PNG\r\n\x1a\n":
        return None
    if data[12:16] != b"IHDR":
        return None
    w, h = struct.unpack(">II", data[16:24])
    return w, h


def md5(data):
    return hashlib.md5(data).hexdigest()


def check_fpk(path):
    if not os.path.isfile(path):
        print(f"校验失败: 文件不存在: {path}", file=sys.stderr)
        return 1

    print(f"校验: {path}")
    print(f"大小: {os.path.getsize(path) / 1024 / 1024:.2f} MB")
    print()

    # ---------- 1. 外层 tar ----------
    try:
        outer = tarfile.open(path, "r:gz")
    except Exception as exc:
        print(f"校验失败: 无法作为 gzip tar 打开: {exc}", file=sys.stderr)
        return 1

    outer_names = set(outer.getnames())
    required_top = {"manifest", "app.tgz", "cmd", "config", "wizard"}
    missing = required_top - outer_names
    if missing:
        err(f"外层缺少必需条目: {sorted(missing)}")

    # ---------- 2. manifest ----------
    manifest_bytes = outer.extractfile("manifest").read() if "manifest" in outer_names else b""
    manifest = {}
    if manifest_bytes:
        for line in manifest_bytes.decode("utf-8", "replace").splitlines():
            if "=" in line and not line.startswith("#"):
                k, _, v = line.partition("=")
                manifest[k.strip()] = v.strip()

    appname = manifest.get("appname")
    if appname != EXPECTED_APPNAME:
        err(f"manifest appname 不符合预期: {appname!r} (期望 {EXPECTED_APPNAME})")
    else:
        print(f"[OK] appname = {appname}")

    # manifest.checksum 必须等于 app.tgz 内容字节的 MD5
    app_tgz_bytes = outer.extractfile("app.tgz").read() if "app.tgz" in outer_names else b""
    if app_tgz_bytes:
        actual = md5(app_tgz_bytes)
        declared = manifest.get("checksum") or manifest.get("app.tgz.checksum")
        if declared is None:
            # 部分 fnpack 版本把校验值写在 manifest.checksum，缺失时仅告警
            warn("manifest 未声明 app.tgz 校验值，跳过一致性校验")
        elif actual != declared:
            err(f"manifest.checksum 不匹配: 声明={declared} 实际={actual}")
        else:
            print(f"[OK] manifest.checksum 与 app.tgz MD5 一致")
    else:
        err("外层缺少 app.tgz（嵌套结构不正确）")

    # ---------- 3. 内层 app.tgz ----------
    app_members = set()
    if app_tgz_bytes:
        try:
            inner = tarfile.open(fileobj=io.BytesIO(app_tgz_bytes), mode="r:gz")
            app_members = set(inner.getnames())
        except Exception as exc:
            err(f"app.tgz 无法解析: {exc}")
            inner = None

        if not any(n.rstrip("/").endswith("native") for n in app_members):
            err("app.tgz 内未找到 native/ 目录（二进制与静态资源）")

        if "ui/config" not in app_members:
            err("app.tgz 内缺少 ui/config（桌面入口声明）")

    # ---------- 4. 图标 ----------
    for name, expect_w, expect_h in [("ICON.PNG", 64, 64), ("ICON_256.PNG", 256, 256)]:
        if name not in outer_names:
            err(f"缺少图标 {name}")
            continue
        data = outer.extractfile(name).read()
        if len(data) > 1024 * 1024:
            err(f"{name} 超过 1024KB 限制: {len(data) / 1024:.1f} KB")
        size = png_size(data)
        if size is None:
            err(f"{name} 不是有效 PNG")
        elif size != (expect_w, expect_h):
            err(f"{name} 尺寸不符: {size} (期望 {expect_w}x{expect_h})")
        else:
            print(f"[OK] {name} 尺寸 {expect_w}x{expect_h}")

    # ---------- 5. wizard ----------
    for wname in ("install", "upgrade", "uninstall", "config"):
        wpath = f"wizard/{wname}"
        if wpath not in outer_names:
            warn(f"wizard/{wname} 缺失（生命周期向导）")
            continue
        raw = outer.extractfile(wpath).read()
        try:
            arr = json.loads(raw.decode("utf-8"))
            if not isinstance(arr, list):
                err(f"wizard/{wname} 不是 JSON 数组")
            else:
                print(f"[OK] wizard/{wname} 为合法 JSON 数组")
        except Exception as exc:
            err(f"wizard/{wname} 不是合法 JSON（内容可能是 shell 脚本）: {exc}")

    # ---------- 6. privilege ----------
    if "config/privilege" not in outer_names:
        err("config/privilege 缺失")
    else:
        try:
            priv = json.loads(outer.extractfile("config/privilege").read().decode("utf-8"))
            if not isinstance(priv, dict):
                err("config/privilege 不是 JSON 对象")
            elif priv.get("defaults", {}).get("run-as") not in ("package", "root"):
                warn(f"privilege.defaults.run-as 取值异常: {priv.get('defaults')}")
        except Exception as exc:
            err(f"config/privilege 解析失败: {exc}")

    # ---------- 7. resource ----------
    if "config/resource" not in outer_names:
        err("config/resource 缺失")
    else:
        try:
            res = json.loads(outer.extractfile("config/resource").read().decode("utf-8"))
            shares = res.get("data-share", {}).get("shares", [])
            for s in shares:
                nm = s.get("name", "")
                if "/@appshare/" in nm or nm.startswith(EXPECTED_APPNAME):
                    pass
                else:
                    warn(f"data-share 名称未包含 appname 前缀: {nm!r}")
        except Exception as exc:
            err(f"config/resource 解析失败: {exc}")

    # ---------- 8. ui/config 网关前缀 ----------
    if "ui/config" in app_members:
        inner2 = tarfile.open(fileobj=io.BytesIO(app_tgz_bytes), mode="r:gz") if app_tgz_bytes else None
        if inner2:
            try:
                uicfg = json.loads(inner2.extractfile("ui/config").read().decode("utf-8"))
                prefix = None
                for _id, entry in uicfg.get(".url", {}).items():
                    prefix = entry.get("gatewayPrefix")
                    gsock = entry.get("gatewaySocket")
                    if prefix != EXPECTED_PREFIX:
                        err(f"gatewayPrefix 不符: {prefix!r} (期望 {EXPECTED_PREFIX})")
                    else:
                        print(f"[OK] gatewayPrefix = {prefix}")
                    if gsock != "app.sock":
                        warn(f"gatewaySocket 非惯例名: {gsock!r}")
            except Exception as exc:
                err(f"app/ui/config 解析失败: {exc}")

    # ---------- 汇总 ----------
    print()
    if warnings:
        print("警告:")
        for w in warnings:
            print(f"  - {w}")
        print()
    if errors:
        print("错误:")
        for e in errors:
            print(f"  - {e}")
        print(f"\n校验失败: {len(errors)} 个错误, {len(warnings)} 个警告")
        return 1
    print("校验通过: 无错误" + (f", {len(warnings)} 个警告" if warnings else ""))
    return 0


def main():
    target = sys.argv[1] if len(sys.argv) > 1 else None
    if target is None:
        here = os.path.dirname(os.path.abspath(__file__))
        # 脚本位于 scripts/ 下，产物在项目根目录的 dist/
        target = os.path.join(here, os.pardir, "dist", f"{EXPECTED_APPNAME}.fpk")
    sys.exit(check_fpk(target))


if __name__ == "__main__":
    main()
