package telemetry

import (
	"context"
	"runtime/debug"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

var buildMeter = otel.Meter("go-chat-msa/metrics/build")

func init() {
	goVersion := "unknown"
	revision := "unknown"
	vcsTime := "unknown"
	modified := "unknown"

	if info, ok := debug.ReadBuildInfo(); ok {
		goVersion = info.GoVersion
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				revision = s.Value
			case "vcs.time":
				vcsTime = s.Value
			case "vcs.modified":
				modified = s.Value
			}
		}
	}

	buildMeter.Float64ObservableGauge("gochat_build_info",
		metric.WithDescription("런타임 빌드 정보"),
		metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
			o.Observe(1, metric.WithAttributes(
				attribute.String("goversion", goVersion),
				attribute.String("vcs_revision", revision),
				attribute.String("vcs_time", vcsTime),
				attribute.String("vcs_modified", modified),
			))
			return nil
		}),
	)
}
