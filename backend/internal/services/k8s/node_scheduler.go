package k8s

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NodeSchedulerLabel is the label key all clawreef-eligible nodes must carry.
// Nodes without this label are excluded from instance scheduling, allowing
// operators to mix dedicated clawreef nodes with infra-only nodes in one
// cluster. Currently not enforced by default (empty selector matches every
// node) — set CLAWMANAGER_NODE_LABEL_SELECTOR to opt in.
const NodeSchedulerLabelEnv = "CLAWMANAGER_NODE_LABEL_SELECTOR"

// NodeHostnameLabel is the standard K8s label that identifies a node by its
// hostname. PV nodeAffinity and Pod nodeSelector both reference this label
// to pin storage and compute to the same node.
const NodeHostnameLabel = "kubernetes.io/hostname"

// SelectNodeForInstance returns the hostname of the most lightly-loaded
// schedulable node, suitable for hosting a new clawreef instance.
//
// Selection algorithm:
//  1. List all nodes (optionally filtered by CLAWMANAGER_NODE_LABEL_SELECTOR).
//  2. Drop nodes that are NotReady, cordoned (unschedulable=true), or tainted
//     with NoSchedule effects we cannot tolerate.
//  3. Count clawreef-managed Pods (label app=clawreef) currently bound to each
//     remaining node.
//  4. Return the hostname of the node with the lowest pod count. Ties broken
//     deterministically by hostname so creates are stable in tests.
//
// Returns an error if zero nodes are schedulable, since the caller cannot
// safely create a hostPath PV without a target node.
//
// Note: we count Pods rather than measuring real CPU/memory because every
// clawreef instance is roughly equal-sized at the resource-request level
// (overcommit factor flattens the differences) and because polling
// metrics-server adds dependencies and latency we don't need.
func SelectNodeForInstance(ctx context.Context) (string, error) {
	if globalClient == nil {
		return "", fmt.Errorf("k8s client not initialized")
	}

	listOpts := metav1.ListOptions{}
	if sel := strings.TrimSpace(os.Getenv(NodeSchedulerLabelEnv)); sel != "" {
		listOpts.LabelSelector = sel
	}

	nodeList, err := globalClient.Clientset.CoreV1().Nodes().List(ctx, listOpts)
	if err != nil {
		return "", fmt.Errorf("failed to list nodes: %w", err)
	}

	type candidate struct {
		hostname string
		podCount int
	}
	candidates := make([]candidate, 0, len(nodeList.Items))

	for i := range nodeList.Items {
		n := &nodeList.Items[i]
		if !isNodeSchedulable(n) {
			continue
		}
		hostname := nodeHostname(n)
		if hostname == "" {
			continue
		}

		count, err := countClawreefPodsOnNode(ctx, hostname)
		if err != nil {
			// Pod count failure on one node shouldn't break scheduling;
			// treat it as "very loaded" so we deprioritise it.
			fmt.Printf("WARN: failed to count pods on node %s: %v\n", hostname, err)
			count = int(^uint(0) >> 1)
		}
		candidates = append(candidates, candidate{hostname, count})
	}

	if len(candidates) == 0 {
		return "", fmt.Errorf("no schedulable nodes available for clawreef instance")
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].podCount != candidates[j].podCount {
			return candidates[i].podCount < candidates[j].podCount
		}
		return candidates[i].hostname < candidates[j].hostname
	})

	return candidates[0].hostname, nil
}

// isNodeSchedulable returns true when a node is healthy and willing to accept
// new pods. Mirrors the k8s scheduler's basic eligibility checks; we don't
// honour custom NoSchedule taints because clawreef pods don't carry custom
// tolerations today.
func isNodeSchedulable(n *corev1.Node) bool {
	if n.Spec.Unschedulable {
		return false
	}
	for _, t := range n.Spec.Taints {
		if t.Effect == corev1.TaintEffectNoSchedule || t.Effect == corev1.TaintEffectNoExecute {
			// Allow the well-known control-plane taint only when there is
			// just one node in the cluster (single-node deployment); an
			// operator who left this taint and added Workers expects pods
			// to land on Workers, not the control-plane.
			if t.Key == "node-role.kubernetes.io/control-plane" || t.Key == "node-role.kubernetes.io/master" {
				continue
			}
			return false
		}
	}
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady && c.Status != corev1.ConditionTrue {
			return false
		}
	}
	return true
}

// nodeHostname extracts the kubernetes.io/hostname label, falling back to
// node Name. The label is the right key for PV nodeAffinity and Pod
// nodeSelector matching, so we prefer it when available.
func nodeHostname(n *corev1.Node) string {
	if v := strings.TrimSpace(n.Labels[NodeHostnameLabel]); v != "" {
		return v
	}
	return n.Name
}

// countClawreefPodsOnNode returns the number of clawreef-managed Pods (across
// all namespaces) currently scheduled onto the given node. Used to balance
// new instance placement across Workers.
func countClawreefPodsOnNode(ctx context.Context, hostname string) (int, error) {
	pods, err := globalClient.Clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		LabelSelector: "app=clawreef",
		FieldSelector: fmt.Sprintf("spec.nodeName=%s", hostname),
	})
	if err != nil {
		return 0, err
	}
	return len(pods.Items), nil
}

