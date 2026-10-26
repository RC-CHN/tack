"""Single source of truth for the unified service release and CI build shards."""
import json

MODERN = "go1.26.3"
LEGACY = "go1.23.12"
MATRIX_ID = "tack-unified-services-v1"
NODE_TARGETS = [
    ("linux", "amd64", ""), ("linux", "386", ""),
    ("linux", "arm", "5"), ("linux", "arm", "6"), ("linux", "arm", "7"),
    ("linux", "arm64", ""), ("linux", "mips", ""), ("linux", "mipsle", ""),
    ("linux", "mips64", ""), ("linux", "mips64le", ""),
    ("windows", "amd64", ""), ("windows", "386", ""), ("windows", "arm64", ""),
]
CONTROL_TARGETS = [("linux", "amd64", ""), ("linux", "arm64", ""), ("windows", "amd64", "")]
TOOL_TARGETS = [("linux", "amd64", ""), ("linux", "arm64", ""),
                ("darwin", "amd64", ""), ("darwin", "arm64", ""),
                ("windows", "amd64", ""), ("windows", "386", ""), ("windows", "arm64", "")]


def target_label(arch, arm=""):
    return ("armv" + arm if arm else arch) + ("-softfloat" if arch.startswith("mips") else "")


def targets():
    result = []
    for component, runtime, platforms in (
            ("tack-node", "modern", NODE_TARGETS),
            ("tack-node", "legacy", [t for t in NODE_TARGETS if t[0] == "linux"]),
            ("tack-control", "modern", CONTROL_TARGETS),
            ("tack", "modern", TOOL_TARGETS)):
        for goos, arch, arm in platforms:
            label = target_label(arch, arm)
            result.append({"id": f"{component}-{goos}-{label}-{runtime}", "component": component,
                           "runtime": runtime, "os": goos, "arch": arch, "arm": arm, "label": label,
                           "toolchain": LEGACY if runtime == "legacy" else MODERN,
                           "go_version": (LEGACY if runtime == "legacy" else MODERN)[2:]})
    return result


def archive_name(target, version):
    variant = "_legacy" if target["runtime"] == "legacy" else ""
    extension = ".zip" if target["os"] == "windows" else ".tar.gz"
    return f"{target['component']}_{version}_{target['os']}_{target['label']}{variant}{extension}"


def validate_matrix(manifest, complete=False):
    """Reject incomplete publication and shards that do not match their identity."""
    if manifest.get("matrix_id") != MATRIX_ID:
        raise ValueError("unknown unified release matrix")
    scope = manifest.get("release_scope")
    if scope not in ("complete", "shard", "smoke"):
        raise ValueError("invalid release scope")
    if complete and (scope != "complete" or manifest.get("source_dirty") is not False
                     or manifest.get("native_only") is not False or manifest.get("node_runtime") != "both"
                     or manifest.get("requested_protection") != "packed"):
        raise ValueError("publication requires the complete clean packed release matrix")
    selected = targets()
    if scope == "shard":
        selected = [target for target in selected if target["id"] == manifest.get("shard")]
        if len(selected) != 1 or manifest.get("native_only") is not False:
            raise ValueError("invalid release shard")
    elif "shard" in manifest:
        raise ValueError("unexpected shard identity")
    expected = {archive_name(target, manifest["version"]): target for target in selected}
    seen = set()
    for artifact in manifest["artifacts"]:
        name = artifact["file"]
        if name in seen or name not in expected:
            raise ValueError("duplicate or unexpected release target: " + name)
        seen.add(name)
        target = expected[name]
        build = artifact["build"]
        if (artifact["component"] != target["component"] or artifact["os"] != target["os"]
                or artifact["arch"] != target["label"] or artifact["toolchain"] != target["toolchain"]
                or build.get("runtime_profile") != target["runtime"]
                or build["arch"] != target["arch"] or build["arm"] != target["arm"]):
            raise ValueError("release target metadata mismatch: " + name)
        capabilities = {"tack-node": {"management", "embedded-chisel"},
                        "tack-control": {"management", "embedded-chisel-server"}}.get(target["component"], set())
        if not capabilities.issubset(build.get("capabilities", [])):
            raise ValueError("unified service capabilities missing: " + name)
        requested = manifest.get("requested_protection")
        if requested == "packed":
            if not ((build["protection"], build.get("packing", {}).get("status")) in (
                    ("packed", "packed"), ("obfuscated", "exception"))):
                raise ValueError("requested protection was lost: " + name)
            if target["runtime"] == "legacy" and build["protection"] != "obfuscated":
                raise ValueError("Legacy must retain its qualified obfuscated profile")
        elif requested not in ("plain", "obfuscated") or build["protection"] != requested:
            raise ValueError("inconsistent requested protection: " + name)
    if not seen or (scope != "smoke" and seen != set(expected)):
        raise ValueError("missing release targets")


if __name__ == "__main__":
    print(json.dumps({"include": targets()}, separators=(",", ":")))
