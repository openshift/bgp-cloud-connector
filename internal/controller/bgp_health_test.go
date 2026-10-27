/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	networkingapi "github.com/openshift/bgp-cloud-connector/api/v1beta1"
)

func TestObserveBGPSessionsFiltersByPeerGroupAndNode(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := networkingapi.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypeWithName(BGPSessionStateGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(BGPSessionStateGVK.GroupVersion().WithKind("BGPSessionStateList"), &unstructured.UnstructuredList{})

	config := &networkingapi.BGPCloudConfiguration{
		Spec: networkingapi.BGPCloudConfigurationSpec{
			RouterNodeSelector: map[string]string{"bgp_router": "true"},
			BGP: networkingapi.BGPConfig{PeerGroups: []networkingapi.PeerGroup{{
				NodeSelector: map[string]string{"topology.kubernetes.io/zone": "a"},
				Neighbors:    []networkingapi.BGPNeighbor{{Address: "10.0.0.1"}},
			}}},
		},
	}
	nodes := []*corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{"bgp_router": "true", "topology.kubernetes.io/zone": "a"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-b", Labels: map[string]string{"bgp_router": "true", "topology.kubernetes.io/zone": "b"}}},
	}
	sessions := []*unstructured.Unstructured{
		newTestBGPSession("session-a", "node-a", "10.0.0.1"),
		newTestBGPSession("session-b", "node-b", "10.0.0.1"),
		newTestBGPSession("session-peer", "node-a", "10.0.0.2"),
	}
	objects := make([]client.Object, 0, len(nodes)+len(sessions))
	for _, node := range nodes {
		objects = append(objects, node)
	}
	for _, session := range sessions {
		objects = append(objects, session)
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

	got, observed, err := observeBGPSessions(context.Background(), fakeClient, config)
	if err != nil {
		t.Fatal(err)
	}
	if !observed || len(got) != 1 || got[0].Node != "node-a" || got[0].Peer != "10.0.0.1" {
		t.Fatalf("unexpected filtered sessions: observed=%t sessions=%+v", observed, got)
	}
}

func TestSetBGPSessionConditionCapsStatusButUsesFullList(t *testing.T) {
	config := &networkingapi.BGPCloudConfiguration{}
	sessions := make([]networkingapi.BGPSessionStatus, 129)
	for i := range sessions {
		sessions[i] = networkingapi.BGPSessionStatus{Node: "node", Peer: fmt.Sprintf("10.0.0.%d", i+1), BGPStatus: "Established"}
	}

	setBGPSessionCondition(config, sessions, true, len(sessions))
	if len(config.Status.BGPSessions) != 128 {
		t.Fatalf("expected 128 status sessions, got %d", len(config.Status.BGPSessions))
	}
	condition := meta.FindStatusCondition(config.Status.Conditions, networkingapi.ConditionBGPSessionsEstablished)
	if condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatalf("expected established condition from full session list, got %+v", condition)
	}
}

func newTestBGPSession(name, node, peer string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": BGPSessionStateGVK.GroupVersion().String(),
		"kind":       BGPSessionStateGVK.Kind,
		"metadata":   map[string]interface{}{"name": name, "namespace": FRRNamespace},
		"status":     map[string]interface{}{"node": node, "peer": peer, "bgpStatus": "Established"},
	}}
}
