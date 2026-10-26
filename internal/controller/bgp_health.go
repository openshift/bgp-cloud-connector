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
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	networkingapi "github.com/openshift/bgp-cloud-connector/api/v1beta1"
)

// reconcileBGPSessionHealth refreshes only the frr-k8s-owned health snapshot.
// Keeping this separate from the main reconciliation path lets us poll often
// without repeating cloud discovery and cloud API writes.
func (r *BGPCloudConfigurationReconciler) reconcileBGPSessionHealth(
	ctx context.Context,
	config *networkingapi.BGPCloudConfiguration,
	baseline networkingapi.BGPCloudConfigurationStatus,
) (observed bool, err error) {
	expected, expectedErr := expectedBGPSessionCount(ctx, r.Client, config)
	if expectedErr != nil {
		err = r.patchConfigStatus(ctx, config, baseline, func(c *networkingapi.BGPCloudConfiguration) {
			meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
				Type:               networkingapi.ConditionBGPSessionsEstablished,
				Status:             metav1.ConditionUnknown,
				Reason:             "ExpectedSessionCountUnavailable",
				Message:            fmt.Sprintf("Unable to determine expected BGP session count: %v", expectedErr),
				ObservedGeneration: c.Generation,
			})
		})
		return false, err
	}

	sessions, observed, sessionErr := observeBGPSessions(ctx, r.Client, config)
	err = r.patchConfigStatus(ctx, config, baseline, func(c *networkingapi.BGPCloudConfiguration) {
		if sessionErr != nil {
			meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
				Type:               networkingapi.ConditionBGPSessionsEstablished,
				Status:             metav1.ConditionUnknown,
				Reason:             "ObservationFailed",
				Message:            fmt.Sprintf("Unable to read frr-k8s BGPSessionState: %v", sessionErr),
				ObservedGeneration: c.Generation,
			})
			return
		}
		setBGPSessionCondition(c, sessions, observed, expected)
	})
	return observed, err
}

// observeBGPSessions reads the frr-k8s-owned session objects without making
// the connector depend on frr-k8s Go types. This keeps the connector's API
// compatible with the frr-k8s CRD while still surfacing the useful fields in
// BGPCloudConfiguration.status.
func observeBGPSessions(ctx context.Context, c client.Client, config *networkingapi.BGPCloudConfiguration) ([]networkingapi.BGPSessionStatus, bool, error) {
	sessions := &unstructured.UnstructuredList{}
	sessions.SetGroupVersionKind(BGPSessionStateGVK)
	if err := c.List(ctx, sessions, client.InNamespace(FRRNamespace)); err != nil {
		// Unit tests and installations that have not enabled frr-k8s yet do not
		// have this CRD. That is an Unknown health signal, not a connector fault.
		if apierrors.IsNotFound(err) || runtime.IsNotRegisteredError(err) || meta.IsNoMatchError(err) {
			return nil, false, nil
		}
		return nil, false, err
	}

	desired := desiredPeers(config)
	out := make([]networkingapi.BGPSessionStatus, 0, len(sessions.Items))
	for i := range sessions.Items {
		obj := &sessions.Items[i]
		peer, _, _ := unstructured.NestedString(obj.Object, "status", "peer")
		if _, ok := desired[peer]; !ok {
			continue
		}
		node, _, _ := unstructured.NestedString(obj.Object, "status", "node")
		bgpStatus, _, _ := unstructured.NestedString(obj.Object, "status", "bgpStatus")
		bfdStatus, _, _ := unstructured.NestedString(obj.Object, "status", "bfdStatus")
		out = append(out, networkingapi.BGPSessionStatus{
			Node: node, Peer: peer, BGPStatus: bgpStatus, BFDStatus: bfdStatus,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Node != out[j].Node {
			return out[i].Node < out[j].Node
		}
		return out[i].Peer < out[j].Peer
	})
	return out, true, nil
}

func expectedBGPSessionCount(ctx context.Context, c client.Client, config *networkingapi.BGPCloudConfiguration) (int, error) {
	nodes := &corev1.NodeList{}
	if err := c.List(ctx, nodes, client.MatchingLabelsSelector{
		Selector: labels.SelectorFromSet(config.Spec.RouterNodeSelector),
	}); err != nil {
		return 0, err
	}

	count := 0
	groups := config.Status.PeerGroups
	if len(groups) == 0 {
		groups = make([]networkingapi.PeerGroupStatus, 0, len(config.Spec.BGP.PeerGroups))
		for i, group := range config.Spec.BGP.PeerGroups {
			groups = append(groups, networkingapi.PeerGroupStatus{
				Key: fmt.Sprintf("%d", i+1), NodeSelector: group.NodeSelector,
				Neighbors: group.Neighbors,
			})
		}
	}
	for i := range nodes.Items {
		nodeLabels := nodes.Items[i].Labels
		for _, group := range groups {
			if !labels.SelectorFromSet(group.NodeSelector).Matches(labels.Set(nodeLabels)) {
				continue
			}
			count += len(group.Neighbors)
		}
	}
	return count, nil
}

func desiredPeers(config *networkingapi.BGPCloudConfiguration) map[string]struct{} {
	peers := make(map[string]struct{})
	if len(config.Status.PeerGroups) > 0 {
		for _, group := range config.Status.PeerGroups {
			for _, peer := range group.Neighbors {
				peers[peer.Address] = struct{}{}
			}
		}
		return peers
	}
	for _, group := range config.Spec.BGP.PeerGroups {
		for _, peer := range group.Neighbors {
			peers[peer.Address] = struct{}{}
		}
	}
	return peers
}

func setBGPSessionCondition(config *networkingapi.BGPCloudConfiguration, sessions []networkingapi.BGPSessionStatus, observed bool, expected int) {
	config.Status.BGPSessions = sessions
	if !observed {
		meta.RemoveStatusCondition(&config.Status.Conditions, networkingapi.ConditionBGPSessionsEstablished)
		return
	}

	condition := metav1.Condition{
		Type:               networkingapi.ConditionBGPSessionsEstablished,
		Status:             metav1.ConditionUnknown,
		Reason:             "WaitingForSessionState",
		Message:            "Waiting for frr-k8s to report BGP session state",
		ObservedGeneration: config.Generation,
	}
	if len(sessions) == 0 {
		condition.Reason = "NoSessionsReported"
		condition.Message = fmt.Sprintf("No BGP sessions reported for %d configured peer(s)", len(desiredPeers(config)))
	} else {
		notEstablished := 0
		for _, session := range sessions {
			if session.BGPStatus != "Established" {
				notEstablished++
			}
		}
		if notEstablished == 0 && len(sessions) >= expected {
			condition.Status = metav1.ConditionTrue
			condition.Reason = "Established"
			condition.Message = fmt.Sprintf("%d/%d expected BGP session(s) are Established", len(sessions), expected)
		} else {
			condition.Status = metav1.ConditionFalse
			condition.Reason = "SessionNotEstablished"
			condition.Message = fmt.Sprintf("%d/%d expected BGP session(s) are Established (%d reported in a non-Established state)", len(sessions)-notEstablished, expected, notEstablished)
		}
	}
	meta.SetStatusCondition(&config.Status.Conditions, condition)
}
