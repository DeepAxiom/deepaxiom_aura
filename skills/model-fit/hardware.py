"""Read what this machine actually has, using nothing but the standard library.

This skill's whole value is being able to run *before* anything is installed —
it is the one that tells you what you can install — so a dependency here would
defeat the point. `psutil` would make the RAM read one line; it would also mean
the skill that answers "what can this machine run" cannot run on a machine
where nothing has been set up yet.

Every probe degrades to None rather than raising. A machine that will not tell
us its VRAM is a machine we score conservatively, not one we refuse to help.
"""
from __future__ import annotations

import ctypes
import os
import platform
import re
import shutil
import subprocess


def _total_ram_bytes() -> int | None:
    system = platform.system()

    if system == "Linux":
        try:
            with open("/proc/meminfo", encoding="utf-8") as fh:
                for line in fh:
                    if line.startswith("MemTotal:"):
                        return int(line.split()[1]) * 1024
        except OSError:
            return None
        return None

    if system == "Windows":
        # GlobalMemoryStatusEx. The struct's first field must be its own size or
        # the call fails — a classic Win32 footgun worth naming.
        class MemoryStatusEx(ctypes.Structure):
            _fields_ = [
                ("dwLength", ctypes.c_ulong),
                ("dwMemoryLoad", ctypes.c_ulong),
                ("ullTotalPhys", ctypes.c_ulonglong),
                ("ullAvailPhys", ctypes.c_ulonglong),
                ("ullTotalPageFile", ctypes.c_ulonglong),
                ("ullAvailPageFile", ctypes.c_ulonglong),
                ("ullTotalVirtual", ctypes.c_ulonglong),
                ("ullAvailVirtual", ctypes.c_ulonglong),
                ("ullAvailExtendedVirtual", ctypes.c_ulonglong),
            ]

        try:
            status = MemoryStatusEx()
            status.dwLength = ctypes.sizeof(MemoryStatusEx)
            if ctypes.windll.kernel32.GlobalMemoryStatusEx(ctypes.byref(status)):
                return int(status.ullTotalPhys)
        except (AttributeError, OSError):
            return None
        return None

    if system == "Darwin":
        try:
            out = subprocess.run(
                ["sysctl", "-n", "hw.memsize"],
                capture_output=True, text=True, timeout=5, check=False,
            )
            return int(out.stdout.strip()) if out.returncode == 0 else None
        except (OSError, ValueError, subprocess.SubprocessError):
            return None

    return None


def _nvidia() -> list[dict]:
    """NVIDIA GPUs via nvidia-smi, which ships with the driver.

    Queried rather than linked: NVML through ctypes would mean guessing at a
    library name that differs per platform and driver version, and nvidia-smi is
    on PATH whenever the driver a GGUF runtime needs is installed anyway.
    """
    if not shutil.which("nvidia-smi"):
        return []
    try:
        out = subprocess.run(
            ["nvidia-smi", "--query-gpu=name,memory.total",
             "--format=csv,noheader,nounits"],
            capture_output=True, text=True, timeout=10, check=False,
        )
    except (OSError, subprocess.SubprocessError):
        return []
    if out.returncode != 0:
        return []

    gpus = []
    for line in out.stdout.strip().splitlines():
        parts = [p.strip() for p in line.split(",")]
        if len(parts) < 2:
            continue
        try:
            gpus.append({"name": parts[0], "vram_bytes": int(float(parts[1])) * 1024 * 1024,
                         "vendor": "nvidia"})
        except ValueError:
            continue
    return gpus


def _apple_silicon() -> list[dict]:
    """Apple Silicon has unified memory: the GPU can address most of system RAM.

    Reported as a GPU with the conventional working budget rather than the full
    amount, because macOS reserves the rest for everything that is not a model.
    """
    if platform.system() != "Darwin" or platform.machine() != "arm64":
        return []
    ram = _total_ram_bytes()
    if not ram:
        return []
    budget = int(ram * 0.75) if ram <= 36 * 1024**3 else int(ram * 0.8)
    return [{"name": f"Apple Silicon ({platform.processor() or 'arm64'})",
             "vram_bytes": budget, "vendor": "apple", "unified": True}]


def _cpu_name() -> str:
    system = platform.system()
    if system == "Linux":
        try:
            with open("/proc/cpuinfo", encoding="utf-8") as fh:
                for line in fh:
                    if line.startswith("model name"):
                        return line.split(":", 1)[1].strip()
        except OSError:
            pass
    elif system == "Darwin":
        try:
            out = subprocess.run(["sysctl", "-n", "machdep.cpu.brand_string"],
                                 capture_output=True, text=True, timeout=5, check=False)
            if out.returncode == 0 and out.stdout.strip():
                return out.stdout.strip()
        except (OSError, subprocess.SubprocessError):
            pass
    elif system == "Windows":
        name = os.environ.get("PROCESSOR_IDENTIFIER", "")
        if name:
            return re.sub(r"\s+", " ", name).strip()
    return platform.processor() or platform.machine() or "unknown"


def detect() -> dict:
    """Everything the scorer needs, with None where the machine would not say."""
    ram = _total_ram_bytes()
    gpus = _nvidia() or _apple_silicon()
    return {
        "os": f"{platform.system()} {platform.release()}".strip(),
        "arch": platform.machine(),
        "cpu": _cpu_name(),
        "cpu_cores": os.cpu_count(),
        "ram_bytes": ram,
        "gpus": gpus,
        "vram_bytes": sum(g["vram_bytes"] for g in gpus) if gpus else 0,
        "unified_memory": bool(gpus and gpus[0].get("unified")),
    }
