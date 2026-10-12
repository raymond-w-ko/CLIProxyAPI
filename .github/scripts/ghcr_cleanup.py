"""Keep this fork's latest GHCR build and two previous builds, with their manifests."""

import json
import os
import re
import subprocess
import sys

OWNER = "raymond-w-ko"
PACKAGE = "cliproxyapi"
IMAGE = f"ghcr.io/{OWNER}/{PACKAGE}"
VERSIONS_API = f"/users/{OWNER}/packages/container/{PACKAGE}/versions"
DIGEST = re.compile(r"sha256:[0-9a-f]{64}")
BUILD_TAG = re.compile(r"sha-[0-9a-f]{40}")
INDEX_TYPES = {
    "application/vnd.oci.image.index.v1+json",
    "application/vnd.docker.distribution.manifest.list.v2+json",
}
MANIFEST_TYPES = {
    "application/vnd.oci.image.manifest.v1+json",
    "application/vnd.docker.distribution.manifest.v2+json",
}


def run_json(*args):
    return json.loads(subprocess.check_output(args, text=True))


def list_versions():
    # All pages are needed: untagged platform/attestation manifests also count.
    pages = run_json("gh", "api", "--paginate", "--slurp", f"{VERSIONS_API}?per_page=100")
    return [version for page in pages for version in page]


def inspect_manifest(digest):
    return run_json("docker", "buildx", "imagetools", "inspect", f"{IMAGE}@{digest}", "--raw")


def retained_manifests(roots, inspect):
    retained = set()

    def visit(digest):
        if not isinstance(digest, str) or not DIGEST.fullmatch(digest):
            raise ValueError(f"Invalid manifest digest: {digest!r}")
        if digest in retained:
            return
        manifest = inspect(digest)
        media_type = manifest.get("mediaType")
        if manifest.get("schemaVersion") != 2 or media_type not in INDEX_TYPES | MANIFEST_TYPES:
            raise ValueError(f"Unsupported manifest: {digest}")
        retained.add(digest)
        if media_type in INDEX_TYPES:
            children = manifest.get("manifests")
            if not isinstance(children, list) or not children:
                raise ValueError(f"Empty or invalid manifest index: {digest}")
            for child in children:
                visit(child["digest"])
        # OCI subjects are manifest references, unlike config/layer blob digests.
        if "subject" in manifest:
            visit(manifest["subject"]["digest"])

    for root in roots:
        visit(root["name"])
    return retained


def cleanup_plan(versions, current_digest, inspect):
    builds = []
    latest = []
    for version in versions:
        if not DIGEST.fullmatch(version["name"]):
            raise ValueError("Package contains an unexpected version name")
        tags = version["metadata"]["container"]["tags"]
        if any(tag != "latest" and not BUILD_TAG.fullmatch(tag) for tag in tags):
            raise ValueError("Package contains an unmanaged tag; refusing cleanup")
        if tags:
            builds.append(version)
        if "latest" in tags:
            latest.append(version)
    if len(latest) != 1 or latest[0]["name"] != current_digest:
        raise ValueError("latest does not match this run's published digest; refusing cleanup")

    # Preserve the just-published build even when a rerun reuses an older digest.
    previous = sorted(
        (version for version in builds if version["id"] != latest[0]["id"]),
        key=lambda version: (version["created_at"], version["id"]),
        reverse=True,
    )
    roots = latest + previous[:2]
    retained = retained_manifests(roots, inspect)
    obsolete = [version for version in versions if version["name"] not in retained]
    # Remove old tagged indexes before their unreferenced child manifests.
    obsolete.sort(key=lambda version: (not bool(version["metadata"]["container"]["tags"]), version["id"]))
    return roots, retained, obsolete


def cleanup(current_digest, list_all=list_versions, inspect=inspect_manifest, delete=None):
    roots, retained, obsolete = cleanup_plan(list_all(), current_digest, inspect)
    if delete is None:
        def delete(version_id):
            subprocess.run(["gh", "api", "--method", "DELETE", f"{VERSIONS_API}/{version_id}"], check=True)

    print(f"Keeping {len(roots)} builds and {len(retained)} referenced manifests", flush=True)
    for version in obsolete:
        print(f"Deleting obsolete package version {version['id']} ({version['name']})", flush=True)
        delete(version["id"])

    remaining = list_all()
    if {version["id"] for version in roots} - {version["id"] for version in remaining}:
        raise ValueError("A retained build is missing after cleanup")
    if any(version["name"] not in retained for version in remaining):
        raise ValueError("Obsolete package versions remain after cleanup")
    # Resolve every retained image again to catch broken manifest references.
    retained_manifests(roots, inspect)
    message = f"GHCR cleanup complete: {len(roots)} builds retained, {len(obsolete)} obsolete versions deleted."
    print(message)
    if summary := os.environ.get("GITHUB_STEP_SUMMARY"):
        with open(summary, "a", encoding="utf-8") as output:
            output.write(f"\n{message}\n")


if __name__ == "__main__":
    try:
        cleanup(os.environ["IMAGE_DIGEST"])
    except (ValueError, KeyError, TypeError, subprocess.CalledProcessError) as error:
        print(f"GHCR cleanup failed: {error}", file=sys.stderr)
        sys.exit(1)
