package telemetry

import (
	"context"
	"errors"
	"fmt"
	"time"

	pyroscope "github.com/grafana/pyroscope-go"

	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

const metricInterval = 15 * time.Second

func InitOTel(ctx context.Context, serviceName, otelEndpoint string) (shutdown func(context.Context) error, err error) {
	if serviceName == "" {
		return nil, errors.New("serviceName must not be empty")
	}
	if otelEndpoint == "" {
		return nil, errors.New("otelEndpoint must not be empty")
	}

	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithProcess(),
		resource.WithOS(),
		resource.WithHost(),
		resource.WithAttributes(semconv.ServiceName(serviceName)),
	)
	if err != nil {
		return nil, fmt.Errorf("create resource: %w", err)
	}

	traceExporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(otelEndpoint),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("create trace exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(0.1))),
	)

	metricExporter, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithEndpoint(otelEndpoint),
		otlpmetrichttp.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("create metric exporter: %w", err)
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter,
			sdkmetric.WithInterval(metricInterval),
		)),
	)

	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	if err := runtime.Start(runtime.WithMinimumReadMemStatsInterval(metricInterval)); err != nil {
		return nil, fmt.Errorf("start runtime instrumentation: %w", err)
	}

	return func(ctx context.Context) error {
		tpErr := tp.Shutdown(ctx)
		mpErr := mp.Shutdown(ctx)
		if tpErr != nil {
			return tpErr
		}
		return mpErr
	}, nil
}

func InitProfiling(serviceName, pyroscopeEndpoint string) (stop func(), err error) {
	profiler, err := pyroscope.Start(pyroscope.Config{
		ApplicationName: serviceName,
		ServerAddress:   pyroscopeEndpoint,
		Logger:          nil,

		ProfileTypes: []pyroscope.ProfileType{
			pyroscope.ProfileCPU,
			pyroscope.ProfileInuseObjects,
			pyroscope.ProfileInuseSpace,
			pyroscope.ProfileGoroutines,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("start pyroscope: %w", err)
	}

	return func() { _ = profiler.Stop() }, nil
}
