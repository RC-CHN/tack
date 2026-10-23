#!/usr/bin/env python3
"""Validate the behavior migration ledger; never infer semantic coverage."""
import argparse
import collections
import json
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parents[1]
INVENTORY = ROOT / "test/fixtures/upstream-inventory.json"


def validate(data, root, complete=False):
    errors, seen = [], set()
    entries = data.get("entries", [])
    if not entries:
        errors.append("empty upstream inventory")
    for entry in entries:
        key = (entry["source"], entry["symbol"])
        label = "#".join(key)
        if key in seen:
            errors.append(f"duplicate: {label}")
        seen.add(key)
        status = entry["status"]
        if status not in ("pending", "covered", "adapted", "not_applicable"):
            errors.append(f"unknown status: {label}")
        if complete and status == "pending":
            errors.append(f"pending: {label}")
        if status != "pending" and not entry["reason"].strip():
            errors.append(f"missing scope/reason: {label}")
        if status in ("covered", "adapted") and not entry["local"]:
            errors.append(f"missing local evidence: {label}")
        for reference in entry["local"]:
            path, _, symbol = reference.partition("#")
            destination = (root / path).resolve()
            if not destination.is_relative_to(root.resolve()) or not destination.is_file():
                errors.append(f"missing/unsafe reference: {reference}")
                continue
            if symbol and not re.search(r"\b" + re.escape(symbol) + r"\b", destination.read_text()):
                errors.append(f"missing symbol: {reference}")
    expected = data.get("expected_counts")
    actual = dict(collections.Counter(e["kind"] for e in entries))
    if expected is not None and expected != actual:
        errors.append(f"entry count changed: {actual}, expected {expected}")
    return errors


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--require-complete", action="store_true")
    args = parser.parse_args()
    data = json.loads(INVENTORY.read_text())
    errors = validate(data, ROOT, args.require_complete)
    print(json.dumps(dict(collections.Counter(e["status"] for e in data["entries"])), sort_keys=True))
    for error in errors:
        print(error, file=sys.stderr)
    return bool(errors)


if __name__ == "__main__":
    sys.exit(main())
