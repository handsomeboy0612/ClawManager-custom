package services

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
)

// timeoutNetError is a minimal net.Error whose Timeout() reports true, used to
// exercise the timeout branch of isUpstreamUnreachable.
type timeoutNetError struct{}

func (timeoutNetError) Error() string   { return "i/o timeout" }
func (timeoutNetError) Timeout() bool   { return true }
func (timeoutNetError) Temporary() bool { return true }

// TestIsUpstreamUnreachable covers the structured classification of dial
// errors: refused / unreachable / reset / dial-timeout are "gateway not
// reachable", while client cancellation and application-level errors are not.
func TestIsUpstreamUnreachable(t *testing.T) {
	refused := &url.Error{
		Op:  "Get",
		URL: "http://10.0.0.1:18789/",
		Err: &net.OpError{
			Op:  "dial",
			Net: "tcp",
			Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED},
		},
	}
	dialGeneric := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("boom")}
	readTimeout := &net.OpError{Op: "read", Net: "tcp", Err: timeoutNetError{}}

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"connection_refused", refused, true},
		{"host_unreachable", &net.OpError{Op: "dial", Err: &os.SyscallError{Syscall: "connect", Err: syscall.EHOSTUNREACH}}, true},
		{"conn_reset", &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}, true},
		{"deadline_exceeded", context.DeadlineExceeded, true},
		{"dial_generic", dialGeneric, true},
		{"read_timeout", readTimeout, true},
		{"client_canceled", context.Canceled, false},
		{"app_level_error", errors.New("upstream returned 500"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUpstreamUnreachable(tc.err); got != tc.want {
				t.Errorf("isUpstreamUnreachable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestDescribeUpstreamUnreachable ensures the friendly message names the port
// and still wraps the original cause for logs.
func TestDescribeUpstreamUnreachable(t *testing.T) {
	cause := errors.New("dial tcp 10.0.0.1:18789: connect: connection refused")
	err := describeUpstreamUnreachable(18789, cause)
	if !errors.Is(err, cause) {
		t.Errorf("wrapped error must preserve the original cause via %%w")
	}
	if msg := err.Error(); !strings.Contains(msg, "18789") || !strings.Contains(msg, "not reachable") {
		t.Errorf("message should name the port and be human-readable, got: %s", msg)
	}
}
