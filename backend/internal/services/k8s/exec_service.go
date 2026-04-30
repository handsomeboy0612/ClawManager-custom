package k8s

import (
	"context"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

// ExecService runs synchronous commands inside a running container via
// the Kubernetes pod/exec subresource over SPDY. It is the lower-level
// transport shared by all features that need to drive in-pod commands:
//   - openclaw_transfer_service: streams tar archives over stdin/stdout
//   - openclaw_runtime_config_service: pipes JSON to `openclaw config set`
//
// Centralising the SPDY plumbing here means there is exactly one place
// to apply timeout, auth, and TLS settings, and one place to audit when
// upgrading client-go.
type ExecService struct {
	client *Client
}

// NewExecService returns a ExecService bound to the global K8s client.
func NewExecService() *ExecService {
	return &ExecService{client: GetClient()}
}

// ExecOptions describes a single exec call.
//
// Stdin/Stdout/Stderr are wired through SPDY directly; for binary or
// large payloads (tarballs, multi-MB JSON) prefer streaming via these
// io.Reader/Writer fields rather than embedding payloads in Command —
// this avoids the ARG_MAX limit (~128 KiB on Linux) and keeps shell
// metachars out of the command line entirely.
type ExecOptions struct {
	// Namespace is the pod's namespace. Required.
	Namespace string
	// PodName is the target pod's name. Required.
	PodName string
	// Container is the target container name within the pod. Required —
	// pod/exec rejects requests without an explicit container when a pod
	// has multiple containers, so we always pass it.
	Container string
	// Command is the argv to exec inside the container. Must be non-empty.
	Command []string
	// Stdin is an optional input stream piped to the command. Pass nil
	// when the command does not read from stdin.
	Stdin io.Reader
	// Stdout, Stderr capture the command's output streams. Either may be
	// nil to discard, or set to a real writer for capture.
	Stdout io.Writer
	Stderr io.Writer
}

// Exec performs a synchronous exec call. Returns nil on the command
// exit code 0. Non-zero exits surface as a wrapped error whose
// underlying type is k8s.io/client-go/util/exec.CodeExitError; callers
// can use errors.As to recover the exit code.
//
// The provided ctx controls the lifetime of the SPDY stream: cancelling
// ctx aborts the exec and returns ctx.Err().
func (s *ExecService) Exec(ctx context.Context, opts ExecOptions) error {
	if s == nil || s.client == nil || s.client.Clientset == nil || s.client.Config == nil {
		return fmt.Errorf("k8s client not initialized")
	}
	if opts.Namespace == "" || opts.PodName == "" {
		return fmt.Errorf("exec: namespace and pod name are required")
	}
	if opts.Container == "" {
		return fmt.Errorf("exec: container name is required")
	}
	if len(opts.Command) == 0 {
		return fmt.Errorf("exec: command is required")
	}

	req := s.client.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(opts.PodName).
		Namespace(opts.Namespace).
		SubResource("exec")

	req.VersionedParams(&corev1.PodExecOptions{
		Container: opts.Container,
		Command:   opts.Command,
		Stdin:     opts.Stdin != nil,
		Stdout:    opts.Stdout != nil,
		Stderr:    opts.Stderr != nil,
		TTY:       false,
	}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(s.client.Config, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("failed to initialize exec stream: %w", err)
	}

	return executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  opts.Stdin,
		Stdout: opts.Stdout,
		Stderr: opts.Stderr,
		Tty:    false,
	})
}
