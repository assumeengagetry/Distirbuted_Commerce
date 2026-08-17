package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"github.com/assumeengagetry/distributed-commerce/internal/observability"
)

func TestNewClientValidation(t *testing.T) {
	_, redisClient := newTestRedis(t)
	valid := testClientConfig()

	var typedNil *redis.Client
	tests := []struct {
		name   string
		client redis.UniversalClient
		cfg    ClientConfig
	}{
		{name: "nil redis client", cfg: valid},
		{name: "typed nil redis client", client: typedNil, cfg: valid},
		{name: "empty queue", client: redisClient, cfg: withClientConfig(valid, func(cfg *ClientConfig) { cfg.Queue = "" })},
		{name: "blank queue", client: redisClient, cfg: withClientConfig(valid, func(cfg *ClientConfig) { cfg.Queue = "  " })},
		{name: "invalid queue", client: redisClient, cfg: withClientConfig(valid, func(cfg *ClientConfig) { cfg.Queue = "Invalid Queue" })},
		{name: "zero enqueue timeout", client: redisClient, cfg: withClientConfig(valid, func(cfg *ClientConfig) { cfg.EnqueueTimeout = 0 })},
		{name: "negative enqueue timeout", client: redisClient, cfg: withClientConfig(valid, func(cfg *ClientConfig) { cfg.EnqueueTimeout = -time.Second })},
		{name: "zero task timeout", client: redisClient, cfg: withClientConfig(valid, func(cfg *ClientConfig) { cfg.TaskTimeout = 0 })},
		{name: "negative task timeout", client: redisClient, cfg: withClientConfig(valid, func(cfg *ClientConfig) { cfg.TaskTimeout = -time.Second })},
		{name: "zero unique TTL", client: redisClient, cfg: withClientConfig(valid, func(cfg *ClientConfig) { cfg.UniqueTTL = 0 })},
		{name: "short unique TTL", client: redisClient, cfg: withClientConfig(valid, func(cfg *ClientConfig) { cfg.UniqueTTL = time.Second - 1 })},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if client, err := NewClient(test.client, test.cfg, observability.NoopProviders()); err == nil || client != nil {
				t.Fatalf("NewClient() = (%v, %v), want (nil, error)", client, err)
			}
		})
	}

	if client, err := NewClient(redisClient, valid, observability.NoopProviders()); err != nil || client == nil {
		t.Fatalf("NewClient() = (%v, %v), want a client", client, err)
	}
}

func TestScheduleSessionCleanupEnqueuesDeterministicUniqueTask(t *testing.T) {
	_, redisClient := newTestRedis(t)
	cfg := testClientConfig()
	client, err := NewClient(redisClient, cfg, observability.NoopProviders())
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	if err := client.ScheduleSessionCleanup(context.Background()); err != nil {
		t.Fatalf("first ScheduleSessionCleanup() error = %v", err)
	}
	if err := client.ScheduleSessionCleanup(context.Background()); err != nil {
		t.Fatalf("duplicate ScheduleSessionCleanup() error = %v", err)
	}

	inspector := asynq.NewInspectorFromRedisClient(redisClient)
	tasks, err := inspector.ListScheduledTasks(cfg.Queue)
	if err != nil {
		t.Fatalf("ListScheduledTasks() error = %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("pending task count = %d, want 1", len(tasks))
	}
	got := tasks[0]
	if got.Type != SessionCleanupTaskType {
		t.Errorf("task type = %q, want %q", got.Type, SessionCleanupTaskType)
	}
	if string(got.Payload) != sessionCleanupPayload {
		t.Errorf("payload = %q, want %q", got.Payload, sessionCleanupPayload)
	}
	if got.Queue != cfg.Queue {
		t.Errorf("queue = %q, want %q", got.Queue, cfg.Queue)
	}
	if got.MaxRetry != sessionCleanupMaxRetry {
		t.Errorf("max retry = %d, want %d", got.MaxRetry, sessionCleanupMaxRetry)
	}
	if got.Timeout != cfg.TaskTimeout {
		t.Errorf("timeout = %v, want %v", got.Timeout, cfg.TaskTimeout)
	}

}

func TestScheduleSessionCleanupDetachesCanceledParent(t *testing.T) {
	_, redisClient := newTestRedis(t)
	cfg := testClientConfig()
	client, err := NewClient(redisClient, cfg, observability.NoopProviders())
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	parent, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.ScheduleSessionCleanup(parent); err != nil {
		t.Fatalf("ScheduleSessionCleanup() error = %v", err)
	}

	tasks, err := asynq.NewInspectorFromRedisClient(redisClient).ListScheduledTasks(cfg.Queue)
	if err != nil {
		t.Fatalf("ListScheduledTasks() error = %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("pending task count = %d, want 1", len(tasks))
	}
}

func newTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return mini, client
}

func testClientConfig() ClientConfig {
	return ClientConfig{
		Queue:          "maintenance",
		EnqueueTimeout: time.Second,
		TaskTimeout:    45 * time.Second,
		UniqueTTL:      time.Minute,
	}
}

func withClientConfig(cfg ClientConfig, change func(*ClientConfig)) ClientConfig {
	change(&cfg)
	return cfg
}
