#!/usr/bin/env python3
"""Forge entrypoint: replace this process with the compiled Go binary.

The binary is built next to this file as ./forge (see DESIGN.md); this
script only execs it in place, passing argv through unchanged, so the
harness sees the Go server's own stdout — including the
"[forge] listening on port N" line it parses.
"""

import os
import sys

_HERE = os.path.dirname(os.path.abspath(__file__))


def main():
    binary = os.path.join(_HERE, "forge")
    if not os.path.isfile(binary):
        sys.stderr.write(
            "[forge] ./forge is missing next to main.py — build it first "
            "(CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o forge .)\n"
        )
        sys.exit(1)
    os.execv(binary, ["forge"] + sys.argv[1:])


if __name__ == "__main__":
    main()
