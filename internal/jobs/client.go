package jobs

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
)

const SessionCleanupTaskType = "auth:prune-expired-sessions:v1"

const (
	sessionCleanupPayload  = `{"schema":1}`
	sessionCleanupMaxRetry = 5
)

type ClientConfig struct {
	Queue          string
	EnqueueTimeout time.Duration
	TaskTimeout    time.Duration
	UniqueTTL      time.Duration
}

type Client struct {
	client         *asynq.Client
	queue          string
	enqueueTimeout time.Duration
	taskTimeout    time.Duration
	uniqueTTL      time.Duration
}

func NewClient(redisClient redis.UniversalClient, cfg ClientConfig) (*Client, error) {
	if nilUniversalClient(redisClient) {
		return nil, fmt.Errorf("redis client is required")
	}
	if !validQueueName(cfg.Queue) {
		return nil, fmt.Errorf("job queue is invalid")
	}
	if cfg.EnqueueTimeout <= 0 {
		return nil, fmt.Errorf("enqueue timeout must be positive")
	}
	if cfg.TaskTimeout <= 0 {
		return nil, fmt.Errorf("task timeout must be positive")
	}
	if cfg.UniqueTTL < time.Second {
		return nil, fmt.Errorf("unique TTL must be at least one second")
	}

	return &Client{
		client:         asynq.NewClientFromRedisClient(redisClient),
		queue:          cfg.Queue,
		enqueueTimeout: cfg.EnqueueTimeout,
		taskTimeout:    cfg.TaskTimeout,
		uniqueTTL:      cfg.UniqueTTL,
	}, nil
}

func validQueueName(value string) bool {
	if strings.TrimSpace(value) != value || len(value) == 0 || len(value) > 64 {
		return false
	}
	for index := range len(value) {
		character := value[index]
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' ||
			character == ':' || character == '-' || character == '_' {
			continue
		}
		return false
	}
	return true
}

func (c *Client) ScheduleSessionCleanup(ctx context.Context) error {
	enqueueCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.enqueueTimeout)
	defer cancel()

	task := NewSessionCleanupTask()
	_, err := c.client.EnqueueContext(
		enqueueCtx,
		task,
		append(SessionCleanupOptions(c.queue, c.taskTimeout),
			asynq.ProcessIn(c.uniqueTTL), asynq.Unique(c.uniqueTTL+c.taskTimeout))...,
	)
	if errors.Is(err, asynq.ErrDuplicateTask) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("enqueue session cleanup task: %w", err)
	}
	return nil
}

func NewSessionCleanupTask() *asynq.Task {
	return asynq.NewTask(SessionCleanupTaskType, []byte(sessionCleanupPayload))
}

func SessionCleanupOptions(queue string, taskTimeout time.Duration) []asynq.Option {
	return []asynq.Option{
		asynq.Queue(queue), asynq.MaxRetry(sessionCleanupMaxRetry), asynq.Timeout(taskTimeout),
	}
}

func nilUniversalClient(client redis.UniversalClient) bool {
	if client == nil {
		return true
	}
	value := reflect.ValueOf(client)
	return value.Kind() == reflect.Ptr && value.IsNil()
}
