/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package gcp

import (
	"context"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// The API server hands back the stored request as decoded JSON, and the
// fake client round-trips objects through JSON the same way. A spec read
// back must compare equal to the one the operator builds, or every
// reconcile rewrites the request. Workload identity is set so its
// optional fields are part of the comparison.
func TestReconcileCredentialsRequest_UnchangedRequestIsNotRewritten(t *testing.T) {
	withWorkloadIdentityEnv(t, fullWorkloadIdentityEnv())
	updates := 0
	c := fake.NewClientBuilder().WithScheme(credentialsTestScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				updates++
				return cl.Update(ctx, obj, opts...)
			},
		}).Build()

	if err := reconcileCredentialsRequest(context.Background(), c, testNamespace, testOwner()); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	// Created, so the second pass reaches the comparison.
	getCredentialsRequest(t, c)
	if err := reconcileCredentialsRequest(context.Background(), c, testNamespace, testOwner()); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if updates != 0 {
		t.Errorf("an unchanged request was rewritten %d time(s)", updates)
	}
}
