package main

import (
	"context"
	"errors"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
)

func TestMutateArchivedTaskRejectsLiveStates(t *testing.T) {
	t.Parallel()
	redisClient, inspector, taskID := testTask(t, "pending")
	if err := mutateArchivedTask(redisClient, inspector, "maintenance", taskID, "delete"); err == nil {
		t.Fatal("mutateArchivedTask() error = nil for pending task")
	}
	info, err := inspector.GetTaskInfo("maintenance", taskID)
	if err != nil || info.State != asynq.TaskStatePending {
		t.Fatalf("pending task after rejection = (%+v, %v)", info, err)
	}
}

func TestMutateArchivedTaskRetriesAndDeletesArchivedTasks(t *testing.T) {
	t.Parallel()
	redisClient, inspector, retryID := testTask(t, "retry")
	if err := inspector.ArchiveTask("maintenance", retryID); err != nil {
		t.Fatalf("ArchiveTask(retry) error = %v", err)
	}
	if err := mutateArchivedTask(redisClient, inspector, "maintenance", retryID, "retry"); err != nil {
		t.Fatalf("mutateArchivedTask(retry) error = %v", err)
	}
	info, err := inspector.GetTaskInfo("maintenance", retryID)
	if err != nil || info.State != asynq.TaskStatePending {
		t.Fatalf("retried task = (%+v, %v)", info, err)
	}

	_, inspector, deleteID := testTaskWithClient(t, redisClient, "delete")
	if err := inspector.ArchiveTask("maintenance", deleteID); err != nil {
		t.Fatalf("ArchiveTask(delete) error = %v", err)
	}
	if err := mutateArchivedTask(redisClient, inspector, "maintenance", deleteID, "delete"); err != nil {
		t.Fatalf("mutateArchivedTask(delete) error = %v", err)
	}
	if _, err := inspector.GetTaskInfo("maintenance", deleteID); !errors.Is(err, asynq.ErrTaskNotFound) {
		t.Fatalf("GetTaskInfo(deleted) error = %v, want ErrTaskNotFound", err)
	}
}

func testTask(t *testing.T, payload string) (*redis.Client, *asynq.Inspector, string) {
	t.Helper()
	server := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	return testTaskWithClient(t, redisClient, payload)
}

func testTaskWithClient(t *testing.T, redisClient *redis.Client, payload string) (*redis.Client, *asynq.Inspector, string) {
	t.Helper()
	client := asynq.NewClientFromRedisClient(redisClient)
	info, err := client.Enqueue(asynq.NewTask("test:task", []byte(payload)), asynq.Queue("maintenance"))
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	if err := redisClient.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("Ping() error = %v", err)
	}
	return redisClient, asynq.NewInspectorFromRedisClient(redisClient), info.ID
}
