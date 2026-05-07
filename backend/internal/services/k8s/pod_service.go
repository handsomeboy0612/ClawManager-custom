package k8s

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// PodService handles Pod operations
type PodService struct {
	client *Client
}

const (
	podDeletionPollInterval = 500 * time.Millisecond
	podDeletionTimeout      = 60 * time.Second
)

// NewPodService creates a new Pod service
func NewPodService() *PodService {
	return &PodService{
		client: globalClient,
	}
}

// GetClient returns the k8s client
func (s *PodService) GetClient() *Client {
	return s.client
}

// customPodFSGroup is the gid applied to mounted volumes for custom pods.
// It matches the OpenClaw image's runtime user (node, uid=1000, gid=1000)
// so the container can write to the persistent volume mounted at the
// pod's writable data directory.
//
// Note: fsGroup alone is not sufficient for storage backends that do not
// implement the kubelet ownership-management hook (notably hostPath PVs and
// some CSI drivers). For those backends the volume is mounted with the host
// directory's original ownership (typically root:root 0755), and the non-root
// container fails with EACCES on first write. To be portable across all
// storage backends we additionally run a privileged init container that
// chowns/chmods the mount before the main container starts. fsGroup is kept
// as a defense-in-depth fallback for backends that *do* honour it.
const customPodFSGroup int64 = 1000

// customPodRunAsUser is the uid applied inside the permission-fixup init
// container. It must be 0 (root) so that chown/chmod succeed regardless of
// the volume's pre-existing ownership.
const customPodInitRunAsRoot int64 = 0

// Overcommit configuration for custom-type pods (e.g. OpenClaw).
//
// customOvercommitFactor controls how aggressively we shrink resource
// Requests relative to Limits. With factor=8, a pod selling "4 vCPU / 8 GB"
// will reserve only 0.5 vCPU / 1 GB on the node, allowing ~8x denser packing.
// Limits are unchanged, so a pod that genuinely tries to use its full quota
// is still capped at the advertised value (CPU is throttled, memory is
// OOMKilled).
//
// Floors prevent specs from collapsing to scheduler-meaningless values:
// requesting 0 CPU would let the scheduler treat a pod as effectively free,
// which can lead to runaway packing and node-level pressure.
const (
	customOvercommitFactor    = 8
	customMinCPURequestMillis = 125 // 0.125 vCPU
	customMinMemRequestMiB    = 256
)

// PodConfig holds configuration for creating a pod
type PodConfig struct {
	InstanceID         int
	InstanceName       string
	UserID             int
	Type               string
	CPUCores           float64
	MemoryGB           int
	GPUEnabled         bool
	GPUCount           int
	Image              string
	MountPath          string
	ContainerPort      int32
	ImagePullPolicy    corev1.PullPolicy
	ExtraEnv           map[string]string
	EnvFromSecretNames []string
	// Command overrides the container's default ENTRYPOINT when non-empty.
	Command []string
	// Args overrides the container's default CMD when non-empty.
	Args []string
}

// CreatePod creates a new pod for an instance
func (s *PodService) CreatePod(ctx context.Context, config PodConfig) (*corev1.Pod, error) {
	if s.client == nil {
		return nil, fmt.Errorf("k8s client not initialized")
	}

	podName := s.client.GetPodName(config.InstanceID, config.InstanceName)
	namespace := s.client.GetNamespace(config.UserID)
	pvcName := s.client.GetPVCName(config.InstanceID)

	// Build resource requirements.
	//
	// For custom-type pods (e.g. OpenClaw), we split Requests and Limits to
	// enable safe overcommit on a single multi-tenant node:
	//   - Limits   = user-selected spec (hard cap; CPU is throttled, memory
	//                triggers OOMKill if exceeded).
	//   - Requests = spec / customOvercommitFactor, used by the kube-scheduler
	//                to decide how many pods can fit on a node. Setting
	//                Requests << Limits gives the pod a Burstable QoS class.
	//
	// OpenClaw is a Node.js gateway proxy: idle CPU is near zero and resident
	// memory is typically 150-500 MiB, so 8x packing is comfortable. Floors
	// keep tiny specs from collapsing to zero requests, which would let the
	// scheduler pack pods unrealistically tightly.
	//
	// Built-in (non-custom) types keep Requests == Limits (Guaranteed QoS) to
	// preserve historical behaviour for resource-sensitive workloads.
	resources := buildResourceRequirements(config)

	// Add GPU resources if enabled
	if config.GPUEnabled && config.GPUCount > 0 {
		resources.Limits["nvidia.com/gpu"] = resource.MustParse(fmt.Sprintf("%d", config.GPUCount))
		resources.Requests["nvidia.com/gpu"] = resource.MustParse(fmt.Sprintf("%d", config.GPUCount))
	}

	// Default container port
	if config.ContainerPort == 0 {
		config.ContainerPort = 3001
	}

	// Default image pull policy to IfNotPresent so that air-gapped and
	// enterprise environments can use locally cached images without being
	// forced to pull from a remote registry (fixes #94).
	pullPolicy := config.ImagePullPolicy
	if pullPolicy == "" {
		pullPolicy = corev1.PullIfNotPresent
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
			Labels: map[string]string{
				"app":           "clawreef",
				"instance-id":   fmt.Sprintf("%d", config.InstanceID),
				"instance-name": config.InstanceName,
				"user-id":       fmt.Sprintf("%d", config.UserID),
				"instance-type": config.Type,
				"managed-by":    "clawreef",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:   corev1.RestartPolicyNever,
			SecurityContext: buildPodSecurityContext(config.Type),
			InitContainers:  buildPermissionInitContainers(config, pullPolicy),
			Containers: []corev1.Container{
				{
					Name:            "desktop",
					Image:           config.Image,
					ImagePullPolicy: pullPolicy,
					Command:         config.Command,
					Args:            config.Args,
					Ports: []corev1.ContainerPort{
						{
							ContainerPort: config.ContainerPort,
							Name:          "http",
						},
					},
				StartupProbe:   buildStartupProbe(config.Type, config.ContainerPort),
				ReadinessProbe: buildReadinessProbe(config.Type, config.ContainerPort),
				LivenessProbe:  buildLivenessProbe(config.Type, config.ContainerPort),
					Resources: resources,
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "data",
							MountPath: config.MountPath,
						},
					},
					Env: []corev1.EnvVar{
						{
							Name:  "INSTANCE_ID",
							Value: fmt.Sprintf("%d", config.InstanceID),
						},
						{
							Name:  "USER_ID",
							Value: fmt.Sprintf("%d", config.UserID),
						},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "data",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvcName,
						},
					},
				},
			},
		},
	}

	for key, value := range config.ExtraEnv {
		pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env, corev1.EnvVar{
			Name:  key,
			Value: value,
		})
	}

	for _, secretName := range config.EnvFromSecretNames {
		if secretName == "" {
			continue
		}
		pod.Spec.Containers[0].EnvFrom = append(pod.Spec.Containers[0].EnvFrom, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
			},
		})
	}

	createdPod, err := s.client.Clientset.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		// Check if pod already exists
		if errors.IsAlreadyExists(err) {
			// Try to get the existing pod with the same name. It may still be terminating.
			existingPod, getErr := s.client.Clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
			if getErr == nil && existingPod != nil {
				if existingPod.DeletionTimestamp == nil {
					deleteErr := s.client.Clientset.CoreV1().Pods(namespace).Delete(ctx, existingPod.Name, metav1.DeleteOptions{})
					if deleteErr != nil && !errors.IsNotFound(deleteErr) {
						return nil, fmt.Errorf("failed to delete existing pod %s: %w", existingPod.Name, deleteErr)
					}
				}

				if waitErr := s.waitForPodDeletion(ctx, namespace, existingPod.Name); waitErr != nil {
					return nil, fmt.Errorf("failed waiting for pod deletion %s: %w", existingPod.Name, waitErr)
				}

				// Retry creation
				createdPod, err = s.client.Clientset.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
				if err != nil {
					return nil, fmt.Errorf("failed to create pod after deletion %s: %w", podName, err)
				}
				return createdPod, nil
			}
		}
		return nil, fmt.Errorf("failed to create pod %s: %w", podName, err)
	}

	return createdPod, nil
}

func intstrFromInt32(port int32) intstr.IntOrString {
	return intstr.FromInt32(port)
}

// GetPod gets a pod by instance ID
func (s *PodService) GetPod(ctx context.Context, userID, instanceID int) (*corev1.Pod, error) {
	if s.client == nil {
		return nil, fmt.Errorf("k8s client not initialized")
	}

	// List pods with instance-id label
	namespace := s.client.GetNamespace(userID)
	selector := fmt.Sprintf("instance-id=%d", instanceID)

	pods, err := s.client.Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("pod not found for instance %d", instanceID)
	}

	return &pods.Items[0], nil
}

// DeletePod deletes a pod
func (s *PodService) DeletePod(ctx context.Context, userID, instanceID int) error {
	if s.client == nil {
		return fmt.Errorf("k8s client not initialized")
	}

	pod, err := s.GetPod(ctx, userID, instanceID)
	if err != nil {
		// Pod doesn't exist, nothing to delete
		if isNotFoundError(err) {
			return nil
		}
		return err
	}

	err = s.client.Clientset.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("failed to delete pod %s: %w", pod.Name, err)
	}

	if err := s.waitForPodDeletion(ctx, pod.Namespace, pod.Name); err != nil {
		return fmt.Errorf("failed waiting for pod %s to be deleted: %w", pod.Name, err)
	}

	return nil
}

// GetPodStatus gets the status of a pod
func (s *PodService) GetPodStatus(ctx context.Context, userID, instanceID int) (*corev1.PodStatus, error) {
	pod, err := s.GetPod(ctx, userID, instanceID)
	if err != nil {
		return nil, err
	}
	return &pod.Status, nil
}

// GetPodIP gets the pod IP
func (s *PodService) GetPodIP(ctx context.Context, userID, instanceID int) (string, error) {
	pod, err := s.GetPod(ctx, userID, instanceID)
	if err != nil {
		return "", err
	}
	return pod.Status.PodIP, nil
}

// PodExists checks if a pod exists
func (s *PodService) PodExists(ctx context.Context, userID, instanceID int) (bool, error) {
	_, err := s.GetPod(ctx, userID, instanceID)
	if err != nil {
		if isNotFoundError(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func isNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return containsSubstring(errStr, "not found") ||
		containsSubstring(errStr, "NotFound")
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// isCustomInstance returns true for instance types whose containers are
// externally supplied and may not expose a predictable health-check port.
// Probes are skipped for these types to avoid false-positive failures.
func isCustomInstance(instanceType string) bool {
	return instanceType == "custom"
}

// buildResourceRequirements computes the Pod's Requests/Limits.
//
// For custom-type pods we apply the overcommit policy described next to the
// customOvercommitFactor constant. For all other types we keep the legacy
// Requests==Limits (Guaranteed QoS) behaviour so resource-sensitive built-in
// workloads are unaffected.
//
// GPU is added by the caller when GPUEnabled, since GPU resources cannot be
// fractional and must always be Requested == Limited 1:1.
func buildResourceRequirements(config PodConfig) corev1.ResourceRequirements {
	cpuLimitStr := fmt.Sprintf("%g", config.CPUCores)
	memLimitStr := fmt.Sprintf("%dGi", config.MemoryGB)

	if !isCustomInstance(config.Type) {
		return corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpuLimitStr),
				corev1.ResourceMemory: resource.MustParse(memLimitStr),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpuLimitStr),
				corev1.ResourceMemory: resource.MustParse(memLimitStr),
			},
		}
	}

	cpuRequestMillis := int64(config.CPUCores * 1000 / customOvercommitFactor)
	if cpuRequestMillis < customMinCPURequestMillis {
		cpuRequestMillis = customMinCPURequestMillis
	}
	memRequestMiB := int64(config.MemoryGB) * 1024 / customOvercommitFactor
	if memRequestMiB < customMinMemRequestMiB {
		memRequestMiB = customMinMemRequestMiB
	}

	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(fmt.Sprintf("%dm", cpuRequestMillis)),
			corev1.ResourceMemory: resource.MustParse(fmt.Sprintf("%dMi", memRequestMiB)),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpuLimitStr),
			corev1.ResourceMemory: resource.MustParse(memLimitStr),
		},
	}
}

// buildPodSecurityContext returns a PodSecurityContext that grants the
// configured fsGroup ownership to mounted volumes for custom-type pods.
// This is required so that non-root container processes (e.g. OpenClaw's
// node user with uid/gid 1000) can write to the persistent volume mounted
// at the pod's data directory. Returns nil for built-in instance types,
// which run as root and do not need fsGroup adjustments.
func buildPodSecurityContext(instanceType string) *corev1.PodSecurityContext {
	if !isCustomInstance(instanceType) {
		return nil
	}
	gid := customPodFSGroup
	return &corev1.PodSecurityContext{
		FSGroup: &gid,
	}
}

// buildPermissionInitContainers returns an init container that fixes the
// ownership and permissions of the mounted data volume so that the non-root
// main container (uid/gid 1000) can write to it. This compensates for storage
// backends (hostPath, certain CSI drivers) that don't honour the pod-level
// fsGroup. Only applied to custom-type pods, since other instance types run
// as root and don't need the fixup.
//
// We reuse the main container image to avoid pulling an additional image at
// pod startup; the image already ships sh + chown + chmod (as proven by the
// supervisor script). The init container itself overrides runAsUser=0 so the
// chown succeeds regardless of the image's default user.
func buildPermissionInitContainers(config PodConfig, pullPolicy corev1.PullPolicy) []corev1.Container {
	if !isCustomInstance(config.Type) {
		return nil
	}
	if config.MountPath == "" {
		return nil
	}
	uid := customPodInitRunAsRoot
	gid := customPodFSGroup
	script := fmt.Sprintf(
		"set -eu; mp=%q; mkdir -p \"$mp\"; chown -R %d:%d \"$mp\"; chmod -R u+rwX,g+rwX \"$mp\"",
		config.MountPath, gid, gid,
	)
	return []corev1.Container{
		{
			Name:            "init-perms",
			Image:           config.Image,
			ImagePullPolicy: pullPolicy,
			Command:         []string{"sh", "-c", script},
			SecurityContext: &corev1.SecurityContext{
				RunAsUser: &uid,
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "data", MountPath: config.MountPath},
			},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("10m"),
					corev1.ResourceMemory: resource.MustParse("16Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("64Mi"),
				},
			},
		},
	}
}

func buildStartupProbe(instanceType string, port int32) *corev1.Probe {
	if isCustomInstance(instanceType) {
		return nil
	}
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{Port: intstrFromInt32(port)},
		},
		FailureThreshold: 30,
		PeriodSeconds:    5,
		TimeoutSeconds:   2,
	}
}

func buildReadinessProbe(instanceType string, port int32) *corev1.Probe {
	if isCustomInstance(instanceType) {
		return nil
	}
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{Port: intstrFromInt32(port)},
		},
		InitialDelaySeconds: 3,
		PeriodSeconds:       5,
		TimeoutSeconds:      2,
		FailureThreshold:    6,
	}
}

func buildLivenessProbe(instanceType string, port int32) *corev1.Probe {
	if isCustomInstance(instanceType) {
		return nil
	}
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{Port: intstrFromInt32(port)},
		},
		InitialDelaySeconds: 15,
		PeriodSeconds:       10,
		TimeoutSeconds:      2,
		FailureThreshold:    3,
	}
}

func (s *PodService) waitForPodDeletion(ctx context.Context, namespace, podName string) error {
	waitCtx, cancel := context.WithTimeout(ctx, podDeletionTimeout)
	defer cancel()

	ticker := time.NewTicker(podDeletionPollInterval)
	defer ticker.Stop()

	for {
		_, err := s.client.Clientset.CoreV1().Pods(namespace).Get(waitCtx, podName, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("failed to check pod %s: %w", podName, err)
		}

		select {
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("timed out waiting for pod %s deletion", podName)
		case <-ticker.C:
		}
	}
}
