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

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCheckAnswers(t *testing.T) {
	const client = "10.1.0.68"
	router := Destination{Node: "router", PodAddress: "10.100.0.3", Forwarding: true}
	other := Destination{Node: "router-2", PodAddress: "10.100.0.5", Forwarding: true}
	plain := Destination{Node: "non-router", PodAddress: "10.100.0.4", Forwarding: false}

	for _, tc := range []struct {
		name    string
		dests   []Destination
		answers map[string]string
		want    string // substring of the error, or "" for none
	}{
		{
			name:    "forwarding pods answer from the client, the other is silent",
			dests:   []Destination{router, other, plain},
			answers: map[string]string{"10.100.0.3": client + ":1", "10.100.0.5": client + ":2", "10.100.0.4": ""},
		},
		{
			name:    "a forwarding pod does not answer",
			dests:   []Destination{router, plain},
			answers: map[string]string{"10.100.0.3": "", "10.100.0.4": ""},
			want:    "10.100.0.3 on router did not answer",
		},
		{
			name:    "a forwarding pod sees another source",
			dests:   []Destination{router, plain},
			answers: map[string]string{"10.100.0.3": "10.0.2.4:9", "10.100.0.4": ""},
			want:    "saw the request come from 10.0.2.4:9",
		},
		{
			name:    "a pod without forwarding answers",
			dests:   []Destination{router, plain},
			answers: map[string]string{"10.100.0.3": client + ":1", "10.100.0.4": client + ":3"},
			want:    "10.100.0.4 on non-router answered",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckAnswers(tc.dests, tc.answers, client)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("expected no error, got %v", err)
			case tc.want != "" && err == nil:
				t.Fatalf("expected an error containing %q, got none", tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("expected an error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestCheckCoverage(t *testing.T) {
	router := Destination{Node: "router", PodAddress: "10.100.0.3", Forwarding: true}
	plain := Destination{Node: "non-router", PodAddress: "10.100.0.4", Forwarding: false}
	for _, tc := range []struct {
		name  string
		dests []Destination
		want  string
	}{
		{name: "both kinds of worker", dests: []Destination{router, plain}},
		{name: "no worker without forwarding", dests: []Destination{router, router}, want: "no worker without forwarding"},
		{name: "no worker with forwarding", dests: []Destination{plain}, want: "no worker's interface forwards"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckCoverage(tc.dests)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("expected no error, got %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("expected an error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestSourceHost(t *testing.T) {
	for in, want := range map[string]string{
		"10.1.0.68:43944": "10.1.0.68",
		"10.1.0.68":       "10.1.0.68",
		"":                "",
	} {
		if got := SourceHost(in); got != want {
			t.Errorf("SourceHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProbeRound(t *testing.T) {
	console := strings.Join([]string{
		"[    8.4] cloud-init[1946]: Cloud-init v. 22.2.2 finished",
		"e2e-probe 17 10.100.0.3 10.0.0.9:40000",
		"e2e-probe 17 end",
		"e2e-probe 170 10.100.0.3 stale",
		"e2e-probe 170 10.100.0.4 ",
		"ip-10-0-16-139 login: e2e-probe 18 10.100.0.3 10.0.0.9:41000\r",
		"e2e-probe 18 10.100.0.4 \r",
		"e2e-probe 18 end\r",
		"e2e-probe 19 10.100.0.3 10.0.0.9:42000",
	}, "\n")

	for _, tc := range []struct {
		round        string
		want         string
		wantComplete bool
	}{
		{"18", "10.100.0.3 10.0.0.9:41000\n10.100.0.4 ", true},
		{"17", "10.100.0.3 10.0.0.9:40000", true},
		{"170", "10.100.0.3 stale\n10.100.0.4 ", false},
		{"19", "10.100.0.3 10.0.0.9:42000", false},
		{"20", "", false},
	} {
		got, complete := ProbeRound(console, tc.round)
		if got != tc.want || complete != tc.wantComplete {
			t.Errorf("ProbeRound(%q) = %q, %v; want %q, %v", tc.round, got, complete, tc.want, tc.wantComplete)
		}
	}
}

func TestNetexecPodsSkipsTerminatingPods(t *testing.T) {
	worker := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   "worker-a",
		Labels: map[string]string{WorkerRoleLabel: ""},
	}}
	pod := func(name, address string, ready bool) *corev1.Pod {
		status := corev1.ConditionFalse
		if ready {
			status = corev1.ConditionTrue
		}
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "prod",
				Labels:    map[string]string{"app": netexecName},
				Annotations: map[string]string{"k8s.ovn.org/pod-networks": `{"prod/cluster-udn-prod":` +
					`{"ip_addresses":["` + address + `/16"],"role":"primary"}}`},
			},
			Spec:   corev1.PodSpec{NodeName: worker.Name},
			Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}},
		}
	}
	// Still Ready while it shuts down, as a pod is until its containers
	// stop. The fake client keeps an object with a deletion timestamp
	// only while a finalizer holds it.
	terminating := func(p *corev1.Pod) *corev1.Pod {
		now := metav1.NewTime(time.Now())
		p.DeletionTimestamp = &now
		p.Finalizers = []string{"test/hold"}
		return p
	}

	for _, tc := range []struct {
		name    string
		pods    []*corev1.Pod
		want    string
		wantErr bool
	}{
		{"the replacement is ready", []*corev1.Pod{
			terminating(pod("netexec-old", "10.100.0.3", true)),
			pod("netexec-new", "10.100.0.9", true),
		}, "10.100.0.9", false},
		{"the replacement is not ready yet", []*corev1.Pod{
			pod("netexec-new", "10.100.0.9", false),
			terminating(pod("netexec-old", "10.100.0.3", true)),
		}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(worker)
			for _, p := range tc.pods {
				b = b.WithObjects(p)
			}
			_, addresses, err := NetexecPods(context.Background(), b.Build(), "prod")
			if tc.wantErr {
				if err == nil {
					t.Errorf("want an error while only a terminating pod is ready, got %v", addresses)
				}
				return
			}
			if err != nil {
				t.Fatalf("NetexecPods: %v", err)
			}
			if got := addresses[worker.Name]; got != tc.want {
				t.Errorf("address for %s = %q, want %q (the pod that is not terminating)", worker.Name, got, tc.want)
			}
		})
	}
}
