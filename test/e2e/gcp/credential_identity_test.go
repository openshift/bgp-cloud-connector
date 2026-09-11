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
			`{"type":"service_account",` +
				`"client_email":"ci-op-817xzc-bgp-cloud-c-wnxfn@openshift-gce-devel-ci.iam.gserviceaccount.com"}`)},
		want: "service_account.json type=service_account account=ci-op-817xzc-bgp-cloud-c-wnxfn",
	}, {
		name: "federated credential CCO wrote",
		data: map[string][]byte{"service_account.json": []byte(
			`{"type":"external_account","service_account_impersonation_url":` +
				`"https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/` +
				`bgpcc-x@example-project.iam.gserviceaccount.com:generateAccessToken"}`)},
		want: "service_account.json type=external_account account=bgpcc-x",
	}, {
		name: "federated credential written by hand, read first as the operator reads it",
		data: map[string][]byte{
			"workload_identity_config.json": []byte(
				`{"type":"external_account","service_account_impersonation_url":` +
					`"https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/` +
					`bgpcc-y@example-project.iam.gserviceaccount.com:generateAccessToken"}`),
			"service_account.json": []byte(
				`{"type":"service_account","client_email":"other@example-project.iam.gserviceaccount.com"}`),
		},
		want: "workload_identity_config.json type=external_account account=bgpcc-y",
	}, {
		name: "federated credential that impersonates nobody",
		data: map[string][]byte{"service_account.json": []byte(`{"type":"external_account"}`)},
		want: "service_account.json type=external_account account=<none>",
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
