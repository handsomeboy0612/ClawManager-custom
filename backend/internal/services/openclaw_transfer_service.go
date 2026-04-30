package services

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"clawreef/internal/services/k8s"
	k8sexec "k8s.io/client-go/util/exec"
)

const (
	openclawConfigDirName      = ".openclaw"
	openclawBaseDir            = "/config"
	openclawExportEmptyExitCode = 42
)

// ErrOpenClawWorkspaceMissing is returned by Export when the .openclaw
// workspace does not exist inside the desktop container. Handlers should
// map this to an HTTP 404 rather than returning an empty 200 body.
var ErrOpenClawWorkspaceMissing = errors.New("openclaw workspace is empty or missing")

type OpenClawTransferService interface {
	Export(ctx context.Context, userID, instanceID int) ([]byte, error)
	Import(ctx context.Context, userID, instanceID int, archive io.Reader) error
}

type openClawTransferService struct {
	podService  *k8s.PodService
	execService *k8s.ExecService
}

func NewOpenClawTransferService() OpenClawTransferService {
	return &openClawTransferService{
		podService:  k8s.NewPodService(),
		execService: k8s.NewExecService(),
	}
}

// buildBaseDirExpr returns a POSIX shell expression that resolves the
// OpenClaw persistent directory inside the desktop container. It honors the
// CLAWMANAGER_AGENT_PERSISTENT_DIR env var (injected by ClawManager at pod
// creation) and falls back to the hardcoded PVC mount path.
//
// $HOME is intentionally NOT used: `kubectl exec` spawns a fresh process as
// root with HOME=/root, which does not match the linuxserver entrypoint's
// runtime user `abc` (HOME=/config).
func buildBaseDirExpr() string {
	return fmt.Sprintf("${CLAWMANAGER_AGENT_PERSISTENT_DIR:-%s}", openclawBaseDir)
}

// buildExportCommand returns the sh -lc command used to stream a gzipped
// tarball of the .openclaw workspace from the desktop container over stdout.
// When the workspace does not exist, the command exits with
// openclawExportEmptyExitCode so the service layer can map it to
// ErrOpenClawWorkspaceMissing instead of returning an empty archive.
func buildExportCommand() []string {
	script := fmt.Sprintf(
		`base_dir="%s"; target_dir="$base_dir/%s"; `+
			`if [ ! -d "$target_dir" ]; then exit %d; fi; `+
			`tar czf - -C "$base_dir" %s`,
		buildBaseDirExpr(),
		openclawConfigDirName,
		openclawExportEmptyExitCode,
		shellQuote(openclawConfigDirName),
	)
	return []string{"sh", "-lc", script}
}

// buildImportCommand returns the sh -lc command used to restore a gzipped
// tarball of the .openclaw workspace into the desktop container from stdin.
// The extract is re-exec'd as user `abc` (uid 1000) via `su` so restored
// files are owned by the runtime user, matching how the linuxserver
// entrypoint writes /config.
func buildImportCommand() []string {
	inner := fmt.Sprintf(
		`base_dir="%s"; target_dir="$base_dir/%s"; `+
			`rm -rf "$target_dir" && mkdir -p "$base_dir" && tar xzf - -C "$base_dir"`,
		buildBaseDirExpr(),
		openclawConfigDirName,
	)
	outer := fmt.Sprintf(`exec su abc -s /bin/sh -c %s`, shellQuote(inner))
	return []string{"sh", "-lc", outer}
}

func (s *openClawTransferService) Export(ctx context.Context, userID, instanceID int) ([]byte, error) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := s.exec(ctx, userID, instanceID, buildExportCommand(), nil, &stdout, &stderr); err != nil {
		if isExportEmptyWorkspaceError(err) {
			return nil, ErrOpenClawWorkspaceMissing
		}
		return nil, formatExecError("export .openclaw", err, stderr.String())
	}

	return stdout.Bytes(), nil
}

// isExportEmptyWorkspaceError reports whether err indicates the export
// command exited with openclawExportEmptyExitCode (signalling that the
// .openclaw workspace does not exist).
func isExportEmptyWorkspaceError(err error) bool {
	if err == nil {
		return false
	}
	var codeErr k8sexec.CodeExitError
	if errors.As(err, &codeErr) {
		return codeErr.Code == openclawExportEmptyExitCode
	}
	// Fallback: remotecommand sometimes wraps the exit code in a plain
	// error whose message contains "exit code N".
	return strings.Contains(err.Error(), fmt.Sprintf("exit code %d", openclawExportEmptyExitCode))
}

func (s *openClawTransferService) Import(ctx context.Context, userID, instanceID int, archive io.Reader) error {
	var stderr bytes.Buffer
	if err := s.exec(ctx, userID, instanceID, buildImportCommand(), archive, nil, &stderr); err != nil {
		return formatExecError("import .openclaw", err, stderr.String())
	}

	return nil
}

func (s *openClawTransferService) exec(ctx context.Context, userID, instanceID int, command []string, stdin io.Reader, stdout, stderr io.Writer) error {
	pod, err := s.podService.GetPod(ctx, userID, instanceID)
	if err != nil {
		return fmt.Errorf("failed to get pod: %w", err)
	}

	return s.execService.Exec(ctx, k8s.ExecOptions{
		Namespace: pod.Namespace,
		PodName:   pod.Name,
		Container: "desktop",
		Command:   command,
		Stdin:     stdin,
		Stdout:    stdout,
		Stderr:    stderr,
	})
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func formatExecError(action string, execErr error, stderr string) error {
	if stderr != "" {
		return fmt.Errorf("failed to %s: %s", action, stderr)
	}
	return fmt.Errorf("failed to %s: %w", action, execErr)
}
