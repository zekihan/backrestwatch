import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[2]
CHART = ROOT / "charts/backrestwatch"
HELM = os.environ.get("HELM", "helm")


def render(*args):
    result = subprocess.run(
        [HELM, "template", "watch", str(CHART), "-n", "monitoring", *args],
        check=True,
        text=True,
        capture_output=True,
    )
    return [d for d in yaml.safe_load_all(result.stdout) if d]


class ChartTests(unittest.TestCase):
    def test_security_persistence_rbac_probes_and_config(self):
        docs = render()
        deployment = next(d for d in docs if d["kind"] == "Deployment")
        self.assertEqual(deployment["spec"]["replicas"], 1)
        self.assertEqual(deployment["spec"]["strategy"]["type"], "Recreate")
        pod = deployment["spec"]["template"]["spec"]
        container = pod["containers"][0]
        self.assertTrue(pod["securityContext"]["runAsNonRoot"])
        self.assertTrue(container["securityContext"]["readOnlyRootFilesystem"])
        self.assertFalse(container["securityContext"]["allowPrivilegeEscalation"])
        self.assertEqual(container["securityContext"]["capabilities"]["drop"], ["ALL"])
        self.assertEqual(
            {
                container[p]["httpGet"]["path"]
                for p in ("startupProbe", "readinessProbe", "livenessProbe")
            },
            {"/healthz"},
        )
        self.assertEqual(container["image"], "ghcr.io/zekihan/backrestwatch:0.1.1")
        rules = next(d for d in docs if d["kind"] == "ClusterRole")["rules"]
        self.assertEqual(
            {r for rule in rules for r in rule["resources"]},
            {"jobs", "cronjobs", "postgresclusters"},
        )
        self.assertTrue(all(rule["verbs"] == ["list"] for rule in rules))
        claim = next(d for d in docs if d["kind"] == "PersistentVolumeClaim")
        self.assertEqual(
            claim["metadata"]["annotations"]["helm.sh/resource-policy"], "keep"
        )
        service = next(d for d in docs if d["kind"] == "Service")
        self.assertEqual(
            service["spec"]["selector"], deployment["spec"]["selector"]["matchLabels"]
        )
        config = json.loads(
            next(d for d in docs if d["kind"] == "ConfigMap")["data"]["config.json"]
        )
        self.assertEqual(config["stateFile"], "/data/evidence.json")
        with tempfile.NamedTemporaryFile(mode="w", suffix=".json") as f:
            json.dump(config, f)
            f.flush()
            subprocess.run(
                [
                    str(ROOT / "dist/backrestwatch"),
                    "--config",
                    f.name,
                    "--check-config",
                ],
                check=True,
                capture_output=True,
            )

    def test_existing_claim_account_and_digest(self):
        digest = "sha256:" + "a" * 64
        docs = render(
            "--set",
            "persistence.existingClaim=retained",
            "--set",
            "serviceAccount.create=false",
            "--set",
            "serviceAccount.name=reader",
            "--set",
            "rbac.create=false",
            "--set",
            f"image.digest={digest}",
        )
        self.assertFalse(
            any(
                d["kind"]
                in (
                    "PersistentVolumeClaim",
                    "ServiceAccount",
                    "ClusterRole",
                    "ClusterRoleBinding",
                )
                for d in docs
            )
        )
        pod = next(d for d in docs if d["kind"] == "Deployment")["spec"]["template"][
            "spec"
        ]
        self.assertEqual(pod["serviceAccountName"], "reader")
        self.assertEqual(
            pod["volumes"][1]["persistentVolumeClaim"]["claimName"], "retained"
        )
        self.assertTrue(pod["containers"][0]["image"].endswith("@" + digest))

    def test_network_policy_and_invalid_inventory(self):
        docs = render("-f", str(ROOT / "examples/network-policy.yaml"))
        policy = next(d for d in docs if d["kind"] == "NetworkPolicy")["spec"]
        self.assertEqual(policy["policyTypes"], ["Ingress", "Egress"])
        self.assertEqual(
            policy["egress"][0]["to"], [{"ipBlock": {"cidr": "10.96.0.1/32"}}]
        )
        self.assertEqual(policy["ingress"][0]["ports"][0]["port"], 8080)
        for override in (
            "config.clusters=[]",
            "config.maxAgeSeconds=0",
            "config.clusters[0].repos.repo5.maxAgeSeconds=10",
            "networkPolicy.enabled=true",
            "serviceAccount.create=false",
        ):
            result = subprocess.run(
                [HELM, "template", "watch", str(CHART), "--set", override],
                capture_output=True,
                text=True,
            )
            self.assertNotEqual(result.returncode, 0, override)


if __name__ == "__main__":
    unittest.main()
