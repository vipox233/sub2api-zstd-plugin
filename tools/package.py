#!/usr/bin/env python3
"""Build a signed .s2plugin; private signing material stays outside the source tree."""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import stat
import zipfile

from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey


def sha256(data):
    return hashlib.sha256(data).hexdigest()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--binary", required=True, type=Path)
    parser.add_argument("--private-key", required=True, type=Path)
    parser.add_argument("--key-id", default="vipox-zstd-20261104")
    parser.add_argument("--output-dir", type=Path, default=Path("dist"))
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    private_path = args.private_key.expanduser().resolve()
    if private_path.is_relative_to(root):
        raise SystemExit("Keep the private key outside the source/release directory.")
    private_path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    if private_path.exists():
        private = serialization.load_pem_private_key(private_path.read_bytes(), password=None)
        if not isinstance(private, Ed25519PrivateKey):
            raise SystemExit("Expected an Ed25519 private key.")
    else:
        private = Ed25519PrivateKey.generate()
        pem = private.private_bytes(serialization.Encoding.PEM,
                                    serialization.PrivateFormat.PKCS8,
                                    serialization.NoEncryption())
        fd = os.open(private_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "wb") as stream:
            stream.write(pem)
    public = private.public_key()
    public_bytes = public.public_bytes(serialization.Encoding.Raw, serialization.PublicFormat.Raw)
    public_b64 = base64.b64encode(public_bytes).decode("ascii")
    manifest = json.loads((root / "manifest.source.json").read_text())
    runtime = "runtimes/linux-amd64/openai-upstream-zstd"
    files = {runtime: args.binary.read_bytes()}
    if files[runtime][:4] != b"\x7fELF" or files[runtime][4:6] != b"\x02\x01":
        raise SystemExit("Expected a 64-bit little-endian Linux ELF executable.")
    if int.from_bytes(files[runtime][18:20], "little") != 62:
        raise SystemExit("Expected the Linux amd64 runtime.")
    for path in sorted((root / "ui").rglob("*")):
        if path.is_symlink():
            raise SystemExit("Symlinks are not allowed in the UI.")
        if path.is_file():
            files[path.relative_to(root).as_posix()] = path.read_bytes()
    for name in ("LICENSE", "LICENSE-GPL-3.0.txt", "LICENSE-Go.txt", "NOTICE.md"):
        files[name] = (root / name).read_bytes()
    manifest["runtimes"] = {"linux-amd64": {"path": runtime}}
    manifest["files"] = {path: sha256(data) for path, data in sorted(files.items())}
    manifest_bytes = (json.dumps(manifest, ensure_ascii=False, indent=2) + "\n").encode()
    signature_bytes = private.sign(manifest_bytes)
    signature = {"algorithm": "ed25519", "key_id": args.key_id,
                 "signature": base64.b64encode(signature_bytes).decode("ascii")}
    public.verify(signature_bytes, manifest_bytes)
    entries = {"manifest.json": manifest_bytes,
               "signature.json": (json.dumps(signature, indent=2) + "\n").encode(), **files}
    output = args.output_dir.resolve()
    output.mkdir(parents=True, exist_ok=True)
    name = "openai-upstream-zstd-" + manifest["version"] + "-linux-amd64.s2plugin"
    package_path = output / name
    with zipfile.ZipFile(package_path, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=9) as package:
        for path, data in entries.items():
            entry = zipfile.ZipInfo(path, date_time=(2026, 11, 4, 0, 0, 0))
            entry.compress_type = zipfile.ZIP_DEFLATED
            entry.create_system = 3
            entry.external_attr = (stat.S_IFREG | (0o755 if path == runtime else 0o644)) << 16
            package.writestr(entry, data)
    with zipfile.ZipFile(package_path) as package:
        if package.testzip() is not None or len(package.namelist()) != len(set(package.namelist())):
            raise SystemExit("Invalid or duplicate archive entries.")
        public.verify(base64.b64decode(json.loads(package.read("signature.json"))["signature"]),
                      package.read("manifest.json"))
        for path, expected in manifest["files"].items():
            if sha256(package.read(path)) != expected:
                raise SystemExit("File hash mismatch: " + path)
    (output / "publisher-public-key.txt").write_text(public_b64 + "\n")
    (output / "trusted-publisher.yaml").write_text(
        "# 合并到现有 config.yaml 的 plugins 区域；不要替换整个配置文件。\n"
        "plugins:\n  trusted_publishers:\n    " + args.key_id + ': "' + public_b64 + '"\n')
    (output / (name + ".sha256")).write_text(sha256(package_path.read_bytes()) + "  " + name + "\n")
    print(json.dumps({"file": str(package_path), "size_bytes": package_path.stat().st_size,
                      "sha256": sha256(package_path.read_bytes()), "key_id": args.key_id,
                      "public_key": public_b64, "files": len(files)}, ensure_ascii=False))


if __name__ == "__main__":
    main()
