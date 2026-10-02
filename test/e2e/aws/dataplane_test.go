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

package aws_e2e

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	awsplatform "github.com/openshift/bgp-cloud-connector/internal/platform/aws"
	e2e "github.com/openshift/bgp-cloud-connector/test/e2e"
)

// The AWS half of the data-plane check: how the client instance is found
// and driven, and how a node's interface says whether it forwards. The
// rule itself, and the netexec pods it is checked against, are in
// test/e2e.

const (
	// How long one round may take to appear on the client's console. A
	// round of two unreachable addresses appeared in under fifty seconds
	// when measured, twenty of them spent in the client's own timeouts.
	roundTimeout = 3 * time.Minute

	// The instance tag the client reads its round and addresses from,
	// and the prefix of what it prints. hack/aws/create-client-instance.sh
	// uses the same name.
	probeTag = "e2e-probe"

	// The tag hack/aws/create-client-instance.sh marks its instance with.
	clientOwnerTag = "bgp-cloud-connector-e2e-client"
)

// clientInstance finds the instance hack/aws/create-client-instance.sh
// built for this cluster, by the tag it puts on it, and returns its id
// and private address. The pods are expected to report exactly that
// address as the source of every request.
func clientInstance(ctx context.Context) (id, address string, err error) {
	out, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:" + clientOwnerTag), Values: []string{clusterID}},
			{Name: aws.String("instance-state-name"), Values: []string{"running"}},
		},
	})
	if err != nil {
		return "", "", err
	}
	for _, res := range out.Reservations {
		for _, inst := range res.Instances {
			return aws.ToString(inst.InstanceId), aws.ToString(inst.PrivateIpAddress), nil
		}
	}
	return "", "", fmt.Errorf("no running client instance tagged %s=%s; create it with hack/aws/create-client-instance.sh",
		clientOwnerTag, clusterID)
}

// destinations pairs every worker's netexec pod with whether the worker's
// interface forwards, read from EC2: source/destination checking switched
// off on its primary interface, as the operator does for router nodes.
func destinations(ctx context.Context, namespace string) ([]e2e.Destination, error) {
	workers, addresses, err := e2e.NetexecPods(ctx, k8sClient, namespace)
	if err != nil {
		return nil, err
	}
	out := make([]e2e.Destination, 0, len(workers))
	for i := range workers {
		node := &workers[i]
		instanceID, _, err := awsplatform.ParseProviderID(node.Spec.ProviderID)
		if err != nil {
			return nil, fmt.Errorf("node %s: %w", node.Name, err)
		}
		inst, err := describeInstance(ctx, instanceID)
		if err != nil {
			return nil, fmt.Errorf("reading the instance behind %s: %w", node.Name, err)
		}
		eni := primaryENI(inst)
		if eni == nil {
			return nil, fmt.Errorf("no primary interface on the instance behind %s", node.Name)
		}
		out = append(out, e2e.Destination{
			Node:       node.Name,
			PodAddress: addresses[node.Name],
			Forwarding: !aws.ToBool(eni.SourceDestCheck),
		})
	}
	return out, nil
}

// probe has the client instance request /clientip from every address
// and returns what each address answered. An address that did not
// answer maps to "".
//
// Nothing connects to the instance. hack/aws/create-client-instance.sh
// gives it a loop that reads the instance's own probeTag through the
// instance metadata service, probes the addresses named there whenever
// the round in it changes, and prints the results to the serial console.
// So driving it needs only EC2 calls -- CreateTags and GetConsoleOutput
// -- and no IAM role, agent or inbound rule on the instance.
func probe(ctx context.Context, instanceID string, addresses []string) (map[string]string, error) {
	round := strconv.FormatInt(time.Now().UnixNano(), 10)
	if _, err := ec2Client.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{instanceID},
		Tags: []ec2types.Tag{{
			Key:   aws.String(probeTag),
			Value: aws.String(round + " " + strings.Join(addresses, " ")),
		}},
	}); err != nil {
		return nil, fmt.Errorf("asking the client for round %s: %w", round, err)
	}

	deadline := time.Now().Add(roundTimeout)
	for {
		out, err := ec2Client.GetConsoleOutput(ctx, &ec2.GetConsoleOutputInput{
			InstanceId: aws.String(instanceID),
			Latest:     aws.Bool(true),
		})
		if err != nil {
			return nil, fmt.Errorf("reading the client's console: %w", err)
		}
		console, err := base64.StdEncoding.DecodeString(aws.ToString(out.Output))
		if err != nil {
			return nil, fmt.Errorf("decoding the client's console: %w", err)
		}
		if results, complete := e2e.ProbeRound(string(console), round); complete {
			GinkgoWriter.Printf("client instance said:\n%s\n", results)
			return e2e.ParseProbeOutput(results, addresses)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("round %s did not appear on the client's console within %s", round, roundTimeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}
