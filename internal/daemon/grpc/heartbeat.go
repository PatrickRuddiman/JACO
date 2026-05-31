package grpc

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/runtime/cgroupv2"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// pressureHeartbeat ticks at interval, samples the local cgroup
// collector, and submits a NodeStatusUpdate{IncludePressure:true}
// through the leader-or-forward helper. Status is left UNSPECIFIED so
// the FSM preserves whatever value the firewall reconciler / membership
// set most recently (see fsm.go Command_NodeStatusUpdate handler).
//
// On collector !ok (non-Linux, missing cgroup v2, unprivileged
// container) the heartbeat skips that tick entirely — the leader sees
// LastPressureAt stay flat, the state-backed source's freshness gate
// rejects the node, the rebalancer keeps the node out of scoring.
//
// Errors from applyOrForward are logged at debug. Sustained failure
// is benign — the leader simply doesn't get fresh samples and the
// rebalancer dormant for that node.
func pressureHeartbeat(
	ctx context.Context,
	logger *slog.Logger,
	hostname string,
	interval time.Duration,
	collector *cgroupv2.Collector,
	apply func(context.Context, []byte) error,
) error {
	if interval <= 0 {
		// Heartbeat disabled — rebalancer stays dormant. Useful in
		// tests; the daemon clamps this to a sensible minimum at
		// config-validation time.
		<-ctx.Done()
		return ctx.Err()
	}
	logger.Info("pressure heartbeat started", "interval", interval)
	t := time.NewTicker(interval)
	defer t.Stop()
	var ticks uint64
	var firstOk bool
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			ticks++
			sample := collector.Read()
			if !sample.Ok {
				if ticks%10 == 1 {
					// Log periodically so an operator can tell
					// the heartbeat is alive but the collector is
					// returning no data (typical on non-Linux,
					// unprivileged container, missing cgroup v2).
					logger.Info("pressure heartbeat: collector returned !ok",
						"tick", ticks)
				}
				continue
			}
			if !firstOk {
				logger.Info("pressure heartbeat: first sample ok",
					"cpu", sample.CPU, "memory", sample.Memory)
				firstOk = true
			}
			cmd := &pb.Command{
				Identity: "pressure-heartbeat",
				Ts:       timestamppb.Now(),
				Payload: &pb.Command_NodeStatusUpdate{NodeStatusUpdate: &pb.NodeStatusUpdate{
					Hostname:        hostname,
					IncludePressure: true,
					CpuPressure:     sample.CPU,
					MemoryPressure:  sample.Memory,
				}},
			}
			data, err := proto.Marshal(cmd)
			if err != nil {
				logger.Debug("pressure heartbeat marshal", "error", err)
				continue
			}
			if err := apply(ctx, data); err != nil && !errors.Is(err, context.Canceled) {
				logger.Debug("pressure heartbeat submit", "error", err)
			}
		}
	}
}
