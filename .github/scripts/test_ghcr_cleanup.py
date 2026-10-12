import copy
import io
import subprocess
from contextlib import redirect_stdout
import unittest
from unittest.mock import patch

import ghcr_cleanup as cleanup


def digest(number):
    return f"sha256:{number:064x}"


def version(number, tags=None):
    return {
        "id": number,
        "name": digest(number),
        "created_at": f"2026-10-{number:02d}T00:00:00Z",
        "metadata": {"container": {"tags": tags or []}},
    }


def index(*children):
    return {
        "schemaVersion": 2,
        "mediaType": "application/vnd.oci.image.index.v1+json",
        "manifests": [{"digest": digest(child)} for child in children],
    }


def manifest():
    return {"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json"}


class RetentionTests(unittest.TestCase):
    def setUp(self):
        self.versions = [version(i, [f"sha-{i:040x}"]) for i in range(1, 6)]
        self.versions[-1]["metadata"]["container"]["tags"].append("latest")
        self.manifests = {digest(i): index(10, 10 + i) for i in range(1, 6)}
        # All builds share a platform manifest; each has a separate attestation.
        for i in range(10, 16):
            self.versions.append(version(i))
            self.manifests[digest(i)] = manifest()
        self.versions.append(version(20))  # Unreferenced upload.
        self.inspect = self.manifests.__getitem__
        self.output = redirect_stdout(io.StringIO())
        self.output.__enter__()
        self.addCleanup(self.output.__exit__, None, None, None)
        self.environment = patch.dict(cleanup.os.environ, {"GITHUB_STEP_SUMMARY": ""})
        self.environment.start()
        self.addCleanup(self.environment.stop)

    def test_keep_three_builds_with_shared_platform_and_attestations(self):
        roots, retained, obsolete = cleanup.cleanup_plan(self.versions, digest(5), self.inspect)
        self.assertEqual([root["id"] for root in roots], [5, 4, 3])
        self.assertEqual(retained, {digest(i) for i in [3, 4, 5, 10, 13, 14, 15]})
        self.assertEqual([item["id"] for item in obsolete], [1, 2, 11, 12, 20])

    def test_rerun_keeps_latest_even_if_digest_is_older(self):
        self.versions[4]["metadata"]["container"]["tags"].remove("latest")
        self.versions[0]["metadata"]["container"]["tags"].append("latest")
        roots, _, _ = cleanup.cleanup_plan(self.versions, digest(1), self.inspect)
        self.assertEqual([root["id"] for root in roots], [1, 5, 4])

    def test_fewer_than_three_builds(self):
        roots, _, obsolete = cleanup.cleanup_plan([self.versions[4]], digest(5), self.inspect)
        self.assertEqual(len(roots), 1)
        self.assertEqual(obsolete, [])

    def test_nested_indexes_and_subjects(self):
        self.manifests[digest(10)] = index(30)
        self.manifests[digest(30)] = manifest()
        self.manifests[digest(30)]["subject"] = {"digest": digest(31)}
        self.manifests[digest(31)] = manifest()
        _, retained, _ = cleanup.cleanup_plan(self.versions, digest(5), self.inspect)
        self.assertTrue({digest(30), digest(31)} <= retained)

    def test_unsafe_plans_delete_nothing(self):
        for problem in ["mismatched latest", "unmanaged tag", "missing manifest", "invalid manifest"]:
            with self.subTest(problem=problem):
                versions = copy.deepcopy(self.versions)
                inspect = self.inspect
                current = digest(5)
                if problem == "mismatched latest":
                    current = digest(1)
                elif problem == "unmanaged tag":
                    versions[0]["metadata"]["container"]["tags"].append("release")
                elif problem == "missing manifest":
                    def inspect(_):
                        raise subprocess.CalledProcessError(1, "docker")
                else:
                    inspect = lambda _: index()
                deleted = []
                with self.assertRaises((ValueError, subprocess.CalledProcessError)):
                    cleanup.cleanup(current, lambda: versions, inspect, deleted.append)
                self.assertEqual(deleted, [])

    def test_cleanup_verifies_remaining_builds(self):
        active = {item["id"]: item for item in self.versions}
        cleanup.cleanup(digest(5), lambda: list(active.values()), self.inspect, active.pop)
        self.assertEqual(set(active), {3, 4, 5, 10, 13, 14, 15})

    def test_failed_deletion_stops_cleanup(self):
        attempted = []

        def delete(version_id):
            attempted.append(version_id)
            raise subprocess.CalledProcessError(1, "gh")

        with self.assertRaises(subprocess.CalledProcessError):
            cleanup.cleanup(digest(5), lambda: self.versions, self.inspect, delete)
        self.assertEqual(attempted, [1])

    def test_verification_rejects_noop_deletion(self):
        with self.assertRaisesRegex(ValueError, "Obsolete package versions remain"):
            cleanup.cleanup(digest(5), lambda: self.versions, self.inspect, lambda _: None)

    def test_version_listing_includes_all_pages(self):
        with patch.object(cleanup, "run_json", return_value=[[1, 2], [3]]) as run:
            self.assertEqual(cleanup.list_versions(), [1, 2, 3])
            self.assertIn("--paginate", run.call_args.args)
            self.assertIn("--slurp", run.call_args.args)


if __name__ == "__main__":
    unittest.main()
