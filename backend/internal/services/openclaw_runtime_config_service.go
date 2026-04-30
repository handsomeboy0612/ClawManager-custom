package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"clawreef/internal/models"
	"clawreef/internal/services/k8s"
)

// openclawRuntimeConfigUpdateScript is the in-pod shell command run via
// SPDY exec to apply a new OpenClaw bootstrap-style config payload.
//
// Wire format: caller pipes the raw `[ {path, value}, ... ]` JSON over
// stdin. The script captures it into a shell variable, then forwards it
// to `openclaw config set --batch-json` as a single argv element. This
// avoids two classes of bug:
//
//  1. Shell injection. The JSON never appears on the command line of
//     the supervising `sh -c ...`; it only enters the shell as the
//     value of a parameter expansion `"$JSON"`, which is NOT re-parsed
//     for command substitution / parameter expansion. So even payloads
//     containing `$()`, backticks, or `;` are inert.
//
//  2. ARG_MAX. SPDY exec passes argv unchanged, but if we put the JSON
//     into our outer command's argv via Go (rather than stdin), every
//     hop in the chain (Go → kube-apiserver → kubelet → CRI) has to
//     allocate copies. Stdin is streamed.
//
// After config-set succeeds, the script kills the gateway by exact
// process name. SPIKE 4 verified the OpenClaw gateway renames its
// process title to literally "openclaw" (not "node"), so `pkill -x`
// matches it without false positives. The supervisor (Phase 3) sees the
// gateway exit and re-launches it within ~1s — Pod, PVC, Service, and
// proxy URL stay unchanged, so existing browser sessions only need to
// reconnect, not re-authenticate.
//
// `pkill ... || true` is intentional: if the gateway has already
// exited (rare race during supervisor restart), we still want the
// config update itself to be reported as success.
const openclawRuntimeConfigUpdateScript = `set -e
JSON="$(cat)"
node /app/openclaw.mjs config set --replace --batch-json "$JSON"
pkill -TERM -x openclaw || true
`

// openclawRuntimeConfigMaxBytes caps the in-flight JSON payload size.
// 64 KiB is comfortably above any realistic OpenClaw bootstrap config
// (token + a handful of model definitions = a few KiB) but small enough
// that a malicious caller cannot DOS the kube-apiserver via a
// runaway exec stream.
const openclawRuntimeConfigMaxBytes = 64 * 1024

// OpenClawRuntimeConfigService applies live config updates to a running
// OpenClaw instance without restarting the Pod. It is the server-side
// counterpart of new-yunwu-api's "edit token / model" feature
// (Phase 5).
type OpenClawRuntimeConfigService interface {
	// UpdateRuntimeConfig pushes batchJSON (the same `[{path,value}]`
	// shape OpenClaw's `config set --batch-json` expects) into the
	// instance's running pod. Returns nil on success; returns an error
	// describing the failure (with stderr, when available) otherwise.
	UpdateRuntimeConfig(ctx context.Context, instance *models.Instance, batchJSON []byte) error
}

type openClawRuntimeConfigService struct {
	podService  *k8s.PodService
	execService *k8s.ExecService
}

// NewOpenClawRuntimeConfigService returns a service backed by the
// global K8s client.
func NewOpenClawRuntimeConfigService() OpenClawRuntimeConfigService {
	return &openClawRuntimeConfigService{
		podService:  k8s.NewPodService(),
		execService: k8s.NewExecService(),
	}
}

func (s *openClawRuntimeConfigService) UpdateRuntimeConfig(ctx context.Context, instance *models.Instance, batchJSON []byte) error {
	if instance == nil {
		return fmt.Errorf("instance is required")
	}
	if len(batchJSON) == 0 {
		return fmt.Errorf("batch JSON payload is required")
	}
	if len(batchJSON) > openclawRuntimeConfigMaxBytes {
		return fmt.Errorf("batch JSON payload too large: %d bytes (max %d)", len(batchJSON), openclawRuntimeConfigMaxBytes)
	}

	// Reject payloads that aren't valid JSON arrays of objects up front.
	// `openclaw config set --batch-json` would also reject them, but
	// failing here gives the caller a structured 400 instead of an
	// opaque "exit code 1 from sh" 500.
	var probe []map[string]interface{}
	if err := json.Unmarshal(batchJSON, &probe); err != nil {
		return fmt.Errorf("invalid batch JSON: %w", err)
	}
	if len(probe) == 0 {
		return fmt.Errorf("batch JSON must contain at least one config entry")
	}
	for i, entry := range probe {
		if _, ok := entry["path"]; !ok {
			return fmt.Errorf("batch JSON entry %d missing required 'path'", i)
		}
		if _, ok := entry["value"]; !ok {
			return fmt.Errorf("batch JSON entry %d missing required 'value'", i)
		}
	}

	if !strings.EqualFold(instance.Status, "running") {
		return fmt.Errorf("instance %d is not running (status=%s); start it before updating config", instance.ID, instance.Status)
	}

	pod, err := s.podService.GetPod(ctx, instance.UserID, instance.ID)
	if err != nil {
		return fmt.Errorf("failed to get pod: %w", err)
	}
	if pod == nil {
		return fmt.Errorf("pod for instance %d not found", instance.ID)
	}

	var stderr bytes.Buffer
	err = s.execService.Exec(ctx, k8s.ExecOptions{
		Namespace: pod.Namespace,
		PodName:   pod.Name,
		Container: "desktop",
		Command:   []string{"sh", "-c", openclawRuntimeConfigUpdateScript},
		Stdin:     bytes.NewReader(batchJSON),
		Stderr:    &stderr,
	})
	if err != nil {
		stderrStr := strings.TrimSpace(stderr.String())
		if stderrStr != "" {
			return fmt.Errorf("openclaw config update failed: %s: %w", stderrStr, err)
		}
		return fmt.Errorf("openclaw config update failed: %w", err)
	}
	return nil
}
