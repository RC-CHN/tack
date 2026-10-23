import importlib.util
import pathlib
import tempfile
import unittest

SPEC = importlib.util.spec_from_file_location("coverage_ledger", pathlib.Path(__file__).with_name("check-fixture-coverage.py"))
ledger = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(ledger)


class CoverageLedgerTests(unittest.TestCase):
    def test_pending_is_not_complete_and_missing_evidence_is_not_coverage(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            entry = dict(source="upstream_test.go", symbol="TestA", kind="Test", status="pending", local=[], reason="")
            data = dict(entries=[entry], expected_counts={"Test": 1})
            self.assertEqual(ledger.validate(data, root), [])
            self.assertTrue(ledger.validate(data, root, True))
            entry.update(status="covered", reason="same observable contract")
            self.assertTrue(ledger.validate(data, root, True))
            entry["local"] = ["test.go#TestLocal"]
            (root / "test.go").write_text("func TestOther() {}")
            self.assertTrue(ledger.validate(data, root, True))
            (root / "test.go").write_text("func TestLocal() {}")
            self.assertEqual(ledger.validate(data, root, True), [])

    def test_exclusions_need_reasons_and_duplicate_rows_are_rejected(self):
        entry = dict(source="upstream_test.go", symbol="TestA", kind="Test", status="not_applicable", local=[], reason="")
        data = dict(entries=[entry])
        self.assertTrue(ledger.validate(data, pathlib.Path.cwd()))
        entry["reason"] = "QUIC-only feature absent in this product"
        self.assertEqual(ledger.validate(data, pathlib.Path.cwd()), [])
        data["entries"].append(dict(entry))
        self.assertTrue(ledger.validate(data, pathlib.Path.cwd()))


if __name__ == "__main__":
    unittest.main()
