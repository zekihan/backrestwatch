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


def render(*args, release="watch", namespace="monitoring"):
    result = subprocess.run(
        [HELM, "template", release, str(CHART), "-n", namespace, "--set", "accessControl.existingSecret=fixture-auth", *args],
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
        self.assertEqual(container["image"], "ghcr.io/zekihan/backrestwatch:0.2.1")
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

    def test_opt_in_sidecar_ports_and_volumes(self):
        values = {
            "extraContainers": [{"name": "tls-proxy", "image": "example.invalid/proxy@sha256:" + "a" * 64, "volumeMounts": [{"name": "tls", "mountPath": "/tls", "readOnly": True}]}],
            "extraVolumes": [{"name": "tls", "secret": {"secretName": "monitor-tls"}}],
            "service": {"extraPorts": [{"name": "https", "port": 8443, "targetPort": 8443, "protocol": "TCP"}]},
            "networkPolicy": {"enabled": True, "apiPeers": ["10.0.0.1/32"], "ingressFrom": [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "gatus"}}}], "ingressPorts": [8443]},
        }
        with tempfile.NamedTemporaryFile(mode="w", suffix=".yaml") as f:
            yaml.safe_dump(values, f)
            f.flush()
            docs = render("-f", f.name)
        pod = next(d for d in docs if d["kind"] == "Deployment")["spec"]["template"]["spec"]
        proxy = next(c for c in pod["containers"] if c["name"] == "tls-proxy")
        self.assertTrue(proxy["volumeMounts"][0]["readOnly"])
        self.assertEqual(next(v for v in pod["volumes"] if v["name"] == "tls")["secret"]["secretName"], "monitor-tls")
        service = next(d for d in docs if d["kind"] == "Service")
        self.assertEqual(next(p for p in service["spec"]["ports"] if p["name"] == "https")["targetPort"], 8443)
        policy = next(d for d in docs if d["kind"] == "NetworkPolicy")
        self.assertEqual(policy["spec"]["ingress"][0]["ports"], [{"port": 8443, "protocol": "TCP"}])

    def test_isolated_service_account_token_only_in_main_container(self):
        docs = render("--set", "serviceAccount.isolateToken=true", "--set", "extraContainers[0].name=proxy", "--set", "extraContainers[0].image=proxy")
        pod = next(d for d in docs if d["kind"] == "Deployment")["spec"]["template"]["spec"]
        self.assertFalse(pod["automountServiceAccountToken"])
        app, proxy = pod["containers"]
        mount = next(m for m in app["volumeMounts"] if m["name"] == "kubernetes-api-access")
        self.assertEqual(mount["mountPath"], "/var/run/secrets/kubernetes.io/serviceaccount")
        self.assertTrue(mount["readOnly"])
        self.assertFalse(proxy.get("volumeMounts"))
        sources = next(v for v in pod["volumes"] if v["name"] == "kubernetes-api-access")["projected"]["sources"]
        self.assertEqual(sources[0]["serviceAccountToken"]["path"], "token")
        self.assertEqual(sources[1]["configMap"]["name"], "kube-root-ca.crt")
        self.assertEqual(sources[2]["downwardAPI"]["items"][0]["fieldRef"]["fieldPath"], "metadata.namespace")
        default_pod = next(d for d in render() if d["kind"] == "Deployment")["spec"]["template"]["spec"]
        self.assertTrue(default_pod["automountServiceAccountToken"])

    def test_network_policy_rejects_invalid_ingress_ports(self):
        base = {"networkPolicy": {"enabled": True, "apiPeers": ["10.0.0.1/32"], "ingressFrom": [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "gatus"}}}]}}
        for ports in (None, [], [0], [65536], [-1], ["8443"], [1.5]):
            base["networkPolicy"]["ingressPorts"] = ports
            with tempfile.NamedTemporaryFile(mode="w", suffix=".yaml") as f:
                yaml.safe_dump(base, f)
                f.flush()
                command = [HELM, "template", "watch", str(CHART), "--set", "accessControl.existingSecret=fixture-auth", "-f", f.name]
                result = subprocess.run(command, capture_output=True, text=True)
                self.assertNotEqual(result.returncode, 0, repr(ports))
                if ports is None or ports == []:
                    guarded = subprocess.run(command + ["--skip-schema-validation"], capture_output=True, text=True)
                    self.assertNotEqual(guarded.returncode, 0, repr(ports))
                    self.assertIn("networkPolicy.ingressPorts", guarded.stderr)

    def test_explicit_access_control_required(self):
        result = subprocess.run([HELM, "template", "watch", str(CHART)], capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("accessControl.existingSecret", result.stderr)
        docs = render()
        pod = next(d for d in docs if d["kind"] == "Deployment")["spec"]["template"]["spec"]
        auth = next(v for v in pod["volumes"] if v["name"] == "backup-auth")
        self.assertEqual(auth["secret"]["secretName"], "fixture-auth")
        self.assertEqual(auth["secret"]["defaultMode"], 0o440)
        self.assertEqual(auth["secret"]["items"], [{"key": "token", "path": "token"}])
        mount = next(v for v in pod["containers"][0]["volumeMounts"] if v["name"] == "backup-auth")
        self.assertTrue(mount["readOnly"])
        self.assertFalse(any(d["kind"] == "Secret" for d in docs))
        config = json.loads(next(d for d in docs if d["kind"] == "ConfigMap")["data"]["config.json"])
        self.assertEqual(config["backupsTokenFile"], mount["mountPath"] + "/token")

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

    def test_cluster_rbac_names_are_collision_resistant(self):
        first = render(release="c", namespace="a-b")
        second = render(release="b-c", namespace="a")
        first_role = next(d for d in first if d["kind"] == "ClusterRole")
        second_role = next(d for d in second if d["kind"] == "ClusterRole")
        self.assertNotEqual(first_role["metadata"]["name"], second_role["metadata"]["name"])

        for docs in (first, second):
            role = next(d for d in docs if d["kind"] == "ClusterRole")
            binding = next(d for d in docs if d["kind"] == "ClusterRoleBinding")
            self.assertEqual(role["metadata"]["name"], binding["metadata"]["name"])
            self.assertEqual(role["metadata"]["name"], binding["roleRef"]["name"])
            self.assertLessEqual(len(role["metadata"]["name"]), 63)

        long_override_a = "a" * 62 + "b"
        long_override_b = "a" * 62 + "c"
        long_a = next(
            d for d in render("--set", f"fullnameOverride={long_override_a}")
            if d["kind"] == "ClusterRole"
        )
        long_b = next(
            d for d in render("--set", f"fullnameOverride={long_override_b}")
            if d["kind"] == "ClusterRole"
        )
        self.assertNotEqual(long_a["metadata"]["name"], long_b["metadata"]["name"])

    def test_network_policy_and_invalid_inventory(self):
        docs = render("-f", str(ROOT / "examples/network-policy.yaml"))
        policy = next(d for d in docs if d["kind"] == "NetworkPolicy")["spec"]
        self.assertEqual(policy["policyTypes"], ["Ingress", "Egress"])
        self.assertEqual(
            policy["egress"][0]["to"], [{"ipBlock": {"cidr": "10.96.0.1/32"}}]
        )
        self.assertEqual(policy["ingress"][0]["ports"][0]["port"], 8080)
        for override in (
            "accessControl.existingSecret=",
            "config.clusters=[]",
            "config.maxAgeSeconds=0",
            "config.clusters[0].repos.repo5.maxAgeSeconds=10",
            "networkPolicy.enabled=true",
            "serviceAccount.create=false",
        ):
            result = subprocess.run(
                [HELM, "template", "watch", str(CHART), "--set", "accessControl.existingSecret=fixture-auth", "--set", override],
                capture_output=True,
                text=True,
            )
            self.assertNotEqual(result.returncode, 0, override)


if __name__ == "__main__":
    unittest.main()
