import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("benchmark_transport", Path(__file__).with_name("benchmark-transport.py"))
driver = importlib.util.module_from_spec(spec)
spec.loader.exec_module(driver)


class QualificationTest(unittest.TestCase):
    def test_completion_and_missing_evidence_fail_despite_zero_exit(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory)
            checks = [{"name": kind + "-" + profile, "exit_code": 0}
                      for kind in ("footprint", "severe") for profile in ("native", "browser")]
            for check in checks:
                (output / (check["name"] + ".json")).write_text(json.dumps({"passed": True, "completed": True}))
            self.assertTrue(driver.qualify(output, checks, 1)["passed"])
            path = output / "severe-browser.json"
            path.write_text(json.dumps({"passed": True, "completed": False}))
            self.assertFalse(driver.qualify(output, checks, 1)["passed"])
            path.unlink()
            self.assertFalse(driver.qualify(output, checks, 1)["passed"])
            self.assertFalse(driver.qualify(output, [], 1)["passed"])

    def test_integrated_matrix_requires_all_three_chisel_completions(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory)
            checks = [{"name": kind + "-" + mode + "-1", "exit_code": 0}
                      for kind in ("footprint", "severe", "chisel")
                      for mode in ("native", "shaped", "shaped-browser")]
            for check in checks:
                (output / (check["name"] + ".json")).write_text(json.dumps({"passed": True, "completed": True}))
            self.assertTrue(driver.qualify(output, checks, 1, modes=3, chisel=True)["passed"])
            (output / "chisel-shaped-browser-1.json").write_text(json.dumps({"passed": True, "completed": False}))
            self.assertFalse(driver.qualify(output, checks, 1, modes=3, chisel=True)["passed"])


if __name__ == "__main__":
    unittest.main()
