package gcp_e2e

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// The whole point of this line in the diagnostics is to be readable in a
// public prow log and to name the account without naming the project, so
// both halves are worth pinning.
func TestCredentialIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		data map[string][]byte
		want string
	}{{
		name: "minted service account key",
		data: map[string][]byte{"service_account.json": []byte(
			`{"type":"service_account","client_email":"ci-op-817xzc-bgp-cloud-c-wnxfn@openshift-gce-devel-ci.iam.gserviceaccount.com"}`)},
		want: "service_account.json type=service_account account=ci-op-817xzc-bgp-cloud-c-wnxfn",
	}, {
		name: "federated credential",
		data: map[string][]byte{"credentials": []byte(
			`{"type":"external_account","client_email":""}`)},
		want: "credentials type=external_account account=<none>",
	}, {
		name: "nothing recognised",
		data: map[string][]byte{"something-else": []byte(`{}`)},
		want: "no credential key recognised",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got := credentialIdentity(&corev1.Secret{Data: tc.data})
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
