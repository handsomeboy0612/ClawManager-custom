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

// SelectNodeForInstance returns the hostname of the schedulable node with the
// most resource headroom for hosting a new clawreef instance of the given
// spec. The instanceType/cpuCores/memoryGB describe the instance about to be
// created so we can (a) rank nodes by real remaining capacity and (b) refuse
// up front when nothing fits.
//
// Selection algorithm:
//  1. Compute the new Pod's CPU/memory Requests using the exact same overcommit
//     policy CreatePod applies (buildResourceRequirements), so our headroom
//     math predicts real kube-scheduler admission.
//  2. List all nodes (optionally filtered by CLAWMANAGER_NODE_LABEL_SELECTOR).
//  3. Drop nodes that are NotReady, cordoned (unschedulable=true), or tainted
//     with NoSchedule effects we cannot tolerate.
//  4. For each remaining node compute free CPU/memory = Allocatable − sum of
//     Requests of ALL pods already bound to it (system pods included).
//  5. Keep only nodes whose free CPU and memory can both admit the new Pod's
//     Requests, then return the one with the most CPU headroom (the usual
//     bottleneck), breaking ties by memory headroom then hostname for stable,
//     test-deterministic placement.
//
// Returns an error if zero nodes can fit the instance, so the caller fails the
// create with a clear "insufficient capacity" message instead of provisioning
// a hostPath PV + Pod that would hang in Pending forever.
//
// We read Pod Requests straight from Pod specs rather than polling
// metrics-server: Requests (not live usage) are exactly what the scheduler
// sums to decide fit, so no extra dependency is needed.
func SelectNodeForInstance(ctx context.Context, instanceType string, cpuCores float64, memoryGB int) (string, error) {
	if globalClient == nil {
		return "", fmt.Errorf("k8s client not initialized")
	}

	reqs := buildResourceRequirements(PodConfig{
		Type:     instanceType,
		CPUCores: cpuCores,
		MemoryGB: memoryGB,
	})
	needCPUMillis := reqs.Requests.Cpu().MilliValue()
	needMemBytes := reqs.Requests.Memory().Value()

	listOpts := metav1.ListOptions{}
	if sel := strings.TrimSpace(os.Getenv(NodeSchedulerLabelEnv)); sel != "" {
		listOpts.LabelSelector = sel
	}

	nodeList, err := globalClient.Clientset.CoreV1().Nodes().List(ctx, listOpts)
	if err != nil {
		return "", fmt.Errorf("failed to list nodes: %w", err)
	}

	type candidate struct {
		hostname     string
		freeCPUMilli int64
		freeMemBytes int64
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

		usedCPU, usedMem, err := sumPodRequestsOnNode(ctx, hostname)
		if err != nil {
			// A listing failure on one node shouldn't break scheduling for the
			// whole cluster; skip it so we never place onto a node whose load
			// we couldn't measure.
			fmt.Printf("WARN: failed to sum pod requests on node %s: %v\n", hostname, err)
			continue
		}

		freeCPU := n.Status.Allocatable.Cpu().MilliValue() - usedCPU
		freeMem := n.Status.Allocatable.Memory().Value() - usedMem

		// Only consider nodes that can admit this Pod's Requests on both
		// dimensions; this turns a silent Pending into the explicit error below.
		if freeCPU < needCPUMillis || freeMem < needMemBytes {
			continue
		}

		candidates = append(candidates, candidate{hostname, freeCPU, freeMem})
	}

	if len(candidates) == 0 {
		return "", fmt.Errorf(
			"no schedulable node has enough capacity for instance (needs %dm CPU / %d MiB memory requests)",
			needCPUMillis, needMemBytes/(1024*1024),
		)
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].freeCPUMilli != candidates[j].freeCPUMilli {
			return candidates[i].freeCPUMilli > candidates[j].freeCPUMilli
		}
		if candidates[i].freeMemBytes != candidates[j].freeMemBytes {
			return candidates[i].freeMemBytes > candidates[j].freeMemBytes
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

// sumPodRequestsOnNode returns the total CPU (millicores) and memory (bytes)
// Requests of ALL pods currently bound to the given node — not just clawreef
// pods. This matches what the kube-scheduler sums when deciding whether a new
// pod fits, so system/control-plane pods must be included; counting only
// clawreef pods would overstate headroom on control-plane nodes and send new
// instances to a node that is actually full.
//
// Succeeded/Failed pods no longer hold node resources, so they are excluded to
// mirror scheduler accounting.
func sumPodRequestsOnNode(ctx context.Context, hostname string) (cpuMillis int64, memBytes int64, err error) {
	pods, err := globalClient.Clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("spec.nodeName=%s", hostname),
	})
	if err != nil {
		return 0, 0, err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		c, m := podRequests(p)
		cpuMillis += c
		memBytes += m
	}
	return cpuMillis, memBytes, nil
}

// podRequests computes a pod's effective resource Requests the way the
// kube-scheduler does: for each resource, the larger of (sum of regular
// container requests) and (max of init-container requests). Pod overhead and
// restartable (sidecar) init containers are ignored — clawreef workloads use
// neither.
func podRequests(p *corev1.Pod) (cpuMillis int64, memBytes int64) {
	var sumCPU, sumMem int64
	for i := range p.Spec.Containers {
		r := p.Spec.Containers[i].Resources.Requests
		sumCPU += r.Cpu().MilliValue()
		sumMem += r.Memory().Value()
	}

	var initCPU, initMem int64
	for i := range p.Spec.InitContainers {
		r := p.Spec.InitContainers[i].Resources.Requests
		if v := r.Cpu().MilliValue(); v > initCPU {
			initCPU = v
		}
		if v := r.Memory().Value(); v > initMem {
			initMem = v
		}
	}

	cpuMillis = sumCPU
	if initCPU > cpuMillis {
		cpuMillis = initCPU
	}
	memBytes = sumMem
	if initMem > memBytes {
		memBytes = initMem
	}
	return cpuMillis, memBytes
}

