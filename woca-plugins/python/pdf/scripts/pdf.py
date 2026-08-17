#!/usr/bin/env python3
"""woca pdf 插件的内嵌脚本（由 woca 物化后在 uv 管理的 venv 中执行）。

协议：子命令 + 参数，stdout 输出单个 JSON 对象；失败时 {"ok": false, "error": ...}
且退出码为 1。woca 的 PdfRead / PdfWrite 工具是唯一调用方。

用法：
  pdf.py text <file> [--pages 1-3,5] [--max-chars N]
  pdf.py metadata <file>
  pdf.py merge <output> <input> [<input> ...]
  pdf.py split <file> <outdir>
  pdf.py rotate <file> <angle> <output>          # angle 为 90 的倍数
  pdf.py encrypt <file> <password> <output>
  pdf.py decrypt <file> <password> <output>
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from pypdf import PdfReader, PdfWriter


def out(obj: dict) -> None:
    sys.stdout.write(json.dumps(obj, ensure_ascii=False))
    sys.stdout.write("\n")
    sys.stdout.flush()


def fail(message: str) -> None:
    out({"ok": False, "error": message})
    sys.exit(1)


def parse_pages(spec: str, total: int) -> list[int]:
    """把 "1-3,5"（1 闭区间）解析成 0 基页码列表。"""
    picked: set[int] = set()
    for part in spec.split(","):
        part = part.strip()
        if not part:
            continue
        if "-" in part:
            lo, _, hi = part.partition("-")
            try:
                lo_i, hi_i = int(lo), int(hi)
            except ValueError:
                raise ValueError(f"无效页码区间 {part!r}")
            if lo_i < 1 or hi_i < lo_i or hi_i > total:
                raise ValueError(f"页码区间 {part!r} 超出范围（共 {total} 页）")
            picked.update(range(lo_i - 1, hi_i))
        else:
            try:
                n = int(part)
            except ValueError:
                raise ValueError(f"无效页码 {part!r}")
            if n < 1 or n > total:
                raise ValueError(f"页码 {part!r} 超出范围（共 {total} 页）")
            picked.add(n - 1)
    if not picked:
        raise ValueError("页码选择为空")
    return sorted(picked)


def open_reader(path: str, password: str | None) -> PdfReader:
    reader = PdfReader(path)
    if reader.is_encrypted:
        if not password:
            raise ValueError("PDF 已加密，需要 password")
        if reader.decrypt(password) == 0:
            raise ValueError("密码错误或不受支持的加密方式")
    return reader


def cmd_text(args: argparse.Namespace) -> dict:
    reader = open_reader(args.file, args.password)
    total = len(reader.pages)
    indices = parse_pages(args.pages, total) if args.pages else range(total)
    limit = args.max_chars if args.max_chars > 0 else 20000
    chunks: list[str] = []
    used = 0
    truncated = False
    for i in indices:
        page = reader.pages[i]
        text = (page.extract_text() or "").strip()
        if not text:
            text = "(本页无可提取文本)"
        header = f"--- 第 {i + 1} 页 ---\n"
        remain = limit - used
        if remain <= 0:
            truncated = True
            break
        piece = header + text
        if len(piece) > remain:
            piece = piece[:remain]
            truncated = True
        chunks.append(piece)
        used += len(piece)
    return {
        "ok": True,
        "file": args.file,
        "pages": total,
        "selected": len(chunks),
        "truncated": truncated,
        "text": "\n\n".join(chunks),
    }


def cmd_metadata(args: argparse.Namespace) -> dict:
    reader = open_reader(args.file, args.password)
    meta: dict[str, object] = {}
    if reader.metadata:
        for key, value in reader.metadata.items():
            name = str(key).lstrip("/")
            try:
                meta[name] = str(value)
            except Exception:  # noqa: BLE001 - 元数据值可能任意畸形
                meta[name] = "<unprintable>"
    return {
        "ok": True,
        "file": args.file,
        "pages": len(reader.pages),
        "encrypted": bool(reader.is_encrypted),
        "metadata": meta,
    }


def cmd_merge(args: argparse.Namespace) -> dict:
    if len(args.inputs) < 2:
        raise ValueError("merge 至少需要两个输入文件")
    writer = PdfWriter()
    total = 0
    for path in args.inputs:
        reader = open_reader(path, args.password)
        for page in reader.pages:
            writer.add_page(page)
        total += len(reader.pages)
    with open(args.output, "wb") as fh:
        writer.write(fh)
    return {"ok": True, "output": args.output, "pages": total, "inputs": args.inputs}


def cmd_split(args: argparse.Namespace) -> dict:
    reader = open_reader(args.file, args.password)
    outdir = Path(args.outdir)
    outdir.mkdir(parents=True, exist_ok=True)
    stem = Path(args.file).stem or "page"
    width = max(2, len(str(len(reader.pages))))
    files: list[str] = []
    for i, page in enumerate(reader.pages, start=1):
        writer = PdfWriter()
        writer.add_page(page)
        target = outdir / f"{stem}-{i:0{width}d}.pdf"
        with open(target, "wb") as fh:
            writer.write(fh)
        files.append(str(target))
    return {"ok": True, "file": args.file, "pages": len(reader.pages), "files": files}


def cmd_rotate(args: argparse.Namespace) -> dict:
    if args.angle % 90 != 0:
        raise ValueError("angle 必须是 90 的倍数")
    reader = open_reader(args.file, args.password)
    total = len(reader.pages)
    indices = set(parse_pages(args.pages, total) if args.pages else range(total))
    writer = PdfWriter()
    for i, page in enumerate(reader.pages):
        if i in indices:
            page.rotate(args.angle)
        writer.add_page(page)
    with open(args.output, "wb") as fh:
        writer.write(fh)
    return {"ok": True, "output": args.output, "pages": total, "rotated": len(indices), "angle": args.angle}


def cmd_encrypt(args: argparse.Namespace) -> dict:
    reader = open_reader(args.file, None)
    if reader.is_encrypted:
        raise ValueError("PDF 已经是加密的")
    writer = PdfWriter()
    for page in reader.pages:
        writer.add_page(page)
    # pypdf 在装有 cryptography 时默认 AES-256；仅给 user_password（owner 同密码）。
    writer.encrypt(args.password)
    with open(args.output, "wb") as fh:
        writer.write(fh)
    return {"ok": True, "output": args.output, "pages": len(reader.pages)}


def cmd_decrypt(args: argparse.Namespace) -> dict:
    reader = open_reader(args.file, args.password)
    writer = PdfWriter()
    for page in reader.pages:
        writer.add_page(page)
    with open(args.output, "wb") as fh:
        writer.write(fh)
    return {"ok": True, "output": args.output, "pages": len(reader.pages)}


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="pdf.py", description="woca 内嵌 PDF 处理脚本")
    sub = parser.add_subparsers(dest="op", required=True)

    def add_file(p: argparse.ArgumentParser, *, password: bool = False) -> None:
        p.add_argument("file")
        if password:
            p.add_argument("--password", default=None)

    p = sub.add_parser("text")
    add_file(p, password=True)
    p.add_argument("--pages", default=None)
    p.add_argument("--max-chars", type=int, default=20000)

    p = sub.add_parser("metadata")
    add_file(p, password=True)

    p = sub.add_parser("merge")
    p.add_argument("output")
    p.add_argument("inputs", nargs="+")
    p.add_argument("--password", default=None)

    p = sub.add_parser("split")
    p.add_argument("file")
    p.add_argument("outdir")
    p.add_argument("--password", default=None)

    p = sub.add_parser("rotate")
    p.add_argument("file")
    p.add_argument("angle", type=int)
    p.add_argument("output")
    p.add_argument("--pages", default=None)
    p.add_argument("--password", default=None)

    p = sub.add_parser("encrypt")
    p.add_argument("file")
    p.add_argument("password")
    p.add_argument("output")

    p = sub.add_parser("decrypt")
    p.add_argument("file")
    p.add_argument("password")
    p.add_argument("output")
    return parser


HANDLERS = {
    "text": cmd_text,
    "metadata": cmd_metadata,
    "merge": cmd_merge,
    "split": cmd_split,
    "rotate": cmd_rotate,
    "encrypt": cmd_encrypt,
    "decrypt": cmd_decrypt,
}


def main() -> None:
    args = build_parser().parse_args()
    try:
        result = HANDLERS[args.op](args)
    except FileNotFoundError as exc:
        fail(f"文件不存在：{exc}")
    except ValueError as exc:
        fail(str(exc))
    except Exception as exc:  # noqa: BLE001 - 顶层兜底，错误内容回传给 agent
        fail(f"{type(exc).__name__}: {exc}")
    out(result)


if __name__ == "__main__":
    main()
