package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"github.com/assumeengagetry/distributed-commerce/internal/config"
	"github.com/assumeengagetry/distributed-commerce/internal/redisclient"
)

type archivedTask struct {
	ID           string    `json:"id"`
	Type         string    `json:"type"`
	Retried      int       `json:"retried"`
	MaxRetry     int       `json:"max_retry"`
	LastError    string    `json:"last_error"`
	LastFailedAt time.Time `json:"last_failed_at"`
}

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: job-admin list [PAGE] | retry TASK_ID | delete TASK_ID")
	}
	cfg, err := config.LoadJobAdmin()
	if err != nil {
		return err
	}
	queueRedis, err := redisclient.New(cfg.Redis)
	if err != nil {
		return fmt.Errorf("create queue Redis client: %w", err)
	}
	defer queueRedis.Close()
	pingCtx, cancel := context.WithTimeout(context.Background(), cfg.Redis.DialTimeout)
	err = queueRedis.Ping(pingCtx).Err()
	cancel()
	if err != nil {
		return fmt.Errorf("ping queue Redis: %w", err)
	}
	inspector := asynq.NewInspectorFromRedisClient(queueRedis)

	switch os.Args[1] {
	case "list":
		page := 1
		if len(os.Args) == 3 {
			page, err = strconv.Atoi(os.Args[2])
			if err != nil || page < 1 {
				return fmt.Errorf("usage: job-admin list [PAGE]")
			}
		} else if len(os.Args) != 2 {
			return fmt.Errorf("usage: job-admin list [PAGE]")
		}
		tasks, err := inspector.ListArchivedTasks(cfg.Queue, asynq.Page(page), asynq.PageSize(100))
		if err != nil && !errors.Is(err, asynq.ErrQueueNotFound) {
			return fmt.Errorf("list archived tasks: %w", err)
		}
		response := make([]archivedTask, 0, len(tasks))
		for _, task := range tasks {
			response = append(response, archivedTask{
				ID: task.ID, Type: task.Type, Retried: task.Retried, MaxRetry: task.MaxRetry,
				LastError: task.LastErr, LastFailedAt: task.LastFailedAt,
			})
		}
		if err := json.NewEncoder(os.Stdout).Encode(response); err != nil {
			return fmt.Errorf("encode archived tasks: %w", err)
		}
		return nil
	case "retry", "delete":
		if len(os.Args) != 3 || os.Args[2] == "" {
			return fmt.Errorf("usage: job-admin %s TASK_ID", os.Args[1])
		}
		return mutateArchivedTask(queueRedis, inspector, cfg.Queue, os.Args[2], os.Args[1])
	default:
		return fmt.Errorf("usage: job-admin list [PAGE] | retry TASK_ID | delete TASK_ID")
	}
}

var releaseLockScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0
`)

func mutateArchivedTask(
	redisClient *redis.Client,
	inspector *asynq.Inspector,
	queue, taskID, operation string,
) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lockKey := "dc:v1:jobs:admin-lock:" + queue + ":" + taskID
	lockToken := uuid.NewString()
	locked, err := redisClient.SetNX(ctx, lockKey, lockToken, 10*time.Second).Result()
	if err != nil {
		return fmt.Errorf("acquire archived task lock: %w", err)
	}
	if !locked {
		return fmt.Errorf("archived task is already being modified")
	}
	defer func() {
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), time.Second)
		defer releaseCancel()
		_ = releaseLockScript.Run(releaseCtx, redisClient, []string{lockKey}, lockToken).Err()
	}()

	info, err := inspector.GetTaskInfo(queue, taskID)
	if err != nil {
		return fmt.Errorf("inspect archived task: %w", err)
	}
	if info.State != asynq.TaskStateArchived {
		return fmt.Errorf("task %s is not archived", taskID)
	}
	if operation == "retry" {
		if err := inspector.RunTask(queue, taskID); err != nil {
			return fmt.Errorf("retry archived task: %w", err)
		}
		return nil
	}
	if err := inspector.DeleteTask(queue, taskID); err != nil {
		return fmt.Errorf("delete archived task: %w", err)
	}
	return nil
}
