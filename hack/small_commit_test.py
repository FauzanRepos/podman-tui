#!/usr/bin/env python3
"""Exercise the local workflow against disposable repositories, not the user's index."""
from pathlib import Path
import subprocess
import tempfile
import unittest

HELPER = Path(__file__).with_name("small-commit.py").resolve()


class CommitWorkflowTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="ptui-commit-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.hooks = self.root / "empty-hooks"
        self.hooks.mkdir(mode=0o700)
        self.git("init", "--template=" + str(self.hooks))
        self.git("config", "user.name", "Isolated workflow fixture")
        self.git("config", "user.email", "fixture@example.invalid")
        self.git("config", "core.hooksPath", str(self.hooks))

    def git(self, *args):
        return subprocess.check_output(["git", *args], cwd=self.root, stderr=subprocess.DEVNULL)

    def invoke(self, filename, validation="true"):
        return subprocess.run(["python3", str(HELPER), "--message", "test(workflow): verify isolated change", "--validate", validation, "--", filename], cwd=self.root, capture_output=True, text=True)

    def test_stages_only_the_selected_logical_change(self):
        (self.root / "selected.txt").write_text("selected change\n")
        (self.root / "unrelated.txt").write_text("user change\n")
        result = self.invoke("selected.txt")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.git("show", "--format=", "--name-only", "HEAD").strip(), b"selected.txt")
        self.assertIn(b"?? unrelated.txt", self.git("status", "--short"))

    def test_existing_index_and_failed_validation_are_preserved(self):
        (self.root / "user.txt").write_text("user staged change\n")
        (self.root / "selected.txt").write_text("selected change\n")
        self.git("add", "user.txt")
        self.assertNotEqual(self.invoke("selected.txt").returncode, 0)
        self.assertEqual(self.git("diff", "--cached", "--name-only").strip(), b"user.txt")
        self.git("commit", "-m", "test: isolated user fixture")
        self.assertNotEqual(self.invoke("selected.txt", "false").returncode, 0)
        self.assertEqual(self.git("diff", "--cached", "--name-only"), b"")

    def test_secret_and_artifact_guards(self):
        payloads = [
            ("selected.txt", "ghp_" + "x" * 32),
            ("selected.txt", "https://" + "user:password" + "@real-host.example/repo"),
            ("selected.txt", "binary\x00data"),
            ("temporary.tmp", "temporary artifact"),
        ]
        for filename, content in payloads:
            with self.subTest(filename=filename, content_type=content[:6]):
                (self.root / filename).write_text(content)
                self.assertNotEqual(self.invoke(filename).returncode, 0)
                self.assertEqual(self.git("diff", "--cached", "--name-only"), b"")

    def test_reserved_negative_security_fixture_still_checks_tokens(self):
        file = self.root / "validation_test.go"
        file.write_text("package fixture\nvar url = \"https://" + "user:password" + "@git.example.invalid/repo\"\n")
        result = self.invoke(file.name)
        self.assertEqual(result.returncode, 0, result.stderr)
        file.write_text(file.read_text() + "// ghp_" + "x" * 32)
        self.assertNotEqual(self.invoke(file.name).returncode, 0)
        self.assertEqual(self.git("diff", "--cached", "--name-only"), b"")


if __name__ == "__main__":
    unittest.main()
