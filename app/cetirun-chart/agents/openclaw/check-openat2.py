#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Fail early when the image builder blocks the OpenClaw install syscall."""

import ctypes
import errno
import os


# linux/openat2.h. CetiRun builds this image for linux/amd64; Linux also uses
# this number on arm64, which keeps direct Dockerfile use on that architecture
# diagnosable without relying on a libc wrapper that may not be present.
SYS_OPENAT2 = 437
AT_FDCWD = -100


class OpenHow(ctypes.Structure):
    _fields_ = (("flags", ctypes.c_ulonglong), ("mode", ctypes.c_ulonglong),
                ("resolve", ctypes.c_ulonglong))


def main():
    libc = ctypes.CDLL(None, use_errno=True)
    libc.syscall.restype = ctypes.c_long
    how = OpenHow()
    descriptor = libc.syscall(
        ctypes.c_long(SYS_OPENAT2), ctypes.c_int(AT_FDCWD), ctypes.c_char_p(b"/"),
        ctypes.byref(how), ctypes.c_size_t(ctypes.sizeof(how)),
    )
    if descriptor < 0:
        error = ctypes.get_errno()
        reason = "not implemented" if error == errno.ENOSYS else os.strerror(error)
        raise SystemExit(
            "OpenClaw's plugin installer requires Linux openat2, but this build "
            f"environment rejected it ({reason}). Use a Linux Docker Buildx builder "
            "that permits openat2; remote builders that block this syscall are unsupported."
        )
    os.close(descriptor)


if __name__ == "__main__":
    main()
