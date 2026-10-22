#!/usr/bin/env python3
"""Tests of testguard, the guard of the plugin tests: python3 deploy/plugins/tests/test_testguard.py"""

import os
import sys
import tempfile
import unittest
from pathlib import Path

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parent))
import testguard  # noqa: E402

SCRIPT = "/plugins/anet/bin/anet-mcp.sh"


class GuardTests(unittest.TestCase):
    def setUp(self):
        d = tempfile.TemporaryDirectory()
        self.addCleanup(d.cleanup)
        self.home = d.name

    def test_a_script_of_a_plugin_gets_the_installation_turned_off(self):
        env = testguard.with_defaults(["sh", SCRIPT], {"HOME": self.home, "PATH": "/usr/bin:/bin"})
        self.assertEqual(env["ANET_PLUGIN_NO_INSTALL"], "1")
        self.assertTrue(env["PATH"].startswith(testguard._FAILBIN + os.pathsep))
        testguard.check(["sh", SCRIPT], env)
        # A test that sets the variable itself keeps it, and must provide a fake curl.
        own = {"HOME": self.home, "PATH": "/usr/bin:/bin", "ANET_PLUGIN_NO_INSTALL": ""}
        self.assertIs(testguard.with_defaults(["sh", SCRIPT], own), own)
        if os.path.exists("/usr/bin/curl"):
            with self.assertRaises(AssertionError):
                testguard.check(["sh", SCRIPT], own)

    def test_the_real_user_directories_are_refused(self):
        real = os.path.realpath(os.path.expanduser("~"))
        if testguard._temporary(real):
            self.skipTest("the home of this run is a temporary directory")
        for key in ("HOME", "ANET_HOME", "ANET_DATA_DIR", "XDG_RUNTIME_DIR"):
            with self.subTest(key=key), self.assertRaises(AssertionError):
                testguard.check(["sh", SCRIPT], {"HOME": self.home, key: real, "ANET_PLUGIN_NO_INSTALL": "1"})
        if testguard.REAL_RUNTIME:
            with self.assertRaises(AssertionError):
                testguard.check(["true"], {"XDG_RUNTIME_DIR": testguard.REAL_RUNTIME})

    def test_a_script_of_a_plugin_never_inherits_the_environment(self):
        with self.assertRaises(AssertionError):
            testguard.check(["sh", SCRIPT], None)
        testguard.check(["sh", "-n", SCRIPT], None)

    def test_a_real_anet_on_path_is_refused(self):
        fake_system = Path(self.home) / "not-temporary"
        # A directory outside the temporary directory cannot be made here; check the rule on a
        # path that is temporary (accepted) instead, and on the real ~/.local/bin when it holds anet.
        fake_system.mkdir()
        (fake_system / "anet").write_text("#!/bin/sh\n")
        testguard.check(["sh", SCRIPT], {"HOME": self.home, "PATH": str(fake_system), "ANET_PLUGIN_NO_INSTALL": "1"})
        real_bin = os.path.join(os.path.expanduser("~"), ".local", "bin")
        if os.path.exists(os.path.join(real_bin, "anet")) and not testguard._temporary(real_bin):
            with self.assertRaises(AssertionError):
                testguard.check(["sh", SCRIPT], {"HOME": self.home, "PATH": real_bin, "ANET_PLUGIN_NO_INSTALL": "1"})


if __name__ == "__main__":
    unittest.main()
