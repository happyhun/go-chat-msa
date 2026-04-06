package hasher

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/crypto/bcrypt"
)

type jobType int

const (
	jobHash jobType = iota
	jobCompare
)

var (
	ErrQueueFull = errors.New("hashing queue full")
	ErrClosed    = errors.New("worker pool is closed")
)

type PoolConfig struct {
	Workers int
	Buffer  int
}

type job struct {
	typ            jobType
	password       string
	hashedPassword string
	resultChan     chan error
	hashResultChan chan string
	errChan        chan error
}

type Pool struct {
	config    PoolConfig
	jobQueue  chan job
	wg        sync.WaitGroup
	isClosed  bool
	mu        sync.RWMutex
	closeOnce sync.Once
}

func DefaultPoolConfig() PoolConfig {
	workers := runtime.GOMAXPROCS(0)
	buffer := 100 * workers

	return PoolConfig{
		Workers: workers,
		Buffer:  buffer,
	}
}

func NewPool(cfg PoolConfig) *Pool {
	if cfg.Workers <= 0 {
		cfg.Workers = runtime.GOMAXPROCS(0)
	}
	if cfg.Buffer <= 0 {
		cfg.Buffer = 1024
	}
	wp := &Pool{
		config:   cfg,
		jobQueue: make(chan job, cfg.Buffer),
	}

	wp.startWorkers()
	return wp
}

func (wp *Pool) HashPassword(ctx context.Context, password string) (string, error) {
	resultChan := make(chan string, 1)
	errChan := make(chan error, 1)

	j := job{
		typ:            jobHash,
		password:       password,
		hashResultChan: resultChan,
		errChan:        errChan,
	}

	if err := wp.enqueue(ctx, j); err != nil {
		return "", err
	}

	select {
	case h := <-resultChan:
		return h, nil
	case err := <-errChan:
		return "", err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (wp *Pool) ComparePassword(ctx context.Context, hashedPassword, password string) error {
	resultChan := make(chan error, 1)

	j := job{
		typ:            jobCompare,
		hashedPassword: hashedPassword,
		password:       password,
		resultChan:     resultChan,
	}

	if err := wp.enqueue(ctx, j); err != nil {
		return err
	}

	select {
	case err := <-resultChan:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (wp *Pool) Close() {
	wp.closeOnce.Do(func() {
		wp.mu.Lock()
		wp.isClosed = true
		close(wp.jobQueue)
		wp.mu.Unlock()

		wp.wg.Wait()
	})
}

func (wp *Pool) startWorkers() {
	for i := 0; i < wp.config.Workers; i++ {
		wp.wg.Add(1)
		go wp.worker()
	}
}

func (wp *Pool) worker() {
	defer wp.wg.Done()

	ctx := context.Background()
	for j := range wp.jobQueue {
		hasherQueueDepth.Record(ctx, float64(len(wp.jobQueue)))
		typeName := jobTypeName(j.typ)
		typeAttr := attribute.String("type", typeName)
		start := time.Now()

		switch j.typ {
		case jobHash:
			hash, err := bcrypt.GenerateFromPassword([]byte(j.password), bcrypt.DefaultCost)
			hasherDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(typeAttr))
			if err != nil {
				hasherJobsTotal.Add(ctx, 1, metric.WithAttributes(typeAttr, attribute.String("status", "error")))
				select {
				case j.errChan <- err:
				default:
				}
			} else {
				hasherJobsTotal.Add(ctx, 1, metric.WithAttributes(typeAttr, attribute.String("status", "ok")))
				select {
				case j.hashResultChan <- string(hash):
				default:
				}
			}

		case jobCompare:
			err := bcrypt.CompareHashAndPassword([]byte(j.hashedPassword), []byte(j.password))
			hasherDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(typeAttr))
			if err != nil {
				hasherJobsTotal.Add(ctx, 1, metric.WithAttributes(typeAttr, attribute.String("status", "error")))
			} else {
				hasherJobsTotal.Add(ctx, 1, metric.WithAttributes(typeAttr, attribute.String("status", "ok")))
			}
			select {
			case j.resultChan <- err:
			default:
			}
		}
	}
}

func (wp *Pool) enqueue(ctx context.Context, j job) error {
	wp.mu.RLock()
	defer wp.mu.RUnlock()

	if wp.isClosed {
		return ErrClosed
	}

	typeName := jobTypeName(j.typ)
	typeAttr := attribute.String("type", typeName)
	select {
	case wp.jobQueue <- j:
		hasherQueueDepth.Record(ctx, float64(len(wp.jobQueue)))
		return nil
	case <-ctx.Done():
		hasherJobsTotal.Add(ctx, 1, metric.WithAttributes(typeAttr, attribute.String("status", "ctx_canceled")))
		return ctx.Err()
	default:
		hasherQueueFullTotal.Add(ctx, 1)
		hasherJobsTotal.Add(ctx, 1, metric.WithAttributes(typeAttr, attribute.String("status", "queue_full")))
		return ErrQueueFull
	}
}
