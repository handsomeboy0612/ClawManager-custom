package k8s

import "testing"

// TestBuildReadinessProbe_Gating verifies the readiness probe is attached for
// built-in types and for OpenClaw gateways running as type=custom (identified
// by openClawGatewayPort), but skipped for other custom pods whose container
// may not listen on its ContainerPort.
func TestBuildReadinessProbe_Gating(t *testing.T) {
	cases := []struct {
		name         string
		instanceType string
		port         int32
		wantProbe    bool
	}{
		{"openclaw_custom_gets_probe", "custom", openClawGatewayPort, true},
		{"other_custom_skipped", "custom", 3001, false},
		{"builtin_ubuntu_gets_probe", "ubuntu", 3001, true},
		{"builtin_openclaw_type_gets_probe", "openclaw", 3001, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe := buildReadinessProbe(tc.instanceType, tc.port)
			if tc.wantProbe {
				if probe == nil {
					t.Fatalf("expected a readiness probe, got nil")
				}
				if probe.TCPSocket == nil {
					t.Fatalf("expected a TCPSocket readiness probe")
				}
				if got := int32(probe.TCPSocket.Port.IntValue()); got != tc.port {
					t.Errorf("probe port = %d, want %d", got, tc.port)
				}
			} else if probe != nil {
				t.Errorf("expected no readiness probe, got %+v", probe)
			}
		})
	}
}

// TestBuildLivenessAndStartupProbe_CustomAlwaysSkipped guards the deliberate
// decision NOT to add liveness/startup probes to custom pods — including the
// OpenClaw gateway. Under RestartPolicyNever a failing liveness probe would
// kill the container without a restart and fight the in-pod supervisor's own
// process-level self-heal, so only readiness is enabled for custom pods.
func TestBuildLivenessAndStartupProbe_CustomAlwaysSkipped(t *testing.T) {
	if p := buildLivenessProbe("custom", openClawGatewayPort); p != nil {
		t.Errorf("liveness probe must stay nil for custom OpenClaw gateway, got %+v", p)
	}
	if p := buildStartupProbe("custom", openClawGatewayPort); p != nil {
		t.Errorf("startup probe must stay nil for custom OpenClaw gateway, got %+v", p)
	}
	if p := buildLivenessProbe("custom", 3001); p != nil {
		t.Errorf("liveness probe must stay nil for other custom pods, got %+v", p)
	}
}
