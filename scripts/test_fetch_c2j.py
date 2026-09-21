import hashlib
import io
from pathlib import Path
import tarfile
import tempfile
import unittest

from fetch_c2j import install, resolve


class C2JReleaseTest(unittest.TestCase):
    def test_latest_stable_and_invalid_version(self):
        self.assertEqual(resolve("latest", lambda _: b'{"tag_name":"v1.2.3"}'), "v1.2.3")
        for version in ("main", "v1.2.3-rc.1", "../../main"):
            with self.assertRaises(ValueError):
                resolve(version)
        with self.assertRaises(ValueError):
            resolve("latest", lambda _: b'{"tag_name":"v1.2.3","prerelease":true}')

    def test_archives_and_corruption(self):
        archive = io.BytesIO()
        with tarfile.open(fileobj=archive, mode="w:gz") as tar:
            for name in ("c2j", "LICENSE"):
                info = tarfile.TarInfo(name)
                content = name.encode()
                info.size = len(content)
                tar.addfile(info, io.BytesIO(content))
        blob = archive.getvalue()
        for arch, suffix in (("amd64", "x86_64"), ("arm64", "arm64")):
            with self.subTest(arch=arch), tempfile.TemporaryDirectory() as directory:
                asset = f"c2j_1.2.3_Linux_{suffix}.tar.gz"
                def fetch(url):
                    if url.endswith("checksums.txt"):
                        return f"{hashlib.sha256(blob).hexdigest()}  {asset}\n".encode()
                    self.assertTrue(url.endswith(asset))
                    return blob
                install("v1.2.3", arch, directory, fetch)
                self.assertEqual(Path(directory, "c2j").read_bytes(), b"c2j")
                self.assertEqual(Path(directory, "C2J-LICENSE").read_bytes(), b"LICENSE")
                self.assertEqual(Path(directory, "c2j-version.txt").read_text(), "v1.2.3\n")
                with self.assertRaisesRegex(ValueError, "Checksum mismatch"):
                    install("v1.2.3", arch, directory, lambda url: fetch(url) if url.endswith("checksums.txt") else b"bad")


if __name__ == "__main__":
    unittest.main()
