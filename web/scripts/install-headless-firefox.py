#!/usr/bin/env python3
"""Download and unpack Ubuntu Firefox/geckodriver packages for the screenshot run."""

import pathlib
import subprocess
import sys
import tempfile


def main():
    target = pathlib.Path(sys.argv[1]).resolve()
    target.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="m11-8-browser-packages-") as temp:
        subprocess.run(["apt-get", "download", "firefox", "firefox-geckodriver"], cwd=temp, check=True)
        for package in pathlib.Path(temp).glob("*.deb"):
            subprocess.run(["dpkg-deb", "-x", str(package), str(target)], check=True)
    firefox = target / "usr/lib/firefox/firefox"
    gecko = target / "usr/bin/geckodriver"
    if not firefox.exists() or not gecko.exists():
        raise SystemExit("Firefox package extraction did not produce firefox and geckodriver")
    print(f"M11_FIREFOX_BINARY={firefox}")
    print(f"M11_GECKODRIVER={gecko}")


if __name__ == "__main__":
    main()
