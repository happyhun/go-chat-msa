package hub

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestSession_CloseMetricsRecordedOnce(t *testing.T) {
	for _, overflow := range []bool{false, true} {
		name := "room closure"
		if overflow {
			name = "overflow before room closure"
		}
		t.Run(name, func(t *testing.T) {
			reader := sdkmetric.NewManualReader()
			provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			defer func() { require.NoError(t, provider.Shutdown(context.Background())) }()
			counter, err := provider.Meter("test").Int64Counter("sessions_closed")
			require.NoError(t, err)
			previous := sessionsClosedTotal
			sessionsClosedTotal = counter
			defer func() { sessionsClosedTotal = previous }()

			serverConn, clientConn := createTestWSPair(t)
			defer serverConn.Close()
			defer clientConn.Close()
			s := newTestSession(serverConn, "user", "room", nil)
			h := newTestHub("room")
			h.sessions[s.id] = s
			wantReason := closeReasonRoomClosed
			if overflow {
				for range sendBufferSize + 1 {
					s.send(t.Context(), egressPacket{data: []byte(`{"content":"queued"}`)})
				}
				wantReason = "send_queue_overflow"
			}
			h.closing.Store(&closeSignal{code: closeCodeTryAgainLater, reason: closeReasonRoomClosed})
			h.shutdown()
			s.closeWithCode(closeCodeServiceRestart, closeReasonShutdown)

			var data metricdata.ResourceMetrics
			require.NoError(t, reader.Collect(t.Context(), &data))
			require.Len(t, data.ScopeMetrics, 1)
			require.Len(t, data.ScopeMetrics[0].Metrics, 1)
			sum, ok := data.ScopeMetrics[0].Metrics[0].Data.(metricdata.Sum[int64])
			require.True(t, ok)
			require.Len(t, sum.DataPoints, 1)
			assert.Equal(t, int64(1), sum.DataPoints[0].Value)
			reason, ok := sum.DataPoints[0].Attributes.Value("reason")
			require.True(t, ok)
			assert.Equal(t, wantReason, reason.AsString())
		})
	}
}
