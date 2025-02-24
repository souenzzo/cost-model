package retry

import (
	"context"
	"fmt"
	"math/rand"
	"time"
)

// RetryCancellationErr is the error type that's returned if the retry is cancelled
var RetryCancellationErr error = fmt.Errorf("RetryCancellationErr")

// IsRetryCancelledError returns true if the error was a cancellation
func IsRetryCancelledError(err error) bool {
	return err != nil && err.Error() == "RetryCancellationErr"
}

// Retry will run the f func until we receive a non error result up to the provided attempts or a cancellation.
func Retry(ctx context.Context, f func() (interface{}, error), attempts uint, delay time.Duration) (interface{}, error) {
	var result interface{}
	var err error

	d := delay
	for r := attempts; r > 0; r-- {
		select {
		case <-ctx.Done():
			return nil, RetryCancellationErr
		default:
		}

		result, err = f()

		if err == nil {
			break
		}

		time.Sleep(d)

		jitter := time.Duration(rand.Int63n(int64(d))) // #nosec No need for a cryptographic strength random here
		d = d + jitter/2
	}

	return result, err
}
