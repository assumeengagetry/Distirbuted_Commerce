package process

import (
	"context"
	"errors"
	"fmt"
)

type Server func(context.Context) error

func Run(ctx context.Context, servers ...Server) error {
	if ctx == nil || len(servers) == 0 {
		return fmt.Errorf("process context and at least one server are required")
	}
	for _, serve := range servers {
		if serve == nil {
			return fmt.Errorf("process server is required")
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, len(servers))
	for _, serve := range servers {
		go func() {
			results <- serve(runCtx)
		}()
	}
	var result error
	for range servers {
		err := <-results
		if err != nil {
			result = errors.Join(result, err)
		}
		cancel()
	}
	return result
}
