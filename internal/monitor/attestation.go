package monitor

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"

	"github.com/zekihan/backrestwatch/internal/config"
	"github.com/zekihan/backrestwatch/internal/kube"
)

const attestationKey = "backrestwatch.io/attestation-key"
const attestationSignature = "backrestwatch.io/attestation-signature"

// An attestation is issued only by an independently trusted backup verifier,
// after verifying the actual backup. Signing caller-supplied metadata is unsafe.
// AttestationMessage returns the exact versioned UTF-8 JSON bytes a trusted
// issuer signs. Object identity, destination and successful completion are bound.
func AttestationMessage(kind string, meta kube.Metadata, cluster kube.Cluster, repo, completed string) []byte {
	message, _ := json.Marshal([]string{"backrestwatch-evidence/v1", kind, meta.Namespace, meta.Name, meta.UID,
		meta.CreationTimestamp, cluster.Metadata.UID, cluster.Metadata.CreationTimestamp, repo,
		repositoryIdentity(cluster, repo), completed})
	return message
}

func evidenceSource(kind string, key []byte) string {
	digest := sha256.Sum256(key)
	return "Attested " + kind + " (sha256:" + hex.EncodeToString(digest[:]) + ")"
}

func attestedSource(c config.Config, kind string, meta kube.Metadata, cluster kube.Cluster, repo, completed string) string {
	key, err := base64.StdEncoding.Strict().DecodeString(c.EvidencePublicKeys[meta.Annotations[attestationKey]])
	if err != nil || len(key) != ed25519.PublicKeySize || meta.Name == "" || meta.UID == "" {
		return ""
	}
	signature, err := base64.StdEncoding.Strict().DecodeString(meta.Annotations[attestationSignature])
	if err != nil || !ed25519.Verify(key, AttestationMessage(kind, meta, cluster, repo, completed), signature) {
		return ""
	}
	return evidenceSource(kind, key)
}

func trustedSource(c config.Config, source string) bool {
	if source == "PostgresCluster scheduled-backup status" {
		return true
	}
	for _, encoded := range c.EvidencePublicKeys {
		key, err := base64.StdEncoding.Strict().DecodeString(encoded)
		if err == nil && len(key) == ed25519.PublicKeySize && (source == evidenceSource("Job", key) || source == evidenceSource("CronJob", key)) {
			return true
		}
	}
	return false
}
